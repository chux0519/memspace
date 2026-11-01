# LongMemEval results — 2026-11-01

These are local replications on the author-published
[LongMemEval-S cleaned dataset](https://huggingface.co/datasets/xiaowu0162/longmemeval/tree/98d7416c24c778c2fee6e6f3006e7a073259d48f).
All modes cover 500 questions. Source metrics exclude the 30 `_abs` questions;
QA includes all 500. Dataset SHA256:

```text
d6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442
```

## Configuration and scope

MemSpace uses the worker built at `078985a`, SHA256
`32f60f08d21e89c1a614ea75dad794b2260883b214ab565b95ed47c91252fcc3`.
It calls production ingestion, search and context-pack functions in isolated
databases. Retrieval uses BGE-M3 embeddings plus keyword search and RRF;
**no reranker and no LLM memory extraction**. The context ceiling is six real
source fragments and 6000 characters. Relation packing is enabled for the
reported delivery configuration; the product default remains off.

| Project | Pinned revision | Measured path |
|---|---|---|
| agentmemory | `3f8784d10c2e996873d4392b85226a5d042def0e` | Native BM25 whole-session component |
| deja-vu | `ad7c9f26874a05bb2a674f34241ac10cd3befb28` | Native CLI search; separately, actual `deja hook-prompt` output |
| MemPalace | `4f7745afbd9d0197c6475b4c7ac846d4932eaeda` | Native raw session-vector benchmark, local MiniLM |
| OmniMemEval | `7c79d46e31dd85f3da2509b430bba9406656dd28` | Shared answer/judge harness for MemSpace's paired QA experiment |

The three retrieval components have all-source@5 rates of 84.68%, 83.83%
and 85.74%, respectively. Source sessions are deduplicated before taking K.
MemPalace raw stores user turns only; deja-vu retains both roles. These are
component results, not measurements of the complete products.

Delivered-source coverage is **341/470 = 72.55%** for the frozen MemSpace
relation pack and **85/470 = 18.09%** for deja-vu's native per-prompt hook.
We capture MemSpace's actual pack text/items and deja-vu's
`hookSpecificOutput.additionalContext`, without externally truncating either.
deja-vu gets a fresh native index and reader for every question, under the
native benchmark's `/work/lme` project. Its native picker allows two sessions;
its 1536 budget constant is not a hard ceiling on the complete framed output.
deja-vu emits nothing for 250/500 questions; MemSpace emits nothing for 16/500.
Mean visible lengths, including empty outputs, are 880 and 3428 characters.
These different defaults are **not an equal-budget algorithm comparison**.
The previously measured deja-vu benchmark probe (55.32%) is a different
entrypoint and is not included as a real-hook result here.

Source-only peer runs deny network connections and receive no API credentials.
Their native CLI rankings were unchanged by observation, and their scores
were independently checked using agentmemory's original loader/scorer.
Deja's real hook uses the execution date; its CLI benchmark uses the question
date. Sources in the final text do not prove that answer-bearing facts survived.
Host-side processing and agent-initiated follow-up tools are outside this scope.
agentmemory's pinned version defaults to no hook injection; MemPalace's startup
context is a different, recency/importance-based mode. Their component retrieval
scores are not filled in as final automatic injection scores, nor is disabled
injection treated as a zero-quality result.

## Paired QA

Every off/on pair uses byte-identical starting databases, the same frozen
worker, input roles/order/timestamps, embeddings and budget ceilings. Query
order alternates; all 500 snapshot controls pass. The reader is
`gpt-4.1-mini-2025-04-14`, the judge `gpt-4o-mini-2024-07-18`, temperature 0.
Off scores 269/500 (53.8%); on scores 305/500 (61.0%): 63 gains, 27 losses.
Actual context length increases from 2362 to 3428 characters on average.
This is one stochastic answer/judge run per arm, not an equal-token ablation,
human-verified truth or a vendor QA leaderboard. These completed QA runs used
model APIs; no new QA was needed for the source-only comparison.

The English benchmark does not establish mixed-language quality, cross-project
isolation, synchronization reliability, production latency or memory usage.
Separate reranker experiments are excluded from every reported MemSpace mode.

## Published receipts and rescoring

`summary.json` contains aggregate scores, versions and original score-audit
fingerprints. Each of the five mode directories contains `scores.ndjson.gz`
and `evidence.ndjson.gz`: 500 rows each, with public dataset question/source
IDs, returned source order and metric values. `duration_ms = 0` in these
normalized scoring receipts is a placeholder, not a latency measurement.
MemPalace's native benchmark pads its ranking after the genuine top 50 with
remaining corpus positions. Receipts preserve that order; this tail is not
evidence of actual retrieval. All published ranking metrics use K at most 10.
`qa-paired.json` contains all 500 paired judgments and original context-token
counts. Those harness counts include list formatting; the final-text token
counts in `summary.json` use only the visible text and local `cl100k_base`.

Files contain no credentials, endpoint configuration, captured private history
or API response logs. `SHA256SUMS` fingerprints the score files and summary.
Full output texts, original database snapshots and the native-hook observation
driver remain local: these receipts allow independent **rescoring**, but are
not a self-contained distribution for rerunning the entire native-hook study.

With the pinned agentmemory checkout, the dataset and local `tsx` available:

```bash
for mode in agentmemory-bm25 deja-cli mempalace-raw memspace-relation-pack deja-prompt-hook; do
  npx --no-install tsx scripts/bench/verify_agentmemory.ts \
    --upstream /path/to/pinned/agentmemory \
    --data /path/to/longmemeval_s_cleaned.json \
    --results "scripts/bench/results/2026-11-01/$mode"
done
```

This runs no inference. See the [source scorer](../../retrieval_peers/README.md)
to score new rankings and the [OmniMemEval adapter](../../omnimemeval/README.md)
to run a fresh controlled MemSpace experiment. No production or agent
configuration is modified by installing these benchmark tools.
