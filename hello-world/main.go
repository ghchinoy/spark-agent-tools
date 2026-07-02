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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/ghchinoy/spark-agent-tools/pkg/mcpauth"
)

func main() {
	// baseURL is only needed for local logging; at request time we always
	// derive the public origin from the incoming request (see requestBaseURL in
	// pkg/mcpauth) so the same binary works locally and behind the Cloud Run
	// proxy.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// The single OAuth authorization server + resource server for this demo.
	// In production you would inject persistent stores and a real IdP here via
	// mcpauth.Options (see pkg/mcpauth for the full Options API).
	authz := mcpauth.NewAuthServer(mcpauth.Options{
		ServerName:              "Hello MCP",
		ServiceDocumentationURI: "https://github.com/ghchinoy/spark-agent-tools",
		// PolicyURI and TOSURI are optional; set them to real URLs in production.
		// They appear in RFC 8414 AS metadata and RFC 7591 DCR responses.
		// PolicyURI: "https://example.com/privacy",
		// TOSURI:    "https://example.com/terms",
	})

	// Build the MCP server (the "echo" tool) and wrap it in the transport
	// multiplexer that speaks both SSE and Streamable HTTP.
	mcpHandler := newMCPHandler()

	// secured = require a valid Bearer JWT before any MCP traffic is served.
	// logMCPMethod wraps the handler to surface the MCP method name from the
	// JSON-RPC body — turning opaque "POST /" log lines into e.g.
	// "[mcp] initialize", "[mcp] tools/list", "[mcp] tools/call echo".
	secured := authz.RequireBearer(logMCPMethod(mcpHandler))

	mux := http.NewServeMux()

	// ── Liveness ────────────────────────────────────────────────────────────
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	// ── Icon ─────────────────────────────────────────────────────────────────
	// Serve the server icon at both /icon.svg and /favicon.ico.
	// Modern browsers and some MCP clients fetch these directly.
	iconHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = fmt.Fprint(w, iconSVG)
	}
	mux.HandleFunc("/icon.svg", iconHandler)
	mux.HandleFunc("/favicon.ico", iconHandler)

	// ── OAuth 2.1 discovery + flow routes (unauthenticated) ─────────────────
	// Mounts: RFC 9728 PRM, RFC 8414 AS metadata, RFC 7591 DCR,
	//         RFC 6749/7636 authorize + token endpoints.
	mcpauth.Mount(mux, authz)

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
	log.Printf("  PRM discovery:       %s", mcpauth.ProtectedResourcePath)
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

// logMCPMethod peeks at the JSON-RPC method field in POST request bodies and
// logs it as "[mcp] <method>" before passing the request through. For
// tools/call it also logs the tool name from the params.
//
// Only the first 512 bytes are read for the peek — enough to extract the
// method and tool name from any real MCP message — and the body is fully
// restored before the downstream handler sees it.
func logMCPMethod(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Body != nil {
			peek, err := io.ReadAll(io.LimitReader(r.Body, 512))
			// Restore the body regardless of whether we could read it.
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(peek), r.Body))
			if err == nil && len(peek) > 0 {
				var msg struct {
					Method string `json:"method"`
					Params struct {
						Name string `json:"name"` // tools/call, prompts/get
					} `json:"params"`
				}
				if json.Unmarshal(peek, &msg) == nil && msg.Method != "" {
					if msg.Params.Name != "" {
						log.Printf("[mcp] %s %s", msg.Method, msg.Params.Name)
					} else {
						log.Printf("[mcp] %s", msg.Method)
					}
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
