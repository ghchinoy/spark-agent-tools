package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// registerTestClient posts a DCR registration and returns the issued client_id.
func registerTestClient(t *testing.T, s *authServer, redirectURI string) string {
	t.Helper()
	body, _ := json.Marshal(registrationRequest{
		RedirectURIs: []string{redirectURI},
		ClientName:   "Test Client",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/register", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleRegister(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register: want 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if _, hasSecret := resp["client_secret"]; hasSecret {
		t.Fatal("public client must not receive a client_secret")
	}
	id, _ := resp["client_id"].(string)
	if !strings.HasPrefix(id, "mcp-client-") {
		t.Fatalf("unexpected client_id: %q", id)
	}
	return id
}

func TestDiscoveryEndpoints(t *testing.T) {
	s := newAuthServer()

	// RFC 9728 PRM — bare path.
	req := httptest.NewRequest(http.MethodGet, protectedResourcePath, nil)
	req.Host = "example.com"
	w := httptest.NewRecorder()
	s.handleProtectedResourceMetadata(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PRM: want 200, got %d", w.Code)
	}
	var prm map[string]any
	_ = json.NewDecoder(w.Body).Decode(&prm)
	if prm["resource"] != "https://example.com" {
		t.Errorf("PRM resource = %v", prm["resource"])
	}

	// RFC 9728 PRM — path-suffixed variant.
	req = httptest.NewRequest(http.MethodGet, protectedResourcePath+"/mcp", nil)
	req.Host = "example.com"
	w = httptest.NewRecorder()
	s.handleProtectedResourceMetadata(w, req)
	_ = json.NewDecoder(w.Body).Decode(&prm)
	if prm["resource"] != "https://example.com/mcp" {
		t.Errorf("PRM suffixed resource = %v", prm["resource"])
	}

	// RFC 8414 AS metadata must advertise DCR + PKCE.
	req = httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	req.Host = "example.com"
	w = httptest.NewRecorder()
	s.handleAuthServerMetadata(w, req)
	var as map[string]any
	_ = json.NewDecoder(w.Body).Decode(&as)
	if as["registration_endpoint"] != "https://example.com/api/oauth/register" {
		t.Errorf("missing/incorrect registration_endpoint: %v", as["registration_endpoint"])
	}
	methods, _ := as["code_challenge_methods_supported"].([]any)
	if len(methods) != 1 || methods[0] != "S256" {
		t.Errorf("expected S256 PKCE support, got %v", as["code_challenge_methods_supported"])
	}
}

func TestRegisterValidation(t *testing.T) {
	s := newAuthServer()

	// Missing redirect_uris → 400.
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/register", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleRegister(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing redirect_uris: want 400, got %d", w.Code)
	}

	// Non-loopback http redirect → 400.
	body := `{"redirect_uris":["http://evil.example/cb"]}`
	req = httptest.NewRequest(http.MethodPost, "/api/oauth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.handleRegister(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("non-loopback http: want 400, got %d", w.Code)
	}
}

// TestFullAuthCodeFlow walks the complete DCR → authorize → token → protected
// resource sequence with PKCE, exactly as Spark does.
func TestFullAuthCodeFlow(t *testing.T) {
	s := newAuthServer()
	redirectURI := "https://client.example/callback"
	clientID := registerTestClient(t, s, redirectURI)

	// PKCE: verifier + S256 challenge.
	verifier := "test-verifier-0123456789-0123456789-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// Approve at the authorize endpoint → expect a 302 back to redirect_uri?code=...
	form := url.Values{
		"client_id":      {clientID},
		"redirect_uri":   {redirectURI},
		"code_challenge": {challenge},
		"state":          {"xyz"},
		"decision":       {"approve"},
	}
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleAuthorize(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("authorize approve: want 302, got %d (%s)", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad Location: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatal("authorize did not return a code")
	}
	if loc.Query().Get("state") != "xyz" {
		t.Errorf("state not echoed back")
	}

	// Exchange the code (with the correct verifier) → access token.
	tokBody, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"client_id":     clientID,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
	req = httptest.NewRequest(http.MethodPost, "/api/oauth/token", strings.NewReader(string(tokBody)))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.handleToken(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var tr tokenResponse
	_ = json.NewDecoder(w.Body).Decode(&tr)
	if tr.AccessToken == "" || tr.TokenType != "Bearer" {
		t.Fatalf("unexpected token response: %+v", tr)
	}

	// The issued token must pass the Bearer middleware.
	protected := s.requireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req = httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+tr.AccessToken)
	w = httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("protected resource with valid token: want 200, got %d", w.Code)
	}
}

func TestTokenRejectsBadPKCE(t *testing.T) {
	s := newAuthServer()
	redirectURI := "https://client.example/callback"
	clientID := registerTestClient(t, s, redirectURI)

	sum := sha256.Sum256([]byte("the-real-verifier"))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	form := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirectURI},
		"code_challenge": {challenge}, "decision": {"approve"},
	}
	req := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s.handleAuthorize(w, req)
	loc, _ := url.Parse(w.Header().Get("Location"))
	code := loc.Query().Get("code")

	// Exchange with the WRONG verifier → invalid_grant.
	body, _ := json.Marshal(map[string]string{
		"grant_type": "authorization_code", "code": code,
		"client_id": clientID, "redirect_uri": redirectURI,
		"code_verifier": "wrong-verifier",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/oauth/token", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.handleToken(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad PKCE: want 400, got %d", w.Code)
	}
}

func TestChallengeCarriesResourceMetadata(t *testing.T) {
	s := newAuthServer()
	protected := s.requireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Host = "example.com"
	w := httptest.NewRecorder()
	protected.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"),
		`resource_metadata="https://example.com/.well-known/oauth-protected-resource"`) {
		t.Errorf("challenge missing resource_metadata pointer: %q", w.Header().Get("WWW-Authenticate"))
	}
}

func TestRedirectURIMatchLoopback(t *testing.T) {
	// Loopback port-agnostic (RFC 8252).
	if !redirectURIMatch("http://127.0.0.1:52341/cb", "http://127.0.0.1:8080/cb") {
		t.Error("loopback ports should be ignored when host+path+scheme match")
	}
	// Different path must not match.
	if redirectURIMatch("http://127.0.0.1:52341/other", "http://127.0.0.1:8080/cb") {
		t.Error("different paths must not match")
	}
	// Non-loopback must be exact.
	if redirectURIMatch("https://a.example/cb", "https://b.example/cb") {
		t.Error("non-loopback hosts must match exactly")
	}
}
