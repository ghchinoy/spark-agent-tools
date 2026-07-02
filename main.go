// Command hello-mcp is a minimal, self-contained Model Context Protocol (MCP)
// server designed to be connected to Google Gemini Spark as a custom Connected
// App. It demonstrates the *complete* scaffolding a hosted MCP server needs to
// be usable by Spark:
//
//   - An MCP tool surface (a single "echo" tool) over Streamable HTTP.
//   - The OAuth 2.1 discovery + authorization chain Spark walks before it will
//     call any tool:
//     RFC 9728  Protected Resource Metadata   (/.well-known/oauth-protected-resource)
//     RFC 8414  Authorization Server Metadata (/.well-known/oauth-authorization-server)
//     RFC 7591  Dynamic Client Registration   (/api/oauth/register)
//     RFC 7636  PKCE authorization_code flow   (/authorize + /api/oauth/token)
//   - A stateless HMAC-signed JWT bearer token that gates the tool surface.
//
// Everything runs in one binary with no external dependencies (auth codes and
// registered clients live in memory). That keeps the tutorial runnable
// anywhere; the docs call out exactly where a production deployment would swap
// in persistent storage and real user authentication.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	// baseURL is only needed for local logging; at request time we always
	// derive the public origin from the incoming request (see requestBaseURL)
	// so the same binary works locally and behind the Cloud Run proxy.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// The single OAuth authorization server + resource server for this demo.
	// In production you would inject persistent stores and a real IdP here.
	authz := newAuthServer()

	// Build the MCP server (the "echo" tool) and wrap it in the transport
	// multiplexer that speaks both SSE and Streamable HTTP.
	mcpHandler := newMCPHandler()

	// secured = require a valid Bearer JWT before any MCP traffic is served.
	secured := authz.requireBearer(mcpHandler)

	mux := http.NewServeMux()

	// ── Liveness ────────────────────────────────────────────────────────────
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	// ── Public OAuth discovery (unauthenticated) ─────────────────────────────
	// RFC 9728 — Protected Resource Metadata. Spark probes this FIRST (both the
	// bare path and a resource-path-suffixed variant like .../mcp), so we serve
	// the exact path and the trailing-slash subtree.
	mux.HandleFunc(protectedResourcePath, authz.handleProtectedResourceMetadata)
	mux.HandleFunc(protectedResourcePath+"/", authz.handleProtectedResourceMetadata)

	// RFC 8414 — Authorization Server Metadata. Advertises the token,
	// registration, and authorization endpoints and PKCE support.
	mux.HandleFunc("/.well-known/oauth-authorization-server", authz.handleAuthServerMetadata)

	// ── OAuth endpoints ──────────────────────────────────────────────────────
	// RFC 7591 — Dynamic Client Registration. This is what makes Spark's
	// "automatic registration" work; without it Spark asks for a manual
	// client ID / secret.
	mux.HandleFunc("/api/oauth/register", authz.handleRegister)

	// RFC 6749 / RFC 7636 — the browser-facing consent page and the
	// back-channel token exchange (PKCE S256).
	mux.HandleFunc("/authorize", authz.handleAuthorize)
	mux.HandleFunc("/api/oauth/token", authz.handleToken)

	// ── MCP tool surface (Bearer JWT required) ───────────────────────────────
	// Primary documented endpoint.
	mux.Handle("/mcp", secured)
	// Also mount at the exact root so clients that treat the server URL as a
	// single Streamable HTTP endpoint (and probe POST / or HEAD /) reach the
	// transport instead of a 404. "/{$}" matches ONLY "/", so unknown paths
	// still 404.
	mux.Handle("/{$}", secured)

	log.Printf("hello-mcp listening on :%s", port)
	log.Printf("  MCP endpoint:        /mcp  (and / )")
	log.Printf("  PRM discovery:       %s", protectedResourcePath)
	log.Printf("  AS metadata:         /.well-known/oauth-authorization-server")
	log.Printf("  DCR register:        /api/oauth/register")
	if err := http.ListenAndServe(":"+port, logRequests(mux)); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// logRequests is a tiny access log so you can watch the Spark discovery/auth
// dance in your terminal (or Cloud Run logs) exactly as described in the docs.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[req] %s %s (User-Agent: %s)", r.Method, r.URL.Path, r.UserAgent())
		// Streamable HTTP / SSE must not be buffered by the proxy.
		w.Header().Set("X-Accel-Buffering", "no")
		next.ServeHTTP(w, r)
	})
}
