# MemSpace

Self-hosted shared memory for AI coding agents. Claude Code and Codex
sessions on all your machines flow into one server; a new session, on any
device and with either agent, picks up where the last one left off.

- **Cross-device, cross-agent:** sessions from every connected machine and
  agent are stored per project (identified by Git remote), with branch and
  HEAD per device.
- **Automatic context:** at session start an agent gets a card for recent
  unfinished work; on each prompt, related history. On demand through MCP.
- **Multilingual search:** optional semantic search with bge-m3 understands
  100+ languages (e.g. English, Chinese, Japanese, Korean, Spanish, French,
  German, Portuguese, Russian, Arabic) and matches across them; keyword
  search works without it, with jieba segmentation and pinyin for Chinese.
- **No LLM required:** session summaries and memories are rule-based.
- **Reliable capture:** hooks never block the agent; events are spooled
  locally, survive being offline and are deduplicated by the server.
- **Web UI:** overview, projects, sessions with full timelines, search,
  memories, devices and administration (invites, revocation).

```text
agent hook ──► memspace hook <agent> ──► normalize ─► redact ─► local spool ──► background sync ──► server ACK ──► delete
                (exits in ms, never         (secrets,          (fsync'd file    (batched, exp.        (one txn,
                 blocks or prints)           size bounds)       per event)       backoff, lock)        dedup by id)
```

## Quick start

### 1. Run the server

The server is a single binary with SQLite. Building it needs Go and a C/C++
toolchain (cgo: the full-text tokenizer and sqlite-vec are compiled in).

```bash
make build                       # → bin/memspace
install -m 755 bin/memspace ~/.local/bin/memspace
memspace server start            # reads ~/.memspace/config.toml
```

Minimal server config (`~/.memspace/config.toml`):

```toml
[server]
address = "0.0.0.0:8765"              # default 127.0.0.1:8765
public_url = "http://192.0.2.10:8765" # how devices and browsers reach it
downloads_dir = "/srv/memspace/clients"   # optional, see "Client binaries"

[database]
path = "/srv/memspace/memspace.db"
```

To keep it running, a systemd user unit is enough:

```ini
# ~/.config/systemd/user/memspace.service
[Unit]
Description=MemSpace server

[Service]
ExecStart=%h/.local/bin/memspace server start
Restart=on-failure

[Install]
WantedBy=default.target
```

```bash
systemctl --user enable --now memspace
loginctl enable-linger "$USER"   # keep user services running after logout
```

### 2. Connect devices

Create a one-time invite on the server (or in the web UI under
Administration, which also shows copy-paste setup commands per platform):

```bash
memspace server invite            # prints msi_…, valid 24 h (--ttl)
```

On each device, one command connects it, installs hooks and MCP for the
detected agents (Claude Code, Codex) and runs the checks:

```bash
memspace onboard http://SERVER:8765 --invite msi_…
```

Agents reach MemSpace's MCP tools through a local stdio bridge,
`memspace mcp stdio`, which reads this device's credentials itself: agent
configs hold only that command, and nothing needs to be exported in your
shell.

Step by step instead:

```bash
memspace client connect http://SERVER:8765 --invite msi_…   # saves ~/.memspace/credentials.json
memspace agent install claude          # hooks in ~/.claude/settings.json + MCP (stdio bridge)
memspace agent install codex [--trust] # hooks in ~/.codex/hooks.json + rollout importer + MCP
memspace agent doctor                  # end-to-end checks, incl. running the installed hook
memspace client status                 # online/offline, pending/rejected/dropped counts
```

Codex only runs hooks the user has reviewed: approve them in Codex's
`/hooks`, or pass `--trust`. Without trusted hooks, sessions are still
imported whenever `memspace client sync` runs. Older Codex history is
imported only on request: `memspace import codex --since YYYY-MM-DD` (or
`--all`).

### 3. Open the web UI

```bash
memspace ui                    # on any connected device: read-only browser session
memspace server login-link     # on the server machine: admin session
                               # (--viewer for read-only; --url behind a reverse proxy)
```

## What agents get

