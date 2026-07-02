package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// EchoArgs is the typed input schema for the "echo" tool. The jsonschema
// struct tags are surfaced to the client (and to Spark) as the tool's
// parameter documentation.
type EchoArgs struct {
	Message string `json:"message" jsonschema:"The text to echo back."`
}

// echoHandler implements the single demo tool: it returns the caller's message
// prefixed with a greeting and a server timestamp. Replace this with your own
// tool logic — everything else in this repo is the reusable scaffolding.
func echoHandler(ctx context.Context, req *mcp.CallToolRequest, args EchoArgs) (*mcp.CallToolResult, any, error) {
	msg := strings.TrimSpace(args.Message)
	if msg == "" {
		msg = "(you sent an empty message)"
	}
	log.Printf("[tool] echo: %q", msg)

	reply := fmt.Sprintf("Hello from your Spark MCP server! You said: %q (at %s)",
		msg, time.Now().UTC().Format(time.RFC3339))

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: reply}},
	}, nil, nil
}

// newMCPHandler builds the MCP server, registers the echo tool, and wraps it in
// a transport multiplexer that serves both SSE and Streamable HTTP. Gemini
// Spark uses Streamable HTTP.
func newMCPHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "hello-spark-mcp",
		Version: "0.1.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "echo",
		Description: "Echo a message back to the caller. A hello-world tool proving the MCP connection and auth work end to end.",
	}, echoHandler)

	getServer := func(*http.Request) *mcp.Server { return server }
	return &mcpMultiplexer{
		sse:        mcp.NewSSEHandler(getServer, nil),
		streamable: mcp.NewStreamableHTTPHandler(getServer, nil),
	}
}

// mcpMultiplexer routes a request to the SSE handler or the Streamable HTTP
// handler based on standard MCP transport traits. A single endpoint can thus
// serve legacy SSE clients and modern Streamable HTTP clients (like Spark).
type mcpMultiplexer struct {
	sse        http.Handler
	streamable http.Handler
}

func (m *mcpMultiplexer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hasSessionID := false
	for k := range r.URL.Query() {
		if strings.EqualFold(k, "sessionid") {
			hasSessionID = true
			break
		}
	}

	// Streamable HTTP traits: a session-id header, a DELETE, or a POST that is
	// not an SSE message POST (no ?sessionid=).
	if r.Header.Get("Mcp-Session-Id") != "" ||
		r.Method == http.MethodDelete ||
		(r.Method == http.MethodPost && !hasSessionID) {
		m.streamable.ServeHTTP(w, r)
		return
	}
	m.sse.ServeHTTP(w, r)
}
