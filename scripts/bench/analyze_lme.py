"""Locate losses in completed private LME receipts; never change retrieval."""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import sqlite3


def rows(path):
    with gzip.open(path, 'rt') as stream:
        result = [json.loads(line) for line in stream]
    assert len(result) == 500
    mapped = {r['question_id']: r for r in result}
    assert len(mapped) == 500
    return mapped


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', required=True, type=Path)
    parser.add_argument('--date', default='2026-10-31')
    args = parser.parse_args()
    root = args.root
    output, scores, evidence = {}, {}, {}
    for mode in ['fts', 'vector', 'hybrid', 'pack', 'hybrid-no-floor']:
        part = root/'results'/(args.date+'-lme')/mode
        scores[mode] = rows(part/'scores.ndjson.gz')
        evidence[mode] = rows(part/'evidence.ndjson.gz')
        assert all(not r.get('error') for r in scores[mode].values())
        positive = [r for r in scores[mode].values() if r['answerable']]
        assert len(positive) == 470
        output[mode] = dict(
            answerable_under_10=sum(r['under_10'] for r in positive),
            answerable_empty=sum(r['empty'] for r in positive),
            below_floor_docs=sum(r.get('below_search_floor', 0) for r in positive),
        )
        if mode == 'pack':
            continue
        candidate_all = 0
        all_candidate_not_top5 = []
        attainable_candidate_not_top5 = []
        missed_even_in_100_docs = []
        candidate_sessions = []
        unreachable_at5 = []
        for r in positive:
            qid = r['question_id']
            ev = evidence[mode][qid]
            key = hashlib.sha256(qid.encode()).hexdigest()[:24]
            index = root/'lme-isolated-projects'/(key+'.db')
            state = json.loads(index.with_suffix('.db.state.json').read_text())
            assert list(state['projects']) == [qid]
            ids = ev['doc_ids'] or []
            found = set()
            ordered = []
            with sqlite3.connect('file:'+str(index)+'?mode=ro', uri=True) as db:
                for doc in ids:
                    sid, project = db.execute('SELECT session_id,project_id FROM search_docs WHERE doc_id=?', (doc,)).fetchone()
                    assert project == state['projects'][qid]
                    source = state['sessions'][sid]
                    if source not in found:
                        ordered.append(source)
                    found.add(source)
            assert ordered[:10] == (ev['returned_sessions'] or []), 'source rank mapping changed'
            if len(ev['gold_sessions']) > 5:
                unreachable_at5.append(qid)
            candidate_sessions.append(len(found))
            complete = set(ev['gold_sessions']).issubset(found)
            candidate_all += complete
            if complete and not r['recall_all_at_5']:
                all_candidate_not_top5.append(qid)
                if len(ev['gold_sessions']) <= 5:
                    attainable_candidate_not_top5.append(qid)
            if not complete:
                missed_even_in_100_docs.append(qid)
        output[mode].update(
            all_sources_in_returned_100_docs=candidate_all,
            candidate_coverage=candidate_all/470,
            all_candidate_but_not_top5=all_candidate_not_top5,
            attainable_candidate_but_not_top5=attainable_candidate_not_top5,
            absent_from_returned_100_docs=missed_even_in_100_docs,
            candidate_distinct_sessions_min=min(candidate_sessions),
            candidate_distinct_sessions_max=max(candidate_sessions),
            unreachable_at5_gold_gt5=unreachable_at5,
        )
    pairs = {}
    for before, after in [('fts', 'hybrid'), ('vector', 'hybrid'), ('hybrid', 'hybrid-no-floor'), ('hybrid', 'pack')]:
        gain, loss = [], []
        for qid, row in scores[before].items():
            if not row['answerable']:
                continue
            a = bool(row['recall_all_at_5'])
            b = bool(scores[after][qid]['recall_all_at_5'])
            if b and not a:
                gain.append(qid)
            if a and not b:
                loss.append(qid)
        pairs[before+' -> '+after] = dict(gain=gain, loss=loss, net_questions=len(gain)-len(loss))
    result = dict(modes=output, strict_at5_pairs=pairs, limits=[
        'Candidate coverage means returned docs only; it is not pre-fusion candidate recall.',
        'Hybrid100 vs pack is a diagnostic comparison, not an instrumented identical candidate list.',
        'Pack sessions do not prove answer-bearing text survived 600-character windows.',
    ])
    path = root/'results'/(args.date+'-lme-diagnostics.json')
    path.write_text(json.dumps(result, indent=2)+'\n')
    os.chmod(path, 0o600)
    print(json.dumps({mode: {k:v for k,v in value.items() if not isinstance(v,list)} for mode,value in output.items()}, indent=2))
    print(json.dumps({key: {k:len(v) if isinstance(v,list) else v for k,v in value.items()} for key,value in pairs.items()}, indent=2))


if __name__ == '__main__':
    main()
