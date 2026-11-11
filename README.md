# MemSpace

Self-hosted shared memory for AI coding agents. Claude Code, Codex,
DeepSeek Harness (DSH), Oh My Pi (omp) and pi sessions on all your machines
flow into one server;
a new session, on any device and with any of these agents, picks up where
the last one left off.

- Sessions from every connected machine and agent are stored per project
  (identified by Git remote), with branch and HEAD per device.
- At session start an agent gets a card for recent unfinished work, and on
  each prompt it gets related history. It can also recall history through MCP.
- Optional semantic search with bge-m3 understands
  100+ languages (e.g. English, Chinese, Japanese, Korean, Spanish, French,
  German, Portuguese, Russian, Arabic) and matches across them; keyword
  search works without it, with jieba segmentation and pinyin for Chinese.
- Session summaries and memories are rule-based and need no LLM.
- Hooks never block the agent; events are spooled
  locally, survive being offline and are deduplicated by the server.
- The web UI has an overview, projects, sessions with full timelines, search,
  memories, devices and administration (invites, revocation).

```text
agent hook ──► memspace hook <agent> ──► normalize ─► redact ─► local spool ──► background sync ──► server ACK ──► delete
                (exits in ms, never         (secrets,          (fsync'd file    (batched, exp.        (one txn,
                 blocks or prints)           size bounds)       per event)       backoff, lock)        dedup by id)
```

## Quick start

### 1. Run the server

The server is a single binary with SQLite. Install a release build (Linux
amd64/arm64, macOS arm64), verified against the release's checksums:

```bash
curl -fsSL https://raw.githubusercontent.com/chux0519/memspace/main/install.sh | sh -s -- --server
memspace server start            # reads ~/.memspace/config.toml
```

Or build it: needs Go and a C/C++ toolchain (cgo: the full-text tokenizer
and sqlite-vec are compiled in).

```bash
make build                       # → bin/memspace
install -m 755 bin/memspace ~/.local/bin/memspace
```

Minimal server config (`~/.memspace/config.toml`):

