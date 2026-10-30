"""Reproduce background embedding contention with synthetic, public inputs.

Compare an old and patched native endpoint. Foreground deadline remains 1s.
All attempts (including timeouts) are retained. Latency of a timed-out request
is censored at the deadline, not a completed-query latency.
"""
import argparse
import concurrent.futures
import hashlib
import json
import math
from pathlib import Path
import time
import urllib.request

LONG = '数据库连接池耗尽导致请求排队，修复认证超时需要限制并发并及时归还连接。' * 14
QUERY = '认证超时'


def embed(url, texts, timeout, priority='query'):
    raw = json.dumps({'input': texts}, ensure_ascii=False).encode()
    req = urllib.request.Request(url, data=raw, headers={'Content-Type':'application/json', 'X-Embedding-Priority':priority})
    start = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            body = json.load(response)
        return dict(ok=True, ms=(time.perf_counter()-start)*1000, body=body)
    except Exception as exc:
        return dict(ok=False, ms=(time.perf_counter()-start)*1000, error=str(exc))


def percentile(values, p):
    xs = sorted(values)
    x = (len(xs)-1)*p
    a,b = math.floor(x),math.ceil(x)
    return xs[a]+(xs[b]-xs[a])*(x-a)


def slim(result):
    return {k:v for k,v in result.items() if k!='body'}


def profile(url, rounds):
    endpoint = url.removesuffix('/v1/embeddings')
    with urllib.request.urlopen(endpoint+'/info', timeout=10) as response:
        info = json.load(response)
    warm = embed(url,[QUERY],20)
    if not warm['ok']:
        raise RuntimeError(warm)
    attempts = []
    batches = []
    for repetition in range(rounds):
        with concurrent.futures.ThreadPoolExecutor(2) as pool:
            bg = pool.submit(embed,url,[LONG]*8,60,'background')
            time.sleep(.12)
            for i in range(8):
                row = slim(embed(url,[QUERY],1))
                row.update(repetition=repetition,index=i)
                attempts.append(row)
            batch = slim(bg.result())
            if not batch['ok']:
                raise RuntimeError(batch)
            batches.append(batch)
        print(f'{url}: completed contention round {repetition+1}/{rounds}',flush=True)
    return dict(url=url,info=info,foreground_deadline_ms=1000,attempts=attempts,batches=batches,
                foreground_timeouts=sum(not r['ok'] for r in attempts),foreground_attempts=len(attempts),
                attempted_latency_ms={f'p{p}':percentile([r['ms'] for r in attempts],p/100) for p in (50,95)})


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--before',required=True)
    p.add_argument('--after',required=True)
    p.add_argument('--out',required=True,type=Path)
    p.add_argument('--rounds',type=int,default=3)
    args=p.parse_args()
    # Unique, mixed-length inputs also validate response ordering and numerics.
    texts=['authentication timeout','证书过期','用户 '+LONG,'JWT refresh token',LONG,'Unicode emoji 截断','singleflight', 'lock order deadlock']
    a=embed(args.before,texts,60,'background')
    b=embed(args.after,texts,60,'background')
    if not a['ok'] or not b['ok']:raise RuntimeError((slim(a),slim(b)))
    cos=[]
    single_cos=[]
    old_batch_vs_single=[]
    new_batch_vs_single=[]
    cosine=lambda vx,vy:sum(u*v for u,v in zip(vx,vy))/math.sqrt(sum(u*u for u in vx)*sum(v*v for v in vy))
    for i,(x,y) in enumerate(zip(a['body']['data'],b['body']['data'])):
        assert x['index']==y['index']==i
        vx,vy=x['embedding'],y['embedding']
        assert len(vx)==len(vy)==1024
        cos.append(cosine(vx,vy))
        old_single=embed(args.before,[texts[i]],30)['body']['data'][0]['embedding']
        new_single=embed(args.after,[texts[i]],30)['body']['data'][0]['embedding']
        single_cos.append(cosine(old_single,new_single))
        old_batch_vs_single.append(cosine(vx,old_single))
        new_batch_vs_single.append(cosine(vy,new_single))
        assert old_single==new_single, 'single-input inference changed'
        assert vy==new_single, 'new batch ordering or single-input semantics changed' 
    assert len(cos)==len(texts)
    assert all(abs(x-y)<1e-12 for x,y in zip(cos,old_batch_vs_single)), "batch difference not explained by existing single/batch discrepancy"
    result=dict(stimulus_sha256=hashlib.sha256((LONG+'\n'+QUERY).encode()).hexdigest(),vector_cosines=cos,single_input_cosines=single_cos,old_batch_vs_single=old_batch_vs_single,new_batch_vs_single=new_batch_vs_single,
                before=profile(args.before,args.rounds),after=profile(args.after,args.rounds),
                limits=['synthetic one background batch and sequential foreground queries','timeouts retained and latency censored at 1s','same host, 12 inference threads; not an unlimited-load guarantee'])
    args.out.write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
    for side in ['before','after']:
        row=result[side]
        print(side,dict(timeouts=row['foreground_timeouts'],attempts=row['foreground_attempts'],latency_ms=row['attempted_latency_ms']))
    if result['after']['foreground_timeouts']:
        raise SystemExit('patched endpoint still timed out; inspect receipts')


if __name__=='__main__':main()
