"""Summarize private baseline artifacts without moving them into the repository."""
import argparse
import hashlib
import json
import os
from pathlib import Path


def read(path):
    return json.loads(path.read_text())


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write(path, value):
    raw = json.dumps(value, ensure_ascii=False, indent=2) + '\n'
    fd = os.open(path, os.O_CREAT | os.O_WRONLY | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'w') as stream:
        stream.write(raw)


def dist(values):
    values = sorted(values)
    if not values:
        return dict(n=0)
    result = dict(n=len(values), mean=sum(values)/len(values), max=values[-1])
    for p in [50, 95, 99]:
        at = (len(values)-1)*p/100
        low = int(at)
        result['p'+str(p)] = values[low] + (values[min(low+1,len(values)-1)]-values[low])*(at-low)
    return result


def hooks(root, day):
    target = root / 'results' / (day + '-hooks')
    manifests, summaries, rows = {}, {}, []
    for scale in ['1x', '10x']:
        path = target / scale
        manifests[scale] = read(path / 'manifest.json')
        summaries[scale] = read(path / 'summary.json')
        samples = [json.loads(line) for line in (path / 'samples.ndjson').read_text().splitlines()]
        assert len(samples) == sum(g['n'] for g in summaries[scale].values())
        for key, group in summaries[scale].items():
            selected = [r for r in samples if r['layer'] + '/' + r['mode'] == key]
            group['memory_empty_rate'] = sum(r['items'] == 0 for r in selected) / len(selected)
            group['bytes'] = dist([r['bytes'] for r in selected])
            called = [r for r in selected if r['semantic_calls'] > 0]
            group['requests_with_semantic'] = len(called)
            group['conditional_semantic_timings'] = {field:dist([r[field] for r in called]) for field in [
                'semantic_ms','query_embedding_http_ms','knn_and_space_checks_ms']}
        for row in samples:
            row = dict(row, scale=scale, memory_empty=row['items'] == 0)
            rows.append(row)
    assert manifests['1x']['requests_sha256'] == manifests['10x']['requests_sha256'], 'different requests'
    assert manifests['10x']['db_counts']['search_docs'] == manifests['1x']['db_counts']['search_docs'] * 10
    assert manifests['10x']['db_counts']['embedding_chunks'] == manifests['1x']['db_counts']['embedding_chunks'] * 10
    summary = dict(scales=summaries, limits=[
        'Build is bounded by production 1.5s/2s deadlines; timeout durations are censored',
        '10x derived index, new project partitions; raw events and per-query project corpus unchanged',
        'MCP Hint is text output but not retrieved memory; memory_empty uses items==0',
        'tokens are embedding.EstimateTokens, not exact billed tokens',
        'first child receipt memory_empty defaults on errors are normalized in this derived view',
    ])
    write(target / 'summary.json', summary)
    with open(target / 'samples.ndjson', 'w') as stream:
        os.chmod(target / 'samples.ndjson', 0o600)
        for row in rows:
            stream.write(json.dumps(row, ensure_ascii=False) + '\n')
    write(target / 'manifest.json', dict(child_manifests=manifests, child_receipts_sha256={
        scale:digest(target / scale / 'samples.ndjson') for scale in manifests},
        same_requests=True, receipts_sha256=digest(target / 'samples.ndjson')))
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', required=True, type=Path)
    parser.add_argument('--date', default='2026-10-31')
    args = parser.parse_args()
    summary = hooks(args.root, args.date)
    lines = ['# Hook baseline ' + args.date, '',
             '| Scale / layer / mode | n | p50 ms | p95 ms | p99 ms | max ms | timeout | memory empty |',
             '|---|---:|---:|---:|---:|---:|---:|---:|']
    for scale, groups in summary['scales'].items():
        for key, group in sorted(groups.items()):
            latency = group['latency_ms']
            lines.append(f"| {scale}/{key} | {group['n']} | {latency['p50']:.1f} | {latency['p95']:.1f} | {latency['p99']:.1f} | {latency['max']:.1f} | {group['timeout_rate']:.1%} | {group['memory_empty_rate']:.1%} |")
    lines += ['', '| Scale / layer / mode | characters p50 / p95 | bytes p50 / p95 | estimated tokens p50 / p95 | empty text | truncated |', '|---|---:|---:|---:|---:|---:|---:|']
    for scale, groups in summary['scales'].items():
        for key, group in sorted(groups.items()):
            c, b, t = group['characters'], group['bytes'], group['estimated_tokens']
            lines.append(f"| {scale}/{key} | {c['p50']:.0f} / {c['p95']:.0f} | {b['p50']:.0f} / {b['p95']:.0f} | {t['p50']:.0f} / {t['p95']:.0f} | {group['empty_rate']:.1%} | {group['truncated_rate']:.1%} |")
    lines += ['', 'Semantic timing distributions below include requests with at least one semantic call; keyword/assembly p95 includes all requests. Residual is KNN plus DB/space overhead.', '',
              '| Scale / layer / mode | requests with semantic | semantic p50 / p95 ms | embedding HTTP p50 / p95 ms | residual p50 / p95 ms | keyword + assembly p95 ms |',
              '|---|---:|---:|---:|---:|---:|']
    for scale, groups in summary['scales'].items():
        for key, group in sorted(groups.items()):
            values = []
            for field in ['semantic_ms','query_embedding_http_ms','knn_and_space_checks_ms']:
                d = group['conditional_semantic_timings'][field]
                values.append(f"{d['p50']:.1f} / {d['p95']:.1f}" if d['n'] else '—')
            lines.append(f"| {scale}/{key} | {group['requests_with_semantic']}/{group['n']} | "+' | '.join(values)+f" | {group['keyword_and_assembly_ms']['p95']:.1f} |")
    lines += ['', *['- '+s for s in summary['limits']], '', '# LongMemEval production ingestion', '', '| Mode | attempts / answerable | all@5 | fractional@5 | any@5 | all@10 | NDCG@10 | errors |', '|---|---:|---:|---:|---:|---:|---:|---:|']
    lme = {}
    for mode in ['fts', 'vector', 'hybrid', 'pack', 'hybrid-no-floor']:
        path = args.root / 'results' / (args.date + '-lme') / mode / 'summary.json'
        if not path.exists():
            continue
        report = read(path)
        lme[mode] = report
        group = report['all']
        q = group['quality']
        values = ' | '.join(f'{q[key]:.2%}' for key in ['recall_all_at_5','fractional_at_5','any_hit_at_5','recall_all_at_10','ndcg_at_10'])
        lines.append(f"| {mode} | {group['attempted']} / {group['answerable']} | {values} | {group['failed']} |")
    if lme:
        lines += ['', 'Each question has an independent index. Search uses at most 100 documents, mapped to distinct source sessions. Pack scores selected sources, not answer-bearing text completeness. Cached/fallback query times do not measure production hook latency.', '',
                  '| Mode | query p50 / p95 ms | answerable under 10 sessions | empty (all500) | pack characters p50 / p95 | pack estimated tokens p50 / p95 |', '|---|---:|---:|---:|---:|---:|']
        for mode, report in lme.items():
            g = report['all']
            fields = []
            for key in ['characters','estimated_tokens']:
                d = g[key]
                fields.append(f"{d['p50']:.0f} / {d['p95']:.0f}" if d['n'] else '—')
            lines.append(f"| {mode} | {g['latency_ms']['p50']:.1f} / {g['latency_ms']['p95']:.1f} | {g['answerable_under_10']}/470 | {g['empty']} | "+' | '.join(fields)+' |')
        lines += ['', '| Question type | answerable n | '+' | '.join(mode+' all@5' for mode in lme)+' |', '|---|---:|'+ '|'.join(['---:']*len(lme))+'|']
        for typ, group in next(iter(lme.values())).items():
            if typ == 'all':
                continue
            values = [f"{r[typ]['quality']['recall_all_at_5']:.2%}" for r in lme.values()]
            lines.append(f"| {typ} | {group['answerable']} | "+' | '.join(values)+' |')
    write(args.root / 'results' / (args.date + '-baseline-summary.json'), dict(hooks=summary,lme=lme))
    report = args.root / 'results' / (args.date + '-baseline-report.md')
    report.write_text('\n'.join(lines)+'\n')
    os.chmod(report,0o600)
    print('\n'.join(lines))


if __name__ == '__main__':
    main()
