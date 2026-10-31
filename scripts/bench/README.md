# MemSpace retrieval baseline

[COMPARISON-PLAN.md](COMPARISON-PLAN.md) defines the next comparison against
agentmemory, Nautilus Compass and Hindsight, including new Chinese/English
retrieval cases, evidence-delivery checks, resource measurements and optimization gates.

[LONGMEMEVAL.md](LONGMEMEVAL.md) records the separate public LongMemEval-S comparison,
using agentmemory's unchanged loader/scorer and verified dataset SHA. This is supplied-session
component retrieval, distinct from the injection regression below.

`data/coding-v1.json` contains **80 synthetic, authored questions** over 60 answer
records in four projects. It is a regression baseline, not independently reviewed
human ground truth, a coding-task completion benchmark, or a competitor comparison.
Forty answer records and 41 original positive queries reuse `eval.py`, which also
informed existing embedding thresholds. The `test` partition is therefore not a
new, unbiased model or threshold evaluation set. No thresholds are tuned by this
runner.

`data/coding-v2-evidence.json` extends v1 to **100 questions**, preserving every
v1 question and answer. It adds shared-session complementary records, long-source
evidence and missing-attribute/scope probes. See [EVIDENCE.md](EVIDENCE.md) for the
controlled comparison, token tradeoff and limitations of its negative labels.
Select it with `-corpus scripts/bench/data/coding-v2-evidence.json`.

[RERANK.md](RERANK.md) records the official Python reranker reference and the
follow-up session-budget fix. `data/session-budget-v1.json` is a separate eight
question packing fixture requiring three or four sources in one session; run it
with `-focused`, which permits small fixtures and skips the standard session-start
fixtures. Do not merge these deliberately easy retrieval probes into headline
quality scores. Default runs still require 50–100 questions.

The fixed questions comprise 41 original queries, 11 paraphrases, 11 single-project
collision queries, one all-project query, seven multi-evidence queries, one budget
variant, and eight negative queries. Labels specify AND across `evidence_groups`
and OR within a group. For example, either payment-idempotency record satisfies
one group; a recovery-steps question must additionally retrieve its recovery
record. Each document has a short literal `evidence` phrase identifying the fact
that must survive into injected text.

There are 24 dev and 56 test questions. Queries sharing evidence, including
transitively connected multi-evidence queries, stay in the same partition.
Questions also carry language and category labels. Negative questions include
near-topic questions whose requested fact exists only outside their scope.

## Run from the repository root

Requires Go, cgo/C++, and SQLite FTS5, as the server does. The runner creates and
removes its own temporary database. It refuses to overwrite an output directory
and never opens a production database.

```bash
# Keyword-only: no embedding endpoint needed.
CGO_ENABLED=1 go run -tags sqlite_fts5 ./scripts/bench/retrieval \
  -out scripts/bench/runs/coding-v1-keyword -repeats 3

# Production chunking, embedding, sqlite-vec, thresholds and RRF.
CGO_ENABLED=1 go run -tags sqlite_fts5 ./scripts/bench/retrieval \
  -out scripts/bench/runs/coding-v1-hybrid -repeats 3 \
  -embedding-url http://127.0.0.1:8771/v1/embeddings -model BAAI/bge-m3

# Standard-library report, with token counts left null.
python3 scripts/bench/tokenize_report.py scripts/bench/runs/coding-v1-hybrid

# Exact reference token counts, not character estimates or Claude billing.
uv run --with tiktoken python scripts/bench/tokenize_report.py \
  scripts/bench/runs/coding-v1-hybrid --encoding cl100k_base
```

Create `scripts/bench/runs` first if needed. `-query-timeout` defaults to the
production embedding deadline of 1s. Without `-embedding-url`, semantic and hybrid
runs are omitted; they are never silently presented as keyword equivalents.
With an endpoint, the runner requires successful index construction before
measurement. It records semantic calls, empty responses and degradation using
the service's error timestamp, active space and circuit breaker. Status queries
run outside measured intervals. Empty semantic results alone do not count as a
failure. The embedding service's existing model fingerprint and thresholds are
included in the manifest; no fake embedding model is used for reported quality.

## What runs

