"""Finish LongMemEval baseline after an existing embedding job, with resumable artifacts.

Does not start a second embed job for the same cache. Only its own loopback cache
server is terminated, after exact pgrep and /proc command-line verification.
"""
import argparse
from datetime import datetime
import json
import os
from pathlib import Path
import signal
import socket
import sqlite3
import subprocess
import time
import urllib.request


def log(message):
    print(datetime.now().isoformat(timespec='seconds'), message, flush=True)


def exact_processes(names):
    found = {}
    for name in names:
        result = subprocess.run(['pgrep', '-x', name], capture_output=True, text=True)
        if result.returncode not in (0, 1):
            raise RuntimeError(result.stderr)
        for pid in result.stdout.split():
            try:
                raw = Path('/proc', pid, 'cmdline').read_bytes()
                argv = [os.fsdecode(p) for p in raw.split(b'\0') if p]
                found[int(pid)] = argv
            except FileNotFoundError:
                pass
    return found


def active_embed_jobs(root, binary):
    matches = {}
    for pid, argv in exact_processes(['lme', 'lme-next', 'lme-final', binary.name]).items():
        if len(argv) < 2 or argv[1] != 'embed' or '-cache' not in argv:
            continue
        try:
            value = Path(argv[argv.index('-cache')+1])
            if not value.is_absolute():
                value = Path(os.readlink(f'/proc/{pid}/cwd')) / value
            if value.resolve() == (root / 'vectors.db').resolve():
                matches[pid] = argv
        except (FileNotFoundError, IndexError):
            pass
    return matches


def stop_owned(process, argv):
    if process.poll() is not None:
        return
    found = exact_processes([Path(argv[0]).name])
    if found.get(process.pid) != argv:
        raise RuntimeError('refusing to signal process with unverified command line')
    log(f'verified owned cache server PID {process.pid}: {argv}; sending TERM')
    os.kill(process.pid, signal.SIGTERM)
    process.wait(timeout=30)


def request(url):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(url, timeout=5) as response:
        return json.load(response)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', required=True, type=Path)
    parser.add_argument('--binary', required=True, type=Path)
    parser.add_argument('--repo', required=True, type=Path)
    parser.add_argument('--date', default='2026-10-31')
    parser.add_argument('--fallback', default='http://192.168.1.101:8791')
    parser.add_argument('--cache-port', type=int, default=18792)
    args = parser.parse_args()
    root, binary, repo = args.root.resolve(), args.binary.resolve(), args.repo.resolve()
    env = dict(os.environ, NO_PROXY='*')
    data, db = root/'data/longmemeval_s_cleaned.json', root/'lme-isolated'
    results = root/'results'/(args.date+'-lme')
    fts = results/'fts'
    # The operator may already have started the full FTS ingestion and run.
    while not (fts/'summary.json').exists():
        workers = {pid:argv for pid,argv in exact_processes(['lme-next','lme-final',binary.name]).items()
                   if len(argv)>1 and argv[1]=='run' and '-mode' in argv and argv[argv.index('-mode')+1]=='fts' and str(db) in argv}
        if not workers:
            raise RuntimeError('FTS is incomplete and has no running worker; inspect its log and resume explicitly')
        log(f'waiting for existing FTS worker {list(workers)}')
        time.sleep(60)
    summary = json.loads((fts/'summary.json').read_text())['all']
    if summary['attempted']!=500 or summary['failed']:
        raise RuntimeError(f'FTS run failed coverage checks: {summary}')
    log('FTS completed all500; waiting for original embedding precomputation')
    while True:
        jobs = active_embed_jobs(root,binary)
        if not jobs:
            break
        with sqlite3.connect('file:'+str(root/'vectors.db')+'?mode=ro',uri=True,timeout=10) as cache:
            count=cache.execute('SELECT count(*) FROM vectors').fetchone()[0]
        log(f'precompute PIDs {list(jobs)}; cached vectors={count}')
        time.sleep(60)
    # This fills query vectors (omitted by the original job) and any missing chunks.
    with open(root/'embed-final.log','a') as output:
        log('original embed exited; resume once with query-vector support')
        subprocess.run([str(binary),'embed','-data',str(data),'-cache',str(root/'vectors.db'),
                        '-url',args.fallback+'/v1/embeddings'],cwd=repo,env=env,stdout=output,stderr=subprocess.STDOUT,check=True)
    with socket.socket() as probe:
        if probe.connect_ex(('127.0.0.1',args.cache_port))==0:
            raise RuntimeError('cache port already occupied; refusing to adopt or stop an unknown service')
    argv=[str(binary),'serve','-cache',str(root/'vectors.db'),'-port',str(args.cache_port),'-fallback',args.fallback]
    server_log=open(root/'cache-server.log','a')
    cache_server=subprocess.Popen(argv,cwd=repo,env=env,stdout=server_log,stderr=subprocess.STDOUT)
    try:
        base=f'http://127.0.0.1:{args.cache_port}'
        for _ in range(60):
            if cache_server.poll() is not None:
                raise RuntimeError('cache server exited; inspect cache-server.log')
            try:
                request(base+'/stats')
                break
            except (OSError,ValueError):
                time.sleep(1)
        else:
            raise RuntimeError('cache server not ready')
        for mode in ['vector','hybrid','pack','hybrid-no-floor']:
            target=results/mode
            if (target/'summary.json').exists():
                report=json.loads((target/'summary.json').read_text())['all']
                if report['attempted']==500 and report['failed']==0:
                    log(f'{mode}: already complete')
                    continue
                raise RuntimeError(f'{mode}: existing incomplete/failed run needs review')
            if target.exists():
                preserved=target.with_name(target.name+'-incomplete-'+datetime.now().strftime('%H%M%S'))
                target.rename(preserved)
                log(f'preserved incomplete artifact directory {preserved}')
            before=request(base+'/stats')
            command=[str(binary),'run','-data',str(data),'-db',str(db),'-out',str(target),
                     '-isolate-projects','-embed-url',base+'/v1/embeddings','-mode','hybrid' if mode=='hybrid-no-floor' else mode]
            if mode=='hybrid-no-floor':
                command+=['-no-floor']
            log(f'starting {mode}')
            with open(root/('lme-'+mode+'.log'),'a') as output:
                subprocess.run(command,cwd=repo,env=env,stdout=output,stderr=subprocess.STDOUT,check=True)
            after=request(base+'/stats')
            (target/'cache-stats.json').write_text(json.dumps(dict(before=before,after=after),indent=2)+'\n')
            os.chmod(target/'cache-stats.json',0o600)
            report=json.loads((target/'summary.json').read_text())['all']
            if report['attempted']!=500 or report['answerable']!=470 or report['failed']:
                raise RuntimeError(f'{mode}: coverage or execution errors: {report}')
            log(f'{mode}: {report["strict_successes"]}/470 all@5; misses delta={after["misses"]-before["misses"]}')
        subprocess.run(['python3',str(repo/'scripts/bench/report_baselines.py'),'--root',str(root),'--date',args.date],cwd=repo,check=True)
        (root/'results'/(args.date+'-execution-complete.json')).write_text(json.dumps(dict(
            finished_at=datetime.now().isoformat(),modes=['fts','vector','hybrid','pack','hybrid-no-floor']),indent=2)+'\n')
        log('all baseline modes completed; final source/receipt audit and notes update remain')
    finally:
        stop_owned(cache_server,argv)
        server_log.close()


if __name__=='__main__':
    main()
