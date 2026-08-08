package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOmiProxyHandler(t *testing.T) {
	// Create a mock Omi server
	mockOmi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/mcp/sse" {
			t.Errorf("expected path /v1/mcp/sse, got %s", r.URL.Path)
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer test-mcp-key" {
			t.Errorf("expected Authorization header 'Bearer test-mcp-key', got %q", authHeader)
		}

		w.Header().Set("Mcp-Session-Id", "test-session-123")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	}))
	defer mockOmi.Close()

	handler := &omiProxyHandler{
		apiBaseURL: mockOmi.URL,
		mcpAPIKey:  "test-mcp-key",
		httpClient: mockOmi.Client(),
	}

	reqBody := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", reqBody)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	if sessID := resp.Header.Get("Mcp-Session-Id"); sessID != "test-session-123" {
		t.Errorf("expected Mcp-Session-Id header 'test-session-123', got %q", sessID)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed reading response body: %v", err)
	}

	expected := `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`
	if string(body) != expected {
		t.Errorf("expected body %q, got %q", expected, string(body))
	}
}
