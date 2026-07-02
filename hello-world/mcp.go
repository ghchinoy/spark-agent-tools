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

// iconDataURI is a self-contained SVG icon embedded as a base64 data URI.
// Using a data URI means no external fetch is required and the icon works
// regardless of the deployment URL. In production, replace with an HTTPS URL
// to a hosted image so clients can cache it independently.
//
// The SVG itself: a blue circle with the letter H, 48×48px, scalable.
const iconDataURI = "data:image/svg+xml;base64," +
	"PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCA0OCA0" +
	"OCI+PGNpcmNsZSBjeD0iMjQiIGN5PSIyNCIgcj0iMjQiIGZpbGw9IiMzYjgyZjYiLz48dGV4dCB4" +
	"PSIyNCIgeT0iMzMiIHRleHQtYW5jaG9yPSJtaWRkbGUiIGZvbnQtZmFtaWx5PSJzeXN0ZW0tdWks" +
	"c2Fucy1zZXJpZiIgZm9udC1zaXplPSIyNiIgZm9udC13ZWlnaHQ9ImJvbGQiIGZpbGw9IndoaXRl" +
	"Ij5IPC90ZXh0Pjwvc3ZnPg=="

// iconSVG is the raw SVG, served at /icon.svg for browsers and as the favicon.
const iconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48">` +
	`<circle cx="24" cy="24" r="24" fill="#3b82f6"/>` +
	`<text x="24" y="33" text-anchor="middle" font-family="system-ui,sans-serif" ` +
	`font-size="26" font-weight="bold" fill="white">H</text></svg>`

// serverIcon is the MCP Icon descriptor used in Implementation and Tool metadata.
var serverIcon = mcp.Icon{
	Source:   iconDataURI,
	MIMEType: "image/svg+xml",
	Sizes:    []string{"any"},
}

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

// newMCPHandler builds the MCP server, registers tools and prompts, and wraps
// it in a transport multiplexer that serves both SSE and Streamable HTTP.
// Gemini Spark uses Streamable HTTP.
func newMCPHandler() http.Handler {
	// Implementation.Name is the programmatic server ID — keep it short, one
	// word, no spaces. Clients use it in logs and as a stable identifier.
	// Implementation.Title is the human-readable display name shown in UIs.
	// Implementation.Instructions is surfaced to the LLM in every initialize
	// response; write it as a brief description of what the server does.
	server := mcp.NewServer(&mcp.Implementation{
		Name:       "hello",
		Title:      "Hello MCP",
		Version:    "0.1.0",
		WebsiteURL: "https://github.com/ghchinoy/spark-agent-tools",
		Icons:      []mcp.Icon{serverIcon},
	}, &mcp.ServerOptions{
		Instructions: "A hello-world MCP server on Cloud Run. " +
			"Use the echo tool to confirm the end-to-end connection and OAuth " +
			"authentication are working. Replace the echo tool with your own tools " +
			"to build a custom Gemini Spark Connected App.",
		// KeepAlive sends a ping on the open GET SSE stream every 30 seconds.
		// Without it, Cloud Run or Spark's OpenAuth client may treat an idle stream
		// as stale and close it, triggering a reopen that the SDK rejects with 409
		// Conflict — producing a stuck "Thinking it through..." session.
		KeepAlive: 30 * time.Second,
	})

	// Tool.Title is the human-readable name shown to users in approval UIs.
	// Tool.Description hints to the LLM what the tool does and when to call it.
	// Write both for a human audience — Spark shows them before each approval click.
	mcp.AddTool(server, &mcp.Tool{
		Name:        "echo",
		Title:       "Echo",
		Description: "Echo a message back to the caller. Confirms the MCP connection and OAuth authentication are working end to end.",
		Icons:       []mcp.Icon{serverIcon},
	}, echoHandler)

	// Prompts are example interactions clients can list (prompts/list) and
	// invoke (prompts/get). They give the LLM and the user a suggested starting
	// point. Whether a specific client surfaces them in its UI varies — register
	// them anyway; it costs nothing and is correct per the MCP spec.
	server.AddPrompt(&mcp.Prompt{
		Name:        "test-connection",
		Title:       "Test connection",
		Description: "Verify the server is reachable and the OAuth authentication is working.",
	}, func(_ context.Context, _ *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Description: "Sends a test message through the echo tool to confirm the connection works end to end.",
			Messages: []*mcp.PromptMessage{
				{
					Role:    "user",
					Content: &mcp.TextContent{Text: "Use the echo tool to say hello and confirm the MCP server connection is working."},
				},
			},
		}, nil
	})

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
