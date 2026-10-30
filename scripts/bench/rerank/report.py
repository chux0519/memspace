"""Choose a replay policy using dev only, then report every split separately."""
import argparse
from collections import defaultdict
import hashlib
import json
from pathlib import Path
import statistics


def percentile(values, fraction):
    if not values:
        return None
    values = sorted(values)
    pos = (len(values) - 1) * fraction
    lo = int(pos)
    return values[lo] + (values[min(lo + 1, len(values) - 1)] - values[lo]) * (pos - lo)


def summarize(rows):
    positive = [r for r in rows if not r["score"]["negative"]]
    # q089-096 are missing-attribute/premise probes, not clean irrelevance labels.
    clean = [r for r in rows if r["score"]["negative"] and not 89 <= int(r["question_id"][1:]) <= 96]
    unsupported = [r for r in rows if 89 <= int(r["question_id"][1:]) <= 96]
    return {"positive_questions": len(positive),
            "all_hit": sum(r["score"]["all_hit"] for r in positive),
            "all_hit_ids": [r["question_id"] for r in positive if r["score"]["all_hit"]],
            "clean_negative_questions": len(clean),
            "clean_negative_nonempty_ids": [r["question_id"] for r in clean if r["text"].strip()],
            "unsupported_attribute_nonempty_ids": [r["question_id"] for r in unsupported if r["text"].strip()],
            "characters_total": sum(len(r["text"]) for r in rows),
            "scope_violations": sum(r["score"]["out_of_scope"] for r in rows),
            "provenance_errors": sum(r["score"]["provenance_errors"] for r in rows)}


def select_policy(groups):
    baseline = summarize([r for r in groups[(12, "baseline", 0)] if r["split"] == "dev"])
    before_pass = set(baseline["all_hit_ids"])
    before_false = set(baseline["clean_negative_nonempty_ids"])
    eligible = []
    for key, rows in groups.items():
        dev = summarize([r for r in rows if r["split"] == "dev"])
        if not before_pass.issubset(dev["all_hit_ids"]):
            continue
        if not set(dev["clean_negative_nonempty_ids"]).issubset(before_false):
            continue
        if dev["scope_violations"] or dev["provenance_errors"]:
            continue
        eligible.append((key, dev))
    # No test outcomes participate in this ordering.
    return min(eligible, key=lambda x: (-x[1]["all_hit"], len(x[1]["clean_negative_nonempty_ids"]),
                                       x[1]["characters_total"], x[0][0], x[0][1], x[0][2]))[0]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("study", type=Path)
    args = ap.parse_args()
    raw = args.study.read_bytes()
    groups = defaultdict(list)
    for line in raw.splitlines():
        row = json.loads(line)
        r = row["receipt"]
        groups[(row["depth"], r["mode"], row["threshold"])].append(r)
    selected = select_policy(groups)
    summaries = {}
    for key, rows in groups.items():
        summaries[str(key)] = {split: summarize([r for r in rows if split == "all" or r["split"] == split])
                               for split in ["dev", "test", "all"]}
    baseline = groups[(12, "baseline", 0)]
    chosen = groups[selected]
    before = {r["question_id"]: r for r in baseline}
    gains = [r["question_id"] for r in chosen if not r["score"]["negative"] and r["score"]["all_hit"] and not before[r["question_id"]]["score"]["all_hit"]]
    losses = [r["question_id"] for r in chosen if not r["score"]["negative"] and not r["score"]["all_hit"] and before[r["question_id"]]["score"]["all_hit"]]
    import tiktoken
    tokenizer = tiktoken.get_encoding("cl100k_base")
    token_summary = {}
    for name, rows in [("baseline", baseline), ("selected", chosen)]:
        tokens = [len(tokenizer.encode(r["text"], disallowed_special=())) for r in rows]
        token_summary[name] = {"p50": statistics.median(tokens), "p95": percentile(tokens, .95)}
    result = {"selected": {"depth": selected[0], "mode": selected[1], "raw_logit_threshold": selected[2]},
              "selection_rule": "maximize dev all-hit, no lost dev passes or new dev clean-negative nonempty; then fewer clean-negative nonempty, fewer characters, shallower depth, lexical policy order",
              "study_sha256": hashlib.sha256(raw).hexdigest(), "gains": gains, "losses": losses,
              "token_encoding": "cl100k_base", "context_tokens": token_summary,
              "baseline": summaries[str((12, "baseline", 0))], "selected_metrics": summaries[str(selected)],
              "all_policies": summaries,
              "limitations": ["test is a known regression split, not independent generalization", "negative labels are not exhaustive relevance judgments", "offline scores replayed through real context Build; no online inference latency"]}
    (args.study.parent / "selection.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    (args.study.parent / "selected-receipts.jsonl").write_text("".join(json.dumps(r, ensure_ascii=False) + "\n" for r in chosen))
    print(json.dumps({k: v for k, v in result.items() if k != "all_policies"}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