| When | Channel | Content |
|---|---|---|
| Session start (not `resume`) | SessionStart `additionalContext` | Cards for recent (72 h) **unfinished** sessions in this project from any agent or device: objective, last report, changed files, tests, unresolved failures, next steps, other-device warning. Otherwise a one-line hint. Server unreachable: last cached card (≤ 24 h). |
| Each substantive prompt | UserPromptSubmit `additionalContext` | Related history in this project (one item per session, never the current session, never repeated); the previous session's card on "继续 / continue". Skipped while offline; 1.5 s deadline. |
| On demand | MCP tools | `memspace_project_context`, `memspace_search`, `memspace_session_resume`, `memspace_get_memory`, `memspace_session_history` |

Injected context is bounded and wrapped in `<memspace_context>`, marked as
historical, possibly stale and not instructions. Disable injection with
`[client] inject = "off"` or `MEMSPACE_INJECT=off`.

| Agent | Capture | Events |
|---|---|---|
| Claude Code | command hooks: SessionStart, UserPromptSubmit, PostToolUse, PostToolUseFailure, Stop, PreCompact, SessionEnd | session.started, user.prompt, tool.completed, tool.failed, file.modified, agent.turn_completed, session.compacting, session.ended |
| Codex | incremental import of `$CODEX_HOME/sessions/**/rollout-*.jsonl` (source of truth); hooks only trigger imports and record session end | session.started, user.prompt, agent.message, tool.completed, tool.failed, file.modified, agent.turn_completed, agent.turn_aborted, session.compacted, session.ended |

## Search

`memspace search [--all] <query>`, the web UI, `POST /api/v1/search` and the
MCP `memspace_search` tool share one engine.

