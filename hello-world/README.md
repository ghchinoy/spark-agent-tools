# hello-world

A minimal MCP server for Gemini Spark: a single `echo` tool behind the full
OAuth 2.1 authorization chain, in one binary with no database. It's the
reference tool in this monorepo — the auth scaffolding lives in
[`../pkg/mcpauth`](../pkg/mcpauth), so this directory holds only tool logic.

- `main.go` — HTTP wiring: routes, middleware, startup
- `mcp.go` — the echo tool + SSE/Streamable-HTTP multiplexer
- `Dockerfile` — local Docker builds (Cloud Run uses the repo-root Dockerfile)
- `scripts/deploy.sh` — one-command Cloud Run deploy
- `Makefile` — developer targets
- `.env.example` — config template; copy to `.env` (gitignored)

## Run it

```bash
make run-dev            # serves on :8080, auth bypassed (fastest loop)
make test               # full DCR → PKCE → token → protected-call flow
make run                # auth enforced; set JWT_SIGNING_KEY
```

## Deploy it

```bash
cp .env.example .env    # set GCP_PROJECT
make deploy             # builds from source, prints your Service URL
```

Then paste the Service URL into Gemini Spark → Connected Apps.

## More

- [Repo README](../README.md) — overview, quick start, documentation map
- [`docs/TUTORIAL.md`](../docs/TUTORIAL.md) — build this server step by step
- [`docs/connecting-spark.md`](../docs/connecting-spark.md) — connect to Spark + troubleshooting
- [`docs/oauth-deep-dive.md`](../docs/oauth-deep-dive.md) — why the OAuth 2.1 chain is required
- [`docs/LESSONS_LEARNED.md`](../docs/LESSONS_LEARNED.md) — field notes from live Spark connections
