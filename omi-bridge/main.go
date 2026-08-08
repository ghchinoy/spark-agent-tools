// Command omi-bridge is a Gemini Spark-ready MCP proxy server for Omi.
//
// It bridges Gemini Spark (which requires OAuth 2.1 + Dynamic Client Registration)
// to Omi's hosted MCP server (https://api.omi.me/v1/mcp/sse, which accepts static
// bearer tokens), injecting your personal OMI_MCP_API_KEY server-side.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/ghchinoy/spark-agent-tools/pkg/mcpauth"
)

// SVG icon for Omi Bridge (Omi brand blue circle with 'O')
const iconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">` +
	`<circle cx="24" cy="24" r="24" fill="#18181b"/>` +
	`<text x="24" y="33" text-anchor="middle" font-family="system-ui,sans-serif" ` +
	`font-size="26" font-weight="bold" fill="white">O</text></svg>`

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	bridgePassphrase := strings.TrimSpace(os.Getenv("OMI_BRIDGE_PASSPHRASE"))

	// Build the OAuth 2.1 authorization server using pkg/mcpauth.
	// This provides the RFC 9728, 8414, 7591 (DCR), and 7636 (PKCE) endpoints
	// that Gemini Spark requires.
	authOpts := mcpauth.Options{
		ServerName:              "Omi Memory Bridge",
		ServiceDocumentationURI: "https://docs.omi.me",
	}

	// If a passphrase is set in OMI_BRIDGE_PASSPHRASE, require it on consent.
	if bridgePassphrase != "" {
		log.Printf("[auth] Passphrase protection ENABLED for consent endpoint")
		authOpts.ResolveSubject = func(r *http.Request) (string, bool) {
			pass := r.FormValue("passphrase")
			if pass == bridgePassphrase {
				log.Printf("[auth] Correct passphrase provided during consent")
				return "omi-user", true
			}
			log.Printf("[auth] Invalid passphrase provided during consent attempt")
			return "", false
		}
	} else {
		log.Printf("[auth] Passphrase protection disabled (set OMI_BRIDGE_PASSPHRASE to restrict consent)")
	}

	authz := mcpauth.NewAuthServer(authOpts)

	// Build the proxy handler that forwards MCP traffic to api.omi.me
	proxyHandler := newOmiProxyHandler()

	// Wrap the proxy in RequireBearer so Spark must present a valid OAuth JWT
	secured := authz.RequireBearer(logMCPMethod(proxyHandler))

	mux := http.NewServeMux()

	// ── Liveness ────────────────────────────────────────────────────────────
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	// ── Icon ─────────────────────────────────────────────────────────────────
	iconHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = fmt.Fprint(w, iconSVG)
	}
	mux.HandleFunc("/icon.svg", iconHandler)
	mux.HandleFunc("/favicon.ico", iconHandler)

	// ── OAuth 2.1 discovery + flow routes (unauthenticated) ─────────────────
	mcpauth.Mount(mux, authz)

	// ── Protected MCP proxy endpoint ─────────────────────────────────────────
	mux.Handle("/mcp", secured)
	mux.Handle("/{$}", secured)

	log.Printf("omi-bridge listening on :%s", port)
	log.Printf("  Target Omi backend: %s/v1/mcp/sse", proxyHandler.apiBaseURL)
	log.Printf("  MCP endpoint:        /mcp (and /)")
	log.Printf("  PRM discovery:       %s", mcpauth.ProtectedResourcePath)
	log.Printf("  AS metadata:         /.well-known/oauth-authorization-server")
	log.Printf("  DCR register:        /api/oauth/register")

	if err := http.ListenAndServe(":"+port, logRequests(mux)); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[req] %s %s (UA: %s)", r.Method, r.URL.Path, r.UserAgent())
		w.Header().Set("X-Accel-Buffering", "no")
		next.ServeHTTP(w, r)
	})
}

func logMCPMethod(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Body != nil {
			peek, err := io.ReadAll(io.LimitReader(r.Body, 1024))
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(peek), r.Body))
			if err == nil && len(peek) > 0 {
				var msg struct {
					Method string `json:"method"`
					Params struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"params"`
				}
				if json.Unmarshal(peek, &msg) == nil && msg.Method != "" {
					if msg.Params.Name != "" {
						argsStr := ""
						if len(msg.Params.Arguments) > 0 && string(msg.Params.Arguments) != "{}" {
							argsStr = fmt.Sprintf(" args=%s", string(msg.Params.Arguments))
							if len(argsStr) > 120 {
								argsStr = argsStr[:117] + "..."
							}
						}
						log.Printf("[mcp] %s %s%s", msg.Method, msg.Params.Name, argsStr)
					} else {
						log.Printf("[mcp] %s", msg.Method)
					}
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
