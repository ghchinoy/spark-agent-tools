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

From Cloud Run logs, a successful first connection (hello-world-spark, 2026-07-02):

```
15:27:05  HEAD /                                           401   UA: Google    (initial probe; 401 + resource_metadata pointer)
15:27:05  GET  /.well-known/oauth-protected-resource       200   UA: Google    (RFC 9728 PRM)
15:27:05  GET  /.well-known/oauth-authorization-server     200   UA: Google    (RFC 8414 ASM; finds registration_endpoint)
15:27:06  POST /api/oauth/register                         201   UA: OpenAuth  (RFC 7591 DCR; name="Google", 6 redirect_uris)
15:27:47  GET  /authorize?…&resource=…&code_challenge=…    200   UA: Mozilla   (user's browser renders consent page)
15:28:05  POST /authorize                                   302   UA: Mozilla   (user clicked Approve; code issued)
15:28:06  POST /api/oauth/token?resource=…                 200   UA: OpenAuth  (PKCE exchange; JWT issued)
15:28:07  POST /                                            200   UA: Google    (MCP initialize)
15:28:07  POST /                                            202   UA: Google    (async message; response follows on GET stream)
15:28:07  GET  /                                                  UA: Google    (SSE stream open)
15:28:08  HEAD /                                           401   UA: Google    (mid-session re-probe; see §3)
15:28:08  GET  /.well-known/oauth-protected-resource       200   UA: Google
15:28:08  GET  /.well-known/oauth-authorization-server     200   UA: Google
          … further tool calls on POST / …
15:30:26  POST /                                            200   UA: Google
          [tool] echo: "hi there"
```

Key observations:

