# Trace source losses in the actual injection pipeline

`lme run -mode pack -trace-pack` records per-call observations from the same
`contextpack.Build` pipeline used by hooks. It does not change thresholds,
limits, scope, ranking, text windows or models. Ordinary hooks/API calls do not
collect or expose these observations.

The stages are actual search results, content-deduplicated candidates,
admitted candidates, session ordering, inspected content, selected history,
sections after delivered-state exclusion, and rendered items/text. The observer
receives copied slices. It is synchronous, so trace runs measure quality and
attribution, **not uninstrumented production latency**.

Use disposable indexes prepared by the existing LME runner. Keep dataset,
vectors, raw trace receipts and reports outside git; they contain source text.

```bash
CGO_ENABLED=1 go build -tags sqlite_fts5 -o "$BENCH_ROOT/bin/lme-trace" ./scripts/bench/lme

"$BENCH_ROOT/bin/lme-trace" run \
  -data "$BENCH_ROOT/data/longmemeval_s_cleaned.json" \
  -db "$BENCH_ROOT/lme-isolated" -isolate-projects \
  -mode pack -trace-pack \
  -embed-url http://127.0.0.1:18792/v1/embeddings \
  -reuse-index-source "$ORIGINAL_INDEX_PRODUCER_SHA256" \
  -out "$BENCH_ROOT/results/traced-pack"

python3 scripts/bench/trace_pack.py "$BENCH_ROOT/results/traced-pack" \
  --baseline "$BENCH_ROOT/results/unchanged-pack-baseline" \
  --out "$BENCH_ROOT/results/source-loss-analysis"
```

The explicit reuse flag retains the original producer and state files; use it
only after verifying ingestion/index derivation is unchanged. Without it the
normal provenance mismatch guard remains in force. The cached embedding server
must already be running; see the LME runner's existing serve command.

The analyzer uses gold source labels **after retrieval**, never in the observer
or candidate selection. It validates full execution coverage, unique question
IDs, source provenance, final trace versus delivered text, and agreement with
the existing scorer. Abstention questions count toward execution only.

`summary.json` reports source-complete counts at each stage. First-failure
counts partition final failures; transition counts can overlap because a query
may lose different required sources at multiple stages. `questions.ndjson.gz`
records the per-source earliest loss and gate signals. Baseline source, item and
text differences are reported explicitly rather than silently ignored.

A missing source at `search` means absent from the actual prompt search pool,
not necessarily absent from the index or the wider API search. The deduplication
stage also has a candidate cap, so a loss there is not automatically an incorrect
content duplicate. `rendered` measures selected source coverage: LongMemEval
has no answer-span labels, so it cannot establish that every answer-bearing fact
survived a 600-character text window. Literal answer matching is not substituted
for a valid span evaluation. Changes to these previously viewed questions are
development experiments; generalization needs independent questions/negative
cases and budget-matched checks.
