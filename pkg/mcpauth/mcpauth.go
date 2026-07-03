// Package mcpauth is a reusable OAuth 2.1 authorization server and resource
// server designed for MCP (Model Context Protocol) servers that need to satisfy
// the Gemini Spark connected-app security requirements.
//
// It implements:
//   - RFC 9728: Protected Resource Metadata
//   - RFC 8414: Authorization Server Metadata
//   - RFC 7591: Dynamic Client Registration
//   - RFC 6749 / RFC 7636: authorization_code + PKCE S256 flow
//
// Quick start:
//
//	authz := mcpauth.NewAuthServer(mcpauth.Options{})
//	mcpauth.Mount(mux, authz)
//	mux.Handle("/mcp", authz.RequireBearer(myMCPHandler))
package mcpauth

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

// ProtectedResourcePath is the RFC 9728 well-known base path. Clients probe it
// directly and also with the resource path appended (e.g. .../mcp), so Mount
// serves both the exact path and the trailing-slash subtree.
const ProtectedResourcePath = "/.well-known/oauth-protected-resource"

// DefaultScope is the OAuth scope this package issues and requires by default.
// Real servers define a scope vocabulary and gate individual tools on specific
// scopes.
const DefaultScope = "mcp:tools"

// -----------------------------------------------------------------------------
// Store interface + in-memory default
// -----------------------------------------------------------------------------

// Client represents a registered OAuth 2.1 public client (RFC 7591).
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

// AuthCode is a transient, single-use authorization code.
type AuthCode struct {
	ClientID      string
	RedirectURI   string
	CodeChallenge string // PKCE S256 challenge
	Subject       string // the authenticated end-user
	ExpiresAt     time.Time
}

// Store is the pluggable persistence interface for registered clients and
// authorization codes. Implement this to swap in a production database.
//
// The four methods document exactly what a production deployment needs to
// persist:
//   - Clients survive restarts so Spark does not need to re-register after
//     each cold start.
//   - Codes need a short TTL (≤ 10 minutes) and single-use enforcement across
//     all server instances.
type Store interface {
	// RegisterClient persists a newly registered client.
	RegisterClient(c *Client) error
	// GetClient looks up a client by its ID. Returns false if not found.
	GetClient(id string) (*Client, bool)
	// SaveCode stores an authorization code with its associated metadata.
	SaveCode(code string, ac *AuthCode) error
	// ConsumeCode fetches and atomically deletes an authorization code.
	// Returns false if the code is not found (already used or never issued).
	ConsumeCode(code string) (*AuthCode, bool)
}

// NewMemoryStore returns a new in-memory Store. Suitable for local development
// and tests. A cold restart forgets all clients and codes.
func NewMemoryStore() Store {
	return &memoryStore{
		clients: make(map[string]*Client),
		codes:   make(map[string]*AuthCode),
	}
}

type memoryStore struct {
	mu      sync.Mutex
	clients map[string]*Client
	codes   map[string]*AuthCode
}

func (m *memoryStore) RegisterClient(c *Client) error {
	m.mu.Lock()
	m.clients[c.ID] = c
	m.mu.Unlock()
	return nil
}

func (m *memoryStore) GetClient(id string) (*Client, bool) {
	m.mu.Lock()
	c, ok := m.clients[id]
	m.mu.Unlock()
	return c, ok
}

func (m *memoryStore) SaveCode(code string, ac *AuthCode) error {
	m.mu.Lock()
	m.codes[code] = ac
	m.mu.Unlock()
	return nil
}

func (m *memoryStore) ConsumeCode(code string) (*AuthCode, bool) {
	m.mu.Lock()
	ac, ok := m.codes[code]
	if ok {
		delete(m.codes, code)
	}
	m.mu.Unlock()
	return ac, ok
}

// -----------------------------------------------------------------------------
// AuthServer
// -----------------------------------------------------------------------------

