# Offline reranker experiment

This experiment uses the official Hugging Face `AutoModelForSequenceClassification`
path for `BAAI/bge-reranker-v2-m3`, revision
`953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e`. It does not deploy a server or add a
reranker dependency to the product. The candidate exporter reads only the runner's
disposable database; inference receives queries and source passages without labels.

Use a Python environment with `torch`, `transformers`, `safetensors`, and
`huggingface_hub`. The reference here uses CPU float32, six threads, batch size 4,
and at most 512 tokens per question/passage pair. It records truncation and exact
model-file hashes. This is an extra 568M-parameter model, not reuse of embedding
weights.

The recorded reference environment is torch `2.11.0+cpu` / transformers `4.57.1`.
Results and limitations are in [RERANK.md](../RERANK.md).

## Download the pinned model

Configure the proxy for external requests, leaving local services direct:

```bash
export HTTP_PROXY=http://127.0.0.1:7890
export HTTPS_PROXY=http://127.0.0.1:7890
export ALL_PROXY=http://127.0.0.1:7890
export NO_PROXY=127.0.0.1,localhost
export HF_HUB_DISABLE_XET=1
export HF_HUB_DOWNLOAD_TIMEOUT=60

python - <<'PY'
from huggingface_hub import snapshot_download
print(snapshot_download(
    'BAAI/bge-reranker-v2-m3',
    revision='953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e',
    allow_patterns=['config.json', 'model.safetensors', 'tokenizer.json',
                    'tokenizer_config.json', 'special_tokens_map.json',
                    'sentencepiece.bpe.model'],
))
PY
```

The original float32 `model.safetensors` is 2,271,071,852 bytes; its SHA256 is
`d9e3e081faff1eefb84019509b2f5558fd74c1a05a2c7db22f74174fcedb5286`.

## Freeze real candidates, score, and replay

From the repository root, using the existing query-vector snapshot or a fresh
`-freeze-queries` run:

```bash
CGO_ENABLED=1 go run -tags sqlite_fts5 ./scripts/bench/retrieval \
  -corpus scripts/bench/data/coding-v2-evidence.json \
  -out scripts/bench/runs/rerank-candidates-v2 \
  -embedding-url http://127.0.0.1:8771/v1/embeddings \
  -query-vectors scripts/bench/runs/evidence-v2-before/query-vectors.json \
  -export-candidates 12,24 -repeats 1

python scripts/bench/rerank/score.py \
  scripts/bench/runs/rerank-candidates-v2/candidates.jsonl \
  --model-path ~/.cache/huggingface/hub/models--BAAI--bge-reranker-v2-m3/snapshots/953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e \
  --out scripts/bench/runs/rerank-scores-v2

python scripts/bench/rerank/replay.py \
  --base-commit a3e2709 \
  --scores scripts/bench/runs/rerank-scores-v2/scores.json \
  --query-vectors scripts/bench/runs/evidence-v2-before/query-vectors.json \
  --out scripts/bench/runs/rerank-replay-v2

uv run --with tiktoken python scripts/bench/rerank/report.py \
  scripts/bench/runs/rerank-replay-v2/study.jsonl
```

The exporter runs separate production searches at depth 12 and 24, including the
existing search floor, filters, and RRF. Neither the top 12 of a depth-24 result nor
an unrestricted scan of every labeled answer substitutes for real candidates.
It returns the matching original chunk where available, with title/body/files as
the fallback. Empty/unknown projects remain empty. Identical question/text pairs
are scored once, and identical texts within a latency probe are deduplicated.

`replay.py` checks out HEAD (or `--base-commit`) in a temporary worktree, overlays the benchmark runner,
then patches only that worktree. It saves the exact patch and adapter source.
`adapter.go.txt` changes candidate depth, ordering, and qualification while
preserving scoped source hydration, source Match metadata, stale-chunk protection,
same-session grouping, the chosen checkout's session policy, total six-source cap, character
budgets, and rendering. `study.go.txt` calls real `contextpack.Build` for every
configuration. It never replaces it with a Python approximation.

The grid is fixed before inference: depths 12/24, raw-logit thresholds
-4/-2/0/2/4/6, and these policies:

- `baseline`: unchanged qualification and order, including a depth-24 ablation.
- `reorder`: change order while retaining the original injection gate.
- `replace`: use the reranker gate instead of the original injection gate.
- `rescue`: keep candidates accepted by either the original or reranker gate.

The raw score is not a probability or an embedding cosine. Reranking runs before
the injection gate, so weak semantic candidates get a chance to qualify.
The existing search floor still applies; reranking cannot rescue a missing source.

`report.py` chooses using **dev only**: maximize visible all-evidence passes with
no lost dev passes and no new clean-negative nonempty cases. Ties prefer fewer
clean-negative nonempty cases, less injected text, shallower retrieval, then a
deterministic policy order. It then reports test and all-question results,
per-question gains/losses, and reference tokens. q089–096 remain separately
reported missing-attribute/premise probes. Even the other negative nonempty counts
are retrieval diagnostics, not judged answer-error rates.

These authored questions and their test split have already been inspected in
earlier work. Results measure regression behavior, not independent generalization.
One repetition per configuration suffices for deterministic quality replay; it
does not establish online reliability.

## Timing limits

Score generation records length-bucketed offline batch forwards. Separately, 16
fixed probes at each candidate depth are recomputed three times, using a warm
resident model and no cross-request score cache. These request timings include
Python tokenization and CPU inference; they exclude HTTP, retrieval, context
packing, queueing, and model startup. The probes deliberately cover short, long,
multi-evidence, negative, and empty cases. Their p95 is not a full-corpus production
p95. Replay timings exclude model inference altogether.

For the follow-up breadth experiment, export `12,24,48` with
`-candidate-floor 0.30`, score with `--reuse-pairs OLD_RUN/pairs.jsonl
--skip-request-timings`, and replay with `--candidate-floor 0.30 --deep48`.
Reuse requires matching model-file hashes, tokenizer files, library versions,
dtype, attention implementation and truncation. The original -2 operating point
remains frozen for this comparison; it is not reselected by test results.
The depth-qualified score format preserves different matching chunks for the
same source at different depths; the adapter also accepts the original format.

The independent session-budget fixture runs without a model:

```bash
CGO_ENABLED=1 go run -tags sqlite_fts5 ./scripts/bench/retrieval \
  -corpus scripts/bench/data/session-budget-v1.json -focused \
  -out scripts/bench/runs/session-budget-example -repeats 3
```

Compare both quality and CPU cost before choosing whether to port inference to
embeddings.cpp. No speedup from native code or quantization is assumed.
