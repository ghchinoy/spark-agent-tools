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

You'd expect the work to be in the *tool* — the code that does something useful.
It isn't. A working tool is about 15 lines. The real work is making your server
**speak OAuth 2.1 the way Spark expects**, because Spark won't call a single tool
until your server proves it's a compliant, securable resource.

Point Spark at a plain MCP URL and you'll likely see:

> *"This server does not support automatic registration. To connect, enter your
> own OAuth client ID and secret below."*

…with your logs filling up with 404s on paths you've never heard of, like
`/.well-known/oauth-protected-resource`.

## What Spark is actually asking for

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
all of this in ~400 lines of well-commented Go, in one binary with no database:

- a single `echo` tool (swap in your own),
- the full RFC 9728 / 8414 / 7591 / 7636 chain,
- a stateless HMAC-JWT bearer layer,
- a `Dockerfile` and a one-command Cloud Run deploy,
- tests that walk the entire register → consent → PKCE → token → call flow.

```bash
git clone https://github.com/ghchinoy/spark-agent-tools
cd spark-agent-tools
cp .env.example .env      # set GCP_PROJECT
make deploy               # prints your Service URL
```

Then paste the Service URL into Gemini Spark → Connected Apps, approve the consent
screen, and ask Spark to use your echo tool.

## Watching the handshake

The satisfying part is tailing the logs during a connection and seeing the theory
become a trace:

```
HEAD /                                          401   ← probe; gets the resource_metadata pointer
GET  /.well-known/oauth-protected-resource      200   ← RFC 9728
GET  /.well-known/oauth-authorization-server    200   ← RFC 8414 → finds registration_endpoint
POST /api/oauth/register                        201   ← RFC 7591 → client_id, no secret
     … you approve in the browser …
POST /api/oauth/token                           200   ← PKCE exchange → JWT
POST /mcp  (Bearer eyJ…)                        200   ← tool calls
```

No 404s. No manual credentials. That's the whole goal.

## From hello-world to production

The tutorial server keeps two things deliberately simple, both clearly flagged:
it stores clients and auth codes **in memory**, and its consent screen approves
**without authenticating a real user**. Production servers persist that state and
authenticate the human before issuing a token. The repo's
[deep-dive doc](https://github.com/ghchinoy/spark-agent-tools/blob/main/docs/oauth-deep-dive.md)
walks through both, and points to a full production reference that adds
persistent storage, a real consent SPA, per-user scopes, and even a second agent
protocol (A2A) — all behind the same auth layer.

## Try it, then extend it

Start with echo. Once the handshake works, the tool surface is yours: query an
API, hit a database, kick off a workflow. Spark handles the orchestration; your
MCP server just exposes capabilities. The scaffolding you built once is the part
that keeps paying off.

*Code, tutorial, and full request traces:
[github.com/ghchinoy/spark-agent-tools](https://github.com/ghchinoy/spark-agent-tools)*