1. Each corpus document becomes an indexed generic user prompt
   (`请记录本次编码任务的进展`) and an indexed answer event. V1 uses separate sessions;
   v2 also permits multiple records in one session within the same project.
   All sessions are initially ended, with events anchored one hour before the run.
   This creates 120 search documents for v1's 60 labeled answers, or 144 for v2's 72.
   The harness is not
   measuring event ingestion or summary generation performance.
2. Search calls production `search.Search` in keyword, semantic and hybrid modes,
   with `limit=5`. The unit is a **document**, not a deduplicated session.
3. Context calls production `contextpack.Build`, including scope, thresholds,
   session deduplication, snippets and character budgets.
4. Hook calls production `inject.Run` against a loopback HTTP endpoint backed by
   `contextpack.Build`, including project detection, HTTP, local delivered-state
   files, prompt extraction and deadlines. Each independent question/repetition
   gets clean state. An immediate second call shares state to measure suppression.
   Endpoint startup and cleanup are outside timing. This is not CLI process
   startup, production authentication, a real network, or the complete agent hook
   command that also captures/syncs events.
5. After retrieval measurements, four sessions receive production-generated
   summaries and are used as separate session-start fixtures. They are not added
   to the 80-question quality denominator and do not change the retrieval corpus.
   All seed records deliberately share an event timestamp. Session-card selection
   therefore also exercises the existing top-10 candidate window and session-ID
   tie ordering; a missing expected card in these four smoke fixtures is not a
   general handoff-accuracy estimate.

Automatic context supports the current project. The one all-project question is
excluded from context/hook runs (79 questions remain). The selected-project
questions each select exactly their current project. The budget variant applies
700 characters to direct context construction; hooks retain the production default
6000-character prompt budget. Session start uses its default 4000-character budget.

## Metrics and receipts

`receipts.jsonl` stores every repetition's query, ranked source references,
projects, snippets, packed text/items, scores, elapsed time, errors, timeouts and
semantic status. `corpus.json` freezes the labels. `manifest.json` records the
corpus/distractor SHA256, source hashes, commit and dirty status, model fingerprint,
thresholds, hardware, runtime, corpus sizes, construction time, database sizes and
process peak RSS on Linux. Copies of the runner source, `go.mod` and `go.sum` are
included because a commit identifier alone cannot reproduce an uncommitted runner.
Peak RSS is for the harness process; the external embedding service is excluded.

The report script writes `summary.json` and `report.md`, plus
`tokenized-receipts.jsonl` when an encoding is selected. It groups metrics by
layer/mode, partition, category and language:

- **Any-hit@5:** at least one required evidence group retrieved.
- **All-evidence-hit@5:** every required group retrieved.
- **Evidence recall@5:** fraction of required groups retrieved, averaged per query.
- **MRR@5:** reciprocal rank of the first relevant result, zero if absent in top 5.
- For injection, a reference alone is insufficient: its literal evidence phrase
  must be visible after rendering. Whitespace, case and highlight markers are
  normalized. This conservative check can miss paraphrased/partial evidence; it
  does not use an LLM judge or claim semantic completeness of an answer.
- Negatives have a separate nonempty-return/injection rate. They never inflate
  positive recall by being counted as successful empty answers.
- Scope violations use actual returned project IDs; provenance errors compare
  returned IDs/names against fixture source metadata.
- The repeat-hook row reports nonempty output, size and latency, with quality
  metrics null: intentionally suppressed memory is not a retrieval miss.
- Latency p50/p95 and character/token distributions include empty output and
  errors. Report error, timeout and degradation rates alongside them.

All runs are warm, sequential and on the same shared machine. Modes use a fixed
order; latency is not controlled for mode-order, external service scheduling or
cache effects. Three repetitions
are repeated measurements of the same questions, not 240 independent questions.
The small corpus cannot establish production-scale latency, and no percentage
should be advertised as a competitor comparison. Search output token counts are
zero because the runner does not render MCP search responses; token metrics apply
to the exact context/hook text only.

## Add private real-world distractors