- **Keyword:** SQLite FTS5 with the [simple](https://github.com/wangfenjin/simple)
  tokenizer and cppjieba, statically linked. English and other ASCII text is
  indexed by word; Chinese by character with pinyin, queried with jieba
  segmentation and a technical-term dictionary (幂等, 去重, 熔断, …); other
  scripts (Japanese, Korean, Cyrillic, accented Latin, …) by character, so
  they match as phrases. Semantic search adds meaning-level matching for all
  of them.
  Every-term matches rank first, then partial matches by how many terms
  they cover; Chinese and English function words are dropped.
- **What is indexed:** derived documents, not raw JSON: the user's own prompt
  text (pasted blocks excluded, their error lines kept), final replies,
  failed commands with the error head, shell commands, changed files and
  session summaries.
- **Summaries and memories (no LLM):** rule-based, built by a durable job
  queue: objective, latest request, the agent's last report, changed files,
  test results (a pass needs success evidence), unresolved failures, next
  steps, with evidence event IDs. Each summarized session yields an episodic
  memory, and each fix (failure → file changes → the same operation passes)
  a procedural one; memories keep their sources.

### Semantic search (optional)

With an embeddings service, search also finds history by meaning ("登录很慢"
finds "认证模块超时", and so does "slow login"), in any of the 100+
languages bge-m3 supports and across languages. Any
OpenAI-compatible `/v1/embeddings` endpoint works; we use
[embeddings.cpp](https://github.com/chux0519/embeddings.cpp)'s native server
with bge-m3:

```bash
# q4_k MLP/vocabulary + q8_0 attention, 384 MB:
# https://huggingface.co/chux0519/bge-m3-gguf-embeddings-cpp
embeddings_server --model bge-m3.q4_k_mlp_emb_q8_attn.gguf --max-tokens 512 --port 8090
```

```toml
[embedding]
url = "http://127.0.0.1:8090/v1/embeddings"   # empty = disabled
model = "bge-m3"
# api_key_env = "EMBEDDINGS_API_KEY"   # bearer key, read from this variable
# dimensions = 0                        # Matryoshka truncation (0 = native)
# query_prefix = ""                     # "" = automatic ("query: " for arctic-embed)
# batch_size = 8
# query_timeout = "1s"
# min_similarity = 0                    # meaning-only matches injected into agents (0 = model default)
# search_min_similarity = 0             # floor for semantic hits in search (0 = model default)
```

Choosing hardware (bge-m3, 1200-character documents, batch 8):

| Host | Build | Quantization | Indexing | Query |
|---|---|---|---|---|
| Apple M4 | Metal (default on macOS) | q4_k MLP mix or q8_0 | 11–12 docs/s | ~20 ms |
| Apple M4 | CPU only | q8_0 | 6.7 docs/s | ~11 ms |
| Xeon E5-2673 v3 (AVX2), 12 threads | CPU | q4_k MLP mix | 2.0 docs/s | ~26 ms |
| same | CPU | q8_0 | 1.4 docs/s | ~45 ms |

Daily use needs a few documents per minute, so any of these is enough;
the speed matters when a large history is first indexed (search stays
keyword-only until the vector index is complete). On a mixed
Chinese/English agent-history benchmark (2040 documents) bge-m3 ranks the
right document first for 66% of queries vs 39% for Snowflake
arctic-embed-m-v2.0.
Scripts to reproduce: `scripts/bench/`.

- **Where it runs:** the MemSpace server calls the service, so it can run on
  the server or on any host the server reaches (e.g. a Mac on the LAN).
- **Indexing:** documents are chunked (~380 estimated tokens), embedded in
  the background and stored with [sqlite-vec](https://github.com/asg017/sqlite-vec)
  (statically linked).
- **Ranking:** keyword and vector results are fused with Reciprocal Rank
  Fusion; results carry `match` (`all` / `any` / `semantic`) and
  `similarity`. `memspace search --mode keyword|semantic` forces one side.
- **Thresholds are per model** (cosine scales differ): bge-m3 inject 0.60 /
  search floor 0.45, arctic-embed 0.35 / 0.20; unknown models get 0.60 / 0.30
  and should be calibrated. The effective values are in the status.
- **Changing the model or service:** each service fingerprint (model,
  quantization incl. per-tensor weight types, dimensions, truncation, query
  prefix) gets its own vector space, built in the background; vectors from
  different models are never compared.
- **Failure:** an unavailable service never blocks ingest or search: indexing
  retries with backoff, queries fall back to keywords.
- **Status:** `GET /api/v1/embedding/status`, shown by `memspace agent doctor`.

## Web UI and administration

The server serves the UI at `/`: overview, projects (location, branch and
HEAD per device), sessions with summary and full event timeline, search
(filters, hybrid/keyword/semantic, highlighted matches), memories with their
sources, and devices with sync state (what each device has not sent yet).

Admin sessions (`memspace server login-link`) can create invites with
per-platform setup commands, withdraw unused invites, revoke devices, list
and sign out browser sessions and make login links for other browsers.
Sessions opened with a device token (`memspace ui`) are read-only, so a
leaked device token cannot invite or revoke devices.

The same from the command line, on the server machine:

```bash
memspace server invite [--ttl 24h]
memspace server device list
memspace server device revoke <device-id>
memspace server login-link [--viewer] [--url URL]
```

**Client binaries:** set `server.downloads_dir` to a directory of client
builds named `memspace-<os>-<arch>[.exe]`; they are served at `/downloads/`
and used by the setup commands in the UI. The client is pure Go:

```bash
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o memspace-darwin-arm64 ./cmd/memspace
# likewise linux/amd64, linux/arm64, darwin/amd64, windows/amd64 (.exe)
```

## Security and privacy

- **Tokens:** device tokens are 32 random bytes, shown once and stored as
  SHA-256 hashes. Registration needs a single-use invite. MCP uses a
  separate read-only token per device (`msm_…`), revoked with the device.
  Agents start the stdio bridge (`memspace mcp stdio`), which reads it from
  `~/.memspace/credentials.json` (mode 0600), so no secret is written into
  agent configs or the shell environment.
- **Redaction:** secrets are redacted on the client and again on the server.
  Content of `.env`, keys and credential files is not captured. Strings are
  capped at 8 KB and payloads at 64 KB.
- **Web UI sign-in:** single-use login links valid for 5 minutes; the code is
  in the URL fragment (not sent to servers, so not in access logs) and is
  exchanged for an HttpOnly, SameSite=Strict cookie (14 days). Cookie
  requests must carry `X-MemSpace-UI`. Strict CSP (same origin, no inline
  scripts), `X-Frame-Options: DENY`, `nosniff`, `no-referrer`; captured
  content is rendered as text, never HTML.
- **Proxies:** the client does not send requests for servers on private
  networks (LAN, Tailscale, loopback, `.local`) through `HTTP(S)_PROXY`;
  desktop proxies usually cannot reach them and agent hooks inherit the
  proxy environment.

## Reliability

- **At-least-once and idempotent:** events carry stable IDs (deterministic
  where the agent provides one). The server deduplicates by `event_id` and
  acknowledges only after the batch transaction commits (`synchronous=FULL`).
- **Spool:** `~/.memspace/spool/{pending,inflight,dead}`, one fsync'd file per
  event, claimed by atomic rename. Transient failures never drop events;
  only events the server rejects as invalid go to `dead/`
  (`memspace client dead [--requeue]`). Capacity (default 50k events /
  256 MB) is enforced; overflow is counted as *dropped* in `client status`.
- **Replay:** each hook starts a detached `client sync` unless one is running
  or the client is backing off (5 s·2ⁿ, max 10 min). `memspace client sync
  --watch` is an optional long-running loop.
- **Project identity:** normalized Git remote (`github.com/org/repo`), else
  the directory name; projects are never merged automatically.

## Configuration

One file serves both roles: `$MEMSPACE_CONFIG`, else `~/.memspace/config.toml`
(`--config` to override). Environment variables override the file:
`MEMSPACE_ADDRESS`, `MEMSPACE_DB_PATH`, `MEMSPACE_OWNER_USER_ID`,
`MEMSPACE_HOME`, `MEMSPACE_SERVER_URL`, `MEMSPACE_EMBEDDING_URL`,
`MEMSPACE_EMBEDDING_MODEL`, `MEMSPACE_INJECT`.

```toml
[server]
address = "127.0.0.1:8765"
public_url = ""            # default http://<address>
downloads_dir = ""

[database]
path = "./data/memspace.db"

[auth]
owner_user_id = "owner"

[client]                   # all optional
home = "~/.memspace"       # or $MEMSPACE_HOME / --home
server_url = ""            # overrides the URL saved by `client connect`
inject = "on"              # "off" disables context injection
spool_max_events = 50000
spool_max_bytes = 268435456
batch_size = 200
backoff_base = "5s"
backoff_max = "10m"

[embedding]                # see "Semantic search"
```

## HTTP API

Protected routes take `Authorization: Bearer <device token>`; read routes
also accept a web UI session. Writes need a device token.

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/health` | health check (no auth) |
| `POST /api/v1/devices` | register a device with a one-time invite; returns the token once |
| `GET /api/v1/devices`, `POST /api/v1/devices/mcp-token` | devices; issue or rotate the MCP token |
| `POST /api/v1/projects`, `GET /api/v1/projects`, `GET /api/v1/projects/{id}` | ensure (by Git identity, then name), list, details with per-device locations |
| `POST /api/v1/sessions`, `GET /api/v1/sessions[?before=&before_id=]`, `/sessions/{id}`, `/sessions/{id}/events` | ensure, list (paged), summary, timeline |
| `POST /api/v1/events/batch`, `GET /api/v1/events/{id}` | ingest (transactional, deduplicated, receipts per event), one event |
| `POST /api/v1/search`, `POST /api/v1/context` | search; context pack (`session_start` / `prompt` / `explicit`) |
| `GET /api/v1/memories`, `/memories/{id}` | memories with sources (`before` / `before_id` paging) |
| `GET /api/v1/overview`, `GET /api/v1/embedding/status` | dashboard data; semantic index state |
| `GET /api/v1/sync/status`, `POST /api/v1/sync/report` | the server's view of a device; the client's spool report |
| `POST /api/v1/ui/login-links`, `/ui/login`, `/ui/logout`, `GET /ui/session` | browser sign-in |
| `/api/v1/admin/…` | invites, device revocation, login links, browser sessions, downloads (admin sessions only) |
| `GET /downloads/{name}` | client binaries from `server.downloads_dir` (no auth) |
| `/mcp` | MCP, streamable HTTP, read-only tools (agents use it through `memspace mcp stdio`) |

## Development

```bash
make build          # CGO_ENABLED=1 go build -tags sqlite_fts5 → bin/memspace
make test           # all tests, incl. end-to-end tests with the real binary
go test -short ./...    # skip the binary end-to-end tests
make build-client   # pure-Go client only (hooks, sync, setup); cannot run the server
make ui             # rebuild the web UI (npm ci && npm run build → web/dist)
cd web && MEMSPACE_DEV_SERVER=http://127.0.0.1:8765 npm run dev   # live reload, API proxied
```

The web UI (React, TypeScript, Vite) lives in `web/`; its build output
`web/dist` is committed and embedded, so `go build` needs no Node.
Vendored C/C++ sources and their licenses are listed in `NOTICE` and in
`internal/fts/simple/README.md`, `internal/vec/README.md`.

## License

Apache License 2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE).
