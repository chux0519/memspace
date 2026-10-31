#!/usr/bin/env python3
"""Score private mechanism receipts AFTER selection; gold never enters selectors."""
import argparse, collections, hashlib, json, math, re
from pathlib import Path

MODES=['baseline','judge_single','dialogue','judge_dialogue','rank_single','rank_dialogue','ungated_original','ungated_dialogue']
def lines(p):return [json.loads(s) for s in Path(p).read_text().splitlines()]
def canon(s):return ' '.join(re.sub(r'[\[\]\ue000\ue001]','',s).lower().split())
def dist(xs):
    xs=sorted(xs)
    if not xs:return {'n':0}
    def percentile(p):
        x=(len(xs)-1)*p;i=int(x);return xs[i]+(xs[min(i+1,len(xs)-1)]-xs[i])*(x-i)
    return {'n':len(xs),'mean':sum(xs)/len(xs),'p50':percentile(.5),'p95':percentile(.95),'max':max(xs)}
def auc(pos,neg):
    if not pos or not neg:return None
    return sum((a>b)+.5*(a==b) for a in pos for b in neg)/(len(pos)*len(neg))
def evaluate(case,pack,label,kind):
    refs=[i['ref'] for i in pack['items']]; text=pack['text']
    mapping={f"doc:{r['hit']['doc_id']}":r['hit'] for c in case['candidates'] for r in c['window']}
    for it in pack['items']:
        h=mapping[it['key']]
        if h['ref_id']!=it['ref'] or h['project_id']!=case['request']['project']['project_id'] or it['project_id']!=h['project_id']:raise ValueError('provenance mismatch')
    if len(refs)>6 or len(text)>6000:raise ValueError('budget exceeded')
    if kind=='lme':
        sessions=[]
        for it in pack['items']:
            sid=mapping[it['key']]['session_id'];source=label['sessions'][sid]
            if source not in sessions:sessions.append(source)
        gold=set(label['gold'])
        return {'positive':bool(gold),'all':bool(gold) and gold<=set(sessions),'all5':bool(gold) and gold<=set(sessions[:5]),'any':bool(gold&set(sessions)),'fraction':len(gold&set(sessions))/len(gold) if gold else 0,'false':not gold and bool(text),'category':label['type']}
    q=label['question'];docs=label['documents'];groups=q['evidence_groups']
    hits=[any(ref in refs and canon(docs[ref]['evidence']) in canon(text) for ref in group) for group in groups]
    return {'positive':bool(groups),'all':bool(groups) and all(hits),'any':any(hits),'fraction':sum(hits)/len(hits) if hits else 0,'false':not groups and bool(text),'category':q['category']}
def main():
    ap=argparse.ArgumentParser();ap.add_argument('--export',required=True);ap.add_argument('--model',required=True);ap.add_argument('--replay',required=True);ap.add_argument('--kind',choices=['fixture','lme'],required=True);args=ap.parse_args()
    import tiktoken
    enc=tiktoken.get_encoding('cl100k_base')
    root=Path(args.export);cases={c['id']:c for c in lines(root/'cases.jsonl')};raw=json.loads((root/'labels.json').read_text())
    labels=raw if args.kind=='lme' else {q['id']:{'question':q,'documents':raw['documents']} for q in raw['questions']}
    models={r['id']:r for r in lines(Path(args.model)/'scores.jsonl')}
    receipts=lines(Path(args.replay)/'receipts.jsonl');detail=[];by_mode=collections.defaultdict(list);seen=set()
    for r in receipts:
        key=r['id'],r['mode']
        if key in seen:raise ValueError('duplicate receipt')
        seen.add(key);s=evaluate(cases[r['id']],r['pack'],labels[r['id']],args.kind)
        s.update(id=r['id'],mode=r['mode'],tokens=len(enc.encode(r['pack']['text'])),characters=len(r['pack']['text']),fragments=len(r['pack']['items']),replay_ms=r['replay_ms'])
        detail.append(s);by_mode[r['mode']].append(s)
    if len(receipts)!=len(cases)*len(MODES):raise ValueError('incomplete receipts')
    baseline={s['id']:s for s in by_mode['baseline']};summary={}
    for mode,rs in by_mode.items():
        pos=[s for s in rs if s['positive']];neg=[s for s in rs if not s['positive']]
        summary[mode]={'positives':len(pos),'all':sum(s['all'] for s in pos),'all_pct':100*sum(s['all'] for s in pos)/len(pos),'any':sum(s['any'] for s in pos),'negative':len(neg),'false_injection':sum(s['false'] for s in neg),'tokens':dist([s['tokens'] for s in rs]),'positive_tokens':dist([s['tokens'] for s in pos]),'negative_tokens':dist([s['tokens'] for s in neg]),'fragments':dist([s['fragments'] for s in rs]),'replay_ms':dist([s['replay_ms'] for s in rs]),'wins':[s['id'] for s in pos if s['all'] and not baseline[s['id']]['all']],'losses':[s['id'] for s in pos if not s['all'] and baseline[s['id']]['all']]}
        if args.kind=='lme':summary[mode]['all5']=sum(s['all5'] for s in pos)
        summary[mode]['categories']={cat:{'n':len(xs),'positive':sum(x['positive'] for x in xs),'all':sum(x['all'] for x in xs),'false':sum(x['false'] for x in xs)} for cat in sorted(set(s['category'] for s in rs)) if (xs:=[s for s in rs if s['category']==cat])}
    diagnostics={}
    for name in ['single','window']:
        pos=[];neg=[];trunc=total=0
        for id,m in models.items():
            label=labels[id];positive=bool(label['gold']) if args.kind=='lme' else bool(label['question']['evidence_groups'])
            (pos if positive else neg).append(max(m[name],default=-100))
            for p in m[name+'_inputs']:total+=1;trunc+=p['document_truncated']
        diagnostics[name]={'score_ms':dist([m[name+'_ms'] for m in models.values()]),'positive_vs_negative_max_score_auc':auc(pos,neg),'document_truncated_pairs':trunc,'pairs':total}
    out=Path(args.replay);(out/'scored.jsonl').write_text(''.join(json.dumps(r)+'\n' for r in detail))
    report={'kind':args.kind,'questions':len(cases),'summary':summary,'model_diagnostics':diagnostics,'model_manifest':json.loads((Path(args.model)/'manifest.json').read_text()),'token_encoding':'cl100k_base','receipts_sha256':hashlib.sha256((out/'receipts.jsonl').read_bytes()).hexdigest(),'limitations':['Authored fixtures are development diagnostics, not human holdout','LME pilot source coverage is not visible answer completeness','Relevance logit zero is an uncalibrated fixed reference; no threshold tuning','Warm inference timings exclude retrieval, transport, rendering; no live hook claim']}
    (out/'report.json').write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
    for mode in MODES:
        s=summary[mode];print(mode,f"all={s['all']}/{s['positives']}",f"false={s['false_injection']}/{s['negative']}",f"tokens_mean={s['tokens']['mean']:.0f}")
    print('CPU scoring',json.dumps(diagnostics))
if __name__=='__main__':main()
