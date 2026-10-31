#!/usr/bin/env python3
"""Attribute source losses in real lme -mode pack -trace-pack receipts.

Labels are read only here, after the retrieval pipeline has finished. Source
coverage is not evidence-span coverage: LongMemEval has no answer-span labels.
"""
import argparse
from collections import Counter
import gzip
import hashlib
import json
from pathlib import Path

STAGES = ("search", "deduplicated", "admitted", "ordered", "selected", "sections", "rendered")


def load_rows(path):
    with gzip.open(path, "rt", encoding="utf-8") as stream:
        rows = [json.loads(line) for line in stream]
    by_id = {row["question_id"]: row for row in rows}
    if len(by_id) != len(rows):
        raise ValueError("duplicate question IDs")
    return by_id


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def analyze(row):
    trace = row.get("trace") or []
    if not trace or trace[-1]["stage"] != "rendered":
        raise ValueError("missing successful trace for " + row["question_id"])
    source_map = row.get("trace_session_sources") or {}
    stages = {}
    by_key = {}
    contents = []
    for event in trace:
        name = event["stage"]
        if name == "content":
            contents.append(event)
            continue
        if name in stages:
            raise ValueError("repeated stage " + name)
        sources = []
        for hit in event.get("hits") or []:
            sid = hit.get("session_id")
            if sid and sid not in source_map:
                raise ValueError("unmapped source session " + sid)
            source = source_map.get(sid)
            by_key["doc:" + str(hit["doc_id"])] = source
            if source and source not in sources:
                sources.append(source)
        for item in event.get("items") or []:
            source = source_map.get(item["ref"]) if item["kind"] == "session" else by_key.get(item["key"])
            if not source:
                raise ValueError("unmapped selected item " + item["key"])
            if source not in sources:
                sources.append(source)
        stages[name] = dict(sources=sources, event=event)
    final = stages["rendered"]
    if final["sources"] != (row.get("returned_sessions") or []):
        raise ValueError("trace and scored sources differ")
    if final["event"].get("text", "") != (row.get("pack") or {}).get("text", ""):
        raise ValueError("trace and delivered text differ")
    if "search" in stages:
        for stage in STAGES:
            if stage not in stages:
                raise ValueError("incomplete trace: " + stage)
    gold = set(row.get("gold_sessions") or [])
    answerable = not row["question_id"].endswith("_abs")
    if answerable and not gold:
        raise ValueError("answerable question without gold sources")
    complete = {stage: bool(gold) and gold <= set(stages.get(stage, {}).get("sources", [])) for stage in STAGES}
    first = next((stage for stage in STAGES if not complete[stage]), None) if answerable else None
    if first == "search" and "search" not in stages:
        first = "query_skipped"
    losses = {}
    previous = gold
    for stage in STAGES:
        present = gold & set(stages.get(stage, {}).get("sources", []))
        losses[stage] = sorted(previous - present)
        previous = present
    source_first = {}
    for source in gold:
        source_first[source] = next((stage for stage in STAGES if source not in stages.get(stage, {}).get("sources", [])), None)
    gate_rejected = []
    admitted_ids = {h["doc_id"] for h in stages.get("admitted", {}).get("event", {}).get("hits", [])}
    for hit in stages.get("deduplicated", {}).get("event", {}).get("hits", []):
        sid = source_map.get(hit.get("session_id"))
        if sid in gold and hit["doc_id"] not in admitted_ids:
            gate_rejected.append({key: hit.get(key) for key in ("doc_id", "kind", "match", "matched_terms", "total_terms", "similarity")})
    return dict(question_id=row["question_id"], answerable=answerable, first_failure=first,
                final_complete=complete["rendered"], complete=complete, losses=losses,
                source_first_failure=source_first, rejected_gold_hits=gate_rejected,
                stage_sources={stage: stages.get(stage, {}).get("sources", []) for stage in STAGES},
                stage_documents={stage: len(stages.get(stage, {}).get("event", {}).get("hits") or stages.get(stage, {}).get("event", {}).get("items") or []) for stage in STAGES},
                content_decisions=dict(Counter(event.get("decision") for event in contents)),
                partial_search=bool(stages.get("search", {}).get("event", {}).get("note")),
                selected_items=len((row.get("pack") or {}).get("items") or []))


