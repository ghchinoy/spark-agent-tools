# Tutorial: connect Omi's MCP server to Gemini Spark

[Omi](https://omi.me) is a personal-AI wearable/app whose backend exposes a
hosted [Model Context Protocol](https://modelcontextprotocol.io) server at
`https://api.omi.me/v1/mcp/sse` — 22 tools covering your memories,
conversations, action items, goals, chat history, contacts, screen activity,
and daily summaries. This tutorial walks through connecting it to
**Gemini Spark** as a custom Connected App, and — because Omi's server takes a
different path than the `pkg/mcpauth` scaffolding in this repo — explains
exactly where that connection currently breaks down and how to work around it.

**Prerequisites:** an Omi account (app or [omi.me](https://omi.me)), and
[Gemini Spark access](https://support.google.com/gemini/answer/17094507)
(personal Google Account, 18+, US, Google AI Pro/Ultra subscription,
[Keep Activity](https://myactivity.google.com/product/gemini?utm_source=help)
on).

> **TL;DR:** Direct Spark ↔ Omi connection doesn't work out-of-the-box today. Live-tested
> against `api.omi.me` (2026-08-08): its OAuth authorization-server metadata
> has no `registration_endpoint`, so Spark can't auto-register, and manually
> entering a client ID fails with `Unknown OAuth client` because Omi's OAuth
> clients are server-side allow-listed (only ChatGPT/Claude today).
> Filed as [Omi GitHub Issue #11263](https://github.com/BasedHardware/omi/issues/11263).
> 
> **Workaround:** We built [`omi-bridge`](../omi-bridge/), a lightweight Go proxy that mounts this repo's `pkg/mcpauth` (OAuth 2.1 + DCR) on the Spark front door and forwards calls to `api.omi.me` with your static `omi_mcp_...` key.
> **Validated (2026-08-08):** Spark connected to `omi-bridge`, retrieved Omi memories, and composed them into a Google Doc in the same conversation turn. See §6 for details.

---

## 0. What Omi's MCP server offers

Full reference: [Omi MCP docs — Introduction](https://docs.omi.me/doc/developer/mcp/introduction) ·
[Tools Reference](https://docs.omi.me/doc/developer/mcp/tools) ·
[Setup](https://docs.omi.me/doc/developer/mcp/setup).

| Domain | Tools |
| :--- | :--- |
| Memories | `get_memories`, `search_memories`, `create_memory`, `edit_memory`, `delete_memory` |
| Conversations | `get_conversations`, `search_conversations`, `get_conversation_by_id` |
| Action items | `get_action_items`, `search_action_items`, `create_action_item`, `complete_action_item`, `update_action_item`, `delete_action_item` |
| Goals | `get_goals` |
| Chat history | `get_chat_messages` |
| People | `get_people` |
| Screen activity | `get_screen_activity` |
| Daily summaries | `get_daily_summaries` |
| X (Twitter) imports | `get_x_posts`, `search_x_posts` |

Transport is **Streamable HTTP** per the **MCP 2025-03-26 spec** — the same
transport this repo's `hello-world` server uses, and the one Gemini Spark
speaks. Omi supports two authentication methods on that one endpoint:

1. A static personal API key (`omi_mcp_...`) sent as `Authorization: Bearer omi_mcp_...`.
2. A full OAuth 2.1 authorization-code + PKCE flow, with its own
   `/authorize` and `/token` endpoints and scopes like `memories.read`,
   `memories.write`, `conversations.read`, `action_items.read/write`,
   `goals.read`, `chat.read`, `screen_activity.read`, `people.read`.

Gemini Spark's custom-app flow expects **OAuth**, not a pasted static bearer
token, so option 2 is the relevant one for this tutorial.

---

## 1. Get an Omi MCP API key (for reference / fallback use)

Even though Spark uses OAuth, get this first — you'll want it for the `curl`
checks in §3 and it's required for every *other* MCP client (Claude Desktop,
Cursor, etc.):

1. Open the Omi app → **Settings → Developer → MCP**.
2. Generate a key. It looks like `omi_mcp_...`.

<sub>This is a different key from the `omi_dev_...` Developer API key used for
direct REST calls — see
[Omi MCP Troubleshooting](https://docs.omi.me/doc/developer/mcp/troubleshooting)
if you mix them up.</sub>

---

## 2. Try connecting Omi directly to Spark

Per Google's own instructions for
[adding a custom app](https://support.google.com/gemini/answer/17209137):

1. Go to [gemini.google.com](https://gemini.google.com).
2. **Settings & help → Connected Apps** (or **Personal Intelligence →
   Connected Apps** — see
   [Use & manage Connected Apps](https://support.google.com/gemini/answer/13695044)).
3. Under **"Custom apps for Spark,"** click **Add a custom app**.
4. Enter the MCP server URL:
   ```
   https://api.omi.me/v1/mcp/sse
   ```
5. Click **Next**.

### What happens

Spark's discovery chain runs the same RFC 9728 → RFC 8414 sequence described
in this repo's [`oauth-deep-dive.md`](oauth-deep-dive.md). Below is the
**actual, live response** from `api.omi.me` (verified 2026-08-08) — not a
hypothetical:

```bash
$ curl -s -i https://api.omi.me/.well-known/oauth-protected-resource
```
```
HTTP/2 200
content-type: application/json

{"resource":"https://api.omi.me/v1/mcp/sse","authorization_servers":["https://api.omi.me"],"scopes_supported":["memories.read","memories.write","conversations.read","action_items.read","action_items.write","goals.read","chat.read","screen_activity.read","people.read"],"bearer_methods_supported":["header"],"resource_documentation":"https://docs.omi.me/doc/developer/mcp/setup"}
```

This resolves fine — Omi's `/.well-known/oauth-protected-resource` document
exists and points at the authorization server. Next, Spark fetches the
authorization server metadata:

```bash
$ curl -s -i https://api.omi.me/.well-known/oauth-authorization-server
```
```
HTTP/2 200
content-type: application/json

{"issuer":"https://api.omi.me","authorization_endpoint":"https://api.omi.me/authorize","token_endpoint":"https://api.omi.me/token","response_types_supported":["code"],"grant_types_supported":["authorization_code","refresh_token"],"code_challenge_methods_supported":["S256"],"token_endpoint_auth_methods_supported":["client_secret_post","none"],"scopes_supported":["memories.read","memories.write","conversations.read","action_items.read","action_items.write","goals.read","chat.read","screen_activity.read","people.read"]}
```

**This is where it stops working automatically.** Compare this to the
`registration_endpoint` field this repo's `pkg/mcpauth` always includes (see
[`oauth-deep-dive.md` — Step 2](oauth-deep-dive.md#step-2--rfc-8414-where-are-your-endpoints)):
Omi's authorization server metadata has **no `registration_endpoint`**. There
is no [Dynamic Client Registration](https://www.rfc-editor.org/rfc/rfc7591)
(RFC 7591) endpoint on `api.omi.me` at all — confirmed live:

```bash
$ curl -s -i -X POST https://api.omi.me/api/oauth/register \
  -H 'content-type: application/json' \
  -d '{"redirect_uris":["https://accounts.google.com/gemini-oauth-cb"]}'
```
```
HTTP/2 404
content-type: application/json

{"detail":"Not Found"}
```

Without `registration_endpoint`, Spark can't self-issue a `client_id` the way
it can against `hello-world`'s `/api/oauth/register`. You'll see the same
message this repo's docs warn about:

> *"This server does not support automatic registration. To connect, enter
> your own OAuth client ID and secret below."*

— exactly the failure mode documented in
[`oauth-deep-dive.md` — The failure you start with](oauth-deep-dive.md#the-failure-you-start-with),
just triggered by Omi's *production* server instead of a naive demo one.

---

## 3. Why "Advanced features → enter your own client ID" doesn't help either

Google's docs describe a fallback for exactly this case: click **"Advanced
features → Show more"** and paste your own OAuth `client_id`/`client_secret`
([source](https://support.google.com/gemini/answer/17209137)). This is where
Omi's OAuth model diverges further from a generic RFC 7591-compliant server:

Omi's OAuth clients are **allow-listed server-side**, not self-service. Every
`client_id` Omi's `/authorize` and `/token` endpoints will accept is one of:

- A client registered via the `MCP_OAUTH_CLIENTS_JSON` environment variable
  (Omi's own operators use this for ChatGPT/Claude connectors).
- A hardcoded default client for known first-party integrations
  (`omi-chatgpt-prod`, `omi-claude-prod`, etc.), each with a fixed,
  pre-approved `redirect_uri` (or `redirect_uri` *prefix*, for connectors like
  `https://chatgpt.com/connector/oauth/...`).
- An optional generic public PKCE client
  (`MCP_OAUTH_PUBLIC_CLIENT_ID`/`MCP_OAUTH_PUBLIC_REDIRECT_URIS`), *if* an Omi
  operator has configured it.

There is no `POST /register`-style endpoint, no self-serve developer console,
and no path by which an individual end user can mint their own `client_id`
against `api.omi.me`. Any `client_id` you type into Spark's "Advanced
features" box will be rejected at the `/authorize` step with
`invalid_request: Unknown OAuth client` unless:

1. Omi's backend operators have added a client entry with **Gemini Spark's
   exact OAuth redirect URI** allow-listed (the way `omi-claude-prod` and
   `omi-chatgpt-prod` are pre-configured today), and
2. You have that client's `client_id` (and secret, if it's confidential
   rather than PKCE-only).

Confirmed live against `api.omi.me` (2026-08-08) — a made-up `client_id` at
the `/authorize` endpoint (the same request Spark's OAuth flow would make):

```bash
$ curl -s -i "https://api.omi.me/authorize?response_type=code&client_id=gemini-spark-test&redirect_uri=https://accounts.google.com/gemini-oauth-cb&resource=https://api.omi.me/v1/mcp/sse&code_challenge=dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk&code_challenge_method=S256"
```
```
HTTP/2 400
content-type: application/json

{"error":"invalid_request","error_description":"Unknown OAuth client"}
```

**Practically:** direct Spark ↔ Omi connection is not available today without
Omi's backend team provisioning a Spark-specific OAuth client — the same way
they did for ChatGPT and Claude. If you operate Omi's backend, see §5 for
what that would take. If you're an end user, the honest status is: **not yet
supported**; track it via
[Omi's GitHub issues](https://github.com/BasedHardware/omi/issues) or
[Discord](http://discord.omi.me).

---

## 4. What *does* work today

While direct Spark integration isn't available, every other MCP client works
against Omi's hosted server right now using the static API key, no OAuth
dance required — see
[Omi MCP Setup](https://docs.omi.me/doc/developer/mcp/setup):

```json
{
  "mcpServers": {
    "omi": {
      "url": "https://api.omi.me/v1/mcp/sse",
      "headers": { "Authorization": "Bearer omi_mcp_YOUR_KEY_HERE" }
    }
  }
}
```

This works in Claude Desktop, Cursor, and any custom Streamable-HTTP MCP
client — verify it yourself:

```bash
curl -s https://api.omi.me/v1/mcp/sse/info | jq

curl -s -X POST https://api.omi.me/v1/mcp/sse \
  -H "Authorization: Bearer omi_mcp_YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'

curl -s -X POST https://api.omi.me/v1/mcp/sse \
  -H "Authorization: Bearer omi_mcp_YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
```

Spark specifically does not offer a "paste a bearer token" custom-app option
— its custom-app UI is OAuth-only — so this path isn't currently exposed to
Spark users even though the underlying endpoint accepts it fine.

> **✅ Independently verified (2026-08-08):** Connecting an MCP client
> (opencode, via `type: "remote"` with `Authorization: Bearer omi_mcp_...`)
> directly to `https://api.omi.me/v1/mcp/sse` initialized cleanly, discovered all
> 22 tools, and successfully executed `get_memories`. This confirms Omi's MCP
> server and Streamable HTTP transport are fully functional over the static
> bearer-token path — the Spark integration gap is strictly the OAuth 2.1 /
> Dynamic Client Registration layer.

---

## 5. If you operate Omi's backend: what Spark support would require

This section is for Omi maintainers, not end users. To make
`https://api.omi.me/v1/mcp/sse` connectable from Spark with **automatic**
registration (no manual client ID), Omi's authorization server
(`backend/routers/mcp_sse.py`, `backend/database/mcp_oauth.py`) would need the
same four-piece chain this repo's `pkg/mcpauth` already implements — see
[`oauth-deep-dive.md`](oauth-deep-dive.md) for the full walkthrough of each
piece:

| Spec | What Omi has today | What's missing for Spark |
| :--- | :--- | :--- |
| RFC 9728 (Protected Resource Metadata) | ✅ `/.well-known/oauth-protected-resource` | — |
| RFC 8414 (Authorization Server Metadata) | ✅ `/.well-known/oauth-authorization-server` | Add `registration_endpoint` |
| RFC 7591 (Dynamic Client Registration) | ❌ none | Add a `POST /api/oauth/register`-equivalent that mints a public PKCE `client_id` per requesting client, persisted in Firestore (`mcp_oauth_clients`) |
| RFC 7636 (PKCE) | ✅ `/authorize` + `/token`, `S256` required | — |

Without changing any *existing* client behavior, the smallest viable step is
adding a self-service registration endpoint (mirroring
`create_or_update_grant`/`get_client` in `database/mcp_oauth.py`) that:

- accepts `POST` with `redirect_uris` + optional `client_name`,
- validates `https` (or loopback `http`) redirect URIs,
- issues a public (`token_endpoint_auth_method: "none"`) `client_id` — Spark
  is a public client, no secret,
- persists the new client to the same `mcp_oauth_clients` Firestore
  collection `get_client()` already reads from, so it survives restarts and
  plugs into existing grant/token issuance unmodified.

The alternative — provisioning a **static, pre-registered** client for Spark
the same way `omi-claude-prod` is configured via `MCP_OAUTH_CLAUDE_CLIENT_ID`
/ `MCP_OAUTH_CLAUDE_REDIRECT_URIS` — is faster to ship but requires knowing
Spark's exact OAuth callback URL in advance and re-deploying whenever it
changes; DCR avoids that coupling entirely, which is why Google recommends it
for "automatic registration" in the first place.

---

## 6. Interim workaround: the `omi-bridge` proxy

Until native DCR or a pre-configured Spark client is added upstream in Omi ([Issue #11263](https://github.com/BasedHardware/omi/issues/11263)), you can run the [`omi-bridge`](../omi-bridge/) proxy included in this repo.

```
┌──────────────┐   OAuth 2.1 + DCR   ┌────────────────┐   Bearer omi_mcp_...   ┌──────────────┐
│ Gemini Spark │ ──────────────────> │   omi-bridge   │ ─────────────────────> │  api.omi.me  │
│ (Custom App) │ <────────────────── │ (pkg/mcpauth)  │ <───────────────────── │  (Omi MCP)   │
└──────────────┘   Streamable HTTP   └────────────────┘   Streamable HTTP      └──────────────┘
```

### What it does

- **Spark front door:** Mounts `pkg/mcpauth` to handle RFC 9728 (Protected Resource Metadata), RFC 8414 (AS Metadata), RFC 7591 (Dynamic Client Registration), and RFC 7636 (PKCE authorization code flow).
- **Backend proxy:** Forwards authenticated MCP JSON-RPC requests to `https://api.omi.me/v1/mcp/sse`, injecting your personal `OMI_MCP_API_KEY` (`omi_mcp_...`) server-side.
- **Security:** Requires a passphrase during browser consent (`OMI_BRIDGE_PASSPHRASE`) so unauthorized users cannot use your bridge instance to access your Omi memories.
- **Persistence & stability:** Uses a pinned `JWT_SIGNING_KEY` and auto-restores client registrations across cold starts/redeploys so Spark connections stay stable.

### Validated End-to-End Workflow (2026-08-08)

We deployed `omi-bridge` on Cloud Run and connected it as a custom app in Gemini Spark:

1. **OAuth Connection:** Spark auto-discovered metadata, performed DCR, redirected to `/authorize`, accepted the passphrase (`your_secret_passphrase`), and obtained a signed JWT.
2. **Memory Retrieval:** Spark issued `tools/call get_memories`, which the bridge forwarded to `api.omi.me`. Omi returned memories cleanly over Streamable HTTP.
3. **Google Workspace Interop:** In the exact same conversation turn, Spark took the retrieved Omi memories and created a new document in **Google Docs**.

This proves that Spark can seamlessly compose custom MCP tools (via `omi-bridge`) with Google's built-in Workspace Connected Apps in a single task.

See [`omi-bridge/README.md`](../omi-bridge/README.md) for quick-start deployment instructions.

---

## 7. Troubleshooting

| Symptom | Cause | Notes |
| :--- | :--- | :--- |
| "This server does not support automatic registration" | Omi's `/.well-known/oauth-authorization-server` has no `registration_endpoint` (§2) | Use `omi-bridge` (§6) or see issue #11263 |
| `invalid_request: Unknown OAuth client` at `/authorize` | The `client_id` you typed isn't in Omi's allow-list | Use `omi-bridge` (§6) or see §3–§5 |
| `invalid_request: redirect_uri is not registered for this client` | Client exists but Spark's redirect URI isn't in its allow-list | Needs an Omi-side config change (§5) |
| "500. That's an error" in Spark UI on consent | Submitted wrong passphrase or clicked Cancel on `/authorize` | Spark renders OAuth `access_denied` redirects as a generic 500 |
| `401 Unauthorized` calling `/v1/mcp/sse` directly | Bad/missing/revoked `omi_mcp_...` key, or missing `Bearer` prefix | See [Omi MCP Troubleshooting](https://docs.omi.me/doc/developer/mcp/troubleshooting) |
| `429` rate limited | Omi's per-user `mcp:sse` rate limit | Wait and retry; reduce call frequency |
| JSON-RPC error `-32002` | Content is behind Omi's paid plan | Locked memories return truncated (70-char) content; locked conversations hide `action_items`/`events` |
| JSON-RPC error `-32001` | Memory or conversation not found | Check the ID |
| JSON-RPC error `-32601` | Unknown tool name | See the tool table in §0 |

---

## 8. Reference

**Omi**
- [MCP Introduction](https://docs.omi.me/doc/developer/mcp/introduction)
- [MCP Setup](https://docs.omi.me/doc/developer/mcp/setup)
- [MCP Tools Reference](https://docs.omi.me/doc/developer/mcp/tools)
- [MCP Troubleshooting](https://docs.omi.me/doc/developer/mcp/troubleshooting)
- [MCP Examples](https://docs.omi.me/doc/developer/mcp/examples)
- [GitHub — BasedHardware/omi](https://github.com/BasedHardware/omi)

**Gemini Spark / Google**
- [Use Gemini Spark to manage tasks & workflows](https://support.google.com/gemini/answer/17094507)
- [Use & manage Connected Apps in Gemini](https://support.google.com/gemini/answer/13695044)
- [Connect & manage custom apps for Gemini Spark](https://support.google.com/gemini/answer/17209137)
- [MCP Authorization spec (2025-03-26)](https://spec.modelcontextprotocol.io/specification/2025-03-26/basic/authorization/)

**RFCs implemented by `pkg/mcpauth` in this repo**
- [RFC 9728](https://www.rfc-editor.org/rfc/rfc9728) — OAuth 2.0 Protected Resource Metadata
- [RFC 8414](https://www.rfc-editor.org/rfc/rfc8414) — OAuth 2.0 Authorization Server Metadata
- [RFC 7591](https://www.rfc-editor.org/rfc/rfc7591) — OAuth 2.0 Dynamic Client Registration
- [RFC 7636](https://www.rfc-editor.org/rfc/rfc7636) — Proof Key for Code Exchange (PKCE)

**This repo**
- [`docs/oauth-deep-dive.md`](oauth-deep-dive.md) — why each RFC is required, with request traces
- [`docs/TUTORIAL.md`](TUTORIAL.md) — build a Spark-ready MCP server from scratch
- [`docs/connecting-spark.md`](connecting-spark.md) — connect a `pkg/mcpauth`-based server to Spark
