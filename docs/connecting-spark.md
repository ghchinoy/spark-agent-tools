# Connecting your server to Gemini Spark

Once your `hello-spark-mcp` server is deployed (or running behind an HTTPS tunnel),
here's how to add it as a custom Connected App in the Gemini web app.

## Prerequisites

Per Google's [requirements](https://support.google.com/gemini/answer/17209137):

- Access to **Gemini Spark**.
- **18+ and in the US.**
- Signed in with a **personal** Google Account (not Workspace/school).
- **Keep Activity** turned on.
- A **publicly reachable HTTPS** MCP server URL. Cloud Run gives you one; for
  local testing use a tunnel (e.g. `cloudflared`, `ngrok`) — Spark can't reach
  `localhost`.

> Custom Connected Apps are currently English-only and configured from the
> **web** app (they then work in the Gemini mobile app too).

## Steps

1. Go to [gemini.google.com](https://gemini.google.com).
2. **Settings & help → Connected Apps** (if you don't see it, open
   **Personal Intelligence → Connected Apps**).
3. Under **"Custom apps for Spark"**, find *"Add a custom app link to get
   started"* (or click **Add a custom app**).
4. Enter your server URL. Both of these work with this repo:
   ```
   https://YOUR-SERVICE.run.app
   https://YOUR-SERVICE.run.app/mcp
   ```
   You do **not** need to open "Advanced features" / enter a client ID & secret —
   this server supports automatic (dynamic) registration.
5. Click **Next**, then follow the prompts:
   - Spark discovers the server, auto-registers, and opens the **consent page**.
   - Approve the connection.
6. Spark redirects back and the app is connected. It's now available in Spark on
   both web and mobile.

## Try it

Ask Spark something that would use your `echo` tool, e.g.:

> *"Use my hello-spark-mcp app to echo 'the road goes ever on'."*

You should see the reply come back through the tool
(`Hello from your Spark MCP server! You said: …`).

## Watching it work

Tail your Cloud Run logs while connecting to see the exact discovery/auth dance
described in [`oauth-deep-dive.md`](oauth-deep-dive.md):

```bash
gcloud run services logs read hello-spark-mcp --region us-central1 --limit 50
```

You'll see, in order: a `HEAD /` probe (401), the two `.well-known` fetches
(200), `POST /api/oauth/register` (201), the consent + token exchange, then
`POST /mcp` tool calls carrying the Bearer token.

## Managing / removing the app

- **Disconnect / remove:** Connected Apps → your app → **More details →
  Disconnect** or **Remove app**.
- **Revoke the link** from your Google Account:
  [myaccount.google.com/connections](https://myaccount.google.com/connections).

## Troubleshooting

| Symptom | Likely cause | Fix |
| :--- | :--- | :--- |
| "does not support automatic registration" | `registration_endpoint` missing from AS metadata, or Spark couldn't reach the `.well-known` docs | Confirm `curl https://…/.well-known/oauth-authorization-server` shows `registration_endpoint`; confirm the URL is public HTTPS |
| Connection fails with 404s in logs | Missing RFC 9728 PRM document | Confirm `curl https://…/.well-known/oauth-protected-resource` returns JSON |
| Consent page never appears | `authorize` endpoint error or bad `redirect_uri` | Check logs for `[authorize]`; ensure the client's redirect_uri validates |
| Tools 401 after connecting | Token not being sent / JWT key changed between deploys | Keep `JWT_SIGNING_KEY` stable across deploys; reconnect the app |