- Spark uses **three distinct User-Agents**, not two:
  - `Google` — background probes (`HEAD /`), well-known discovery, and all MCP
    tool calls
  - `OpenAuth` — DCR registration (`POST /api/oauth/register`) and token
    exchange (`POST /api/oauth/token`)
  - `Mozilla/5.0` (the user's actual browser) — the `/authorize` consent page
    (`GET` and `POST`)

  Filter logs by `userAgent:"OpenAuth"` to isolate the OAuth leg; filter by
  `userAgent:"Google"` to see tool traffic. The browser leg only appears during
  the one-time consent step.

- The `?resource=` query parameter appears on **both** the `/authorize` and
  `/api/oauth/token` requests (RFC 8707 Resource Indicators). Accept and ignore
  it — don't reject it as an unexpected field.

- Spark registered with `client_name: "Google"` and sent **6 `redirect_uris`**,
  all pointing to `https://oauth-redirect.googleusercontent.com/r/…`. Accept them
  all; don't cap at a number lower than 6. These are `https` URIs, so standard
  redirect-URI validation passes without special-casing.

- Tool calls land on **`POST /`** (the root endpoint), not on a named path like
  `/mcp`. The `/{$}` exact-root mount in `main.go` is not cosmetic — without it,
  Spark's tool calls 404.

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

## 4. Spark may probe both Cloud Run URL forms before settling on one

Cloud Run services have two URL formats:

```
https://<service>-<random-hash>-uc.a.run.app          (region-scoped, shown in console)
https://<service>-<numeric-project-id>.us-central1.run.app  (numeric project URL)
```

In practice, Spark ran the full discovery + DCR sequence on the first URL, then
switched to the second URL and ran it again:

```
15:24:11  POST /api/oauth/register  201  on syzu5sozjq-uc.a.run.app   (first URL)
15:27:06  POST /api/oauth/register  201  on 308690897031.us-central1.run.app  (second URL)
```

Two registrations, two `client_id`s. The full auth flow and all tool calls then
used the numeric URL.

**Why it doesn't break things:** `requestBaseURL` derives the public origin from
the incoming request's `Host` header (honoring `X-Forwarded-Host`), so both URL
forms produce correct OAuth metadata — each registration and its subsequent token
are self-consistent for the URL that issued them.

**What it reveals about in-memory state:** if those two requests hit different
Cloud Run instances, the second instance wouldn't know about the first
registration. With a persistent store this is harmless; with in-memory state it
works only because a single-instance deployment handled both. This is the most
concrete illustration of why in-memory state is a demo limitation and not a
production approach.

---

## 5. Spark uses the Streamable HTTP transport, not SSE

Spark uses the **Streamable HTTP MCP transport** (not the legacy SSE transport),
and it hits the **root endpoint `/`**, not a named path. In the multiplexer the
pattern is:

```
POST /   200   rapid RPC — Spark sends a message, server responds synchronously
POST /   202   async RPC — server accepted the message; response comes on the GET stream
GET  /         long-lived SSE stream — server pushes responses for the 202 messages
DELETE / 401   session teardown — Spark sends this after each conversation turn
```

The `202 Accepted` status means "I received your message; watch the GET stream
for the response." You will see `POST / → 200` and `POST / → 202` interleaved
in logs, with each `202` paired to a `GET /` SSE connection already open.

**`DELETE / → 401` after every turn.** Spark sends `DELETE /` with a
`Mcp-Session-Id` header to terminate the Streamable HTTP session after each
conversation turn. These consistently return `401` even when the same session's
`POST /` requests succeed with the Bearer token moments later. This indicates
Spark does not include the `Authorization` header on `DELETE` requests. The 401
is harmless — the session continues for subsequent turns — but it produces
noise in logs. Do not treat `DELETE 401` as a session or auth failure.

`GET /` connections are what create long-latency entries in Cloud Run logs.
Which leads directly to the next lesson.

---

## 6. Cloud Run's default 300-second timeout kills long-lived streaming connections

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

## 7. CIMD and DCR coexist — run both, dispatch by client_id shape

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

## 8. Your consent SPA must handle opaque client_ids

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

## 9. Log filtering tips for Spark sessions

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

## 10. What the DCR registration actually looks like in logs

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

## 11. Spark requires explicit per-tool-call user confirmation

Spark presents a confirmation UI to the user **before every individual tool call**
— the user must click Allow or Deny in the browser before the HTTP request reaches
your server. This is a deliberate client-side "human in the loop" safety policy and
cannot be changed from the server side.

From Google's docs: *"Currently, Gemini requires manual confirmation for any write
actions."* In practice this applies to all tool calls, read or write.

The consequence is visible in server logs: each tool call appears as a separate
cluster with multi-second gaps between them — not because the server is slow, but
because the gaps are the user's click latency. Our tool calls execute in 2–90ms;
the server is idle waiting for the human to approve.

```
14:49:11  enquire_lexicon: wild         ← user clicked Allow
14:50:08  enquire_lexicon: disorder     ← 57s gap = user clicked Allow
14:50:38  enquire_lexicon: rúcina       ← 30s gap
14:51:04  enquire_lexicon: RUK          ← 26s gap
14:52:06  get_word_details: '92766143'  ← 37s gap
```

A question that requires 8 tool calls to answer fully requires 8 separate clicks.
**Design implications:**
- For research-heavy tools, minimize the number of round trips — Spark's latency is
  dominated by click latency, not network or server time.
- Prefer tools with broad, expressive queries over many narrow lookups. One
  well-designed tool call that returns rich results beats four narrow ones.
- Describe tools clearly: Spark shows the tool name and arguments to the user before
  they approve. A clear tool name (`enquire_lexicon`) and legible argument values
  (`query='star', language='q'`) build user trust and reduce hesitation-clicks.
- Clients like opencode use session-level trust (OAuth connection = blanket approval)
  and produce a very different call pattern — no gaps, all calls fire immediately.

---

## 12. The 409 Conflict on GET /sse: streaming channel conflicts and stuck sessions

### What 409 means in Streamable HTTP

In the MCP Streamable HTTP transport, `GET /sse` opens the server-to-client
push channel — the half of the connection the server uses to deliver async
responses. The MCP Go SDK enforces **one active GET stream per session ID**: if a
second `GET /sse` arrives while one is already open for that session, it returns
`409 Conflict`.

In production logs this looks like a ~30–90 second heartbeat of 409s, interspersed
with successful `POST /sse` tool calls:

```
15:38:17  GET /sse → 409
15:38:21  POST /sse → 200   enquire_lexicon: 'mix'        ← tool call still works
15:38:24  GET /sse → 409
15:39:15  POST /sse → 200   enquire_lexicon: 'rúcina'     ← still works
15:42:06  GET /sse → 409
15:42:13  POST /sse → 200   get_derivations: '92766143'   ← still works
```

### Why tool calls still succeed despite 409s

The Streamable HTTP protocol has two response paths:

- **Synchronous (POST → 200):** The server writes the tool result directly into
  the `POST` response body. No GET stream needed. Fast lexicon lookups almost
  always take this path.
- **Async (POST → 202 + GET stream):** The server acknowledges receipt via 202
  and later pushes the result on the open GET stream.

The 409 only breaks the async path. Read-only tool calls that complete in
milliseconds (like `enquire_lexicon`) typically return 200 synchronously, so
they succeed even when the GET stream is in conflict. The session stays
functional for most tool calls while the 409s persist.

### Does hello-world see this too?

Yes. The hello-world server (root `/{$}` endpoint, short echo sessions) also
produces `GET / → 409` in logs after a cold start or redeploy:

```
15:33:38  GET / → 409   (session unknown after prior cold start)
15:40:31  GET / → 409   (again, after reconnect attempt)
```

The named-path (`/sse`) vs root (`/`) mount makes no difference — the MCP Go
SDK enforces the one-active-stream-per-session-ID rule regardless of path. Short
sessions reduce exposure but do not eliminate it. Apply `KeepAlive` regardless.

### What causes the conflict

The 409 pattern emerged during and after a Cloud Run **deployment rollout**. When
traffic shifts from the old revision to the new one mid-session:

1. Spark's existing GET stream, held open on the old instance, is killed as the
   old instance drains.
2. Spark immediately tries to reopen a GET stream on the new instance.
3. The new instance has no record of this session, or — if `--session-affinity`
   is set but a reconnect races with cleanup — the SDK still has a stale entry.
4. Result: `404` (unknown session) or `409` (duplicate stream) on `GET /sse`.

From the logs, immediately after the new revision started:

```
14:47:21  GET /sse → 404   (session ID unknown to new instance)
14:47:22  GET /sse → 404
14:47:24  Truncated response body   (old instance connections killed)
           → Spark reconnects; subsequent GET /sse land as 409
```

### The stuck-session ("Thinking it through…") failure mode

When the LLM finishes generating its response but needs to deliver it via a **202
async path** and the GET stream is stuck in 409 conflict, the response is
generated on the server but never reaches the client. Spark's UI shows the
permanent "Thinking it through..." spinner:

- The server has processed all tool calls successfully (all visible in logs).
- No further `[Tool Call]` entries appear — the LLM has finished research.
- `GET /sse → 409` repeats every ~60s — Spark polling for a stream it can't open.
- No error surfaced to the user; it just never resolves.

**Recovery:** disconnect and reconnect the Spark → server connection. This clears
the session state on both sides and starts fresh.

### Mitigations

- **Server-side keepalive — `mcp.ServerOptions{KeepAlive: 30 * time.Second}`
  (validated, primary mitigation):** The MCP Go SDK's `ServerOptions.KeepAlive`
  field sends a ping to the client on the open GET stream at the specified interval.
  This keeps the stream demonstrably alive, giving Spark's OpenAuth library no
  reason to close and reopen it. If Spark stops responding to pings, the SDK
  closes the session cleanly — the client gets a fresh reconnect rather than a
  permanently stuck 409. Wire it in when constructing the server:
  ```go
  server := mcp.NewServer(&mcp.Implementation{Name: "my-server", Version: "1.0"},
      &mcp.ServerOptions{KeepAlive: 30 * time.Second})
  ```

- **`--no-traffic` deploy (validated, prevents rollout-induced 409s):** Deploy
  the new revision without immediately shifting traffic, so active sessions are
  not disrupted. Cut over once sessions are idle:
  ```bash
  gcloud run deploy my-mcp-server --no-traffic [… other flags]
  # when ready:
  gcloud run services update-traffic my-mcp-server --to-latest --region us-central1
  ```

- **`--session-affinity`** (already in the deploy script): routes a client's
  requests to the same Cloud Run instance, reducing the chance that a reconnect
  hits a different instance with no session state.

- **Track 409 rate as a health signal**: a sustained `GET /sse → 409` rate with
  no new `[Tool Call]` log entries is a reliable indicator of a stuck session.
  A log-based alert on this pattern lets you detect it without waiting for user
  reports of a frozen "Thinking it through..." spinner.

---

## 13. MCP method logging: seeing inside the POST / black box

All MCP protocol traffic over Streamable HTTP is `POST /`. Without body
logging, you cannot distinguish `initialize` from `tools/list` from
`prompts/list` from `tools/call` — they all look identical in Cloud Run logs.

Add a middleware that peeks at the JSON-RPC `method` field before passing the
request to the MCP handler:

```go
func logMCPMethod(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Method == http.MethodPost && r.Body != nil {
            peek, err := io.ReadAll(io.LimitReader(r.Body, 512))
            r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(peek), r.Body))
            if err == nil {
                var msg struct {
                    Method string `json:"method"`
                    Params struct{ Name string `json:"name"` } `json:"params"`
                }
                if json.Unmarshal(peek, &msg) == nil && msg.Method != "" {
                    if msg.Params.Name != "" {
                        log.Printf("[mcp] %s %s", msg.Method, msg.Params.Name)
                    } else {
                        log.Printf("[mcp] %s", msg.Method)
                    }
                }
            }
        }
        next.ServeHTTP(w, r)
    })
}
```

Apply it between `RequireBearer` and the MCP handler so you only log
authenticated traffic:

```go
secured := authz.RequireBearer(logMCPMethod(mcpHandler))
```

With this in place, logs show the exact MCP protocol sequence:

```
[mcp] initialize
[mcp] tools/list
[mcp] prompts/list          ← or absent, which tells you the client skips it
[mcp] tools/call echo
[tool] echo: "hello"
```

This is the only reliable way to confirm whether a client calls `prompts/list`,
`resources/list`, or any other capability — HTTP-level logs alone cannot tell you.

---

## 14. MCP spec fields Spark's current client does not surface

The MCP spec (via the Go SDK) supports `Icons`, `Title`, `WebsiteURL`, and
`Instructions` on `Implementation`, and `Icons` and `Title` on `Tool`. It also
supports `prompts` as a discoverable capability. Based on observed behavior with
Spark's Connected Apps UI (as of July 2026):

- **`Icons`** — delivered inline as a data URI in the `initialize` response.
  No separate HTTP fetch is made for the icon. Spark's UI does not render it.
- **`Title`** — present in the `initialize` response. Spark's UI shows the
  Cloud Run service name, not the `Implementation.Title`.
- **`Instructions`** — present in the `initialize` response. Not visible in
  the Spark UI; likely consumed by the LLM context rather than rendered.
- **`prompts/list`** — the server advertises the `prompts` capability in
  `initialize`. Whether Spark calls `prompts/list` requires MCP method logging
  to confirm (HTTP logs alone cannot tell you). No prompt-related UI appeared.
- **`service_documentation`** (RFC 8414) — present in AS metadata which Spark
  fetches. Not surfaced in Spark's Connected Apps UI.

These fields are **correct per the MCP spec** and worth setting for other
clients (Claude Desktop, opencode) that do read them. They are also
forward-compatible — Spark may surface them in future releases.

---

## 13. Spark may respond in an unexpected language regardless of user locale

After a successful multi-tool research session that produced genuine, linguistically
correct Tolkien-language neologisms, Spark delivered the entire response in
**Mandarin Chinese** — despite the user's system locale being English, the question
being asked in English, and the user's self-reported Mandarin proficiency being
approximately two phrases.

The response content itself was correct and sophisticated (proper Quenya abstract
suffixes, Sindarin soft mutation rules applied accurately). The language of delivery
was simply wrong.

This is entirely a **Spark/Gemini behavior**, not a server issue. The MCP server
has no influence over the language Spark uses to compose its final response — that
is generated by Gemini's LLM after all tool results have been collected, and the
language selection appears to operate independently of both the user's account locale
and the language of the original question.

**Practical implications:**
- Do not attempt to influence Spark's response language from the server side (e.g.
  via tool descriptions or result formatting) — there is no reliable mechanism for
  this.