def summarize(rows, summaries, baseline=None):
    positives = [s for s in summaries if s["answerable"]]
    failures = [s for s in positives if not s["final_complete"]]
    stages = {}
    for stage in STAGES:
        complete = sum(s["complete"][stage] for s in positives)
        stages[stage] = dict(complete=complete, denominator=len(positives), rate=complete / len(positives),
                             questions_losing_sources_here=sum(bool(s["losses"][stage]) for s in positives))
    result = dict(attempted=len(rows), scored=len(positives), unscored=len(rows)-len(positives),
                  final_failures=len(failures), first_failure=dict(Counter(s["first_failure"] for s in failures)),
                  stages=stages, failed_at_item_cap=sum(s["selected_items"] >= 6 for s in failures),
                  failed_below_item_cap=sum(s["selected_items"] < 6 for s in failures),
                  scope="source-session coverage, all selected sources; not visible-answer completeness or an LLM score")
    if baseline is not None:
        if rows.keys() != baseline.keys():
            raise ValueError("baseline question coverage differs")
        result["baseline_changes"] = {
            "source_rankings": [qid for qid, row in rows.items() if (row.get("returned_sessions") or []) != (baseline[qid].get("returned_sessions") or [])],
            "text": [qid for qid, row in rows.items() if (row.get("pack") or {}).get("text", "") != (baseline[qid].get("pack") or {}).get("text", "")],
            "items": [qid for qid, row in rows.items() if (row.get("pack") or {}).get("items", []) != (baseline[qid].get("pack") or {}).get("items", [])],
        }
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("results", type=Path)
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--expected", type=int, default=500)
    args = parser.parse_args()
    manifest = json.loads((args.results / "manifest.json").read_text())
    if manifest.get("mode") != "pack" or not manifest.get("trace_pack"):
        raise ValueError("requires a traced pack run")
    rows = load_rows(args.results / "evidence.ndjson.gz")
    if len(rows) != args.expected:
        raise ValueError("incomplete execution coverage")
    scores = load_rows(args.results / "scores.ndjson.gz")
    if rows.keys() != scores.keys() or any(s.get("error") for s in scores.values()):
        raise ValueError("execution errors or score/evidence coverage mismatch")
    summaries = [analyze(row) for row in rows.values()]
    for row in summaries:
        if row["answerable"] and row["final_complete"] != bool(scores[row["question_id"]]["recall_all_at_10"]):
            raise ValueError("trace/scorer disagreement")
    report = summarize(rows, summaries, load_rows(args.baseline / "evidence.ndjson.gz"))
    report["hashes"] = {name: sha(args.results / name) for name in ("manifest.json", "scores.ndjson.gz", "evidence.ndjson.gz")}
    report["baseline_evidence_sha256"] = sha(args.baseline / "evidence.ndjson.gz")
    args.out.mkdir(mode=0o700)
    with gzip.open(args.out / "questions.ndjson.gz", "wt", encoding="utf-8") as f:
        for row in summaries:
            f.write(json.dumps(row, ensure_ascii=False) + "\n")
    (args.out / "summary.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    lines = ["# Injection source-loss attribution", "", "| Stage | Complete / scored | Rate | Questions losing gold sources here (overlapping) |", "|---|---:|---:|---:|"]
    for stage, value in report["stages"].items():
        lines.append(f"| {stage} | {value['complete']}/{value['denominator']} | {value['rate']:.2%} | {value['questions_losing_sources_here']} |")
    lines += ["", "First failing stage partitions the final failures: " + json.dumps(report["first_failure"]),
              "", "Baseline output changes: " + json.dumps({k: len(v) for k, v in report["baseline_changes"].items()}),
              "", "Source presence does not certify that answer-bearing facts survived a text window. Gold labels are used only in this offline analyzer.", ""]
    (args.out / "report.md").write_text("\n".join(lines))
    print("\n".join(lines))


if __name__ == "__main__":
    main()