// ResolveSubjectFunc is an optional hook for real IdP integration. It is
// called on the POST /authorize path to identify and authenticate the end user.
// Return (userID, true) to approve or ("", false) to deny.
//
// If nil, the demo behaviour applies: the subject is always "spark-user" and
// approval is taken from the form's "decision" field. A real deployment MUST
// authenticate the user (e.g. Google Sign-In / Firebase Auth) before returning
// approved=true.
//
// See docs/oauth-deep-dive.md and the production reference (eldamo-server) for
// how to wire in a real IdP.
type ResolveSubjectFunc func(r *http.Request) (subject string, approved bool)

// Options configures an AuthServer.
type Options struct {
	// JWTSigningKey is the HMAC key used to sign access tokens. If empty, the
	// JWT_SIGNING_KEY environment variable is used, falling back to an insecure
	// dev key with a warning. Never rely on the dev fallback in production.
	JWTSigningKey string

	// Store is the backing persistence layer. Defaults to NewMemoryStore().
	// PRODUCTION: plug in a DB-backed implementation here.
	Store Store

	// ResolveSubject overrides the user-identity step in the POST /authorize
	// handler. See ResolveSubjectFunc for the full contract.
	// PRODUCTION: set this to authenticate the user with your IdP.
	ResolveSubject ResolveSubjectFunc

	// Scope is the OAuth scope this server issues. Defaults to DefaultScope.
	Scope string

	// ServerName appears in the browser consent page.
	// Defaults to "MCP server".
	ServerName string

	// ServiceDocumentationURI is a URL to human-readable documentation about
	// this server (RFC 8414 "service_documentation"). Included in AS metadata
	// if non-empty; MCP clients and developers may surface it.
	ServiceDocumentationURI string

	// PolicyURI is a URL to the server's privacy policy (RFC 8414
	// "op_policy_uri"; RFC 7591 "policy_uri"). Included in AS metadata and DCR
	// responses if non-empty.
	PolicyURI string

	// TOSURI is a URL to the server's terms of service (RFC 8414 "op_tos_uri";
	// RFC 7591 "tos_uri"). Included in AS metadata and DCR responses if
	// non-empty.
	TOSURI string

	// TokenTTL is the lifetime of issued access tokens. Defaults to 24 * time.Hour.
	// For production, short lifetimes are recommended. For local/tutorial use,
	// a longer TTL (e.g., 30 days or more) prevents Spark from getting stuck
	// when tokens expire, as Spark's current client does not automatically
	// refresh or re-authorize expired tokens silently.
	TokenTTL time.Duration
}

// AuthServer is a minimal, self-contained OAuth 2.1 authorization server AND
// resource server. Configure it with Options and mount it with Mount.
type AuthServer struct {
	jwtKey   []byte
	store    Store
	resolve  ResolveSubjectFunc
	scope    string
	name     string
	docURI   string
	policy   string
	tos      string
	tokenTTL time.Duration
}

// NewAuthServer constructs an AuthServer from the provided Options.
func NewAuthServer(opts Options) *AuthServer {
	key := opts.JWTSigningKey
	if key == "" {
		key = os.Getenv("JWT_SIGNING_KEY")
	}
	if key == "" {
		// Dev fallback. NEVER rely on this in production — set a strong random
		// JWT_SIGNING_KEY (the deploy script generates one for you).
		key = "dev-insecure-signing-key-change-me"
		log.Printf("[warn] JWT_SIGNING_KEY not set; using an insecure dev key")
	}

	store := opts.Store
	if store == nil {
		store = NewMemoryStore()
	}

	scope := opts.Scope
	if scope == "" {
		scope = DefaultScope
	}

	name := opts.ServerName
	if name == "" {
		name = "MCP server"
	}

	tokenTTL := opts.TokenTTL
	if tokenTTL == 0 {
		tokenTTL = 720 * time.Hour // Default to 30 days (720 hours) for robust local/demo use
	}

	return &AuthServer{
		jwtKey:   []byte(key),
		store:    store,
		resolve:  opts.ResolveSubject,
		scope:    scope,
		name:     name,
		docURI:   opts.ServiceDocumentationURI,
		policy:   opts.PolicyURI,
		tos:      opts.TOSURI,
		tokenTTL: tokenTTL,
	}
}