```toml
[server]
address = "0.0.0.0:8765"              # default 127.0.0.1:8765
public_url = "http://192.0.2.10:8765" # how devices and browsers reach it
# downloads_dir = "/srv/memspace/clients"  # optional mirror, see "Client binaries"

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

Install the client on each device (pure Go, from the GitHub release):

```bash
curl -fsSL https://raw.githubusercontent.com/chux0519/memspace/main/install.sh | sh    # Linux, macOS
irm https://raw.githubusercontent.com/chux0519/memspace/main/install.ps1 | iex           # Windows (PowerShell)
```

Then one command connects it, installs hooks and MCP for the
detected agents (Claude Code, Codex, DSH, omp, pi) and runs the checks:

```bash
memspace onboard http://SERVER:8765
```

The device asks to join and shows a code (e.g. `K7QF-2M9P`). Approve it in
the web UI under Administration (管理 → 待审批设备), after checking that the
code matches, or on the server with `memspace server approve K7QF-2M9P`;
the device then finishes on its own. Requests expire after 10 minutes. To
skip approval, create a one-time invite (`memspace server invite`, or Add
device in the web UI) and pass `--invite msi_…`.

Agents reach MemSpace's MCP tools through a local stdio bridge,
`memspace mcp stdio`, which reads this device's credentials itself: agent
configs hold only that command, and nothing needs to be exported in your
shell.

For setup through Claude Code, Codex, omp or pi, send the agent this on the new device:

```text
Set up MemSpace on this machine: follow
https://raw.githubusercontent.com/chux0519/memspace/main/docs/agent-setup.md
```

The agent asks where your server is, installs the client, and tells you
the code to approve; it asks before changing its own configuration. See
[docs/agent-setup.md](docs/agent-setup.md).

To connect and install hooks separately:

```bash
memspace client connect http://SERVER:8765   # asks for approval (or --invite msi_…); saves ~/.memspace/credentials.json
memspace agent install claude          # hooks in ~/.claude/settings.json + MCP (stdio bridge)
memspace agent install codex [--trust] # hooks in ~/.codex/hooks.json + rollout importer + MCP
memspace agent install dsh             # rows in $DSH_HOME/cordis.patch.yml + session-log importer + MCP
memspace agent install omp             # extension in ~/.omp/agent/hooks/post + MCP in ~/.omp/agent/mcp.json
memspace agent install pi              # extension in ~/.pi/agent/extensions + MCP in ~/.pi/agent/mcp.json
memspace agent doctor                  # end-to-end checks, incl. running the installed hook
memspace client status                 # online/offline, pending/rejected/dropped counts
```

Codex only runs hooks the user has reviewed: approve them in Codex's
`/hooks`, or pass `--trust`. Without trusted hooks, sessions are still
imported whenever `memspace client sync` runs. Older Codex history is
imported only on request: `memspace import codex --since YYYY-MM-DD` (or
`--all`).

DSH: `agent install dsh` adds two rows, in a marked block, to the
home-level `$DSH_HOME/cordis.patch.yml` (`$DSH_HOME` defaults to `~/.dsh`,
or what your `dsh` launcher script exports): DSH's Claude Code hook bridge,
running MemSpace's prompt hook, and an MCP client running the stdio bridge.
DSH runs hooks inside its workspace sandbox, so the hook only adds context;
the MCP bridge, which runs outside it, imports and uploads new session logs
every minute while DSH runs and once when it exits. Restart DSH after
installing (the web profile reloads on its own). Older DSH history:
`memspace import dsh --all`.

omp: `agent install omp` generates
`~/.omp/agent/hooks/post/memspace.ts`, an extension that forwards omp's
session, prompt, tool, compaction and shutdown events to `memspace hook omp`,
and registers the stdio MCP bridge in `~/.omp/agent/mcp.json`. omp loads both
at startup, so restart running sessions. The extension lives in the agent
directory, which a named omp profile relocates (`PI_CODING_AGENT_DIR`).

pi: `agent install pi` generates `~/.pi/agent/extensions/memspace.ts`, a pi
extension that captures the session's start, end and compaction, the user's
prompts, tool results (with the files a writing tool actually changed) and the
agent's answers, and adds the MCP server `memspace` to `~/.pi/agent/mcp.json`
next to the servers already there. It records those observations through
`memspace hook pi`; it also subscribes to a few more pi events that only keep
its own bookkeeping straight (which message a run is on, which turn a tool call
belongs to) and are not sent anywhere. The hook runs asynchronously (spawned,
with a deadline, failures swallowed) so capture never blocks pi's UI,
cancellation or exit, and MemSpace's context arrives as an extra message,
leaving pi's prompt, its MCP server instructions and other extensions
untouched.

Each submission is recorded once: the prompt pi announces for a run, keeping
the same `prompt_id` when a template or skill expands it. A prompt typed while
pi is already working is recorded from the user message the model actually
receives, so that turn's tools and answer belong to it; it carries the expanded
text, and gets no injected context, which pi offers no channel for. A turn
cancelled before it wrote any reply reports an empty answer instead of
borrowing the previous turn's text; text produced before the cancellation is
kept.

pi loads both at startup: restart running sessions, or `/reload`. The agent
directory follows `PI_CODING_AGENT_DIR`. `pi mcp list` shows the registered
server.

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
| Each substantive prompt | UserPromptSubmit `additionalContext` | Related history in this project (up to six source fragments, never the current session, never repeated); the previous session's card on "继续 / continue". Skipped while offline; 1.5 s deadline. |
| On demand | MCP tools | `memspace_project_context`, `memspace_search`, `memspace_session_resume`, `memspace_get_memory`, `memspace_session_history` |

Injected context is bounded and wrapped in `<memspace_context>`, marked as
historical, possibly stale and not instructions. Disable injection with
`[client] inject = "off"` or `MEMSPACE_INJECT=off`.

| Agent | Capture | Events |
|---|---|---|
| Claude Code | command hooks: SessionStart, UserPromptSubmit, PostToolUse, PostToolUseFailure, Stop, PreCompact, SessionEnd | session.started, user.prompt, tool.completed, tool.failed, file.modified, agent.turn_completed, session.compacting, session.ended |
| Codex | incremental import of `$CODEX_HOME/sessions/**/rollout-*.jsonl` (source of truth); hooks only trigger imports and record session end | session.started, user.prompt, agent.message, tool.completed, tool.failed, file.modified, agent.turn_completed, agent.turn_aborted, session.compacted, session.ended |
| DSH | incremental import of `$DSH_HOME/sessions/**/session.v4.jsonl.zstd`, run by the MCP bridge; subagent work is folded into the parent session. The session-start card comes with the first prompt (DSH's SessionStart hooks run too late) | session.started, user.prompt, tool.completed, tool.failed, file.modified, agent.turn_completed, agent.turn_aborted |
| Oh My Pi (omp) | generated extension (`hooks/post/memspace.ts`) forwarding session_start, before_agent_start, tool_result, session_stop, session.compacting, session_shutdown | session.started, user.prompt, tool.completed, tool.failed, file.modified, agent.turn_completed, session.compacting, session.ended |
| pi | generated extension (`extensions/memspace.ts`) that captures prompts, tool results, file changes, answers and session boundaries; a prompt queued while pi works is counted from the message the model receives | session.started, user.prompt, tool.completed, tool.failed, file.modified, agent.turn_completed, session.compacting, session.ended |

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
# The server binary (linux-amd64, linux-arm64, darwin-arm64 or windows-amd64.exe;
# see the embeddings.cpp README for CPU requirements and checksums):
curl -fLo embeddings_server \
  https://github.com/chux0519/embeddings.cpp/releases/latest/download/embeddings_server-linux-amd64
chmod +x embeddings_server
# The model, q4_k MLP/vocabulary + q8_0 attention, 384 MB:
curl -fLO https://huggingface.co/chux0519/bge-m3-gguf-embeddings-cpp/resolve/main/bge-m3.q4_k_mlp_emb_q8_attn.gguf
./embeddings_server --model bge-m3.q4_k_mlp_emb_q8_attn.gguf --max-tokens 512 --port 8090
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

[context]
admission_mode = "hybrid"              # hybrid | semantic | none
# semantic_min_similarity = 0.60       # omitted inherits embedding.min_similarity/model default
keyword_min_coverage = 0.50            # partial keyword matches: fraction in [0,1]
allow_exact_keywords = true           # admit full keyword matches without semantic evidence
```

