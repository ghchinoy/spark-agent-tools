package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// omiProxyHandler forwards incoming MCP requests from Gemini Spark to Omi's
// hosted MCP server (https://api.omi.me/v1/mcp/sse), injecting your personal
// OMI_MCP_API_KEY server-side.
//
// Because Gemini Spark requires OAuth 2.1 + Dynamic Client Registration (which
// this binary's pkg/mcpauth layer handles on the Spark-facing front door), this
// proxy bridges Spark to Omi's static-bearer-token backend seamlessly.
type omiProxyHandler struct {
	apiBaseURL string
	mcpAPIKey  string
	httpClient *http.Client
}

func newOmiProxyHandler() *omiProxyHandler {
	apiBase := strings.TrimRight(os.Getenv("OMI_API_BASE_URL"), "/")
	if apiBase == "" {
		apiBase = "https://api.omi.me"
	}
	mcpKey := strings.TrimSpace(os.Getenv("OMI_MCP_API_KEY"))
	if mcpKey == "" {
		log.Printf("[WARNING] OMI_MCP_API_KEY environment variable is empty!")
	}

	return &omiProxyHandler{
		apiBaseURL: apiBase,
		mcpAPIKey:  mcpKey,
		httpClient: &http.Client{
			// Timeout for standard calls. Streamable HTTP/SSE connections stream
			// until complete or disconnected.
			Timeout: 120 * time.Second,
		},
	}
}

func (h *omiProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	targetURL := fmt.Sprintf("%s/v1/mcp/sse", h.apiBaseURL)

	// Create outbound request to Omi
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		log.Printf("[proxy] error creating outbound request: %v", err)
		http.Error(w, "Internal proxy error", http.StatusInternalServerError)
		return
	}

	// Copy relevant headers from client
	for _, header := range []string{"Accept", "Content-Type", "Mcp-Session-Id"} {
		if val := r.Header.Get(header); val != "" {
			outReq.Header.Set(header, val)
		}
	}

	// Inject Omi MCP API key
	if h.mcpAPIKey != "" {
		outReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", h.mcpAPIKey))
	}

	log.Printf("[proxy] %s -> %s (session: %s)", r.Method, targetURL, r.Header.Get("Mcp-Session-Id"))

	start := time.Now()
	resp, err := h.httpClient.Do(outReq)
	duration := time.Since(start)
	if err != nil {
		log.Printf("[proxy] error forwarding to Omi after %v: %v", duration, err)
		http.Error(w, fmt.Sprintf("Error communicating with Omi backend: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	log.Printf("[proxy] Omi status=%d duration=%v", resp.StatusCode, duration)
	if resp.StatusCode >= 400 {
		log.Printf("[proxy] [ERROR] Omi returned HTTP %d", resp.StatusCode)
	}

	// Copy response headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	// Disable proxy buffering for SSE / Streamable HTTP responses
	w.Header().Set("X-Accel-Buffering", "no")

	w.WriteHeader(resp.StatusCode)

	// Stream response body back to Spark, flushing periodically if supported
	flusher, isFlusher := w.(http.Flusher)
	buf := make([]byte, 4096)
	firstChunk := true

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if firstChunk {
				firstChunk = false
				chunkStr := string(buf[:n])
				if strings.Contains(chunkStr, `"isError":true`) || strings.Contains(chunkStr, `"error":{`) {
					snippet := strings.ReplaceAll(strings.ReplaceAll(chunkStr, "\n", " "), "\r", "")
					if len(snippet) > 180 {
						snippet = snippet[:177] + "..."
					}
					log.Printf("[proxy] [MCP-ERROR] Omi returned JSON-RPC error: %s", snippet)
				}
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				log.Printf("[proxy] error writing response to client: %v", writeErr)
				break
			}
			if isFlusher {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Printf("[proxy] error reading response from Omi: %v", readErr)
			}
			break
		}
	}
}
