#!/usr/bin/env python3
"""Score source-session rankings without inference or answer-text heuristics."""
from __future__ import annotations
import argparse
import gzip
import hashlib
import json
import math
from pathlib import Path

DATA_SHA = 'd6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442'

def question_scores(gold, ranked):
    gold = set(gold)
    ranked = list(dict.fromkeys(ranked))
    if not gold:
        raise ValueError('answerable question has no gold sources')
    result = {}
    for k in (1, 5, 10):
        found = gold.intersection(ranked[:k])
        result[f'recall_all_at_{k}'] = float(found == gold)
        result[f'fractional_at_{k}'] = len(found) / len(gold)
        result[f'any_hit_at_{k}'] = float(bool(found))
    dcg = sum(1 / math.log2(i + 2) for i, sid in enumerate(ranked[:10]) if sid in gold)
    ideal = sum(1 / math.log2(i + 2) for i in range(min(10, len(gold))))
    result['ndcg_at_10'] = dcg / ideal
    return result

def score_rows(data, rankings, name):
    questions = {q['question_id']: q for q in data}
    if len(questions) != len(data) or set(rankings) != set(questions):
        raise ValueError('rankings must cover each dataset question exactly once')
    scores, evidence = [], []
    for qid, q in questions.items():
        ranked = list(dict.fromkeys(rankings[qid]))
        allowed = set(q['haystack_session_ids'])
        if not set(ranked) <= allowed:
            raise ValueError(f'out-of-corpus source in {qid}')
        gold = set(q['answer_session_ids'])
        if not gold <= allowed:
            raise ValueError(f'out-of-corpus gold in {qid}')
        answerable = not qid.endswith('_abs')
        if answerable:
            metrics = question_scores(gold, ranked)
        else:
            metrics = {f'{kind}_at_{k}': None for k in (1,5,10)
                       for kind in ('recall_all','fractional','any_hit')}
            metrics['ndcg_at_10'] = None
        scores.append({'question_id': qid, 'mode': name, 'category': q['question_type'],
                       'answerable': answerable, 'duration_ms': 0,
                       'returned_sessions': len(ranked), **metrics})
        # The canonical loader clears targets for abstention items even when
        # the raw dataset still names a related session. Do not score it as
        # evidence for an answer the question explicitly makes unavailable.
        evidence.append({'question_id': qid, 'returned_sessions': ranked,
                         'gold_sessions': sorted(gold) if answerable else []})
    positives = [s for s in scores if s['answerable']]
    fields = [k for k in scores[0] if k.startswith(('recall_', 'fractional_', 'any_hit_', 'ndcg_'))]
    def aggregate(rows):
        return {'n': len(rows), **{key: sum(r[key] for r in rows) / len(rows) for key in fields}}
    summary = {'name': name, 'covered': len(scores), 'scored': len(positives),
               'unanswerable_not_scored': len(scores)-len(positives),
               'metrics': aggregate(positives),
               'categories': {c: aggregate([s for s in positives if s['category'] == c])
                              for c in sorted({s['category'] for s in positives})},
               'limits': ['Source retrieval only; no QA, abstention or answer completeness score.',
                          'K counts distinct source sessions; deduplicate before taking top K.',
                          'Index transformations and returned context budgets may differ by product.']}
    return summary, scores, evidence

def write_outputs(out, data, rankings, name):
    out.mkdir(parents=True, exist_ok=False)
    summary, scores, evidence = score_rows(data, rankings, name)
    for kind, rows in [('scores', scores), ('evidence', evidence)]:
        with gzip.open(out / f'{kind}.ndjson.gz', 'wt', encoding='utf-8') as f:
            for row in rows:
                f.write(json.dumps(row, ensure_ascii=False) + '\n')
    (out/'summary.json').write_text(json.dumps(summary, ensure_ascii=False, indent=2)+'\n')
    return summary

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--data', type=Path, required=True)
    p.add_argument('--rankings', type=Path, required=True,
                   help='JSON object: question_id -> ranked raw source session IDs')
    p.add_argument('--out', type=Path, required=True)
    p.add_argument('--name', required=True)
    args = p.parse_args()
    if hashlib.sha256(args.data.read_bytes()).hexdigest() != DATA_SHA:
        raise ValueError('unsupported dataset bytes; use pinned official cleaned S file')
    data = json.loads(args.data.read_text())
    if len(data) != 500:
        raise ValueError('expected full500 covered /470 answerable')
    summary = write_outputs(args.out, data, json.loads(args.rankings.read_text()), args.name)
    if summary['scored'] != 470:
        raise ValueError('answerability coverage changed')
    print(json.dumps(summary, ensure_ascii=False))

if __name__ == '__main__':
    main()
