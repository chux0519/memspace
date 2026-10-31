#!/usr/bin/env python3
"""Apply a small reviewable adapter overlay to a pinned, disposable checkout."""
import argparse,hashlib,json,shutil,subprocess
from pathlib import Path

HERE=Path(__file__).resolve().parent
sha=lambda b:hashlib.sha256(b).hexdigest()
def install(checkout):
    root=Path(checkout).resolve();pin=json.loads((HERE/'upstream.json').read_text());marker=root/'.memspace-overlay.json'
    old=json.loads(marker.read_text()) if marker.exists() else {'files':{}}
    mutations={}
    for name,expected in pin['files'].items():
        raw=(root/name).read_bytes()
        if sha(raw)==old['files'].get(name):continue
        if sha(raw)!=expected:raise ValueError('upstream file changed: '+name)
        text=raw.decode()
        if name.endswith('registry.py'):
            text=text.replace('\nSUPPORTED_LIBS =', '\n_LIB_CLIENT_REGISTRY["memspace"] = ("memspace_client", "MemSpaceClient")\n\nSUPPORTED_LIBS =',1)
        elif name.endswith('ingest_helpers.py'):
            for token in ['_CHAT_TIME_LIBS = frozenset({','_SESSION_KEY_LIBS = frozenset({']:
                if token not in text:raise ValueError('missing dispatch anchor')
                text=text.replace(token,token+'\n    "memspace",',1)
        elif name=='docs/user_memory/README.md':
            text+='\n## MemSpace local adapter\n\nMemSpace uses a local Go worker and private SQLite stores. See [MemSpace](memspace.md) for setup, modes and limitations. Initial validation targets LongMemEval; other benchmarks require separate validation.\n'
        elif name=='docs/user_memory/README_zh.md':
            text+='\n## MemSpace 本地适配器\n\nMemSpace 使用独立 Go worker 和私有 SQLite 评测库。构建、模式和边界见 [MemSpace](memspace_zh.md)。首轮验证 LongMemEval，其他题集需单独验收。\n'
        elif name=='env_examples/README.md':
            text+='\n## MemSpace\n\nUse [`.env.memspace`](.env.memspace) for the isolated local worker adapter. See [setup](../docs/user_memory/memspace.md). Answer/judge credentials remain separate from the embedding service.\n'
        else:
            anchor='"lcmx": generic_text_search,'
            if text.count(anchor)!=1:raise ValueError('missing search dispatch anchor')
            text=text.replace(anchor,anchor+'\n'+('        ' if 'locomo/' in name else '    ')+'"memspace": generic_text_search,')
        mutations[name]=text.encode()
    extras={'scripts/client_factory/memspace_client.py':(HERE/'memspace_client.py').read_bytes(),'env_examples/.env.memspace':(HERE/'env.example').read_bytes(),'docs/user_memory/memspace.md':(HERE/'README.md').read_bytes(),'docs/user_memory/memspace_zh.md':(HERE/'README_zh.md').read_bytes()}
    for name,raw in extras.items():
        p=root/name
        if p.exists() and p.read_bytes()!=raw and sha(p.read_bytes())!=old['files'].get(name):raise ValueError('refusing to overwrite existing '+name)
        mutations[name]=raw
    # Validate all files before mutating any. No network, install or live API calls.
    hashes=dict(old['files'])
    for name,raw in mutations.items():
        p=root/name;p.parent.mkdir(parents=True,exist_ok=True);p.write_bytes(raw);hashes[name]=sha(raw)
    marker.write_text(json.dumps({'upstream':pin['revision'],'files':hashes},indent=2)+'\n')
    return hashes
if __name__=='__main__':
    ap=argparse.ArgumentParser();ap.add_argument('--checkout',required=True);args=ap.parse_args();print(json.dumps(install(args.checkout),indent=2))
