#!/usr/bin/env python3
"""Fixed multilingual reference experiment. No labels, tuning or model API calls."""
import argparse, hashlib, json, math, os, resource, time
from pathlib import Path

REVISION = '1427fd652930e4ba29e8149678df786c240d8825'
WEIGHT_SHA = '5daeca2481a76b5976a2bdc32f0a78532b6716da4f8cd3ff59460ef8d2f359b4'

def sha(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for b in iter(lambda: f.read(1024*1024), b''): h.update(b)
    return h.hexdigest()

def bound(ids, n):
    if len(ids) <= n: return ids
    return ids[:(n+1)//2] + ids[-(n//2):] if n > 1 else ids[:n]

def allocate(lengths, anchor, budget):
    # Anchor keeps at least half the document budget. Fill unused space fairly.
    out = [0]*len(lengths)
    out[anchor] = min(lengths[anchor], (budget+1)//2)
    rest = budget-out[anchor]
    while rest and any(a < b for a,b in zip(out,lengths)):
        neighbors = [i for i in range(len(lengths)) if i != anchor and out[i] < lengths[i]]
        for i in (neighbors or [anchor]):
            if rest and out[i] < lengths[i]: out[i] += 1; rest -= 1
    return out

def main():
    ap=argparse.ArgumentParser();ap.add_argument('--cases',required=True);ap.add_argument('--out',required=True);ap.add_argument('--model',required=True)
    args=ap.parse_args(); dest=Path(args.out);dest.mkdir(mode=0o700,parents=True,exist_ok=False)
    modelpath=Path(args.model)
    if sha(modelpath/'model.safetensors') != WEIGHT_SHA: raise ValueError('wrong model weights')
    import torch, transformers
    from transformers import AutoTokenizer, AutoModelForSequenceClassification
    torch.set_num_threads(6);torch.set_num_interop_threads(1)
    tok=AutoTokenizer.from_pretrained(modelpath,local_files_only=True)
    started=time.perf_counter()
    model=AutoModelForSequenceClassification.from_pretrained(modelpath,local_files_only=True,attn_implementation='eager').float().eval()
    load_ms=1000*(time.perf_counter()-started)
    if model.config.num_labels != 1: raise ValueError('expected a single relevance logit')
    cases=[json.loads(s) for s in Path(args.cases).read_text().splitlines()]
    def pair(q,c,window):
        query=tok.encode(q,add_special_tokens=False); qtrunc=len(query)>128;query=bound(query,128)
        budget=512-len(query)-tok.num_special_tokens_to_add(pair=True)
        anchor=next(i for i,r in enumerate(c['window']) if r['hit']['doc_id']==c['hit']['doc_id'])
        records=c['window'] if window else [c['window'][anchor]]
        anchor=anchor if window else 0
        parts=[]
        for r in records:
            body=(c['chunk'] or r['body']) if r['hit']['doc_id']==c['hit']['doc_id'] else r['body']
            text=f"[{r['hit']['kind']}] {r['hit']['occurred_at']}\n{body}\n"
            parts.append(tok.encode(text,add_special_tokens=False))
        sizes=allocate([len(x) for x in parts],anchor,budget)
        doc=sum((bound(x,n) for x,n in zip(parts,sizes)),[])
        ids=tok.build_inputs_with_special_tokens(query,doc)
        assert len(ids)<=512
        return {'input_ids':ids,'attention_mask':[1]*len(ids)}, {'input_sha256':hashlib.sha256(json.dumps(ids,separators=(',',':')).encode()).hexdigest(),'tokens':len(ids),'query_truncated':qtrunc,'document_truncated':any(n<len(x) for n,x in zip(sizes,parts))}
    def infer(inputs):
        result=[]
        with torch.inference_mode():
            for i in range(0,len(inputs),4):
                batch=tok.pad(inputs[i:i+4],padding=True,return_tensors='pt')
                result.extend(model(**batch).logits[:,0].tolist())
        if not all(math.isfinite(x) for x in result): raise ValueError('nonfinite model score')
        return result
    warm=next(c for c in cases if c['candidates']); inp,_=pair(warm['query'],warm['candidates'][0],True)
    infer([inp]*4);infer([inp]*4)
    with (dest/'scores.jsonl').open('x') as out:
        for i,c in enumerate(cases):
            row={'id':c['id']}
            for name,window in [('single',False),('window',True)]:
                at=time.perf_counter(); pairs=[pair(c['query'],v,window) for v in c['candidates']]
                row[name]=infer([p[0] for p in pairs]);row[name+'_ms']=1000*(time.perf_counter()-at)
                row[name+'_inputs']=[p[1] for p in pairs]
            out.write(json.dumps(row)+'\n');out.flush()
            if (i+1)%10==0 or i+1==len(cases): print(f'scored {i+1}/{len(cases)}',flush=True)
    manifest={'cases_sha256':sha(args.cases),'model':'cross-encoder/mmarco-mMiniLMv2-L12-H384-v1','revision':REVISION,'weights_sha256':WEIGHT_SHA,'files_sha256':{p.name:sha(p) for p in modelpath.iterdir() if p.is_file()},'torch':torch.__version__,'transformers':transformers.__version__,'threads':6,'interop_threads':1,'batch_size':4,'max_length':512,'query_max_tokens':128,'dtype':'float32','attention':'eager','warmup_batches':2,'load_ms':load_ms,'peak_rss_kib':resource.getrusage(resource.RUSAGE_SELF).ru_maxrss,'gate':'raw relevance logit > 0; fixed binary reference, NOT calibrated answerability','scores_sha256':sha(dest/'scores.jsonl'),'source_sha256':sha(__file__),'latency_scope':'warm per-question CPU tokenizer + inference; excludes retrieval/IPC/render; no request score cache'}
    (dest/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
if __name__=='__main__':main()