The context values above preserve the existing policy; they are compatibility
defaults, not universal relevance recommendations. `semantic` uses only semantic
qualification; `none` disables qualification while preserving scope, source
validation and output limits. An explicit context semantic threshold of `0`
means zero, unlike the legacy embedding setting's automatic `0`. Context
qualification overrides do not change search ranking or the existing strong-hit
text expansion threshold. See [admission comparisons](scripts/bench/ADMISSION.md)
for definitions and a reproducible way to choose values from evidence.

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

### How RRF ranks results

Keyword search and vector search each produce a ranked list. Reciprocal Rank
Fusion (RRF) adds a vote for a document's position in each list:

```text
score(document) = sum(1 / (60 + rank))
```

Ranks start at 1. Both lists have equal weight; a document present in only one
gets one vote. For example, keyword rank 1 plus vector rank 5 scores about
0.03178; keyword rank 2 alone scores about 0.01613. Raw BM25 scores and cosine
similarities have different scales, so RRF combines their ranks. Ties keep
the first-seen order, with keyword results added first. The implementation
merges by document ID and retains semantic similarity for later admission
checks. See [`fuse`](internal/search/query.go).

The constant `60`, named `rrfK` in the code, dates to the initial
implementation. It has no config setting. We have not established which
value works best for this workload. RRF uses no learned weights or extra
inference model.

RRF orders candidates. Separate rules decide what enters the context: the
six-fragment prompt path requests up to 48 search results, keeps up to 24
after source deduplication, then applies admission and packing. With BGE-M3's
default hybrid policy, a hit can qualify through cosine similarity >= 0.60,
an exact keyword match, or partial keyword coverage >= 0.50. These admission
settings are configurable under `[context]`; they are compatibility defaults,
not thresholds proven best across datasets. The final cap remains six
fragments and 6000 characters. [Admission comparisons](scripts/bench/ADMISSION.md)
describe the controls and calibration procedure.

