# Mechanism ablations (offline, no default changes)

Compare transferable mechanisms before choosing dataset-specific thresholds.
Research and measured results live in the private notes repository; source text,
indexes, models and per-question receipts stay outside this repository.

The primary modes form a 2×2 comparison:

| Mode | Admission/ordering | Evidence delivered |
| --- | --- | --- |
| baseline | Current production rules | Current selected event fragments |
| judge_single | Fixed multilingual cross-encoder sees query + single source | Selected event fragments |
| dialogue | Current production anchor selection | Anchors plus adjacent messages in spare slots |
| judge_dialogue | Cross-encoder sees query + bounded adjacent dialogue | Anchors plus adjacent messages in spare slots |

`rank_single` and `rank_dialogue` disable rejection and are diagnostic controls:
contrast them with the corresponding judge mode to distinguish ordering from
rejection. Their possible extra injections must be reported alongside recall.
The dialogue modes also preserve selected prompt roles rather than suppressing
prompts when a reply from the same session is present.

All modes replay the same production deduplicated candidate pool (at most 24).
The baseline must exactly match the original pack. Neighbors are at most the
immediately preceding and following indexed prompt/reply in the same
owner/project/session. They are not generated text or full-session retrieval.
Selected anchors retain priority; actual neighbor messages fill spare delivery
slots, with the same maximum 6 original fragments and 6000 Unicode characters.
Model windows follow chronological order; delivered fragments retain anchor
priority and carry original roles, timestamps and refs. Neighbor expansion can
add noise and cannot repair sources absent from the original anchor pool.

## Reference judgment

The initial model is `cross-encoder/mmarco-mMiniLMv2-L12-H384-v1`, revision
`1427fd652930e4ba29e8149678df786c240d8825`, full float32 on CPU, 6 threads,
batches of 4, max 512 tokens. Query head/tail is bounded to 128 model tokens;
windows reserve at least half the document token budget for the anchor and
share the remainder among neighbors. Oversized fragment head/tail truncation
and exact token input hashes are recorded. Pair lengths are asserted <=512.

This is a generic multilingual relevance model, **not a trained answerability
judge**. The preregistered binary reference is raw logit >0 (sigmoid >0.5),
with no claim that model training calibrates this boundary for memory admission.
`-judge-logit-min` exposes the experimental boundary for reproducibility;
it is not a product default or a recommendation. This experiment freezes it at
0 and does not search alternative values. Rejection failures, ranking-only
controls and positive-vs-negative score AUROC should all be inspected.
No gold answers, evidence labels, question type or dataset split enter the
scorer. LME type labels are used solely for deterministic sampling upstream.

## Run

```bash
# New private directories; fixture database is disposable.
go run -tags sqlite_fts5 ./scripts/bench/retrieval \
  -corpus scripts/bench/data/dialogue-mechanisms-v1.json \
  -out "$HOME/memspace-bench/dialogue-export" -focused \
  -embedding-url http://127.0.0.1:18792/v1/embeddings \
  -freeze-queries -export-mechanisms

# Requires an already verified local model snapshot and CPU torch/transformers.
python scripts/bench/mechanisms/score.py \
  --cases "$HOME/memspace-bench/dialogue-export/cases.jsonl" \
  --model /path/to/pinned-model --out "$HOME/memspace-bench/dialogue-model"

go run -tags sqlite_fts5 ./scripts/bench/mechanisms \
  -cases "$HOME/memspace-bench/dialogue-export/cases.jsonl" \
  -scores "$HOME/memspace-bench/dialogue-model/scores.jsonl" \
  -out "$HOME/memspace-bench/dialogue-replay" -judge-logit-min 0

# Requires tiktoken with the cl100k_base encoding.
python scripts/bench/mechanisms/report.py \
  --export "$HOME/memspace-bench/dialogue-export" \
  --model "$HOME/memspace-bench/dialogue-model" \
  --replay "$HOME/memspace-bench/dialogue-replay" --kind fixture
```

Repeat with `admission-probes-v1.json`. This is a previously used developmental
fixture, not fresh holdout evidence. `dialogue_fixture.py` generates 48 authored
probes across 12 scenes, with original conditions, terse replies, corrections,
protected environments and missing approvals. Its labels are scored only after
selection. Do not treat this constructed fixture as proof of generalization.

`lme mechanisms` exports a deterministic pilot from verified full LongMemEval-S
and existing independently READY per-question indexes. It selects 10 answerable
questions per type and 5 abstentions by SHA256(question ID), writing the sample
before retrieval. Supply `-data`, `-indexes`, `-out`, `-reuse-index-source` and
optionally `-embed-url`, following [index provenance rules](LME-TRACE.md).
Use `report.py --kind lme`. Source-session coverage is not visible answer-span
completeness; the pilot is not a new full-500 or competitor headline score.

The initial run measures each question once after warmup; it is a single-host
comparison, not repeated steady-state SLA evidence.
CPU score time includes warm tokenization + inference for all candidates of
one query, excludes embedding/retrieval/IPC/render, and uses no score cache.
Replay time is separate and includes fresh scoped neighbor database lookups for
dialogue modes (the frozen window is checked against current source records). Peak scorer RSS includes its Python runtime/model and
cannot be equated to incremental server memory without integration. Do not
run competing CPU workloads while measuring or label replay as live hook time.

`ungated_original` and `ungated_dialogue` are additional confound controls:
remove admission while retaining the original candidate ordering, without a
model. Compare rank modes against them before attributing any recovery to the
cross-encoder. These are not proposed default policies. They require no extra
inference and can reuse exactly the same frozen model scores and candidates.
