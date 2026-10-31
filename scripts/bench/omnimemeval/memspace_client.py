"""OmniMemEval adapter: isolated local MemSpace workers; no inference SDK required."""
from __future__ import annotations
import atexit
import fcntl
import hashlib
import json
import os
from pathlib import Path
import selectors
import shutil
import subprocess
import threading
import time
import uuid

_REGISTRY = {}
_GUARD = threading.RLock()
_USER_LOCKS = {}

def _hash(text): return hashlib.sha256(text.encode('utf-8')).hexdigest()
def _canonical(path):
    p = Path(path).expanduser().absolute()
    if p.resolve() != p: raise ValueError('benchmark path must not contain symlinks')
    return p

def _owned_root(path):
    root = _canonical(path)
    protected=[Path.home()/'.local/share/memspace',Path.home()/'.config/memspace',Path.home()/'.claude',Path.home()/'.codex']
    if any(root==p or p in root.parents or root in p.parents for p in protected):
        raise ValueError('refusing production, agent or ancestor directory')
    root.parent.mkdir(parents=True,exist_ok=True)
    with (root.parent/('.omni-root-'+_hash(str(root))+'.lock')).open('a+b') as lock_file:
        fcntl.flock(lock_file,fcntl.LOCK_EX)
        return _initialize_root(root)

def _initialize_root(root):
    root.mkdir(parents=True,exist_ok=True,mode=0o700)
    marker = root/'.memspace-omnimemeval.json'
    # Only our marker may establish ownership; arbitrary preexisting data is rejected.
    if not marker.exists():
        if any(root.iterdir()): raise ValueError('refusing unowned nonempty evaluation root')
        with marker.open('x') as f: json.dump({'format':1,'run_id':str(uuid.uuid4())},f)
        marker.chmod(0o600)
    data = json.loads(marker.read_text())
    if data.get('format') != 1 or not isinstance(data.get('run_id'),str): raise ValueError('invalid root marker')
    return root

def _user_key(user_id):
    if not isinstance(user_id,str) or not user_id or len(user_id)>512: raise ValueError('invalid user_id')
    return _hash(user_id)

def _lock(key):
    with _GUARD: return _USER_LOCKS.setdefault(key,threading.RLock())

