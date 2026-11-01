# Public-data source retrieval comparison

This scorer compares **source-session retrieval**, without an answering model
or LLM judge. It accepts a JSON object mapping each question ID to the raw
session IDs returned by a system, in rank order:

```json
{"question-id": ["source-session-a", "source-session-b"]}
```

Use the author-published LongMemEval-S cleaned file, SHA256
`d6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442`.
All 500 questions must be covered. The 30 IDs ending in `_abs` have no scored
retrieval target, even when the raw file still names related sessions. The
denominator is the remaining 470 questions.

```sh
python3 scripts/bench/retrieval_peers/score.py \
  --data /path/to/longmemeval_s_cleaned.json \
  --rankings /path/to/rankings.json \
  --name peer-native \
  --out /path/to/new-results

python3 -m unittest discover -s scripts/bench/retrieval_peers -p 'test_*.py'
```

The output includes per-question scores and returned/gold source IDs, plus
aggregate and category results. Rankings are deduplicated before taking K:

- `recall_all_at_5`: every required source is among the first five distinct sources.
- `fractional_at_5`: the fraction of required sources found, averaged across questions.
- `any_hit_at_5`: at least one required source is found.

These are different metrics. None proves that a generated answer is correct,
or that a short excerpt actually includes the information inside its source.
Raw retriever results and the source IDs that survive an automatic context
builder should be exported and scored as separate modes. Record each mode's
text transformations, model, filters and budget.

For an independent check, the gzip receipts match the existing pinned
agentmemory verifier:

```sh
npx tsx scripts/bench/verify_agentmemory.ts \
  --upstream /path/to/agentmemory-at-3f8784d10c2e996873d4392b85226a5d042def0e \
  --data /path/to/longmemeval_s_cleaned.json \
  --results /path/to/new-results
```

The verifier checks the original loader/scorer source hashes and every
question. It performs no inference. See its source for the expected upstream
files.

The 2026-11-01 local replication pinned deja-vu
`ad7c9f26874a05bb2a674f34241ac10cd3befb28` and MemPalace
`4f7745afbd9d0197c6475b4c7ac846d4932eaeda`. It used isolated stores,
network-denied evaluation processes, and the native raw retrieval paths.
MemPalace's raw benchmark stores user turns only; deja-vu retains both roles.
Those differences belong in the report rather than being silently normalized
away. Private run artifacts retain source archives, model fingerprints,
per-question responses and source-order audits.
