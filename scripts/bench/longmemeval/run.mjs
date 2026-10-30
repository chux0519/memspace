// Use agentmemory's pinned loader, BM25 adapter and scorer without editing them.
import { createHash } from 'node:crypto';
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { spawn, execFileSync } from 'node:child_process';
import { createInterface } from 'node:readline';

const args = Object.fromEntries(process.argv.slice(2).reduce((pairs, v, i, all) => i % 2 ? pairs : [...pairs, [v.replace(/^--/, ''), all[i + 1]]], []));
for (const name of ['vendor', 'data', 'out']) if (!args[name]) throw new Error(`--${name} is required`);
const vendor = resolve(args.vendor), data = resolve(args.data), out = resolve(args.out);
const sha = p => createHash('sha256').update(readFileSync(p)).digest('hex');
if (sha(data) !== 'd6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442') throw new Error('dataset SHA256 mismatch');
const load = path => import(pathToFileURL(join(vendor, path)).href);
const { loadLongMemEval } = await load('eval/runner/load.ts');
const { runEvaluation } = await load('eval/runner/run.ts');
const { bm25Adapter } = await load('eval/runner/adapters/bm25.ts');
const { grepAdapter } = await load('eval/runner/adapters/grep.ts');
const questions = loadLongMemEval(data, args.limit ? Number(args.limit) : undefined);
const adapters = args.reference === 'none' ? [] : [grepAdapter, bm25Adapter];
let child;
const pending = [];
const call = message => new Promise((resolve, reject) => {
  pending.push({ resolve, reject });
  child.stdin.write(JSON.stringify(message) + '\n');
});
if (args.binary) {
  child = spawn(resolve(args.binary), [], { stdio: ['pipe', 'pipe', 'inherit'] });
  const lines = createInterface({ input: child.stdout });
  lines.on('line', line => {
    const p = pending.shift();
    if (!p) throw new Error('unsolicited worker response');
    try { const r = JSON.parse(line); r.error ? p.reject(new Error(r.error)) : p.resolve(r); }
    catch (err) { p.reject(err); }
  });
  child.on('exit', code => { for (const p of pending.splice(0)) p.reject(new Error(`worker exited ${code}`)); });
  for (const mode of (args.modes ?? 'keyword,bm25').split(',')) {
    adapters.push({
      name: mode==='session' ? 'memspace-session-rrf-porter' : mode==='bm25' ? 'memspace-bm25-exact-porter' : `memspace-${mode}-porter`,
      async init(sessions) {
        await call({ op: 'index', view:mode==='session'?'multi':'whole', sessions: sessions.map(s => ({ id: s.id, content: s.content, timestamp: s.timestamp ?? '' })) });
        return { sessions: new Map(sessions.map(s => [s.id, s])) };
      },
      async query(query, state, k) {
        const result = await call({ op: 'query', query, k, mode });
        return result.ranked.map(r => ({ ...r, content: state.sessions.get(r.sessionId).timestamp
          ? `[Source session date: ${state.sessions.get(r.sessionId).timestamp}]\n${state.sessions.get(r.sessionId).content}`
          : state.sessions.get(r.sessionId).content }));
      },
      async teardown() { await call({ op: 'close' }); },
    });
  }
}
let sourceSnapshot = null;
if (args.binary) {
  const snapshot = out + '.source';
  mkdirSync(snapshot, { recursive: false });
  execFileSync('git', ['archive', '--format=tar.gz', '-o', join(snapshot, 'base.tar.gz'), 'HEAD']);
  writeFileSync(join(snapshot, 'working-tree.patch'), execFileSync('git', ['diff', '--binary', 'HEAD']));
  const extras = execFileSync('git', ['ls-files', '--others', '--exclude-standard'], { encoding: 'utf8' }).trim().split('\n')
    .filter(p => p.startsWith('internal/') || p.startsWith('scripts/bench/longmemeval/'));
  writeFileSync(join(snapshot, 'extra-files.txt'), extras.join('\n') + '\n');
  if (extras.length) execFileSync('tar', ['-czf', join(snapshot, 'extra-files.tar.gz'), '-T', join(snapshot, 'extra-files.txt')]);
  sourceSnapshot = { directory: snapshot, baseSha256: sha(join(snapshot, 'base.tar.gz')),
    patchSha256: sha(join(snapshot, 'working-tree.patch')),
    extrasSha256: extras.length ? sha(join(snapshot, 'extra-files.tar.gz')) : null };
}
try {
  const rows = await runEvaluation({ benchmark: 'longmemeval-matched', questions, adapters,
    k: 5, outDir: out, datasetFiles: [data], config: {
      agentmemoryRevision: '3f8784d10c2e996873d4392b85226a5d042def0e',
      memspaceRevision: execFileSync('git', ['rev-parse', 'HEAD'], { encoding: 'utf8' }).trim(),
      memspaceDirty: execFileSync('git', ['status', '--short'], { encoding: 'utf8' }).trim(),
      workerSha256: args.binary ? sha(resolve(args.binary)) : null,
      memspaceSourceSnapshot: sourceSnapshot,
      runnerSha256: sha(new URL(import.meta.url)),
      claim: 'matched supplied-session retrieval; no reader/judge/capture score; timings are diagnostic cold queries',
      safety: 'worker receives source sessions and query only; no gold IDs, question types or answers',
    } });
  if (rows.some(r => r.status === 'error')) process.exitCode = 1;
  writeFileSync(join(out, 'runner.mjs.txt'), readFileSync(new URL(import.meta.url)));
} finally {
  if (child) { child.stdin.end(); child.kill(); }
}
