"""Summarize receipts; optionally count tokens with a named tiktoken encoding.

Install tiktoken to enable --encoding cl100k_base. This is a reference
encoding, not a claim about Claude's billed tokens. Without --encoding,
token counts remain null, never estimates from characters.
"""
import argparse
from collections import defaultdict
import hashlib
import json
import math
from pathlib import Path
import statistics


def percentile(values, p):
    values = sorted(values)
    if not values:
        return None
    pos = (len(values) - 1) * p
    lo, hi = math.floor(pos), math.ceil(pos)
    return values[lo] + (values[hi] - values[lo]) * (pos - lo)


def summarize(rows):
    positive = [r for r in rows if not r["score"]["negative"]]
    negative = [r for r in rows if r["score"]["negative"]]
    avg = lambda rs, key: statistics.mean(r["score"][key] for r in rs) if rs else None
    calls = sum(r["semantic"]["calls"] for r in rows)
    quality = rows[0]["layer"] != "hook_repeat"
    return dict(
        samples=len(rows), positive_samples=len(positive), negative_samples=len(negative),
        # Repetitions are repeated measurements, not independent questions.
        unique_questions=len({r["question_id"] for r in rows}),
        any_hit=avg(positive, "any_hit") if quality else None,
        top1=statistics.mean(r["score"]["mrr"] == 1 for r in positive) if positive and quality else None,
        reference_all_hit=statistics.mean(r.get("reference_all_hit", False) for r in positive) if positive and quality else None,
        all_evidence_hit=avg(positive, "all_hit") if quality else None,
        evidence_recall=avg(positive, "evidence_recall") if quality else None,
        mrr=avg(positive, "mrr") if quality else None,
        negative_nonempty_rate=statistics.mean(r["score"]["returned"] > 0 if r["layer"] == "search" else bool(r["text"].strip()) for r in negative) if negative else None,
        out_of_scope_results=sum(r["score"]["out_of_scope"] for r in rows),
        returned_results=sum(r["score"]["returned"] for r in rows),
        provenance_errors=sum(r["score"].get("provenance_errors", 0) for r in rows),
        nonempty_rate=statistics.mean(bool(r["text"].strip()) for r in rows),
        error_rate=statistics.mean(bool(r.get("error")) for r in rows),
        timeout_rate=statistics.mean(r["timeout"] for r in rows),
        semantic_calls=calls,
        semantic_degraded_rate=sum(r["semantic"]["degraded"] for r in rows)/calls if calls else None,
        latency_ms={f"p{p}": percentile([r["duration_ms"] for r in rows], p/100) for p in (50, 95)},
        characters={f"p{p}": percentile([r["characters"] for r in rows], p/100) for p in (50, 95)},
        tokens={f"p{p}": percentile([r["tokens"] for r in rows], p/100) for p in (50, 95)} if all(r["tokens"] is not None for r in rows) else None,
    )


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("run", type=Path)
    p.add_argument("--encoding", help="optional tiktoken encoding, e.g. cl100k_base")
    args = p.parse_args()
    encoder = None
    if args.encoding:
        import tiktoken
        encoder = tiktoken.get_encoding(args.encoding)
    manifest = json.loads((args.run / "manifest.json").read_text())
    frozen = manifest.get("quality_only_frozen_queries", False)
    raw = (args.run / "receipts.jsonl").read_bytes()
    rows = [json.loads(line) for line in raw.splitlines()]
    corpus = json.loads((args.run / "corpus.json").read_text())
    languages = {q["id"]: q["language"] for q in corpus["questions"]}
    questions = {q["id"]: q for q in corpus["questions"]}
    groups = defaultdict(list)
    for row in rows:
        row["tokens"] = len(encoder.encode(row["text"], disallowed_special=())) if encoder else None
        row["language"] = languages.get(row["question_id"], "fixture")
        question = questions.get(row["question_id"])
        if question:
            refs = {h["ref_id"] for h in row.get("results", [])} if row["layer"] == "search" else {h["ref"] for h in row.get("pack", {}).get("items", [])}
            row["reference_all_hit"] = bool(question["evidence_groups"]) and all(any(ref in refs for ref in group) for group in question["evidence_groups"])
        else:
            row["reference_all_hit"] = row["score"]["all_hit"]
        base = f'{row["layer"]}/{row["mode"]}'
        for key in (base + "/all", base + "/" + row["split"], base + "/category/" + row["category"], base + "/language/" + row["language"]):
            groups[key].append(row)
    result = dict(quality_only_frozen_queries=frozen, latency_scope="excludes query embedding; not production latency" if frozen else "warm sequential loopback", receipts_sha256=hashlib.sha256(raw).hexdigest(), encoding=args.encoding, token_scope="exact injected text; reference encoding, excludes user prompt and model output", groups={k:summarize(v) for k,v in sorted(groups.items())})
    if frozen:
        result["query_vectors_sha256"] = hashlib.sha256((args.run / "query-vectors.json").read_bytes()).hexdigest()
    (args.run / "summary.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    if encoder:
        import importlib.metadata
        result["tiktoken_version"] = importlib.metadata.version("tiktoken")
        (args.run / "tokenized-receipts.jsonl").write_text("".join(json.dumps(r,ensure_ascii=False)+"\n" for r in rows))
        (args.run / "summary.json").write_text(json.dumps(result,ensure_ascii=False,indent=2)+"\n")
    lines = [f"# MemSpace {manifest['corpus_version']} baseline", "", ("QUALITY ONLY: frozen real query vectors; latency excludes query embedding and is not comparable to live runs.\n\n" if frozen else "") + "Synthetic authored corpus; warm sequential loopback runs. Repeated samples do not enlarge the question set.", "", f"Token encoding: {args.encoding or 'not measured'}.", "", "| Layer / mode | Questions | Any hit | All evidence | Evidence recall | p50 ms | p95 ms | Tokens p50 / p95 |", "|---|---:|---:|---:|---:|---:|---:|---:|"]
    pct = lambda x: "—" if x is None else f"{x:.1%}"
    for key, s in result["groups"].items():
        if not key.endswith("/all"):
            continue
        tok = s["tokens"]
        latency50 = "—" if frozen else f'{s["latency_ms"]["p50"]:.2f}'
        latency95 = "—" if frozen else f'{s["latency_ms"]["p95"]:.2f}'
        lines.append(f'| {key[:-4]} | {s["unique_questions"]} | {pct(s["any_hit"])} | {pct(s["all_evidence_hit"])} | {pct(s["evidence_recall"])} | {latency50} | {latency95} | {"—" if tok is None else str(round(tok["p50"],1))+" / "+str(round(tok["p95"],1))} |')
    lines += ["", "Quality excludes negative questions and hook_repeat. Search uses top-5 documents; injection requires reference + visible evidence. Hook defaults do not inherit question-specific context budgets. See summary.json for split/category/language, negative, scope, timeout and degradation metrics.", ""]
    (args.run / "report.md").write_text("\n".join(lines))
    print("\n".join(lines))


if __name__ == "__main__":
    main()
