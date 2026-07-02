# Deep dive: why Spark needs four OAuth specs (with request traces)

This is the "why" behind the code — the story of what actually happens when you
paste an MCP URL into Gemini Spark, why a naive server fails, and how each RFC in
the chain fixes a specific failure. It's written to be useful even if you never
read the Go.

---

## The failure you start with

You stand up an MCP server at `https://your-server.example/mcp`, paste it into
Spark, and get:

> *"This server does not support automatic registration. To connect, enter your
> own OAuth client ID and secret below."*

Meanwhile your access logs look like this:

```
GET  /.well-known/oauth-protected-resource/mcp   404
GET  /.well-known/oauth-protected-resource       404
POST /                                            404
HEAD /                                            404
```

Two independent things are wrong, and they stack:

1. Spark can't **discover** how to authenticate (the 404 storm).
2. Even if it could, it can't **register** itself automatically (the error message).

Modern MCP clients — Spark, opencode, Claude, and others — implement the
[MCP Authorization spec](https://spec.modelcontextprotocol.io/specification/2025-03-26/basic/authorization/),
which is built on ordinary OAuth 2.1 discovery. Your server has to speak it.

---

## Step 1 — RFC 9728: "which authorization server protects you?"

Before authenticating, the client asks the *resource* (your MCP endpoint) a
question: **who issues tokens for you?** That answer lives in a
**Protected Resource Metadata** document (RFC 9728).

The client probes the well-known path — first with the resource path appended,
then bare:

```
GET /.well-known/oauth-protected-resource/mcp
GET /.well-known/oauth-protected-resource
```

Your server must answer both with JSON like:

```json
{
  "resource": "https://your-server.example/mcp",
  "authorization_servers": ["https://your-server.example"],
  "scopes_supported": ["mcp:tools"],
  "bearer_methods_supported": ["header"]
}
```

`authorization_servers` is the pointer the client was looking for. Without this
document, discovery dead-ends at 404 and nothing else happens — this alone breaks
**every** modern MCP client, not just Spark.

There's a second half: when an unauthenticated request hits a protected
endpoint, the `401` should tell the client where the metadata lives:

```
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Bearer error="unauthorized",
  resource_metadata="https://your-server.example/.well-known/oauth-protected-resource"
```

> In this repo: `handleProtectedResourceMetadata` and `challenge` in `pkg/mcpauth/mcpauth.go`.

---

## Step 2 — RFC 8414: "where are your endpoints?"

Now the client fetches the **Authorization Server Metadata** (RFC 8414) from the
server it was just pointed to:

```
GET /.well-known/oauth-authorization-server
```

```json
{
  "issuer": "https://your-server.example",
  "authorization_endpoint": "https://your-server.example/authorize",
  "token_endpoint": "https://your-server.example/api/oauth/token",
  "registration_endpoint": "https://your-server.example/api/oauth/register",
  "code_challenge_methods_supported": ["S256"],
  "token_endpoint_auth_methods_supported": ["none"]
}
```

Three fields matter most:

- `registration_endpoint` — this is what turns "automatic registration" **on**
  (Step 3). Omit it and you get the "does not support automatic registration"
  message.
- `code_challenge_methods_supported: ["S256"]` — advertises PKCE (Step 4).
- `token_endpoint_auth_methods_supported: ["none"]` — declares that clients are
  **public** (no client secret). Spark is a public client.

> In this repo: `handleAuthServerMetadata` in `pkg/mcpauth/mcpauth.go`.

---

## Step 3 — RFC 7591: "register yourself, no human needed"

Spark can't ask a human to go create an OAuth app in a developer console. Instead
it uses **Dynamic Client Registration** (RFC 7591): it POSTs its own metadata and
receives a `client_id`.

```
POST /api/oauth/register
{ "redirect_uris": ["https://…/callback"], "client_name": "Google" }

201 Created
{ "client_id": "mcp-client-abc123…", "token_endpoint_auth_method": "none" }
```

Key design choices for a Spark-facing server:

- **Public client → no `client_secret`.** Security comes from PKCE + strict
  redirect-URI validation, not a shared secret.
- **Validate `redirect_uris`** — accept `https`, or `http` on a loopback host
  (RFC 8252, for native/loopback clients). Reject everything else.
- **Persist the client** (in production). This demo keeps it in memory, so a
  cold start makes Spark re-register — harmless, but noisy.

> In this repo: `handleRegister` + `validateRedirectURI` in `pkg/mcpauth/mcpauth.go`.

### DCR vs. CIMD (a fork in the road)

There are two ways a client can obtain an identity:

| Mechanism | `client_id` shape | Who uses it |
| :--- | :--- | :--- |
| **DCR** (RFC 7591) | opaque string the server issues | **Gemini Spark**, most standard OAuth clients |
| **CIMD** (Client ID Metadata Document) | an HTTPS URL the client hosts | opencode and other CIMD-aware agents |

They're interchangeable front doors that converge on the same token flow. This
tutorial implements **DCR** because that's what Spark speaks. A server can
support both at once (the production reference does) — just advertise
`registration_endpoint` *and* `client_id_metadata_document_supported: true` and
dispatch on the `client_id` shape.

---

## Step 4 — RFC 7636: consent + PKCE token exchange

With a `client_id` in hand, the client runs the standard authorization-code flow
with **PKCE** (RFC 7636), which protects the code from interception without a
client secret.

1. Client generates a random `code_verifier` and its
   `code_challenge = BASE64URL(SHA256(verifier))`.
2. It opens the browser at:
   ```
   GET /authorize?response_type=code&client_id=…&redirect_uri=…
       &code_challenge=…&code_challenge_method=S256&state=…
   ```
3. **The user consents.** *(This is where you authenticate the human — see
   below.)* Your server issues a single-use `code` and redirects to
   `redirect_uri?code=…&state=…`.
4. Client exchanges the code at the token endpoint, proving it holds the verifier:
   ```
   POST /api/oauth/token
   grant_type=authorization_code&code=…&client_id=…
     &redirect_uri=…&code_verifier=…
   ```
5. Server recomputes `SHA256(verifier)`, checks it equals the stored challenge,
   and returns a signed JWT:
   ```json
   { "access_token": "eyJ…", "token_type": "Bearer", "expires_in": 3600 }
   ```

From then on the client sends `Authorization: Bearer eyJ…` with every MCP call,
and your middleware validates the signature locally — no database hit per request.

> In this repo: `handleAuthorize`, `handleToken`, `RequireBearer` in `pkg/mcpauth/mcpauth.go`.

---

## Step 5 — one more thing Spark does: probe the base URL

Spark (and some other clients) treat the server URL as a single Streamable-HTTP
endpoint and probe the **origin root**:

```
POST /    → should not be a bare 404
HEAD /    → should not be a bare 404
```

Mount your MCP handler at the exact root (`/{$}` in Go's `net/http`, which matches
*only* `/`) in addition to `/mcp`. Then a base-URL probe returns a proper `401`
auth challenge (which also carries the `resource_metadata` pointer from Step 1),
and users can paste either the bare domain or `.../mcp`. Unknown paths still 404.

> In this repo: the `mux.Handle("/{$}", secured)` line in `hello-world/main.go`.

---

## The whole dance, in one trace

Here's a real successful Spark connection, annotated:

```
HEAD /                                          401   ← probe; gets WWW-Authenticate + resource_metadata
GET  /.well-known/oauth-protected-resource      200   ← RFC 9728 PRM
GET  /.well-known/oauth-authorization-server    200   ← RFC 8414; finds registration_endpoint
POST /api/oauth/register                        201   ← RFC 7591 DCR → client_id
   … user consents in the browser …
GET  /authorize?…                               200   ← consent page
POST /api/oauth/token                           200   ← PKCE exchange → JWT
POST /mcp  (Bearer eyJ…)                        200   ← tool calls, reusing the token
POST /mcp  (Bearer eyJ…)                        200
```

No 404s, no manual client ID/secret. That's the goal.

---

## From demo to production

This repo is intentionally a *hello world*. Two things to change before real use:

### 1. Persist state
`registered_clients` and `auth codes` are in-memory maps here. Replace them with
a database:
- **Auth codes** need a short TTL (5 min) and must be single-use across
  instances — store them with an expiry and delete on exchange.
- **Registered clients** should survive restarts so Spark doesn't re-register on
  every cold start.

### 2. Authenticate the end user
The consent page in this demo approves **without a login** — it never proves
*who* is connecting. A real server must authenticate the human at Step 4 (before
issuing the code) and record their identity in the token's `sub` claim. Common
choices: Google Sign-In, Firebase Auth, or your existing IdP. You typically also
maintain an allow-list / user record so you can grant scopes and revoke access.

The [eldamo-server](https://github.com/ghchinoy/eldamoapi) reference does exactly
this: Firestore-backed code/client/user storage, a Firebase consent SPA at a
separate domain (proxied to avoid CORS), per-user scopes embedded in the JWT, and
the same RFC stack serving both MCP *and* A2A from one binary.

---

## Reference

- [MCP Authorization spec](https://spec.modelcontextprotocol.io/specification/2025-03-26/basic/authorization/)
- [RFC 9728](https://www.rfc-editor.org/rfc/rfc9728) — OAuth 2.0 Protected Resource Metadata
- [RFC 8414](https://www.rfc-editor.org/rfc/rfc8414) — OAuth 2.0 Authorization Server Metadata
- [RFC 7591](https://www.rfc-editor.org/rfc/rfc7591) — OAuth 2.0 Dynamic Client Registration
- [RFC 7636](https://www.rfc-editor.org/rfc/rfc7636) — Proof Key for Code Exchange (PKCE)
- [RFC 8252](https://www.rfc-editor.org/rfc/rfc8252) — OAuth 2.0 for Native Apps (loopback redirect matching)
- [Gemini Spark — add a custom app](https://support.google.com/gemini/answer/17209137)
