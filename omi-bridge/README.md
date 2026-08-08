# omi-bridge — Gemini Spark Bridge for Omi MCP

`omi-bridge` is a Gemini Spark-ready MCP proxy server for [Omi](https://omi.me). It bridges Gemini Spark (which requires OAuth 2.1 + Dynamic Client Registration) to Omi's hosted MCP server (`https://api.omi.me/v1/mcp/sse`), injecting your personal `OMI_MCP_API_KEY` server-side.

---

## Architecture

```
┌──────────────┐   OAuth 2.1 + DCR   ┌────────────────┐   Bearer omi_mcp_...   ┌──────────────┐
│ Gemini Spark │ ──────────────────> │   omi-bridge   │ ─────────────────────> │  api.omi.me  │
│ (Custom App) │ <────────────────── │ (pkg/mcpauth)  │ <───────────────────── │  (Omi MCP)   │
└──────────────┘   Streamable HTTP   └────────────────┘   Streamable HTTP      └──────────────┘
```

### Why this bridge is needed
Gemini Spark requires custom Connected Apps to support **OAuth 2.1 + Dynamic Client Registration (RFC 7591)**. Omi's hosted server (`api.omi.me`) requires pre-registered OAuth clients (only ChatGPT/Claude today) and lacks a public DCR endpoint ([Omi GitHub Issue #11263](https://github.com/BasedHardware/omi/issues/11263)).

`omi-bridge` solves this:
1. **Front door (Spark-facing):** Mounts `pkg/mcpauth` to implement RFC 9728 (Protected Resource Metadata), RFC 8414 (AS Metadata), RFC 7591 (Dynamic Client Registration), and RFC 7636 (PKCE authorization code flow).
2. **Backend (Omi-facing):** Forwards authenticated Streamable HTTP / JSON-RPC requests to `https://api.omi.me/v1/mcp/sse`, injecting your `omi_mcp_...` key as a `Bearer` token.

---

## Key Features & Security Design

- **Passphrase Consent Gate:** Set `OMI_BRIDGE_PASSPHRASE` in `.env`. When Spark opens `/authorize`, the browser consent page prompts for this passphrase. Unauthenticated users cannot authorize Spark connections to your bridge instance.
- **Pinned `JWT_SIGNING_KEY`:** Prevents token invalidation across deployments. If left blank, `deploy.sh` generates one once, but pinning it in `.env` ensures issued access tokens survive future code deployments.
- **Stateless Client Auto-Restoration:** `pkg/mcpauth` auto-restores dynamically registered public clients (`mcp-client-...`) across server restarts/cold starts, eliminating connection drops.
- **Detailed MCP & Upstream Logging:** Logs tool calls with argument snippets (`[mcp] tools/call get_memories args=...`), upstream round-trip latency (`[proxy] Omi status=200 duration=45ms`), and inspects initial response chunks for MCP/JSON-RPC error payloads (`[proxy] [MCP-ERROR] ...`).

---

## Quick Start

### 1. Prerequisites
- Go 1.25+
- `gcloud` CLI (logged into your GCP project with Cloud Run / Cloud Build permissions)
- An Omi MCP API key (`omi_mcp_...`) generated at [app.omi.me](https://app.omi.me) → **Settings → Developer → MCP**

### 2. Configuration
Copy `.env.example` to `.env`:

```bash
cp .env.example .env
```

Edit `omi-bridge/.env`:
```ini
# Google Cloud target for deployment
GCP_PROJECT=your-gcp-project-id
GCP_REGION=us-central1
SERVICE_NAME=omi-mcp-bridge

# Pin a static HMAC signing key (generate with: openssl rand -hex 32)
JWT_SIGNING_KEY=your_random_32_byte_hex_key_here

# Your personal Omi MCP API key
OMI_MCP_API_KEY=omi_mcp_your_key_here

# Optional: Passphrase required on the /authorize consent screen
OMI_BRIDGE_PASSPHRASE=your_secret_passphrase
```

### 3. Run & Test Locally
```bash
# Run tests
make test

# Start local server (default :8080)
make run
```

Test discovery endpoints:
```bash
curl -s localhost:8080/.well-known/oauth-protected-resource | jq
curl -s localhost:8080/.well-known/oauth-authorization-server | jq
```

### 4. Deploy to Google Cloud Run
```bash
make deploy
```

`scripts/deploy.sh` builds the container image via Cloud Build using `omi-bridge/cloudbuild.yaml` (maintaining root context for `go.mod` and `pkg/mcpauth`) and deploys to Cloud Run with `--timeout 3600` and `--session-affinity`.

The script outputs your Service URL:
```
Service URL: https://omi-mcp-bridge-xyz.a.run.app
```

### 5. Connect to Gemini Spark
1. Go to [gemini.google.com](https://gemini.google.com) → **Settings & help → Connected Apps**.
2. Click **Add a custom app**.
3. Paste your Cloud Run Service URL:
   ```
   https://omi-mcp-bridge-xyz.a.run.app
   ```
4. Click **Next**. Spark auto-discovers endpoints, registers, and opens the consent page.
5. Enter your `OMI_BRIDGE_PASSPHRASE` on the consent page and click **Approve & Connect**.

---

## Log Telemetry

`omi-bridge` outputs structured logs visible in Google Cloud Logging:

| Log Tag | Example Payload | What it indicates |
| :--- | :--- | :--- |
| `[req]` | `[req] POST / (UA: Google)` | Incoming HTTP request and User-Agent |
| `[auth]` | `[auth] Correct passphrase provided during consent` | Consent screen authentication |
| `[dcr]` | `[dcr] registered public client "mcp-client-..."` | Dynamic Client Registration by Spark |
| `[mcp]` | `[mcp] tools/call get_memories args={"limit":10}` | Tool execution with argument preview |
| `[proxy]` | `[proxy] Omi status=200 duration=48ms` | Upstream latency to `api.omi.me` |
| `[proxy] [MCP-ERROR]` | `[proxy] [MCP-ERROR] Omi returned JSON-RPC error: ...` | Upstream JSON-RPC error payload |

---

## Troubleshooting

| Symptom | Cause | Solution |
| :--- | :--- | :--- |
| **Only `echo` tool appears** | Deployed with repo-root `Dockerfile` instead of `cloudbuild.yaml` | Run `make deploy` (uses `cloudbuild.yaml` build target) |
| **"500. That's an error" on consent** | Invalid passphrase or clicked Cancel on `/authorize` | Re-open `/authorize` in Spark and enter exact `OMI_BRIDGE_PASSPHRASE` |
| **Spark tools list becomes empty after deploy** | `JWT_SIGNING_KEY` was not pinned in `.env` | Generate and set `JWT_SIGNING_KEY` in `.env` so keys persist across deploys |
| **Upstream 401 from Omi** | Using `omi_dev_...` (Developer REST API) key instead of `omi_mcp_...` (MCP) key | Generate correct MCP key at [app.omi.me](https://app.omi.me) → Settings → Developer → MCP |

---

## License

Apache 2.0 — see [LICENSE](../LICENSE).
