# spark-agent-tools — MCP servers for Gemini Spark

A monorepo of [Model Context Protocol (MCP)](https://modelcontextprotocol.io)
servers designed to connect to **Gemini Spark** as custom Connected Apps, each
with a complete OAuth 2.1 authorization layer baked in.

> Gemini Spark is a personal AI agent in the Gemini app that can automate
> workflows using built-in and **custom Connected Apps** — the latter are hosted
> MCP servers you point Spark at.
> ([overview](https://support.google.com/gemini/answer/17094507) ·
> [connected apps](https://support.google.com/gemini/answer/13695044) ·
> [add a custom app](https://support.google.com/gemini/answer/17209137))

---

## Quick start

**Prerequisites:** Go 1.25+. To deploy, also the `gcloud` CLI and a GCP project
with billing.

Run the `hello-world` server locally with auth bypassed — the fastest way to see
it work:

```bash
cd hello-world
make run-dev            # serves on :8080, AUTH_BYPASS=true
```

Watch the OAuth discovery chain respond:

```bash
curl -s localhost:8080/.well-known/oauth-protected-resource | jq
curl -s localhost:8080/.well-known/oauth-authorization-server | jq
curl -s -X POST localhost:8080/api/oauth/register \
  -H 'content-type: application/json' \
  -d '{"redirect_uris":["https://example.com/cb"],"client_name":"demo"}' | jq
```

Run the full test suite (it walks DCR → authorize → PKCE token → protected call
in-process):

```bash
make test
```

To enforce the real OAuth/JWT layer instead of bypassing it:

```bash
JWT_SIGNING_KEY=$(openssl rand -hex 32) make run
```

## Deploy and connect to Spark

```bash
cd hello-world
cp .env.example .env    # set GCP_PROJECT
make deploy             # builds from source, prints your Service URL
```

Paste the printed Service URL into Gemini Spark → Connected Apps. The full
click-through and a troubleshooting table are in
[`docs/connecting-spark.md`](docs/connecting-spark.md).

> **See also:** Google Cloud's official tutorial
> [Build and deploy a remote MCP server on Cloud Run](https://cloud.google.com/run/docs/tutorials/deploy-remote-mcp-server)
> shows the same deployment pattern in Python using FastMCP and Cloud Run's
> built-in IAM auth. That approach works well for developer tooling. This repo
> adds the OAuth 2.1 discovery + authorization chain (RFC 9728 / 8414 / 7591 /
> 7636) that Gemini Spark specifically requires on top of it.

---

## Documentation

| If you want to… | Read | 
| :--- | :--- |
| Build a Spark-ready MCP server step by step | [`docs/TUTORIAL.md`](docs/TUTORIAL.md) |
| Connect a deployed server to Spark, with troubleshooting | [`docs/connecting-spark.md`](docs/connecting-spark.md) |
| Understand *why* Spark needs the OAuth 2.1 chain | [`docs/oauth-deep-dive.md`](docs/oauth-deep-dive.md) |
| See how Spark actually behaves — real traces and gotchas | [`docs/LESSONS_LEARNED.md`](docs/LESSONS_LEARNED.md) |

---

## Why this exists

When you paste a plain MCP URL into Spark, you may hit:

> *"This server does not support automatic registration. To connect, enter your
> own OAuth client ID and secret below."*

…and the server logs fill with `404`s. That's because Spark expects a
**standards-compliant OAuth 2.1 resource server**, specifically:

| Spec | Endpoint | Why Spark needs it |
| :--- | :--- | :--- |
| **RFC 9728** Protected Resource Metadata | `/.well-known/oauth-protected-resource` | Discover *which* authorization server protects the tools |
| **RFC 8414** Authorization Server Metadata | `/.well-known/oauth-authorization-server` | Find the token / registration / authorize endpoints |
| **RFC 7591** Dynamic Client Registration | `/api/oauth/register` | "Automatic registration" — get a `client_id` with no human in the loop |
| **RFC 7636** PKCE authorization-code flow | `/authorize` + `/api/oauth/token` | Get a user-consented bearer token |

The shared `pkg/mcpauth` package implements all four. Each tool subdirectory
wires it in with a handful of lines so tool code is purely tool logic. See
[`docs/oauth-deep-dive.md`](docs/oauth-deep-dive.md) for the full story of *why*
each piece is required.

---

## Repository layout

```
go.mod                          single Go module for the whole repo
pkg/mcpauth/                    shared OAuth 2.1 package (RFC 9728/8414/7591/7636)
  mcpauth.go                      Store interface, AuthServer, Mount, RequireBearer
  mcpauth_test.go                 full-flow tests (DCR, PKCE, JWT, discovery)

hello-world/                    MCP server: one "echo" tool, complete auth scaffolding
  main.go                         HTTP wiring: routes, middleware, startup
  mcp.go                          the echo tool + SSE/Streamable-HTTP multiplexer
  Dockerfile                      distroless multi-stage build (context = repo root)
  scripts/deploy.sh               one-command Cloud Run deploy
  Makefile                        developer targets (run, run-dev, test, build, deploy)
  .env.example                    config template; copy to hello-world/.env

docs/
  TUTORIAL.md                   build-it-yourself walkthrough
  connecting-spark.md           connect from the Spark UI + troubleshooting table
  oauth-deep-dive.md            why RFC 9728/8414/7591/7636, with request traces
  LESSONS_LEARNED.md            field notes from live Spark connections
  architecture.webp             the discovery + auth flow, visualized
```

To add a new tool: create a subdirectory (e.g. `calendar-tool/`), add
`main.go` + `mcp.go`, import `pkg/mcpauth`, and call `mcpauth.Mount(mux, authz)`.

---

## What to change for production

`hello-world` keeps two things minimal so it runs anywhere with no external
dependencies. Both are flagged in `pkg/mcpauth` and in
[`docs/oauth-deep-dive.md`](docs/oauth-deep-dive.md#from-demo-to-production),
and both need changing for a real deployment:

1. **In-memory state.** Registered clients and auth codes live in RAM, so a cold
   start forgets them. Plug in a DB-backed `mcpauth.Store` implementation for
   production.
2. **No real end-user login.** The consent screen approves without authenticating
   a human. Set `mcpauth.Options.ResolveSubject` to authenticate the user with
   your IdP before issuing a code.

For a production-grade reference that does both — persistent Firestore storage, a
Firebase consent SPA, per-user scopes, and the same RFC stack serving **two**
protocols (MCP + A2A) — see the
[eldamo-server](https://github.com/ghchinoy/eldamoapi) project this tutorial was
distilled from.

## License

Apache 2.0 — see [LICENSE](LICENSE).
