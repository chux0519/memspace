"""Score a frozen, label-free candidate export with the official HF model.

CPU/float32 is deliberate: this is the numerical reference, not native latency.
Model inference never reads corpus labels or dev/test partitions.
"""
import argparse
import hashlib
import json
from pathlib import Path
import platform
import time


def digest(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def assemble_scores(rows, pairs):
    result = {}
    for row in rows:
        by_ref = result.setdefault(str(row["depth"]), {}).setdefault(row["query"], {})
        for c in row["candidates"]:
            ref = c["hit"]["ref_id"]
            value = pairs[(row["query"], c["source_text"])]["logit"]
            if ref in by_ref and by_ref[ref] != value:
                raise ValueError("same query/reference/depth mapped to different chunks")
            by_ref[ref] = value
    return {"schema_version": 2, "scores": result}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("candidates", type=Path)
    ap.add_argument("--model-path", required=True, type=Path)
    ap.add_argument("--out", required=True, type=Path)
    ap.add_argument("--threads", type=int, default=6)
    ap.add_argument("--batch-size", type=int, default=4)
    ap.add_argument("--max-length", type=int, default=512)
    ap.add_argument("--reuse-pairs", type=Path)
    ap.add_argument("--skip-request-timings", action="store_true")
    args = ap.parse_args()
    args.out.mkdir(mode=0o700, parents=True, exist_ok=False)
    (args.out / "score.py").write_bytes(Path(__file__).read_bytes())
    import torch
    import transformers
    from transformers import AutoModelForSequenceClassification, AutoTokenizer

    torch.set_num_threads(args.threads)
    torch.set_num_interop_threads(1)
    start = time.perf_counter()
    tok = AutoTokenizer.from_pretrained(args.model_path, local_files_only=True)
    model = AutoModelForSequenceClassification.from_pretrained(
        args.model_path, local_files_only=True, torch_dtype=torch.float32,
        attn_implementation="eager",
    ).eval()
    load_ms = (time.perf_counter() - start) * 1000
    rows = [json.loads(s) for s in args.candidates.read_text().splitlines()]
    pairs = {}
    for row in rows:
        for c in row["candidates"]:
            text = c["source_text"]
            key = (row["query"], text)
            if key not in pairs:
                pairs[key] = {"query": key[0], "source_text": text}
    # Length bucketing reduces padding; stable order and batch metadata recorded.
    reused = {}
    if args.reuse_pairs:
        previous = json.loads((args.reuse_pairs.parent / "manifest.json").read_text())
        if (previous["max_length"] != args.max_length or previous["dtype"] != "float32"
                or previous["attention"] != "eager" or previous["torch"] != torch.__version__
                or previous["transformers"] != transformers.__version__):
            raise ValueError("cached pair model or truncation differs")
        for name, expected in previous["model_files_sha256"].items():
            if digest(args.model_path / name) != expected:
                raise ValueError(f"cached model file changed: {name}")
        for line in args.reuse_pairs.read_text().splitlines():
            r = json.loads(line)
            key = (r["query"], r["source_text"])
            if key in pairs:
                pairs[key].update(r)
                reused[key] = r
    examples = sorted([p for key, p in pairs.items() if key not in reused], key=lambda p: len(p["query"]) + len(p["source_text"]))
    full_lengths = [len(tok(p["query"], p["source_text"], truncation=False)["input_ids"]) for p in examples]
    tokens = [tok(p["query"], p["source_text"], truncation=True,
                  max_length=args.max_length) for p in examples]
    warm = tok(["查询"], ["候选原文"], return_tensors="pt", padding=True)
    with torch.inference_mode():
        model(**warm)
    batches = []
    start = time.perf_counter()
    with (args.out / "pairs.jsonl").open("x") as f, torch.inference_mode():
        for p in reused.values():
            f.write(json.dumps(p, ensure_ascii=False) + "\n")
        for at in range(0, len(examples), args.batch_size):
            batch_tokens = tokens[at:at + args.batch_size]
            inputs = tok.pad(batch_tokens, return_tensors="pt")
            t = time.perf_counter()
            logits = model(**inputs, return_dict=True).logits.reshape(-1).float().tolist()
            elapsed = (time.perf_counter() - t) * 1000
            batches.append({"pairs": len(logits), "padded_tokens": inputs["input_ids"].shape[1], "forward_ms": elapsed})
            for i, logit in enumerate(logits):
                p = examples[at + i]
                p.update(logit=logit, input_tokens=len(batch_tokens[i]["input_ids"]),
                         full_input_tokens=full_lengths[at + i],
                         truncated=full_lengths[at + i] > args.max_length)
                f.write(json.dumps(p, ensure_ascii=False) + "\n")
            f.flush()
            print(f"scored {min(at + args.batch_size, len(examples))}/{len(examples)} pairs, batch {elapsed:.0f}ms", flush=True)
    elapsed_ms = (time.perf_counter() - start) * 1000
    result = assemble_scores(rows, pairs)
    (args.out / "scores.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    # Fixed probes, selected before seeing scores. No cross-request memoization
    # or padding with other queries: measure resident-model CPU request cost.
    probes = {"q001", "q002", "q008", "q011", "q029", "q052", "q070", "q081",
              "q085", "q086", "q089", "q093", "q097", "q098", "q099", "q100"}
    request_timings = []
    with torch.inference_mode():
        for rep in range(3):
            for row in rows:
                if args.skip_request_timings or row["question_id"] not in probes:
                    continue
                t = time.perf_counter()
                texts = sorted(set(c["source_text"] for c in row["candidates"]), key=len)
                for at in range(0, len(texts), args.batch_size):
                    batch = texts[at:at + args.batch_size]
                    inputs = tok([row["query"]] * len(batch), batch, truncation=True,
                                 padding=True, max_length=args.max_length, return_tensors="pt")
                    model(**inputs)
                request_timings.append({"question_id": row["question_id"], "depth": row["depth"],
                                        "repetition": rep, "candidate_count": len(row["candidates"]),
                                        "unique_pairs": len(texts),
                                        "duration_ms": (time.perf_counter() - t) * 1000})
            print(f"finished resident-model request timing round {rep + 1}/3", flush=True)
    metadata = {"model": "BAAI/bge-reranker-v2-m3", "revision": args.model_path.resolve().name,
                "model_files_sha256": {p.name: digest(p) for p in args.model_path.glob("*") if p.is_file()},
                "candidates_sha256": digest(args.candidates), "script_sha256": digest(__file__),
                "torch": torch.__version__, "transformers": transformers.__version__,
                "python": platform.python_version(), "device": "cpu", "dtype": "float32",
                "attention": "eager", "threads": args.threads, "max_length": args.max_length,
                "batch_size": args.batch_size, "unique_pairs": len(pairs), "new_pairs": len(examples),
                "reused_pairs": len(reused), "reuse_pairs_sha256": digest(args.reuse_pairs) if args.reuse_pairs else None,
                "load_ms": load_ms,
                "total_scoring_ms": elapsed_ms, "batches": batches,
                "request_timings": request_timings,
                "request_latency_scope": "16 fixed probes per depth, 3 repetitions; warm CPU tokenizer+model; dedup identical texts within request; no HTTP/search/packing; not full-corpus or end-to-end latency",
                "latency_scope": "length-bucketed offline batches, excludes tokenization, not request latency"}
    (args.out / "manifest.json").write_text(json.dumps(metadata, indent=2) + "\n")


if __name__ == "__main__":
    main()
