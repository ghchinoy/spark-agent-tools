# spark-agent-tools — a hello-world MCP server for Gemini Spark

A minimal, **self-contained** [Model Context Protocol (MCP)](https://modelcontextprotocol.io)
server you can deploy to Google Cloud Run and connect to **Gemini Spark** as a
custom Connected App — in one binary, with no database.

It ships a single `echo` tool, but the point isn't the tool. The point is the
**scaffolding** that makes a hosted MCP server *usable by Spark*: the OAuth 2.1
discovery + authorization chain that Spark walks before it will call a single
tool. Get that right once and you can hang any tools you like off it.

> Gemini Spark is a personal AI agent in the Gemini app that can automate
> workflows using built-in and **custom Connected Apps** — the latter are hosted
> MCP servers you point Spark at.
> ([overview](https://support.google.com/gemini/answer/17094507) ·
> [connected apps](https://support.google.com/gemini/answer/13695044) ·
> [add a custom app](https://support.google.com/gemini/answer/17209137))

---

## Why this exists

When you paste a plain MCP URL into Spark, you may hit:

> *"This server does not support automatic registration. To connect, enter your
> own OAuth client ID and secret below."*

…and the server logs fill with `404`s. That's because Spark expects a **standards
-compliant OAuth 2.1 resource server**, specifically:

| Spec | Endpoint | Why Spark needs it |
| :--- | :--- | :--- |
| **RFC 9728** Protected Resource Metadata | `/.well-known/oauth-protected-resource` | Discover *which* authorization server protects the tools |
| **RFC 8414** Authorization Server Metadata | `/.well-known/oauth-authorization-server` | Find the token / registration / authorize endpoints |
| **RFC 7591** Dynamic Client Registration | `/api/oauth/register` | "Automatic registration" — get a `client_id` with no human in the loop |
| **RFC 7636** PKCE authorization-code flow | `/authorize` + `/api/oauth/token` | Get a user-consented bearer token |

This repo implements all four in ~400 lines of well-commented Go, then gates a
tiny MCP tool surface behind the resulting JWT. See
[`docs/oauth-deep-dive.md`](docs/oauth-deep-dive.md) for the full story of *why*
each piece is required (it's a good read even if you never touch Go).

---

## Quick start (local)

```bash
# 1. Run with auth bypassed — fastest way to see the echo tool work
make run-dev            # serves on :8080, AUTH_BYPASS=true

# 2. Or run with the real OAuth/JWT layer enabled
JWT_SIGNING_KEY=$(openssl rand -hex 32) make run
```

Watch the discovery endpoints respond:

```bash
curl -s localhost:8080/.well-known/oauth-protected-resource | jq
curl -s localhost:8080/.well-known/oauth-authorization-server | jq
curl -s -X POST localhost:8080/api/oauth/register \
  -H 'content-type: application/json' \
  -d '{"redirect_uris":["https://example.com/cb"],"client_name":"demo"}' | jq
```

Run the tests (they walk the full DCR → authorize → PKCE token → protected call
flow):

```bash
make test
```

## Deploy to Cloud Run

```bash
cp .env.example .env         # set GCP_PROJECT
make deploy                  # builds from source, prints your Service URL
```

Then paste the Service URL into Gemini Spark — see
[`docs/connecting-spark.md`](docs/connecting-spark.md).

---

## What's in here

```
main.go                 HTTP wiring: routes, middleware, startup
mcp.go                  the echo tool + SSE/Streamable-HTTP multiplexer
oauth.go                the self-contained OAuth 2.1 authorization server
oauth_test.go           full-flow tests (DCR, PKCE, JWT, discovery)
Dockerfile              distroless multi-stage build
scripts/deploy.sh       one-command Cloud Run deploy
docs/TUTORIAL.md        build-it-yourself walkthrough
docs/connecting-spark.md   connect from the Spark UI
docs/oauth-deep-dive.md    why RFC 9728 / 8414 / 7591 / PKCE, with request traces
docs/architecture.webp     the discovery + auth flow, visualized
```

## Scope & honesty about the demo

To stay runnable anywhere, this server makes two deliberate simplifications that
you **must** change for production (each is flagged in the code and in
[`docs/oauth-deep-dive.md`](docs/oauth-deep-dive.md#from-demo-to-production)):

1. **In-memory state.** Registered clients and auth codes live in RAM, so a cold
   start forgets them. Use a database (Firestore, Postgres, …) in production.
2. **No real end-user login.** The consent screen approves without
   authenticating a human. A real server authenticates the user (Google
   Sign-In / Firebase Auth / your IdP) *before* issuing a code.

For a production-grade reference that does both — persistent Firestore storage, a
Firebase consent SPA, per-user scopes, and the same RFC stack serving **two**
protocols (MCP + A2A) — see the
[eldamo-server](https://github.com/ghchinoy/eldamoapi) project this tutorial was
distilled from.

## License

Apache 2.0 — see [LICENSE](LICENSE).
