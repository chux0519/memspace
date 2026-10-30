"""Audit complete matched runs and emit aggregate component retrieval comparisons.

Usage: python3 compare.py RUN... --out comparison.json
Raw scores/evidence remain in the supplied run directories.
"""
import argparse
from collections import defaultdict
import hashlib
import json
import math
from pathlib import Path
import random
import statistics

DATA_SHA = 'd6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442'
SOURCE_SHA = '25c0fc4f610b612675600ebac66da592716081b7cf97b34c018b417f1b34520f'


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def percentile(values, p):
    v = sorted(values)
    position = (len(v)-1)*p
    lo, hi = math.floor(position), math.ceil(position)
    return v[lo] + (v[hi]-v[lo])*(position-lo)


def paired(candidate, baseline):
    differences = [candidate[q]['recallAllAtK']-baseline[q]['recallAllAtK'] for q in sorted(baseline)]
    wins, losses = differences.count(1), differences.count(-1)
    n = wins+losses
    p = min(1, 2*sum(math.comb(n, i) for i in range(min(wins, losses)+1))/2**n) if n else 1
    rng = random.Random(20261030)
    boot = [sum(rng.choices(differences, k=len(differences)))/len(differences) for _ in range(10000)]
    return dict(wins=wins, losses=losses, strict_rate_difference=statistics.mean(differences),
                paired_bootstrap_95_ci=[percentile(boot, .025), percentile(boot, .975)],
                mcnemar_exact_two_sided_p=p, caution='descriptive; variants developed against this public benchmark, not an untouched holdout')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('runs', nargs='+', type=Path)
    parser.add_argument('--out', required=True, type=Path)
    args = parser.parse_args()
    systems, receipts, common = {}, {}, None
    for run in args.runs:
        manifest = json.loads((run/'manifest.json').read_text())
        assert manifest['source']['contentSha256'] == SOURCE_SHA, 'changed official loader/scorer source'
        assert manifest['datasetFiles'][0]['sha256'] == DATA_SHA, 'changed dataset'
        selected = set(manifest['selectedQuestionIds'])
        assert len(selected) == 500
        assert common is None or common == selected
        common = selected
        groups = defaultdict(list)
        for line in (run/'scores.ndjson').read_text().splitlines():
            row = json.loads(line)
            assert row['status'] == 'ok' and row['k'] == 5
            groups[row['adapter']].append(row)
        for adapter_name, rows in groups.items():
            adapter = run.name + '/' + adapter_name
            assert adapter not in systems, 'duplicate run/adapter'
            assert len(rows) == 500 and {r['questionId'] for r in rows} == selected, 'incomplete/duplicate coverage'
            positive = [r for r in rows if r['answerability'] == 'answerable']
            assert len(positive) == 470
            assert all(r['recallAllAtK'] is None for r in rows if r['answerability'] != 'answerable')
            receipts[adapter] = {r['questionId']: r for r in positive}
            typed = defaultdict(list)
            for r in positive:
                typed[r['questionType']].append(r)
            systems[adapter] = dict(coverage=500, errors=0, answerable=470,
                strict_successes=sum(r['recallAllAtK'] for r in positive),
                strict_all_at_5=statistics.mean(r['recallAllAtK'] for r in positive),
                any_successes=sum(r['hit'] for r in positive), fractional_recall=statistics.mean(r['recallAtK'] for r in positive),
                query_ms={f'p{p}':percentile([r['latencyMs'] for r in rows], p/100) for p in [50,95]},
                ingest_ms_p50=statistics.median(r['ingestionMs'] for r in rows),
                types={k:dict(successes=sum(r['recallAllAtK'] for r in v), questions=len(v)) for k,v in sorted(typed.items())},
                run=run.name, manifest_sha256=digest(run/'manifest.json'), scores_sha256=digest(run/'scores.ndjson'),
                evidence_sha256=digest(run/'evidence.ndjson'), worker_sha256=manifest['config'].get('workerSha256'))
    baseline = next(rows for name, rows in receipts.items() if name.endswith('/agentmemory-bm25-component'))
    for adapter, rows in receipts.items():
        systems[adapter]['paired_vs_agentmemory'] = paired(rows, baseline)
    result = dict(dataset_sha256=DATA_SHA, official_source_sha256=SOURCE_SHA, k=5,
                  scope='supplied-session retrieval component; no capture, injection, answer or judge evaluation',
                  latency_scope='diagnostic; different storage engines and cold/warm behavior; not a production latency claim',
                  systems=systems)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(result, ensure_ascii=False, indent=2)+'\n')
    for name, system in systems.items():
        print(name, system['strict_successes'], '/ 470', f"{system['strict_all_at_5']:.2%}")


if __name__ == '__main__':
    main()
