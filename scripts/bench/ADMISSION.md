# Compare and configure context admission

The legacy `0.60` BGE-M3 and `0.50` keyword-coverage values are compatibility
defaults. The original semantic study used 41 authored positive queries and five
easy unrelated queries. Its document-pair pass rates are not a product's
query-level false-injection rate. These numbers are not universal calibration.

Server TOML:

```toml
[context]
admission_mode = "hybrid"
# semantic_min_similarity = 0.60  # omitted: inherit embedding.min_similarity/model default
keyword_min_coverage = 0.50
allow_exact_keywords = true
```

| Setting | Meaning |
|---|---|
| `admission_mode = "hybrid"` | Any enabled semantic, exact-keyword or partial-keyword route may qualify |
| `admission_mode = "semantic"` | Only positive semantic similarity at/above the threshold qualifies; ignores both keyword routes |
| `admission_mode = "none"` | Skip qualification; scope, owner/current-session filters, source validation, deduplication and rendering limits still apply |
| `semantic_min_similarity` | Finite `[0,1]`; explicit `0` means zero, not automatic. A hit with no semantic score cannot pass this route |
| `keyword_min_coverage` | Finite `[0,1]`; minimum matched/total fraction for partial (`any`) keyword hits |
| `allow_exact_keywords` | Whether full (`all`) keyword matches bypass semantic qualification. Disabling it does not disable partial keyword matches |

Context settings affect qualification only. Search floor, ranking, 48/24/6
limits, source windows and character budgets remain unchanged. In particular,
changing a context semantic threshold does not also expand previously weak text
windows. The legacy embedding injection threshold still supplies the default
and determines strong-hit text expansion. Context settings apply to REST prompt
packs and MCP explicit context through server middleware, not client JSON fields.
Invalid numbers, unknown context keys and unknown benchmark JSON keys fail.

For benchmarks, `-admission-policy POLICY.json` uses the same fields with JSON
`mode` instead of TOML `admission_mode`. Hooks and the synthetic retrieval runner
record both requested and resolved policy. Existing runs without the flag keep
the old policy.

## Reproducible comparison

`data/admission-profiles-v1.json` freezes 14 policies: semantic thresholds
0.45/0.50/0.55/0.60/0.65/0.70/0.75, partial coverage 0.25/0.50/0.75/1.00,
exact-keyword ablation, semantic-only ablations, and no qualification.
These are experimental comparison points, not recommended settings.

```bash
CGO_ENABLED=1 go build -tags sqlite_fts5 -o "$BENCH_ROOT/bin/lme-admission" ./scripts/bench/lme
"$BENCH_ROOT/bin/lme-admission" sweep \
  -data "$BENCH_ROOT/data/longmemeval_s_cleaned.json" \
  -indexes "$BENCH_ROOT/lme-isolated-projects" \
  -policies scripts/bench/data/admission-profiles-v1.json \
  -embed-url http://127.0.0.1:18792/v1/embeddings \
  -reuse-index-source "$ORIGINAL_INDEX_PRODUCER_SHA256" \
  -out "$BENCH_ROOT/results/admission-grid"
```

The cache server must already be running. Only READY disposable indexes with
the explicit original producer are accepted. Each question performs one actual
search, then supplies the exact in-memory candidates **including semantic source
chunks** to `BuildFromCandidates` for every policy. Default replay must match
the original pack byte-for-byte and item-for-item. Qualification, scope hydration,
selection and rendering use the production implementation; no gold labels enter
these stages. Profile order rotates. Each output directory has the original
source-session scorer's receipts and can be independently audited.

Replay timings exclude search and model calls. Use the hooks runner with the
real CPU provider to measure end-to-end latency separately. Identical maximum
budgets do not imply identical actual token use: report the extra tokens with
any recall increase. Removing qualification still retains the search floor.

`data/admission-probes-v1.json` has 80 agent-authored queries in 20 scene groups:
40 positive, 20 missing-attribute negatives, and 20 answers available only in
another project. English/Chinese sources have both same-language and cross-language
positive queries. Eight scene groups are development (32 queries), twelve are
held out (48). All query variants of a scene stay together. The source bank is
shared for retrieval; no test outcome labels are used to choose a policy.
Separate dev/test files retain identical documents. These labels are authored
by the same agent, not independent human blind evaluation.

Run every policy on LongMemEval (a previously viewed development regression)
and the dev probes. Freeze at most two nonbaseline candidates before inspecting
test outcomes; run only those plus baseline on test. Existing coding-v2 is a
separate regression, not a fresh holdout. Compare visible fact completeness,
false injection on explicit negatives, project/provenance errors and actual token
cost. Do not count LongMemEval's 30 abstention cases as verified retrieval negatives.

## Choosing values

Use dev Pareto tradeoffs rather than an invented weighted score: more complete
positive evidence, fewer false injections, and lower token/latency cost. Require
at least no regression against the current policy before treating a candidate
as a replacement. Freeze candidates and report held-out gains and losses by
scene group; translated variants do not multiply independent sample size.

A threshold is supported only for the tested model fingerprint, text granularity,
language mix, scope and deployment budget. Repeat calibration when those change.
If no setting improves completeness without worsening negatives/cost, the data
does not justify a new universal default. Keep compatibility behavior while
reporting the available tradeoffs and the remaining relevance failures.
