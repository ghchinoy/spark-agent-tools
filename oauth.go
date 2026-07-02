package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// protectedResourcePath is the RFC 9728 well-known base path. Clients probe it
// directly and also with the resource path appended (e.g. .../mcp), so we serve
// the exact path and the subtree.
const protectedResourcePath = "/.well-known/oauth-protected-resource"

// demoScope is the single scope this server issues and requires. Real servers
// define a scope vocabulary and gate individual tools on specific scopes.
const demoScope = "mcp:tools"

// authServer is a minimal, self-contained OAuth 2.1 authorization server AND
// resource server. For the tutorial it keeps all state in memory. The comments
// mark exactly what to replace for production.
type authServer struct {
	jwtKey []byte

	mu sync.Mutex
	// clients holds RFC 7591 dynamically-registered clients, keyed by client_id.
	// PRODUCTION: persist these (e.g. Firestore/Postgres). In-memory means every
	// cold start forgets all clients and forces Spark to re-register.
	clients map[string]*registeredClient
	// codes holds transient authorization codes, keyed by the code string.
	// PRODUCTION: persist with a short TTL and enforce single use across instances.
	codes map[string]*authCode
}

type registeredClient struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

type authCode struct {
	ClientID      string
	RedirectURI   string
	CodeChallenge string // PKCE S256 challenge
	Subject       string // the authenticated end-user (demo: "spark-user")
	ExpiresAt     time.Time
}

func newAuthServer() *authServer {
	key := os.Getenv("JWT_SIGNING_KEY")
	if key == "" {
		// Dev fallback. NEVER rely on this in production — set a strong random
		// JWT_SIGNING_KEY (the deploy script generates one for you).
		key = "dev-insecure-signing-key-change-me"
		log.Printf("[warn] JWT_SIGNING_KEY not set; using an insecure dev key")
	}
	return &authServer{
		jwtKey:  []byte(key),
		clients: make(map[string]*registeredClient),
		codes:   make(map[string]*authCode),
	}
}

// -----------------------------------------------------------------------------
// Discovery: RFC 9728 (Protected Resource Metadata) + RFC 8414 (AS Metadata)
// -----------------------------------------------------------------------------

// handleProtectedResourceMetadata serves RFC 9728. Spark fetches this to learn
// WHICH authorization server protects this resource before it authenticates.
// Public/unauthenticated. Also answers path-suffixed probes (e.g. /mcp).
func (s *authServer) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	base := requestBaseURL(r)

	// If the client appended a resource path (e.g. /mcp), reflect it as the
	// resource identifier; otherwise the resource is the base URL.
	resource := base
	if suffix := strings.TrimPrefix(r.URL.Path, protectedResourcePath); suffix != "" && suffix != "/" {
		resource = base + suffix
	}

	writeCORS(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 resource,
		"authorization_servers":    []string{base},
		"scopes_supported":         []string{demoScope},
		"bearer_methods_supported": []string{"header"},
	})
}

// handleAuthServerMetadata serves RFC 8414. Crucially it advertises the
// registration_endpoint (RFC 7591) so Spark can self-register, plus PKCE
// support and the public-client auth method.
func (s *authServer) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	base := requestBaseURL(r)

	writeCORS(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/authorize",
		"token_endpoint":                        base + "/api/oauth/token",
		"registration_endpoint":                 base + "/api/oauth/register",
		"scopes_supported":                      []string{demoScope},
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// -----------------------------------------------------------------------------
// RFC 7591 Dynamic Client Registration
// -----------------------------------------------------------------------------

type registrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name,omitempty"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	Scope                   string   `json:"scope,omitempty"`
}

// handleRegister implements RFC 7591 for PUBLIC (PKCE) clients: it issues an
// opaque client_id and NO client_secret. Open registration is acceptable here
// because the client is public and abuse is bounded by strict redirect_uri
// validation. PRODUCTION: consider rate-limiting and persistence.
func (s *authServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req registrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_client_metadata", "error_description": "body must be JSON client metadata",
		})
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_redirect_uri", "error_description": "redirect_uris is required",
		})
		return
	}
	for _, u := range req.RedirectURIs {
		if err := validateRedirectURI(u); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid_redirect_uri", "error_description": err.Error(),
			})
			return
		}
	}

	client := &registeredClient{
		ID:           "mcp-client-" + randomString(32),
		Name:         req.ClientName,
		RedirectURIs: req.RedirectURIs,
		CreatedAt:    time.Now(),
	}
	s.mu.Lock()
	s.clients[client.ID] = client
	s.mu.Unlock()

	log.Printf("[dcr] registered public client %q (name=%q, %d redirect_uris)",
		client.ID, client.Name, len(client.RedirectURIs))

	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"redirect_uris":              client.RedirectURIs,
		"client_name":                client.Name,
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// -----------------------------------------------------------------------------
// Authorization endpoint (consent) + token endpoint (PKCE exchange)
// -----------------------------------------------------------------------------