## Experimental evidence relationships

The optional `[evidence]` setting defaults to `mode = "off"`. `shadow` records
source relationships without changing injected context; `on` additionally packs
validated related sources together within the existing fragment/character
budgets. Native question/answer references and generator provenance are captured;
scoped corrections and exact document citations require explicit retained source
references. This remains experimental; the fixed benchmark results below do
not establish a general accuracy gain. See
[evidence relationships](internal/evidence/README.md) for the configuration,
source/version checks, limitations and inspection API.

## Benchmarks

We test retrieval and context packing separately on the author-published
LongMemEval-S cleaned dataset. Each public-data run covers all 500 questions.
Source metrics score the 470 answerable questions; QA includes the 30
unanswerable questions too. Results are from 2026-10-30 and 2026-11-01. Dataset
hashes, versions and per-question scores are in the
[benchmark record](scripts/bench/results/2026-11-01/README.md).

The MemSpace configurations below use no reranker or LLM memory extraction.
The bounded context pack uses keyword search plus BGE-M3 and RRF. The
whole-session BM25 component is keyword-only. The QA experiment adds a reader
and judge after retrieval.

### What we measure

Source retrieval asks whether the first five distinct sessions contain every
required source. Delivered-context coverage asks whether those sources survive
into the text prepared for the agent. A short excerpt can cite the right
session while omitting the answer, so we report QA separately: a fixed reader
answers from the context and a fixed judge grades it.

### Delivered context

Both modes cover the same 500 questions; source coverage is scored on 470.
Text length includes empty outputs across all 500 questions.

| Configuration | All required sources in final context | Mean characters | Empty outputs |
|---|---:|---:|---:|
| MemSpace, experimental relation packing enabled | 341/470 (72.55%) | 3428 | 16/500 |
| deja-vu, native per-prompt hook defaults | 85/470 (18.09%) | 880 | 250/500 |

MemSpace retains all required source sessions for roughly three out of four
answerable questions within its bounded pack. In this comparison it delivers
more history up front and uses about 3.9 times as much text as deja-vu.
The policies differ: MemSpace allows six fragments and 6000 characters;
deja-vu's picker allows two sessions and uses a native 1536 budget constant
plus framing rules. It often declines automatic injection and leaves further
recall to the agent. We have not compared the two at an equal budget.

For MemSpace we reuse the frozen evaluation pack. For deja-vu we run its
actual `hook-prompt` command and read
`hookSpecificOutput.additionalContext`. Host-side processing and subsequent
agent tool calls are outside this measurement. The MemSpace result uses
experimental relation packing, which remains off by default.

### Source retrieval components

| Pinned native configuration | All required sources in top 5 | At least one required source in top 5 |
|---|---:|---:|
| MemSpace whole-session BM25 + Porter, exact English words | 403/470 (85.74%) | 97.02% |
| agentmemory BM25 component | 398/470 (84.68%) | 96.17% |
| deja-vu keyword search | 394/470 (83.83%) | 97.45% |
| MemPalace raw vector search | 403/470 (85.74%) | 96.60% |
| agentmemory's grep baseline | 339/470 (72.13%) | 88.72% |

MemSpace's BM25 result is five questions, or 1.06 percentage points, above
agentmemory's. It wins 11 paired cases and loses six; the difference is small
and not statistically significant (exact McNemar p = 0.332). These public
questions were used during development, so the result needs confirmation on
new data. Earlier MemSpace variants and their regressions remain in the
[full retrieval comparison](scripts/bench/LONGMEMEVAL.md).

The MemSpace BM25 and agentmemory baselines index supplied whole sessions,
bypassing capture and context packing. This gives the retrieval component
access to more text than the bounded injection path. MemPalace's raw vector
benchmark retains user turns only; deja-vu retains both roles. The 85.74%
BM25 result and 72.55% delivered-context result therefore describe different
paths, rather than a measured before/after drop in the same pipeline.