class _Worker:
    def __init__(self,client,key):
        self.client,self.key=client,key
        self.directory=client.root/key
        self.lock_file=(client.root/(key+'.lock')).open('a+b')
        try: fcntl.flock(self.lock_file,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError:
            self.lock_file.close();raise RuntimeError('evaluation user already has a worker in another process')
        self.stderr=(client.root/(key+'.stderr')).open('ab')
        cmd=[str(client.binary),'--dir',str(self.directory),'--user-key',key,'--mode',client.mode,'--model',client.model]
        if client.evidence_mode!='off':cmd += ['--evidence-mode',client.evidence_mode,'--evidence-max-related',str(client.evidence_limit)]
        if client.embedding:cmd += ['--embedding-url',client.embedding]
        try:self.proc=subprocess.Popen(cmd,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=self.stderr,bufsize=0)
        except BaseException:self.stderr.close();self.lock_file.close();raise
        self.buffer=b''
    def rpc(self,op,**fields):
        request={'id':str(uuid.uuid4()),'op':op,**fields};start=time.monotonic()
        if self.proc.poll() is not None: raise RuntimeError('worker exited; retry with the same session/batch IDs')
        payload=(json.dumps(request,ensure_ascii=False)+'\n').encode()
        # FileIO writes may be short for large session payloads.
        view=memoryview(payload)
        while view:
            n=self.proc.stdin.write(view)
            if not n:raise RuntimeError('worker input closed')
            view=view[n:]
        with selectors.DefaultSelector() as sel:
            sel.register(self.proc.stdout,selectors.EVENT_READ)
            while b'\n' not in self.buffer:
                remaining=self.client.timeout-(time.monotonic()-start)
                if remaining<=0 or not sel.select(remaining):
                    self.stop();raise TimeoutError('MemSpace worker timeout; request outcome unknown, retry the same stable IDs')
                chunk=os.read(self.proc.stdout.fileno(),65536)
                if not chunk:raise RuntimeError('worker exited before response; see private stderr receipt')
                self.buffer+=chunk
        line,self.buffer=self.buffer.split(b'\n',1);response=json.loads(line)
        if response.get('id')!=request['id']:raise RuntimeError('worker response identity mismatch')
        # Private replayable receipt; no credentials, no gold or expected answer.
        record={'request':request,'response':response,'client_request_ms':1000*(time.monotonic()-start)}
        with (self.client.root/(self.key+'.receipts.jsonl')).open('a') as f:f.write(json.dumps(record,ensure_ascii=False)+'\n')
        if response.get('error'):raise RuntimeError(response['error'])
        return response['data']
    def stop(self):
        if self.proc.poll() is None:
            self.proc.stdin.close()
            try:self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.proc.terminate()
                try:self.proc.wait(timeout=5)
                except subprocess.TimeoutExpired:self.proc.kill();self.proc.wait(timeout=5)
        for f in [self.proc.stdout,self.stderr,self.lock_file]:
            if f and not f.closed:f.close()

class MemSpaceClient:
    """add/search/delete contract. Each eval user owns a worker and SQLite DB.

    MEMSPACE_WORKER: compiled worker executable (required)
    MEMSPACE_DB_ROOT: new private evaluation root (required)
    MEMSPACE_EMBEDDING_URL: empty explicitly selects keyword-only
    MEMSPACE_EMBED_MODEL: default BAAI/bge-m3
    MEMSPACE_MODE: context_pack (default) or separate search diagnostic
    MEMSPACE_RPC_TIMEOUT: seconds, default 900 (ingestion included)
    """
    def __init__(self):
        self.binary=_canonical(os.environ['MEMSPACE_WORKER'])
        if not self.binary.is_file() or not os.access(self.binary,os.X_OK):raise ValueError('MEMSPACE_WORKER must be executable')
        with _GUARD:self.root=_owned_root(os.environ['MEMSPACE_DB_ROOT'])
        self.embedding=os.environ.get('MEMSPACE_EMBEDDING_URL','').strip()
        self.model=os.environ.get('MEMSPACE_EMBED_MODEL','BAAI/bge-m3')
        self.mode=os.environ.get('MEMSPACE_MODE','context_pack')
        self.evidence_mode=os.environ.get('MEMSPACE_EVIDENCE_MODE','off')
        self.evidence_limit=int(os.environ.get('MEMSPACE_EVIDENCE_MAX_RELATED','24'))
        if not 1<=self.evidence_limit<=512:raise ValueError('evidence relation work bound must be 1..512')
        if self.evidence_mode not in ['off','shadow','on']:raise ValueError('invalid evidence mode')
        if self.mode not in ['context_pack','search']:raise ValueError('invalid MEMSPACE_MODE')
        self.timeout=float(os.environ.get('MEMSPACE_RPC_TIMEOUT','900'))
        if not 0<self.timeout<=86400:raise ValueError('invalid RPC timeout')
        self.config=(str(self.binary),self.embedding,self.model,self.mode,self.evidence_mode,self.evidence_limit)
    def _worker(self,key):
        token=(str(self.root),key)
        with _GUARD:
            worker=_REGISTRY.get(token)
            if worker and worker.client.config!=self.config:raise ValueError('evaluation worker configuration changed')
            if worker and worker.proc.poll() is not None:worker.stop();del _REGISTRY[token];worker=None
            if worker is None:worker=_Worker(self,key);_REGISTRY[token]=worker
            return worker
    def add(self,messages,user_id,*args,**kwargs):
        if not messages:return None
        session=kwargs.get('session_key') or kwargs.get('session_id') or kwargs.get('conv_id') or (args[0] if args else None)
        if not isinstance(session,str) or not session:raise ValueError('stable session_key required; register session dispatch in OmniMemEval')
        batch=kwargs.get('batch_key') or kwargs.get('request_id') or 'complete-session'
        rows=[]
        for message in messages:
            if not isinstance(message.get('content'),str):raise ValueError('content must be a string')
            stamp=message.get('chat_time') or kwargs.get('timestamp')
            if not isinstance(stamp,str):raise ValueError('chat_time/timestamp must be RFC3339; missing time must not become now')
            rows.append({'role':message['role'],'content':message['content'],'chat_time':stamp,'id':str(message.get('id') or '')})
        key=_user_key(user_id)
        with _lock((str(self.root),key)):
            worker=self._worker(key);result=worker.rpc('add',session_key=session,batch_key=str(batch),messages=rows)
            worker.rpc('ready')
            return result
    def search(self,query,user_id,top_k,**kwargs):
        if not isinstance(top_k,int) or isinstance(top_k,bool) or top_k<=0:raise ValueError('top_k must be positive integer')
        key=_user_key(user_id)
        with _lock((str(self.root),key)):
            return self._worker(key).rpc('search',query=query,top_k=top_k)['texts']
    def delete(self,user_id):
        key=_user_key(user_id);token=(str(self.root),key)
        with _lock(token):
            with _GUARD:worker=_REGISTRY.pop(token,None)
            if worker:worker.stop()
            directory=self.root/key
            if not directory.exists():return None
            if directory.is_symlink() or directory.resolve().parent!=self.root:raise ValueError('unsafe cleanup path')
            marker=directory/'worker.json'
            if not marker.is_file() or json.loads(marker.read_text()).get('user_key')!=key:raise ValueError('refusing cleanup of unowned directory')
            # Prevent another process from deleting an active user's store.
            with (self.root/(key+'.lock')).open('a+b') as f:
                try:fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)
                except BlockingIOError:raise RuntimeError('evaluation worker still active in another process')
                shutil.rmtree(directory)
    def close(self):
        with _GUARD:
            keys=[k for k,w in _REGISTRY.items() if w.client is self]
            for key in keys:_REGISTRY.pop(key).stop()

def _shutdown():
    for worker in list(_REGISTRY.values()):worker.stop()
    _REGISTRY.clear()
atexit.register(_shutdown)
