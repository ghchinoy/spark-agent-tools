# Tutorial: build a secure, Spark-ready MCP server on Cloud Run

This walkthrough builds the server in this repo from first principles. By the end
you'll have a Cloud Run-hosted MCP server that Gemini Spark can discover,
auto-register with, and call — protected by your own OAuth 2.1 / JWT layer.

To run without reading further: see the [README](../README.md). To understand
why each piece exists: read [`oauth-deep-dive.md`](oauth-deep-dive.md) alongside
this.

**Prerequisites:** Go 1.25+, the `gcloud` CLI, a GCP project with billing, and
(to connect from Spark) Gemini Spark access.

---

## 0. The shape of the problem

Spark won't call your tools until your server proves it's a compliant OAuth 2.1
resource server. So we build in this order:

1. The MCP tool surface.
2. The OAuth discovery documents (so Spark can find the auth server).
3. Dynamic client registration (so Spark can self-onboard).
4. The consent + PKCE token flow (so Spark can get a bearer token).
5. The bearer middleware (so tools require a valid token).
6. Deploy + connect.

---

## 1. The MCP tool surface (`hello-world/mcp.go`)

Use the official Go SDK. Define a typed args struct, a handler, register the
tool, and wrap it in a transport multiplexer that speaks both SSE and Streamable
HTTP (Spark uses Streamable HTTP):

```go
type EchoArgs struct {
    Message string `json:"message" jsonschema:"The text to echo back."`
}

func echoHandler(ctx context.Context, req *mcp.CallToolRequest, args EchoArgs) (*mcp.CallToolResult, any, error) {
    return &mcp.CallToolResult{
        Content: []mcp.Content{&mcp.TextContent{Text: "You said: " + args.Message}},
    }, nil, nil
}

server := mcp.NewServer(&mcp.Implementation{Name: "hello", Version: "0.1.0"}, nil)
mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "…"}, echoHandler)
```

The multiplexer (`mcpMultiplexer`) routes by transport traits: a `Mcp-Session-Id`
header, a `DELETE`, or a non-SSE `POST` → Streamable HTTP; otherwise SSE.

> §7 expands this `Implementation` with `Title`, `Icons`, and `Instructions`.
> Here we keep it minimal to focus on the tool surface.

**This is the only file you rewrite to add real tools.** Everything below is
reusable scaffolding from `pkg/mcpauth`.

---

## 2. Discovery documents (`pkg/mcpauth`)

Two public, unauthenticated endpoints. Both are wired automatically by
`mcpauth.Mount(mux, authz)` in `hello-world/main.go`.

**RFC 9728 — Protected Resource Metadata.** Answers both the bare path and the
resource-suffixed variant Spark probes first:

```go
mux.HandleFunc("/.well-known/oauth-protected-resource", authz.handleProtectedResourceMetadata)
mux.HandleFunc("/.well-known/oauth-protected-resource/", authz.handleProtectedResourceMetadata)
```

It returns `{ resource, authorization_servers, scopes_supported, … }`, deriving
the public origin from the request (honoring `X-Forwarded-*` behind Cloud Run).

**RFC 8414 — Authorization Server Metadata.** Advertise your endpoints —
crucially `registration_endpoint` (enables auto-registration), `S256` PKCE, and
`token_endpoint_auth_methods_supported: ["none"]` (public client).

---

## 3. Dynamic Client Registration (`/api/oauth/register`)

Accept `POST /api/oauth/register` with `redirect_uris`, issue an opaque
`client_id`, **no secret**. Validate redirect URIs (`https`, or `http` on
loopback). Store the client via the `mcpauth.Store` interface (in-memory default; swap in
a DB-backed implementation for production).

This single endpoint is the difference between Spark saying *"does not support
automatic registration"* and Spark connecting automatically.

---

## 4. Consent + PKCE token exchange

**`/authorize` (browser).** `GET` renders a consent page; `POST` (on approval)
mints a single-use code and 302-redirects to the client's `redirect_uri`.

> ⚠️ **This is where real user login goes.** The demo approves without
> authenticating a human. In production, authenticate the user here (Google
> Sign-In / Firebase / your IdP) and put their identity in the code, then the
> token's `sub`.

**`/api/oauth/token` (back channel).** Exchange the code for a JWT, verifying
PKCE: recompute `BASE64URL(SHA256(code_verifier))` and compare to the stored
`code_challenge`. Consume the code (single use).

---

## 5. Bearer middleware (`RequireBearer`)

Wrap the MCP handler. Extract `Authorization: Bearer …`, validate the HMAC JWT
signature locally (no DB per request). On failure return `401` with a
`WWW-Authenticate` header that includes the `resource_metadata` pointer — this is
what lets a client (re)discover the auth server after a rejection.

Mount the protected handler at both `/mcp` and the exact root `/{$}` so base-URL
probes (`POST /`, `HEAD /`) get a real challenge instead of a 404:

```go
secured := authz.RequireBearer(mcpHandler)
mux.Handle("/mcp", secured)
mux.Handle("/{$}", secured)   // matches ONLY "/", not a catch-all
```

---

## 6. Run, test, deploy

```bash
cd hello-world
make test          # walks DCR → authorize → PKCE → token → protected call
make run-dev       # local, auth bypassed
```

Deploy from source (the script creates a minimal service account and a JWT key):

```bash
cd hello-world
cp .env.example .env    # set GCP_PROJECT
make deploy
```

Then follow [`connecting-spark.md`](connecting-spark.md) to add the printed
Service URL to Spark.

