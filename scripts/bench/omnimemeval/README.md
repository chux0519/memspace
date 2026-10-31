# MemSpace adapter for OmniMemEval

This is an offline-installable adapter overlay for OmniMemEval commit
`7c79d46e31dd85f3da2509b430bba9406656dd28`. It does not modify an upstream
installation automatically and does not publish results. The client uses a
compiled local worker; no MemSpace cloud account or agent config changes.

```bash
go build -tags sqlite_fts5 -o /absolute/path/to/omnimemeval-worker \
  ./scripts/bench/omnimemeval/worker
python scripts/bench/omnimemeval/install_adapter.py --checkout /path/to/disposable/OmniMemEval
```

Copy `env_examples/.env.memspace` in that checkout to a private env file and
configure the worker, evaluation root, embeddings, answer and judge providers.
Use the pinned harness's normal LME runner with `--lib memspace`. Start with
one ingestion/search unit and `--to-step 2` before full QA. This is not a score.
The initial integration targets LongMemEval; other datasets need their own
smoke and complete runs before claiming support. No full-QA result is implied
by installing the adapter.

`context_pack` (default) returns exactly the visible production prompt pack,
wrapped by the unchanged shared answer template. Its maximum is 6 real source
items and 6000 characters; top_k below 6 additionally bounds visible items,
without shrinking retrieval depth. The returned string list contains one whole
pack, not one alleged source. Receipts retain the actual source Items. `search`
is a separately labeled diagnostic using real search snippets. Do not combine
scores from the two modes or fetch full event text from the returned refs.
Empty packs stay empty. Queries are read-only.

Each evaluation user has a hashed directory, a worker process and a private
SQLite DB. Workers call the production authenticated ingestion handlers and
shared search/packing functions. They do not rewrite the retrieval algorithm.
The embedding service is process-local, so two users cannot accidentally share
another user's active index. Repeated complete-session add calls deduplicate;
reusing a session/batch ID with different text is an error. Stable session IDs,
original roles and RFC3339 timestamps are mandatory. register time/session
helpers via the overlay, not just the client registry. `ready` completes
embedding indexing before search; provider/index errors do not become silent
keyword fallback. An empty embedding URL explicitly declares keyword-only.

Streaming cleanup closes only the specified user's worker and removes its
owned DB directory; other users remain intact. Private `*.receipts.jsonl` and
stderr logs remain at the evaluation root for audit. The root rejects existing
unowned data and production/ancestor paths. No cleanup command is sent to a
running MemSpace service. Use a new root for a different mode or model config.

The actual API here is a local JSONL worker, without network/auth transport
latency. Client timings include RPC and readiness checks, while worker
`duration_ms` measures context construction after readiness. Neither is live
hook latency. Ingestion and indexing time belong to add/ready and must be
reported separately. Worker startup is outside the search timer.
The original imported sessions are marked ended and the production rule-based
summary/memory generator is settled before add completes. No synthetic
unfinished-work summaries are generated. Summary jobs are not run by a second
background scheduler in the worker. System/tool messages are retained as source events,
but only production-supported event kinds are indexed.

Offline checks after building the worker:

```bash
MEMSPACE_TEST_WORKER=/absolute/path/to/omnimemeval-worker \
  python -m unittest discover -s scripts/bench/omnimemeval -p 'test_*.py'
go test -tags sqlite_fts5 ./scripts/bench/omnimemeval/worker ./internal/contextpack
```

The overlay validates pinned source hashes and refuses unrelated edits. Review
its diff, docs and upstream offline test output before proposing a contribution.
It does not change answer/judge prompts, rerun failed questions for favorable
scores, or synthesize missing results. Publish manifests, commands, versions,
per-question outcomes, failures and token usage following the upstream
CONTRIBUTING contract. Credentials and private endpoint logs stay private.

Judge calibration reuses frozen successful official answers and their existing
reference labels. It does not repeat answer generation or change the official
score. For example, to compare a candidate that does not accept temperature=0:

```bash
python scripts/bench/omnimemeval/judge_compare.py \
  --judged /private/baseline-combined/judged.json \
  --harness /path/to/pinned/OmniMemEval \
  --out /private/new-calibration \
  --model gpt-5.6-luna --env-file /private/provider.env
```

The selected key is `OPENAI_API_KEY`; the optional selected endpoint is
`OPENAI_BASE_URL`. Neither unrelated env entries nor keys are copied to results.
Omit `--env-file` to use existing process configuration. The output directory
must be new. Defaults expect all 500 indices exactly once and four concurrent
calls. The manifest records input/prompt hashes and candidate parameters; the
pinned prompt file is parsed as string literals, without executing that checkout.
API responses, resolved models, usage, latency and errors remain per question.
Malformed labels are errors, with no label-based retries. A partial run is marked
incomplete, never presented as all-question agreement. Temperature is omitted by
default; use `--temperature 0` only for a compatible model and disclose the choice.
The candidate's default reasoning is retained. Agreement measures consistency
with the reference judge; human review of disagreements is still needed.