// Mount wires all standard OAuth 2.1 discovery and flow routes onto mux.
// The following routes are registered:
//
//	GET  /.well-known/oauth-protected-resource      RFC 9728
//	GET  /.well-known/oauth-protected-resource/...  RFC 9728 (path-suffixed probes)
//	GET  /.well-known/oauth-authorization-server    RFC 8414
//	POST /api/oauth/register                        RFC 7591 DCR
//	GET  /authorize                                 RFC 6749 consent page
//	POST /authorize                                 RFC 6749 / RFC 7636 code issue
//	POST /api/oauth/token                           RFC 7636 PKCE exchange
//
// The MCP tool surface is NOT mounted here. The caller should add:
//
//	mux.Handle("/mcp", authz.RequireBearer(mcpHandler))
func Mount(mux *http.ServeMux, s *AuthServer) {
	mux.HandleFunc(ProtectedResourcePath, s.handleProtectedResourceMetadata)
	mux.HandleFunc(ProtectedResourcePath+"/", s.handleProtectedResourceMetadata)
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleAuthServerMetadata)
	mux.HandleFunc("/api/oauth/register", s.handleRegister)
	mux.HandleFunc("/authorize", s.handleAuthorize)
	mux.HandleFunc("/api/oauth/token", s.handleToken)
}

// RequireBearer returns middleware that validates the HMAC-signed JWT on
// protected requests. On failure it returns 401 with a WWW-Authenticate header
// carrying the RFC 9728 resource_metadata pointer so the client can
// (re)discover the authorization server.
//
// If the AUTH_BYPASS environment variable is "true", all token checks are
// skipped. This escape hatch is for local development only — never set it on
// a publicly accessible instance.
func (s *AuthServer) RequireBearer(next http.Handler) http.Handler {
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

// -----------------------------------------------------------------------------
// Discovery: RFC 9728 (Protected Resource Metadata) + RFC 8414 (AS Metadata)
// -----------------------------------------------------------------------------

// handleProtectedResourceMetadata serves RFC 9728. Spark fetches this to learn
// WHICH authorization server protects this resource before it authenticates.
// Public/unauthenticated. Also answers path-suffixed probes (e.g. /mcp).
func (s *AuthServer) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
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
	if suffix := strings.TrimPrefix(r.URL.Path, ProtectedResourcePath); suffix != "" && suffix != "/" {
		resource = base + suffix
	}

	writeCORS(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 resource,
		"authorization_servers":    []string{base},
		"scopes_supported":         []string{s.scope},
		"bearer_methods_supported": []string{"header"},
	})
}

// handleAuthServerMetadata serves RFC 8414. Crucially it advertises the
// registration_endpoint (RFC 7591) so Spark can self-register, plus PKCE
// support and the public-client auth method.
func (s *AuthServer) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
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

	meta := map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/authorize",
		"token_endpoint":                        base + "/api/oauth/token",
		"registration_endpoint":                 base + "/api/oauth/register",
		"scopes_supported":                      []string{s.scope},
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	}
	// RFC 8414 optional informational fields.
	if s.docURI != "" {
		meta["service_documentation"] = s.docURI
	}
	if s.policy != "" {
		meta["op_policy_uri"] = s.policy
	}
	if s.tos != "" {
		meta["op_tos_uri"] = s.tos
	}
	writeCORS(w)
	writeJSON(w, http.StatusOK, meta)
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
func (s *AuthServer) handleRegister(w http.ResponseWriter, r *http.Request) {
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

	client := &Client{
		ID:           "mcp-client-" + randomString(32),
		Name:         req.ClientName,
		RedirectURIs: req.RedirectURIs,
		CreatedAt:    time.Now(),
	}
	if err := s.store.RegisterClient(client); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "server_error", "error_description": "failed to register client",
		})
		return
	}

	log.Printf("[dcr] registered public client %q (name=%q, %d redirect_uris)",
		client.ID, client.Name, len(client.RedirectURIs))

	reg := map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"redirect_uris":              client.RedirectURIs,
		"client_name":                client.Name,
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	// RFC 7591 optional informational fields.
	if s.policy != "" {
		reg["policy_uri"] = s.policy
	}
	if s.tos != "" {
		reg["tos_uri"] = s.tos
	}
	writeJSON(w, http.StatusCreated, reg)
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
   <p><strong>{{.ClientName}}</strong> wants to connect to your <strong>{{.ServerName}}</strong>
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
	ClientID, ClientName, ServerName, RedirectURI, CodeChallenge, State string
}