var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Authorize {{.ClientName}}</title>
<style>
 body{font-family:system-ui,sans-serif;background:#0b1020;color:#e5e7eb;display:flex;
   min-height:100vh;align-items:center;justify-content:center;margin:0}
 .card{background:#151b2e;border:1px solid #263049;border-radius:14px;padding:2rem;max-width:420px}
 h1{font-size:1.15rem;margin:0 0 .5rem} p{color:#9fb0c7;font-size:.9rem;line-height:1.5}
 code{background:#0b1020;padding:.1rem .35rem;border-radius:4px;font-size:.8rem;word-break:break-all}
 .row{display:flex;gap:.75rem;margin-top:1.5rem}
 button{flex:1;padding:.7rem;border-radius:8px;border:0;font-size:.95rem;cursor:pointer}
 .approve{background:#3b82f6;color:#fff} .deny{background:transparent;color:#9fb0c7;border:1px solid #263049}
</style></head><body>
 <div class="card">
   <h1>Authorize application</h1>
   <p><strong>{{.ClientName}}</strong> wants to connect to your <strong>hello-spark-mcp</strong> server
      and use its tools.</p>
   <p>Redirect: <code>{{.RedirectURI}}</code></p>
   <form method="POST" action="/authorize">
     <input type="hidden" name="client_id" value="{{.ClientID}}">
     <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
     <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
     <input type="hidden" name="state" value="{{.State}}">
     <div class="row">
       <button class="approve" name="decision" value="approve" type="submit">Approve &amp; Connect</button>
       <button class="deny" name="decision" value="deny" type="submit">Cancel</button>
     </div>
   </form>
 </div>
</body></html>`))

type consentData struct {
	ClientID, ClientName, RedirectURI, CodeChallenge, State string
}

// handleAuthorize is the browser-facing consent endpoint (RFC 6749 §4.1).
//
//	GET  → render a consent page for the requesting client.
//	POST → on approval, mint a single-use authorization code and 302 back to the
//	       client's redirect_uri with ?code=...&state=...
//
// DEMO SIMPLIFICATION: there is no end-user login here. A real deployment MUST
// authenticate the user (e.g. Google Sign-In / Firebase Auth) BEFORE issuing a
// code, and record which user approved. See docs/oauth-deep-dive.md and the
// production reference (eldamo-server) for that step.
func (s *authServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		if q.Get("response_type") != "code" {
			http.Error(w, "unsupported response_type (want 'code')", http.StatusBadRequest)
			return
		}
		clientID := q.Get("client_id")
		redirectURI := q.Get("redirect_uri")
		if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "PKCE code_challenge with method S256 is required", http.StatusBadRequest)
			return
		}
		client, err := s.validateClientRedirect(clientID, redirectURI)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		name := client.Name
		if name == "" {
			name = clientID
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = consentTmpl.Execute(w, consentData{
			ClientID:      clientID,
			ClientName:    name,
			RedirectURI:   redirectURI,
			CodeChallenge: q.Get("code_challenge"),
			State:         q.Get("state"),
		})

	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		clientID := r.FormValue("client_id")
		redirectURI := r.FormValue("redirect_uri")
		state := r.FormValue("state")

		if r.FormValue("decision") != "approve" {
			// User cancelled → bounce back with an error per RFC 6749.
			s.redirectBack(w, r, redirectURI, url.Values{
				"error": {"access_denied"}, "state": {state},
			})
			return
		}
		if _, err := s.validateClientRedirect(clientID, redirectURI); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		code := randomString(32)
		s.mu.Lock()
		s.codes[code] = &authCode{
			ClientID:      clientID,
			RedirectURI:   redirectURI,
			CodeChallenge: r.FormValue("code_challenge"),
			Subject:       "spark-user", // DEMO: replace with the authenticated user id
			ExpiresAt:     time.Now().Add(5 * time.Minute),
		}
		s.mu.Unlock()

		log.Printf("[authorize] issued code for client %q", clientID)
		s.redirectBack(w, r, redirectURI, url.Values{"code": {code}, "state": {state}})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope,omitempty"`
}

// handleToken exchanges a single-use authorization code for a Bearer JWT,
// verifying the PKCE code_verifier against the stored S256 challenge.
func (s *authServer) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	grantType, code, clientID, redirectURI, verifier := parseTokenParams(r)

	if grantType != "authorization_code" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "unsupported_grant_type", "error_description": "only authorization_code is supported",
		})
		return
	}
	if code == "" || clientID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_request", "error_description": "missing code or client_id",
		})
		return
	}

	// Fetch + single-use consume the code.
	s.mu.Lock()
	ac, ok := s.codes[code]
	if ok {
		delete(s.codes, code)
	}
	s.mu.Unlock()

	if !ok || time.Now().After(ac.ExpiresAt) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_grant", "error_description": "authorization code invalid or expired",
		})
		return
	}
	if ac.ClientID != clientID || (redirectURI != "" && ac.RedirectURI != redirectURI) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_grant", "error_description": "client_id or redirect_uri mismatch",
		})
		return
	}

	// PKCE S256 verification: BASE64URL(SHA256(verifier)) must equal the stored challenge.
	if ac.CodeChallenge != "" {
		sum := sha256.Sum256([]byte(verifier))
		computed := base64.RawURLEncoding.EncodeToString(sum[:])
		if verifier == "" || computed != ac.CodeChallenge {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid_grant", "error_description": "PKCE verification failed",
			})
			return
		}
	}

	token, err := s.issueJWT(ac.Subject, clientID, requestBaseURL(r), time.Hour)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "server_error", "error_description": "failed to sign token",
		})
		return
	}
	log.Printf("[token] issued access token for subject %q client %q", ac.Subject, clientID)
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   3600,
		Scope:       demoScope,
	})
}

