# spark-agent-tools: Agent Onboarding Instructions

Welcome. This document gives you the architectural context, conventions, and
gotchas you need to contribute to this repo with high precision. Read it before
touching code.

---

## What this repo is and where it is going

**Today:** a single, self-contained hello-world MCP server demonstrating the
complete OAuth 2.1 scaffolding Gemini Spark requires. One binary, no external
dependencies, designed to be readable and deployable in one command.

**Near-term (tracked in bd):** a **monorepo of MCP servers** where each tool
lives in its own subdirectory (e.g. `hello-world/`, `calendar-tool/`, etc.) and
shares a common auth package (`pkg/mcpauth`) so tool code is purely tool logic.

Two tasks govern that evolution — claim them before starting either:
- `spark-agent-tools-m28.6` — extract `oauth.go` into `pkg/mcpauth` (shared
  package with a pluggable store interface). **Land this first.**
- `spark-agent-tools-m28.5` — move the current root Go source into `hello-world/`
  and wire it to import `pkg/mcpauth`. **Depends on m28.6.**

Until those tasks land, the canonical entry points are all at the **repo root**.

---

## Repository layout (current)

```
main.go         HTTP wiring: mux, route mounting, startup log
mcp.go          MCP tool handlers + SSE/Streamable-HTTP multiplexer
oauth.go        Self-contained OAuth 2.1 server (see "Auth layer" below)
oauth_test.go   Full-flow tests (DCR → authorize → PKCE → token → call)
Dockerfile      Distroless multi-stage build
scripts/deploy.sh   Cloud Run source-based deploy + minimal SA creation
Makefile        Developer targets (run, run-dev, test, build, deploy)
.env.example    Config template; copy to .env (gitignored)
.golangci.yml   golangci-lint v2 config — 0-issue bar enforced in CI
docs/
  TUTORIAL.md           Build-it-yourself walkthrough
  oauth-deep-dive.md    Why each RFC, with annotated real Spark request traces
  connecting-spark.md   Connect from the Spark UI + troubleshooting table
  blog-post.md          Draft blog post
  architecture.dot/.webp Discovery + auth flow diagram
```

---

## Developer quick reference

```bash
make run-dev   # AUTH_BYPASS=true — fastest local loop, no token needed
make run       # Full auth enforced; requires JWT_SIGNING_KEY in env
make test      # go test ./... — runs the full OAuth flow in-process
make build     # Produces bin/hello-mcp
make deploy    # Cloud Run source deploy (reads .env)
golangci-lint run   # Must report 0 issues before any commit
```

**Port conflicts:** `PORT=8091 make run-dev` avoids the common :8080 collision.
Clean up stale background processes: `lsof -ti:8091 | xargs kill -9`.

**Smoke-test the discovery chain** (the four probes Spark makes, in order):
```bash
BASE=http://localhost:8080
curl -s $BASE/.well-known/oauth-protected-resource | jq      # RFC 9728 PRM
curl -s $BASE/.well-known/oauth-authorization-server | jq    # RFC 8414
curl -s -X POST $BASE/api/oauth/register \
  -H 'content-type: application/json' \
  -d '{"redirect_uris":["https://example.com/cb"]}' | jq    # RFC 7591 DCR
curl -si $BASE/mcp | grep -i www-authenticate                # 401 + PRM pointer
```

---

## Auth layer — the heart of the repo

`oauth.go` is a **self-contained OAuth 2.1 authorization + resource server**.
It is intentionally not split across files so the tutorial is easy to follow
end-to-end. Here is the map:

| Symbol | RFC | Role |
| :--- | :--- | :--- |
| `handleProtectedResourceMetadata` | RFC 9728 | Discovery step 1 — "who issues tokens for you?" |
| `handleAuthServerMetadata` | RFC 8414 | Discovery step 2 — "where are your endpoints?" |
| `handleRegister` + `buildClientRegistration` | RFC 7591 | Dynamic Client Registration — "automatic registration" in Spark |
| `handleAuthorize` | RFC 6749 | Browser consent page (GET renders, POST issues code) |
| `handleToken` | RFC 7636 | PKCE S256 code exchange → HMAC-SHA256 JWT |
| `requireBearer` | — | Middleware: validates JWT, emits RFC 9728 challenge on failure |
| `requestBaseURL` | — | Derives `scheme://host` from request, honours `X-Forwarded-*` |

**The two intentional demo simplifications** — both flagged in the code and in
`docs/oauth-deep-dive.md`:
1. **In-memory stores.** `registeredClient` and `authCode` maps live in the
   `authServer` struct. A cold start forgets them. For production, plug in a DB.
