# Tutorial: build a secure, Spark-ready MCP server on Cloud Run

This walkthrough builds the server in this repo from first principles. By the end
you'll have a Cloud Run-hosted MCP server that Gemini Spark can discover,
auto-register with, and call — protected by your own OAuth 2.1 / JWT layer.

If you just want to run what's here, see the [README](../README.md). If you want
to understand *why* each piece exists, read
[`oauth-deep-dive.md`](oauth-deep-dive.md) alongside this.

**Prerequisites:** Go 1.25+, the `gcloud` CLI, a GCP project with billing, and
(to connect from Spark) Gemini Spark access.

---

## 0. The shape of the problem

Spark won't call your tools until your server proves it's a compliant OAuth 2.1
resource server. So we build in this order:

1. The MCP tool surface (the fun part, but useless alone).
2. The OAuth discovery documents (so Spark can find the auth server).
3. Dynamic client registration (so Spark can self-onboard).
4. The consent + PKCE token flow (so Spark can get a bearer token).
5. The bearer middleware (so the tools are actually protected).
6. Deploy + connect.

---

## 1. The MCP tool surface (`mcp.go`)

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

server := mcp.NewServer(&mcp.Implementation{Name: "hello-spark-mcp", Version: "0.1.0"}, nil)
mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "…"}, echoHandler)
```

The multiplexer (`mcpMultiplexer`) routes by transport traits: a `Mcp-Session-Id`
header, a `DELETE`, or a non-SSE `POST` → Streamable HTTP; otherwise SSE.

**This is the only file you rewrite to add real tools.** Everything below is
reusable scaffolding.

---

## 2. Discovery documents (`oauth.go`)

Two public, unauthenticated endpoints.

**RFC 9728 — Protected Resource Metadata.** Answer both the bare path and the
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

## 3. Dynamic Client Registration (`handleRegister`)

Accept `POST /api/oauth/register` with `redirect_uris`, issue an opaque
`client_id`, **no secret**. Validate redirect URIs (`https`, or `http` on
loopback). Store the client (in-memory here; a DB in production).

This single endpoint is the difference between Spark saying *"does not support
automatic registration"* and Spark just working.

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

## 5. Bearer middleware (`requireBearer`)

Wrap the MCP handler. Extract `Authorization: Bearer …`, validate the HMAC JWT
signature locally (no DB per request). On failure return `401` with a
`WWW-Authenticate` header that includes the `resource_metadata` pointer — this is
what lets a client (re)discover the auth server after a rejection.

Mount the protected handler at both `/mcp` and the exact root `/{$}` so base-URL
probes (`POST /`, `HEAD /`) get a real challenge instead of a 404:

```go
secured := authz.requireBearer(mcpHandler)
mux.Handle("/mcp", secured)
mux.Handle("/{$}", secured)   // matches ONLY "/", not a catch-all
```

---

## 6. Run, test, deploy

```bash
make test          # walks DCR → authorize → PKCE → token → protected call
make run-dev       # local, auth bypassed
```

Deploy from source (the script creates a minimal service account and a JWT key):

```bash
cp .env.example .env    # set GCP_PROJECT
make deploy
```

Then follow [`connecting-spark.md`](connecting-spark.md) to add the printed
Service URL to Spark.

---

## 7. Verify the chain by hand

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

## Where to go next

- Add a real tool in `mcp.go` (call an API, query a DB, etc.).
- Swap in-memory stores for a database (see
  [oauth-deep-dive.md → From demo to production](oauth-deep-dive.md#from-demo-to-production)).
- Add real user authentication at the consent step.
- Study the [eldamo-server](https://github.com/ghchinoy/eldamoapi) reference for
  a production build that does all of the above and serves A2A too.