```bash
python3 scripts/bench/export_distractors.py \
  ~/.local/share/memspace/memspace.db \
  scripts/bench/runs/noise-200-head400.json --count 200 --seed 7 --max-chars 400

CGO_ENABLED=1 go run -tags sqlite_fts5 ./scripts/bench/retrieval \
  -out scripts/bench/runs/coding-v1-noise200-head400 -repeats 3 \
  -embedding-url http://127.0.0.1:8771/v1/embeddings \
  -distractors scripts/bench/runs/noise-200-head400.json
```

The first-pass noise comparison uses 200 documents capped at 400 Unicode
characters each. The full 2000-document snapshot was frozen but its index build
was stopped before producing measurements; it is not a completed benchmark.
Remove `--max-chars` and increase `--count` for a more expensive full-text run.

The exporter opens SQLite read-only and uses SQLite backup to capture a consistent
snapshot including WAL. Sampling starts from sorted doc IDs with a fixed seed;
the exported JSON SHA256, rather than the changing live database, defines the
frozen input. Distractor contents retain private history. Their project identities
are remapped round-robin onto the four synthetic projects, and they enter as
answer events with generic prompts. This is a noise-sensitivity test, not a replay
of the production database or a test of real project mapping. Distractors are
unjudged: an answer-equivalent distractor is still treated as nonrelevant, so review
such apparent false positives before interpreting the quality change.

Runs are ignored by Git, and raw files are created inside private output
directories. Do not publish private distractors or returned history without review.
Only the synthetic dataset and reviewed aggregate baseline belong in the repo.

## Checks

```bash
CGO_ENABLED=1 go test -race -tags sqlite_fts5 ./scripts/bench/retrieval
python3 -m unittest discover -s scripts/bench -p 'test_*.py'
```

The original `eval.py`, `speed.py` and `regress.py` remain embedding-only tools.
Their old printed `recall@5` is actually any-hit@5, not fractional/all-evidence
recall. Use this harness for product-path measurements.

## Isolate injection quality from embedding-service availability

`-freeze-queries` records real query embeddings once before measurement and uses
those vectors with production KNN throughout the run. `-query-vectors PATH` reuses
that exact snapshot and checks the model fingerprint and dimensions. It still
requires the endpoint to index documents. No similarity thresholds change. Both
modes are **quality only**: query-embedding time and query-service failures are
excluded, so report tables hide latency and must not be compared with live timing.

```bash
CGO_ENABLED=1 go run -tags sqlite_fts5 ./scripts/bench/retrieval \
  -out scripts/bench/runs/quality-frozen -repeats 3 \
  -embedding-url http://127.0.0.1:8771/v1/embeddings -freeze-queries

CGO_ENABLED=1 go run -tags sqlite_fts5 ./scripts/bench/retrieval \
  -out scripts/bench/runs/quality-replay -repeats 3 \
  -embedding-url http://127.0.0.1:8771/v1/embeddings \
  -query-vectors scripts/bench/runs/quality-frozen/query-vectors.json
```

Use a separate worktree containing the old production builders for before/after
comparisons, copying only the benchmark runner and its data. The current runner
also snapshots `history.go` and `recall.go`; these new helpers can be copied into
an old worktree unused by its original builders. Verify the old builders against
HEAD, and compare identical corpus, query-vector snapshot, model fingerprint and
returned search sources before attributing a change to injection.

`-diagnose-only` writes current packs and missing-evidence candidates alongside
**counterfactual baseline gate labels**, not an instrumented trace of current
policy. Use the saved pre-fix diagnostic run for baseline attribution; do not
interpret the old-gate labels as proof that a modified policy rejected a source.

Live runs retain `semantic.last_error` on degradation. See [INJECTION.md](INJECTION.md)
for the fixes, controlled comparison and the live-service availability limitation.

## Follow-up: background contention and high-scoring implementations

See [TIMEOUT.md](TIMEOUT.md) for the reproduced scheduling issue, native-server fix,
local deployment and live rerun, and [COMPETITOR-INJECTION.md](COMPETITOR-INJECTION.md)
for verified metric definitions and implementation lessons from high-scoring projects.

Mechanism-first ablations (independent multilingual judgment and bounded dialogue
windows): see [MECHANISMS.md](MECHANISMS.md). These are offline experiments;
production defaults and the admission-threshold archive are unchanged.