// handleAuthorize is the browser-facing consent endpoint (RFC 6749 §4.1).
//
//	GET  → render a consent page for the requesting client.
//	POST → on approval, mint a single-use authorization code and 302 back to the
//	       client's redirect_uri with ?code=...&state=...
//
// DEMO SIMPLIFICATION: when ResolveSubject is nil there is no end-user login.
// A real deployment MUST authenticate the user (e.g. Google Sign-In / Firebase
// Auth) via the ResolveSubject hook BEFORE issuing a code. See
// docs/oauth-deep-dive.md and the production reference (eldamo-server).
func (s *AuthServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
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
			ServerName:    s.name,
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

		// Determine the subject and whether the user approved.
		var subject string
		var approved bool
		if s.resolve != nil {
			// Production path: delegate to the caller-supplied IdP hook.
			subject, approved = s.resolve(r)
		} else {
			// DEMO SIMPLIFICATION: trust the form's "decision" field and use a
			// fixed subject. Replace via Options.ResolveSubject for production.
			approved = r.FormValue("decision") == "approve"
			subject = "spark-user"
		}

		if !approved {
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
		ac := &AuthCode{
			ClientID:      clientID,
			RedirectURI:   redirectURI,
			CodeChallenge: r.FormValue("code_challenge"),
			Subject:       subject,
			ExpiresAt:     time.Now().Add(5 * time.Minute),
		}
		if err := s.store.SaveCode(code, ac); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "server_error", "error_description": "failed to save authorization code",
			})
			return
		}

		log.Printf("[authorize] issued code for client %q subject %q", clientID, subject)
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
func (s *AuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
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
	ac, ok := s.store.ConsumeCode(code)
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

	token, err := s.issueJWT(ac.Subject, clientID, requestBaseURL(r), s.tokenTTL)
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
		ExpiresIn:   int(s.tokenTTL.Seconds()),
		Scope:       s.scope,
	})
}

// -----------------------------------------------------------------------------
// Internal helpers
// -----------------------------------------------------------------------------

func (s *AuthServer) challenge(w http.ResponseWriter, r *http.Request, desc string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer error="unauthorized", error_description=%q, resource_metadata=%q`,
		desc, requestBaseURL(r)+ProtectedResourcePath))
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error": "unauthorized", "error_description": desc,
	})
}

func (s *AuthServer) issueJWT(subject, clientID, issuer string, ttl time.Duration) (string, error) {
	claims := jwt.MapClaims{
		"sub":       subject,
		"client_id": clientID,
		"iss":       issuer,
		"scopes":    []string{s.scope},
		"type":      "access",
		"iat":       time.Now().Unix(),
		"exp":       time.Now().Add(ttl).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.jwtKey)
}

func (s *AuthServer) validateClientRedirect(clientID, redirectURI string) (*Client, error) {
	if clientID == "" {
		return nil, fmt.Errorf("missing client_id")
	}
	client, ok := s.store.GetClient(clientID)
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

func (s *AuthServer) redirectBack(w http.ResponseWriter, r *http.Request, redirectURI string, params url.Values) {
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
