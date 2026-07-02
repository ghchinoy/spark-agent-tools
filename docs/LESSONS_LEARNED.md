# Lessons Learned: connecting a production MCP server to Gemini Spark

Field notes from connecting [eldamo-server](https://github.com/ghchinoy/eldamoapi)
— a real, production Go MCP + A2A server — to Gemini Spark. These are the things
that only show up when you read the Cloud Run logs, not when you read the RFCs.

The [oauth-deep-dive.md](oauth-deep-dive.md) explains *what* each spec requires.
This document records *what Spark actually does* at runtime and the surprises we
hit along the way.

---

## 1. The 404 storm is the first symptom, not the root cause

When Spark can't discover your server, logs fill with 404s before you ever see
the "does not support automatic registration" message:

```
GET /.well-known/oauth-protected-resource/sse   404
GET /.well-known/oauth-protected-resource       404
POST /                                           404
HEAD /                                           404
```

The important thing to understand: these are **two independent failures stacked**.

- The 404s on `/.well-known/oauth-protected-resource` mean Spark never finds the
  authorization server at all — discovery dead-ends before it reaches your RFC
  8414 document. This breaks *every* modern MCP client, not just Spark. We saw
  `opencode/1.17.11` hitting the same two paths identically.
- The "does not support automatic registration" message is a *second* failure that
  only surfaces once discovery works — Spark found the auth server but it has no
  `registration_endpoint`.

Fix the 404s first (RFC 9728 PRM). The error message resolves separately (RFC 7591
DCR). Don't conflate them.

---

## 2. The exact request sequence Spark sends

From Cloud Run logs (User-Agent: `Google` / `OpenAuth`), a successful first
connection looks like this:

```
HEAD /sse                                         401   UA: Google    (probe)
GET  /.well-known/oauth-protected-resource        200   UA: Google    (RFC 9728)
GET  /.well-known/oauth-authorization-server      200   UA: Google    (RFC 8414)
POST /api/oauth/register                          201   UA: OpenAuth  (RFC 7591 DCR)
  … user sees consent page, signs in …
POST /api/oauth/authorize-callback                200   UA: Chrome    (consent approved)
POST /api/oauth/token?resource=…/sse              200   UA: OpenAuth  (PKCE exchange)
POST /sse  (Authorization: Bearer eyJ…)           200   UA: Google    (tool calls)
POST /sse  (Authorization: Bearer eyJ…)           200   UA: Google
```

Key observations:
- Spark uses **two different User-Agents**: `OpenAuth` for the OAuth leg (DCR
  registration and token exchange), `Google` for everything else including tool
  calls.
- The token endpoint is called with a `?resource=` query parameter (e.g.
  `?resource=https://your-server.example/sse`). Your token handler must accept
  (and can safely ignore) this parameter — don't reject it as an unexpected field.
- Spark registered with `client_name: "Google"` and sent **6 `redirect_uris`**
  covering its various callback surfaces. Accept them all; don't cap at a number
  lower than 6.

---

## 3. Spark re-probes on each conversation turn — this is normal

After the initial connection, Spark fires a bare `HEAD /sse` (without an
`Authorization` header) before some (not all) subsequent conversation turns:

```
HEAD /sse                                         401   (no token)
GET  /.well-known/oauth-protected-resource        200   (re-discovery)
GET  /.well-known/oauth-authorization-server      200   (re-discovery)
POST /sse  (Authorization: Bearer eyJ…)           200   (tool calls — same token)
```

This is **Spark/OpenAuth client behavior, not server enforcement**. The server
is doing the right thing: a tokenless `HEAD` correctly returns 401. Two things
prove it is not forced re-auth:

1. No new `POST /api/oauth/token` occurs after the first exchange — the same JWT
   is reused throughout.
2. The re-probing is not consistent: some conversation turns skip it entirely,
   going straight to tool calls.

The `OpenAuth` library appears to do a connection health-check before some turns
— re-validating that the endpoint still returns an auth challenge and that the
server's OAuth configuration has not changed since it last fetched the discovery
documents. `opencode`, which uses CIMD, does none of this: it issues
`POST /sse` with a Bearer token directly every time.

**Practical impact:** each re-probe adds two cheap discovery GETs (both static
JSON responses, no database) and an unnecessary `[Auth] Debug Headers` log line.
Gate the header-dump log to fire only when a token is present but invalid, not
when the token is simply absent — otherwise Spark's health-check pattern makes
the logs noisy.

---

## 4. Spark uses the Streamable HTTP transport, not SSE

Despite the endpoint being named `/sse`, Spark uses the **Streamable HTTP MCP
transport** (not the legacy SSE transport). In the multiplexer this means:

- `POST /sse` — rapid RPC: Spark sends a tool request, server responds
  immediately (2–5ms). Used for every tool call.
- `GET /sse` — long-lived streaming connection: server pushes events to the
  client. Spark holds this open for the duration of the session.

The `GET /sse` connections are what create the long-latency entries in Cloud Run
logs. Which leads directly to the next lesson.

---

## 5. Cloud Run's default 300-second timeout kills long-lived streaming connections

**This is the most important production gotcha.**

Cloud Run's default request timeout is **300 seconds**. The Streamable HTTP
transport's `GET /sse` streaming connection is intended to stay open for the
duration of an MCP session. Cloud Run kills it at exactly 5 minutes — every time:

```
GET /sse   200   301.001993986s   ← Cloud Run terminated this
GET /sse   200   300.999934106s   ← and this
GET /sse   200   301.000387862s   ← and this
Truncated response body. Usually implies that the request timed out…
```

When this happens, Spark loses the session and has to reconnect — triggering
another HEAD probe + discovery + re-use-cached-token cycle.

**The fix:** add `--timeout 3600` to your `gcloud run deploy` command. Cloud Run
supports up to 3600 seconds (1 hour). The `POST /sse` tool calls are unaffected
(they complete in milliseconds); only the persistent streaming GET needs the
extended timeout.

```bash
gcloud run deploy my-mcp-server \
  --timeout 3600 \     # ← this line
  --session-affinity \ # keep clients on the same instance
  # … other flags
```

If you omit `--timeout`, your server will appear to work but sessions will drop
every 5 minutes silently.

---

## 6. CIMD and DCR coexist — run both, dispatch by client_id shape

opencode connected to the same server at the same time as Spark, using the CIMD
(Client ID Metadata Document) mechanism instead of DCR. Both worked
simultaneously without any conflict:

```
opencode/1.17.11  POST /sse  200   (CIMD path — client_id is an https URL)
Google            POST /sse  200   (DCR path  — client_id is "mcp-client-…")
```

The dispatch is trivial: if `client_id` starts with `https://` or `http://`, it
is a CIMD URL → fetch and validate the metadata document. Otherwise it is a
DCR-issued opaque string → look up in your registered-clients store.

Advertise both mechanisms in your RFC 8414 document:

```json
{
  "registration_endpoint": "https://…/api/oauth/register",
  "client_id_metadata_document_supported": true
}
```

Clients self-select the path they support. There is no need to choose one.

---

## 7. Your consent SPA must handle opaque client_ids

If you build a browser-based consent page, do not assume `client_id` is always a
URL. DCR-issued client_ids (e.g. `"mcp-client-tEQk1WspGqUaakNRS8vWFoWsjsm2GjPj"`)
will throw a `SyntaxError` in `new URL(clientId)`.

In our implementation, the catch block displayed **"Invalid Client ID URL"** to
the user, which was alarming even though the authorization POST was functionally
correct (it forwarded `clientId` verbatim).

Detect the shape first:

```javascript
let isURL = false;
try {
    const parsed = new URL(clientId);
    isURL = parsed.protocol === 'https:' || parsed.protocol === 'http:';
} catch (_) { }

if (isURL) {
    // CIMD path: use hostname as display fallback, try to fetch metadata doc
} else {
    // DCR path: display client_id as-is, label as "Registered Application"
    // DO NOT attempt fetch(clientId) — it is not a URL
}
```

The approval POST is unaffected — pass `clientId` verbatim to your backend
regardless of shape. The backend's `resolveClient` function handles the dispatch.

---

## 8. Log filtering tips for Spark sessions

| What you want to see | Filter |
| :--- | :--- |
| Full auth flow (DCR + token) | `textPayload =~ "\[DCR\]\|\[OAuth\]"` |
| Spark's re-probes only | `httpRequest.requestMethod="HEAD" AND httpRequest.status=401` |
| Tool calls only | `textPayload =~ "\[Tool Call\]"` |
| OAuth leg only | `httpRequest.userAgent:"OpenAuth"` |
| Spark vs opencode traffic | `httpRequest.userAgent:"Google"` vs `httpRequest.userAgent:"opencode"` |

In Cloud Run logs, Spark's OAuth leg (`OpenAuth` UA) and its tool calls
(`Google` UA) appear as separate log entries even for the same session — filter
by both when debugging an end-to-end flow.

---

## 9. What the DCR registration actually looks like in logs

When Spark registers for the first time you will see exactly one line like this:

```
[DCR] Registered public client 'mcp-client-tEQk1WspGqUaakNRS8vWFoWsjsm2GjPj'
      (name="Google", 6 redirect_uris)
```

If your storage is in-memory (as in the hello-world), this registration is lost
on a cold start and Spark will re-register on its next connection — harmless and
transparent to the user, but produces a new `client_id` each time. In production,
persist registered clients (e.g. Firestore with the `client_id` as the document
key) so the same `client_id` survives restarts.

---

## Summary checklist for a production Spark-facing server

- [ ] `GET /.well-known/oauth-protected-resource` returns 200 (bare path)
- [ ] `GET /.well-known/oauth-protected-resource/sse` (or your resource path) returns 200
- [ ] `401` on protected endpoints carries `WWW-Authenticate: Bearer resource_metadata="…"`
- [ ] `GET /.well-known/oauth-authorization-server` includes `registration_endpoint`
- [ ] `POST /api/oauth/register` returns 201, no `client_secret`, accepts ≥ 6 `redirect_uris`
- [ ] Token endpoint accepts (and ignores) the `?resource=` query parameter
- [ ] Consent SPA handles opaque `client_id` strings without treating them as URLs
- [ ] `gcloud run deploy` uses `--timeout 3600` (not the default 300s)
- [ ] `--session-affinity` is set for Cloud Run (keeps streaming sessions on one instance)
- [ ] Registered clients are persisted across cold starts