2. **No real end-user login.** `handleAuthorize` approves without authenticating
   a human. In production, verify the user with an IdP before issuing a code.

When adding new OAuth behaviour, preserve both flags — do not silently promote
the demo to "production ready" by adding partial persistence or a fake login.
Either fix it completely (with a DB + IdP) or keep the limitation explicit.

---

## Architecture principles — non-negotiable

**Public discovery, protected protocol.** `/.well-known/*` and `/api/oauth/register`
are unauthenticated. Everything else that touches data or issues tokens goes
through `requireBearer`. Do not add auth to discovery or remove it from tools.

**Framework-free.** The MCP Go SDK returns a plain `http.Handler`. Use stdlib
`net/http` + the existing middleware chain. Do not introduce gin/echo/chi.

**`/{$}` is an exact-root match, not a catch-all.** The `mux.Handle("/{$}", ...)`
line in `main.go` matches *only* `/` (Go 1.22+ semantics), so unknown paths
still 404. If you add routes, add them explicitly — do not change the root mount
into a catch-all.

**Thin tool files, fat auth package (post-refactor).** Once `pkg/mcpauth` exists,
a new tool's `main.go` should do nothing except: call `mcpauth.Mount(mux, authz)`,
register its tools, and listen. All auth logic stays in the shared package.

**`requestBaseURL` is the only source of truth for origins.** It honours
`X-Forwarded-Proto` and `X-Forwarded-Host` so the same binary answers correctly
both locally and behind the Cloud Run / GFE proxy. Never hard-code a hostname
in an OAuth response.

---

## Quality gates — all must pass before merge

```bash
go mod tidy && git diff --exit-code go.mod go.sum  # no stale deps
go build ./...
go test -race -count=1 ./...
go vet ./...
gofmt -l .   # empty output = clean
golangci-lint run   # must report "0 issues."
```

CI runs the same gates on every push/PR (`.github/workflows/ci.yml`).

**Trust the linter.** An `ineffassign` finding once exposed a real auth bug in
the production reference (a shadowed `err` silently bypassed a user check). Do
not add `//nolint` without understanding *why* the linter is firing — it is
often right.

---

## Security conventions

**JWT signing key.** `JWT_SIGNING_KEY` is read from the environment. The dev
fallback (`"dev-insecure-signing-key-change-me"`) is intentionally named so it
is never confused with a real key. `scripts/deploy.sh` generates a cryptographic
key automatically if none is set. **Never commit a real key.**

**`AUTH_BYPASS=true`** is a local-only escape hatch that skips all JWT
verification. It is checked at the top of `requireBearer`. Never set it in a
deployed or publicly accessible instance — the code logs a warning when it is
active.

**Redirect URI validation.** `validateRedirectURI` accepts `https` or `http` on
loopback (RFC 8252 native-client rule). `redirectURIMatch` compares loopback
URIs port-agnostically (Spark uses ephemeral ports). If you touch either
function, make sure `TestRedirectURIMatchLoopback` covers the new cases.

**No secrets in logs.** `logRequests` in `main.go` logs method, path, and
user-agent only — never headers or body. Keep it that way.

---

## Issue tracking (bd / beads)

Run `bd prime` for full AI workflow context. Quick reference:

```bash
bd ready                         # find unblocked work
bd update <id> --claim --status=in_progress   # claim before starting
bd create --title="…" --type=task --priority=2  # new issue
bd close <id> --reason="…"       # close when done
```

**Always claim a task before writing code.** The epic is `spark-agent-tools-m28`;
tasks are `m28.N`. Check `bd blocked` before starting `m28.5` (it depends on
`m28.6` landing first).

---

## Diagrams

Diagrams are authored as Graphviz DOT in `docs/`, rendered to WebP:

```bash
dot -Tpng -Gdpi=144 docs/architecture.dot -o /tmp/arch.png
cwebp -q 90 /tmp/arch.png -o docs/architecture.webp
# brew install graphviz webp
```

`shape=actor` is unsupported in Graphviz 14 and silently falls back to `box` —
use `shape=box` directly.

---

## Production reference

This repo is a tutorial. For a production-grade server running the same RFC stack
with persistent Firestore storage, Firebase user auth, per-user scopes, and dual
MCP + A2A protocols from one binary, see the
[eldamo-server](https://github.com/ghchinoy/eldamoapi) project this repo was
distilled from. When the demo simplifications flag says "see the production
reference", that's where to look.
