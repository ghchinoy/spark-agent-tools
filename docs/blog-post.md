# Extending Gemini Spark: building your first custom MCP server

*Draft blog post — pairs with the [spark-agent-tools](https://github.com/ghchinoy/spark-agent-tools) repo.*

---

Gemini Spark is the newest addition to the Gemini app: a personal AI agent that
can automate custom workflows and manage schedules, performing tasks on your
behalf using built-in and custom Connected Apps. The available Connected Apps
include Google Workspace tools (Gmail, Calendar, Docs, Drive), a remote browser
(formerly Project Mariner), and optional third-party apps like Instacart,
OpenTable, and Canva.

It also has the ability to use **custom Connected Apps via hosted MCP servers** —
which means you can extend Spark with your own tools and workflows. This post
shows how to build the simplest possible one: a "hello world" MCP server, hosted
on Cloud Run, that Spark can securely connect to and call.

> Links: [Spark overview](https://support.google.com/gemini/answer/17094507) ·
> [Connected Apps](https://support.google.com/gemini/answer/13695044) ·
> [Add a custom app](https://support.google.com/gemini/answer/17209137)

## The surprise: the hard part isn't the tool

A working tool is about 15 lines. The real work is making your server
**speak OAuth 2.1 the way Spark expects**, because Spark won't call a single tool
until your server proves it's a compliant, securable resource.

Point Spark at a plain MCP URL and you'll likely see:

> *"This server does not support automatic registration. To connect, enter your
> own OAuth client ID and secret below."*

…with your logs filling up with 404s on paths you've never heard of, like
`/.well-known/oauth-protected-resource`.

## The four specs Spark requires

Spark walks a standards-based discovery-and-authorization chain before its first
tool call. Four specs, each solving one problem:

1. **RFC 9728 — Protected Resource Metadata.** "Which authorization server issues
   tokens for you?" Served at `/.well-known/oauth-protected-resource`. Miss this
   and discovery dead-ends — this is the 404 storm, and it breaks *every* modern
   MCP client, not just Spark.
2. **RFC 8414 — Authorization Server Metadata.** "Where are your token,
   registration, and authorize endpoints?"
3. **RFC 7591 — Dynamic Client Registration.** "Let me register myself and get a
   `client_id`, no human in the loop." This is exactly what "automatic
   registration" means — implement it and the error message disappears.
4. **RFC 7636 — PKCE authorization-code flow.** The user consents in a browser;
   the client proves possession of a secret it never transmits, and gets a bearer
   token.

Get those four right, gate your tools behind the resulting JWT, and Spark
connects with no manual client ID, no secret, no friction.

## The repo

[**spark-agent-tools**](https://github.com/ghchinoy/spark-agent-tools) implements
all of this in well-commented Go, in one binary with no database:

- a reusable `pkg/mcpauth` package with the full RFC 9728 / 8414 / 7591 / 7636 chain,
- a `hello-world` tool that wires it in with a handful of lines,
- a single `echo` tool (swap in your own),
- a stateless HMAC-JWT bearer layer,
- a `Dockerfile` and a one-command Cloud Run deploy,
- tests that walk the entire register → consent → PKCE → token → call flow.

The auth package holds the scaffolding; a tool subdirectory holds just tool logic.
Adding a second tool means a new directory that imports `pkg/mcpauth`.

```bash
git clone https://github.com/ghchinoy/spark-agent-tools
cd spark-agent-tools/hello-world
cp .env.example .env      # set GCP_PROJECT
make deploy               # prints your Service URL
```

Then paste the Service URL into Gemini Spark → Connected Apps, approve the consent
screen, and ask Spark to use your echo tool.

## Watching the handshake

Tail the logs during a connection and you'll see the theory become a real trace:

```
HEAD /                                          401   ← probe; gets the resource_metadata pointer
GET  /.well-known/oauth-protected-resource      200   ← RFC 9728
GET  /.well-known/oauth-authorization-server    200   ← RFC 8414 → finds registration_endpoint
POST /api/oauth/register                        201   ← RFC 7591 → client_id, no secret
     … you approve in the browser …
POST /api/oauth/token                           200   ← PKCE exchange → JWT
POST /  [mcp] initialize                        200   ← MCP session starts
POST /  [mcp] tools/list                        200   ← Spark discovers your tools
POST /  [mcp] tools/call echo                   200   ← the tool runs
```

No 404s, no manual credentials. Spark connects to the bare server URL and drives
the whole MCP protocol over `POST /`.

## The echo tool, used creatively

Once connected, I asked Spark: "What's the weather in Fort Collins now? Use the
echo tool." The echo tool only returns whatever you send it, so this shouldn't
answer a weather question. Spark's Gemini model handled it anyway. The server log:

```
[mcp] tools/call echo
[tool] echo: "The current weather in Fort Collins, CO on July 2, 2026 is sunny
with some patchy smoke due to an active Air Quality Alert. The temperature is
around 74°F and is expected to reach a high of 93°F to 95°F later today."
```

Gemini read the tool description, understood echo is a pass-through, pulled the
weather from its own knowledge, and passed it as the `message` argument. The tool
echoed it back and Spark presented the result. It answered the question and used
the tool, exactly as asked.

The lesson: the tool `Description` and the server `Instructions` are read by the
model and shape how it calls your tools. Write them for the model, not just as
documentation.

## From hello-world to production

The tutorial server keeps two things simple: it stores clients and auth codes
**in memory**, and its consent screen approves **without authenticating a real
user**. Production servers persist that state and authenticate the human before
issuing a token. The repo's
[deep-dive doc](https://github.com/ghchinoy/spark-agent-tools/blob/main/docs/oauth-deep-dive.md)
walks through both, and points to a full production reference that adds
persistent storage, a real consent SPA, per-user scopes, and even a second agent
protocol (A2A) — all behind the same auth layer.

## A few things that matter more than you'd expect

**Server name.** The MCP `Implementation.Name` field is sent in every
`initialize` handshake. Keep it short — one lowercase word, no spaces. Use
`Title` for the human-readable display name. Clients use `Name` as a stable
identifier in logs; `Title` is what users see.

**Tool descriptions have two audiences.** The model reads `Description` to decide
how to call your tool (as the weather example showed), and Spark shows the tool
name and arguments to the user before every approval click. Write it to serve
both: clear enough for the model to reason about, legible enough that a human
approving the call knows what they're allowing.

**Set `--timeout 3600` on Cloud Run.** The default is 300 seconds. Spark's
Streamable HTTP transport holds a `GET /` SSE connection open for the duration
of a session — Cloud Run kills it at exactly 5 minutes without this flag,
forcing a reconnect every time. The timeout only applies to the persistent
streaming GET; short tool calls complete in milliseconds and are unaffected.

**Set a `KeepAlive` on the MCP server.** Pass
`mcp.ServerOptions{KeepAlive: 30 * time.Second}` when you build the server. The
SDK pings the open GET stream every 30 seconds so neither Cloud Run nor Spark
treats it as idle. Without it, an idle stream can close and reopen, and the SDK
rejects the reopened stream with a `409 Conflict` that leaves Spark stuck on a
"Thinking it through…" spinner. One line, no downside.

**Icons, prompts, and `Instructions` are MCP-general.** The MCP spec supports
`Icons`, `Title`, and `WebsiteURL` on servers and tools, a server-level
`Instructions` string, and discoverable `prompts`. Set them. Spark's current UI
doesn't surface the icon or title yet, and it doesn't call `prompts/list` — but
`Instructions` already reaches its model, and other MCP clients (Claude Desktop,
opencode) do read the rest. They cost nothing and are ready when Spark adds them.

## Try it, then extend it

Start with echo. Once the handshake works, the tool surface is yours: query an
API, hit a database, kick off a workflow. Spark handles the orchestration; your
MCP server just exposes capabilities. Write the auth scaffolding once; adding
new tools is a handler function.

*Code, tutorial, and full request traces:
[github.com/ghchinoy/spark-agent-tools](https://github.com/ghchinoy/spark-agent-tools)*