// -----------------------------------------------------------------------------
// Bearer middleware
// -----------------------------------------------------------------------------

// requireBearer validates the HMAC-signed JWT on protected requests. On failure
// it returns 401 with a WWW-Authenticate header carrying the RFC 9728
// resource_metadata pointer so a client can (re)discover the auth server.
func (s *authServer) requireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("AUTH_BYPASS") == "true" {
			next.ServeHTTP(w, r)
			return
		}
		tokenStr := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			tokenStr = strings.TrimPrefix(h, "Bearer ")
		}
		if tokenStr == "" {
			s.challenge(w, r, "missing access token")
			return
		}
		tok, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return s.jwtKey, nil
		})
		if err != nil || !tok.Valid {
			s.challenge(w, r, "invalid or expired token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *authServer) challenge(w http.ResponseWriter, r *http.Request, desc string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer error="unauthorized", error_description=%q, resource_metadata=%q`,
		desc, requestBaseURL(r)+protectedResourcePath))
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error": "unauthorized", "error_description": desc,
	})
}

func (s *authServer) issueJWT(subject, clientID, issuer string, ttl time.Duration) (string, error) {
	claims := jwt.MapClaims{
		"sub":       subject,
		"client_id": clientID,
		"iss":       issuer,
		"scopes":    []string{demoScope},
		"type":      "access",
		"iat":       time.Now().Unix(),
		"exp":       time.Now().Add(ttl).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.jwtKey)
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func (s *authServer) validateClientRedirect(clientID, redirectURI string) (*registeredClient, error) {
	if clientID == "" {
		return nil, fmt.Errorf("missing client_id")
	}
	s.mu.Lock()
	client, ok := s.clients[clientID]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown client_id (register via /api/oauth/register first)")
	}
	for _, u := range client.RedirectURIs {
		if redirectURIMatch(redirectURI, u) {
			return client, nil
		}
	}
	return nil, fmt.Errorf("redirect_uri not registered for this client")
}

func (s *authServer) redirectBack(w http.ResponseWriter, r *http.Request, redirectURI string, params url.Values) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			if v != "" {
				q.Set(k, v)
			}
		}
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// validateRedirectURI enforces https, or http on a loopback host (RFC 8252),
// for a dynamically-registered redirect URI.
func validateRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid redirect_uri %q", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1") {
		return nil
	}
	return fmt.Errorf("redirect_uri %q must be https or http on a loopback host", raw)
}

// redirectURIMatch compares redirect URIs, ignoring the port for loopback hosts
// (RFC 8252 — native clients use ephemeral loopback ports).
func redirectURIMatch(requested, allowed string) bool {
	if requested == allowed {
		return true
	}
	rq, err1 := url.Parse(requested)
	al, err2 := url.Parse(allowed)
	if err1 != nil || err2 != nil {
		return false
	}
	isLoop := func(h string) bool { return h == "localhost" || h == "127.0.0.1" }
	if isLoop(rq.Hostname()) && isLoop(al.Hostname()) {
		return rq.Scheme == al.Scheme && rq.Path == al.Path
	}
	return false
}

// requestBaseURL derives the public scheme://host for the request, honoring the
// X-Forwarded-* headers set by the Cloud Run / GFE proxy.
func requestBaseURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil && (strings.HasPrefix(r.Host, "localhost:") || strings.HasPrefix(r.Host, "127.0.0.1:")) {
		scheme = "http"
	}
	if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

func parseTokenParams(r *http.Request) (grantType, code, clientID, redirectURI, verifier string) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var m map[string]string
		if json.NewDecoder(r.Body).Decode(&m) == nil {
			return m["grant_type"], m["code"], m["client_id"], m["redirect_uri"], m["code_verifier"]
		}
		return
	}
	_ = r.ParseForm()
	return r.FormValue("grant_type"), r.FormValue("code"), r.FormValue("client_id"),
		r.FormValue("redirect_uri"), r.FormValue("code_verifier")
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
}