The final BM25 rerun measured query p50/p95 of 26.6/71.6 ms, versus roughly
3.1/3.6 ms for agentmemory. MemSpace used SQLite and worker IPC; agentmemory
used an in-memory JavaScript index. These timings describe the component
runners on one machine and exclude live hook and answer latency.

All source scores use agentmemory's pinned loader/scorer. The five published
2026-11-01 modes each passed an independent 500-question score check with
zero mismatches. Source-only replications and the final-hook measurement
made no generative model API calls.

### Controlled question-answering experiment

Using the pinned OmniMemEval harness, both MemSpace arms start from identical
database snapshots and use the same worker, embeddings, source data and
six-fragment/6000-character ceilings. Only relation packing changes.

| Relation packing | Correct answers, all 500 questions | Mean context characters |
|---|---:|---:|
| Off | 269/500 (53.8%) | 2362 |
| On | 305/500 (61.0%) | 3428 |

The reader was `gpt-4.1-mini-2025-04-14`; the judge was
`gpt-4o-mini-2024-07-18`, both at temperature 0. The enabled arm gains 63
answers and loses 27, a gain of 7.2 percentage points. The enabled pack uses
more of the available character budget. Each arm had one answer/judge run;
the labels have not been checked against human judgments. These completed
QA runs used model APIs; the later source-only comparisons reused the packs
without new QA calls. Comparing this 61.0% with a vendor's QA score would
also require matching its reader, judge and input pipeline.

### Other completed experiments

On a separate internal 100-question coding regression, the 79 positive
questions eligible for automatic injection scored 57/79 (72.2%) under the
baseline and 67/79 (84.8%) in an offline BGE reranker replay. The latter was
a dev-selected experiment; it is absent from the product configurations
above. CPU reranker time at 12 candidates was p50 0.86 s and p95 4.62 s,
before retrieval and packing, exceeding the hook's 1.5 s total deadline on
some requests. See the [reranker study](scripts/bench/RERANK.md).

The EverOS comparison was invalidated after we found that our adapter had
reordered source messages. It has no valid comparative score here.
Hindsight and Nautilus Compass informed implementation research; we have
not completed equivalent product benchmarks for them.

Reproduction: [source scorer](scripts/bench/retrieval_peers/README.md),
[OmniMemEval adapter and paired run](scripts/bench/omnimemeval/README.md),
and [versions and score receipts](scripts/bench/results/2026-11-01/README.md).
This English dataset does not measure mixed-language retrieval, production
latency/RSS, cross-project isolation or multi-device synchronization.

## Web UI and administration

The server serves the UI at `/`: overview, projects (location, branch and
HEAD per device), sessions with summary and full event timeline, search
(filters, hybrid/keyword/semantic, highlighted matches), memories with their
sources, and devices with sync state (what each device has not sent yet).

Admin sessions (`memspace server login-link`) can approve devices waiting
to join, create invites with per-platform setup commands, withdraw unused invites, revoke devices, list
and sign out browser sessions and make login links for other browsers.
Sessions opened with a device token (`memspace ui`) are read-only, so a
leaked device token cannot invite or revoke devices.

The same from the command line, on the server machine:

```bash
memspace server approve [code]       # no code: list devices waiting for approval
memspace server deny <code>
memspace server invite [--ttl 24h]
memspace server device list
memspace server device revoke <device-id>
memspace server login-link [--viewer] [--url URL]
```

**Client binaries** are published with each GitHub release and installed
by `install.sh` / `install.ps1`; the web UI's setup commands pin the
server's version. `/downloads/memspace-<os>-<arch>[.exe]` on the server
redirects to the release of the server's version. For devices that cannot
reach GitHub, set `server.downloads_dir` to a directory of client builds
with those names: the server then serves them itself, and the UI offers
them too. The client is pure Go:

```bash
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o memspace-darwin-arm64 ./cmd/memspace
# likewise linux/amd64, linux/arm64, darwin/amd64, windows/amd64 (.exe)
```

**Upgrades.** Upgrade the server first (re-run `install.sh --server`); the
clients follow it:

- After a sync, a client older than the server downloads the release of the
  server's version from GitHub (through `HTTPS_PROXY` when set), checks it
  against the release's `SHA256SUMS`, runs it, and replaces itself (the old
  binary stays as `memspace.prev`). Running agents keep the old binary
  until they restart. Clients never upgrade past the server, and
  development builds are never replaced. `[client] auto_upgrade = "off"`
  turns this off; `memspace upgrade` does it by hand.
- A failed upgrade is retried every 6 hours and shown in `memspace agent
  doctor`, on the web UI's devices page, and once a day in the
  session-start context, which asks the agent to tell you.
- The server asks GitHub once a day for the latest release and the web UI
  shows when a newer one exists (`server.update_check = "off"` to stop).

## Security and privacy

- **Tokens:** device tokens are 32 random bytes, shown once and stored as
  SHA-256 hashes. A device joins only with an administrator's approval
  (the code shown on the device must match the one in the UI; the token is
  created when the device claims it, so it is never stored in plaintext)
  or with a single-use invite. MCP uses a separate read-only token per
  device (`msm_…`), revoked with the device.
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
`MEMSPACE_EMBEDDING_MODEL`, `MEMSPACE_INJECT`, `MEMSPACE_AUTO_UPGRADE`,
`MEMSPACE_UPDATE_CHECK`.

```toml
[server]
address = "127.0.0.1:8765"
public_url = ""            # default http://<address>
downloads_dir = ""
update_check = "on"        # "off": don't ask GitHub daily for the latest release

[database]
path = "./data/memspace.db"

[auth]
owner_user_id = "owner"

[client]                   # all optional
home = "~/.memspace"       # or $MEMSPACE_HOME / --home
server_url = ""            # overrides the URL saved by `client connect`
inject = "on"              # "off" disables context injection
auto_upgrade = "on"        # "off": no automatic upgrades to the server's version
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
| `POST /api/v1/device-requests`, `/device-requests/claim` | ask to join (no auth; open requests capped, 10 min); claim the token once approved, with the request's secret |
| `GET /api/v1/devices`, `POST /api/v1/devices/mcp-token` | devices; issue or rotate the MCP token |
| `POST /api/v1/projects`, `GET /api/v1/projects`, `GET /api/v1/projects/{id}` | ensure (by Git identity, then name), list, details with per-device locations |
| `POST /api/v1/sessions`, `GET /api/v1/sessions[?before=&before_id=]`, `/sessions/{id}`, `/sessions/{id}/events` | ensure, list (paged), summary, timeline |
| `POST /api/v1/events/batch`, `GET /api/v1/events/{id}` | ingest (transactional, deduplicated, receipts per event), one event |
| `POST /api/v1/search`, `POST /api/v1/context` | search; context pack (`session_start` / `prompt` / `explicit`) |
| `GET /api/v1/memories`, `/memories/{id}` | memories with sources (`before` / `before_id` paging) |
| `GET /api/v1/overview`, `GET /api/v1/embedding/status` | dashboard data; semantic index state |
| `GET /api/v1/sync/status`, `POST /api/v1/sync/report` | the server's view of a device; the client's spool report |
| `POST /api/v1/ui/login-links`, `/ui/login`, `/ui/logout`, `GET /ui/session` | browser sign-in |
| `/api/v1/admin/…` | device requests (approve / deny), invites, device revocation, login links, browser sessions, downloads (admin sessions only) |
| `GET /downloads/{name}` | client binaries from `server.downloads_dir`, else a redirect to the GitHub release of the server's version (no auth) |
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

Releases: pushing a tag `v*` runs `.github/workflows/release.yml`, which
builds the clients and the server binaries, tests on each server platform,
and publishes them with `SHA256SUMS`, `LICENSE` and `NOTICE`.

The web UI (React, TypeScript, Vite) lives in `web/`; its build output
`web/dist` is committed and embedded, so `go build` needs no Node.
Vendored C/C++ sources and their licenses are listed in `NOTICE` and in
`internal/fts/simple/README.md`, `internal/vec/README.md`.

## License

Apache License 2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE).