> **Related reading:** Google Cloud's official
> [Build and deploy a remote MCP server on Cloud Run](https://cloud.google.com/run/docs/tutorials/deploy-remote-mcp-server)
> tutorial covers the same Cloud Run deployment mechanics in Python using
> FastMCP. It uses Cloud Run's IAM-based auth (`--no-allow-unauthenticated` +
> `gcloud run services proxy`) rather than the public-facing OAuth 2.1 chain
> this repo implements — the two approaches are complementary depending on
> whether your clients are developer tools or public Spark users.

---

## 7. Server identity and discoverability

These fields are **MCP-general** — all MCP clients benefit, not just Spark.
They are set in `hello-world/mcp.go` and `hello-world/main.go`.

### Server metadata (`Implementation`)

```go
mcp.NewServer(&mcp.Implementation{
    Name:       "hello",           // programmatic ID: short, one word, no spaces
    Title:      "Hello MCP",       // human-readable display name shown in client UIs
    Version:    "0.1.0",
    WebsiteURL: "https://github.com/ghchinoy/spark-agent-tools",
    Icons:      []mcp.Icon{{
        Source:   iconDataURI,     // data URI or HTTPS URL; data URIs need no external fetch
        MIMEType: "image/svg+xml",
        Sizes:    []string{"any"}, // "any" = scalable (SVG)
    }},
}, &mcp.ServerOptions{
    Instructions: "A hello-world MCP server…",  // sent in every initialize response
})
```

`Name` is the programmatic identifier — keep it lowercase, no spaces, one word.
Clients use it in logs and as a stable key. `Title` is what humans see in UIs.
`Instructions` is included in every `initialize` response and gives the LLM
context about what the server does and when to use it.

Icons use the `mcp.Icon` struct, which accepts an HTTP/HTTPS URL or a base64
data URI. A data URI is self-contained and works before the server has a public
URL; replace it with an HTTPS URL in production so clients can cache it.

### Tool metadata

```go
mcp.AddTool(server, &mcp.Tool{
    Name:        "echo",
    Title:       "Echo",          // human-readable; shown in Spark's approval UI
    Description: "Echo a message back…",  // LLM hint + shown to users before each approval
    Icons:       []mcp.Icon{serverIcon},
}, echoHandler)
```

Write `Description` for a human audience, not just the LLM. Spark shows the
tool name and arguments to the user before every approval click — a clear
description builds trust and reduces hesitation.

### Example prompts

```go
server.AddPrompt(&mcp.Prompt{
    Name:        "test-connection",
    Title:       "Test connection",
    Description: "Verify the server is reachable and authenticated.",
}, func(_ context.Context, _ *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
    return &mcp.GetPromptResult{
        Messages: []*mcp.PromptMessage{{
            Role:    "user",
            Content: &mcp.TextContent{Text: "Use the echo tool to say hello…"},
        }},
    }, nil
})
```

Prompts are example interactions clients can enumerate (`prompts/list`) and
invoke (`prompts/get`). Whether a specific client surfaces them in its UI
varies — register them anyway. They cost nothing and signal to the LLM how the
server is meant to be used.

### RFC optional fields (`mcpauth.Options`)

```go
mcpauth.NewAuthServer(mcpauth.Options{
    ServerName:              "Hello MCP",
    ServiceDocumentationURI: "https://github.com/ghchinoy/spark-agent-tools",
    PolicyURI:               "https://example.com/privacy",  // RFC 8414 op_policy_uri
    TOSURI:                  "https://example.com/terms",    // RFC 8414 op_tos_uri
    TokenTTL:                30 * 24 * time.Hour,            // Prevent client lockouts on expiration
})
```

`ServiceDocumentationURI` appears in RFC 8414 AS metadata as `service_documentation`.
`PolicyURI` and `TOSURI` appear in both AS metadata and DCR registration responses
(`policy_uri`, `tos_uri`). Other MCP clients read these even if Spark doesn't
currently surface them in its UI.

`TokenTTL` defines the lifetime of issued access tokens (defaults to 30 days). Since Spark's current custom app client does not automatically trigger re-authorization or refresh loops when a token expires (which results in an empty tools panel and locked out session), configuring a long-lived `TokenTTL` ensures seamless connectivity for developers.

### Favicon

Serve the icon at `/favicon.ico` and `/icon.svg` to avoid 404s in browser
sessions and logs:

```go
mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "image/svg+xml")
    _, _ = fmt.Fprint(w, iconSVG)
})
```

---

## 8. Verify the chain by hand

```bash
BASE=https://YOUR-SERVICE.run.app
curl -s $BASE/.well-known/oauth-protected-resource | jq
curl -s $BASE/.well-known/oauth-authorization-server | jq '.registration_endpoint'
curl -s -X POST $BASE/api/oauth/register -H 'content-type: application/json' \
  -d '{"redirect_uris":["https://example.com/cb"]}' | jq
curl -s -i $BASE/mcp | grep -i www-authenticate   # 401 + resource_metadata pointer
```

If all four respond as expected, Spark will too.

---

## 9. Where to go next

- Add a real tool in `hello-world/mcp.go` (call an API, query a DB, etc.).
- Swap the in-memory store for a database by implementing `mcpauth.Store` (see
  [oauth-deep-dive.md → From demo to production](oauth-deep-dive.md#from-demo-to-production)).
- Add real user authentication by setting `mcpauth.Options.ResolveSubject` to
  authenticate against your IdP before issuing a code.
- Create a new tool subdirectory that imports `pkg/mcpauth` — the monorepo
  structure exists exactly for this.
- Study the [eldamo-server](https://github.com/ghchinoy/eldamoapi) reference for
  a production build that does all of the above and serves A2A too.