- If you need language control, it must come from the user's Spark/Gemini account
  settings or from explicit language instructions in the prompt itself
  ("respond in English").
- From a debugging perspective: an unexpected response language is a Gemini concern,
  not an MCP server concern. If your logs show clean tool calls, clean 202 delivery,
  and no 409s or truncations, your server did its job correctly — regardless of what
  language the answer arrives in.

---

## Summary checklist for a production Spark-facing server

- [ ] `GET /.well-known/oauth-protected-resource` returns 200 (bare path)
- [ ] `GET /.well-known/oauth-protected-resource/sse` (or your resource path) returns 200
- [ ] `401` on protected endpoints carries `WWW-Authenticate: Bearer resource_metadata="…"`
- [ ] `GET /.well-known/oauth-authorization-server` includes `registration_endpoint`
- [ ] `POST /api/oauth/register` returns 201, no `client_secret`, accepts ≥ 6 `redirect_uris`
- [ ] Token endpoint accepts (and ignores) the `?resource=` query parameter
- [ ] Consent SPA handles opaque `client_id` strings without treating them as URLs
 - [ ] `requestBaseURL` derives origin from the request `Host` header, not a hardcoded value (both Cloud Run URL forms work without config)
 - [ ] `JWT_SIGNING_KEY` is pinned in the deploy config — a rotated key invalidates all existing tokens; Spark's re-enable flow does not restart OAuth automatically
 - [ ] `mcp.ServerOptions{KeepAlive: 30 * time.Second}` passed to `mcp.NewServer` (prevents GET stream idle closure → 409 conflict)
 - [ ] `gcloud run deploy` uses `--timeout 3600` (not the default 300s)
 - [ ] `--session-affinity` is set for Cloud Run (keeps streaming sessions on one instance)
 - [ ] Use `--no-traffic` when deploying while sessions are active; cut over manually with `update-traffic --to-latest`
 - [ ] Registered clients are persisted across cold starts (in-memory state re-registers on every cold start and on each Cloud Run URL form Spark probes)
 - [ ] `DELETE / → 401` is expected and harmless — Spark omits `Authorization` on session teardown requests; do not treat as an auth failure
 - [ ] MCP method logging added (`logMCPMethod` wrapper) so `POST /` lines show the actual protocol method (`initialize`, `tools/list`, `tools/call echo`, etc.)
