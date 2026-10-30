/** Verify private receipts using the pinned upstream loader and scorer.
 * Run with tsx: --upstream PATH --data JSON --results MODE_DIR [--allow-partial]
 * This never retrieves, changes rankings, or calls an inference service.
 */
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { readFileSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { gunzipSync } from 'node:zlib';

const pinned = '3f8784d10c2e996873d4392b85226a5d042def0e';
const expected = {
  'load.ts': '4e15e3c6968cc558c40982b117beea981a6366942cb97473e2848f043169d0a1',
  'score.ts': '51ed764b1747db26d70aecd8b677e22923ea38ccbf4c97ec782da612fea1bcb6',
};
function arg(name: string): string {
  const at = process.argv.indexOf(name);
  assert(at >= 0 && process.argv[at + 1], `missing ${name}`);
  return resolve(process.argv[at + 1]);
}
function sha(path: string): string {
  return createHash('sha256').update(readFileSync(path)).digest('hex');
}
function ndjson(path: string): any[] {
  return gunzipSync(readFileSync(path)).toString('utf8').trim().split('\n').map(s => JSON.parse(s));
}

async function main() {
  const upstream = arg('--upstream'), data = arg('--data'), results = arg('--results');
  const partial = process.argv.includes('--allow-partial');
  const sourceHashes: Record<string, string> = {};
  for (const [file, want] of Object.entries(expected)) {
    const path = join(upstream, 'eval/runner', file);
    sourceHashes[file] = sha(path);
    assert.equal(sourceHashes[file], want, 'upstream source changed');
  }
  assert.equal(sha(data), 'd6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442');
  const { loadLongMemEval, opaqueSessionId } = await import(pathToFileURL(join(upstream, 'eval/runner/load.ts')).href);
  const { scoreQuestion, aggregate } = await import(pathToFileURL(join(upstream, 'eval/runner/score.ts')).href);
  const questions = new Map<string, any>(loadLongMemEval(data).map((q: any) => [q.id, q]));
  const scores = ndjson(join(results, 'scores.ndjson.gz'));
  const evidence = new Map(ndjson(join(results, 'evidence.ndjson.gz')).map(r => [r.question_id, r]));
  assert.equal(new Set(scores.map(r => r.question_id)).size, scores.length);
  assert.equal(evidence.size, scores.length);
  if (!partial) assert.equal(scores.length, 500);
  const upstreamScores = [];
  for (const receipt of scores) {
    assert(!receipt.error, `failed attempt ${receipt.question_id}: ${receipt.error}`);
    const q = questions.get(receipt.question_id);
    assert(q, 'unknown question');
    const row = evidence.get(q.id)!;
    const ranked = (row.returned_sessions ?? []).map((id: string) => ({ sessionId: opaqueSessionId(q.id, id) }));
    const allowed = new Set(q.haystack.map((s: any) => s.id));
    assert(ranked.every((r: any) => allowed.has(r.sessionId)), 'out-of-corpus source');
    assert.equal(new Set(ranked.map((r: any) => r.sessionId)).size, ranked.length);
    assert.equal(receipt.returned_sessions, ranked.length);
    const gold = row.gold_sessions.map((id: string) => opaqueSessionId(q.id, id)).sort();
    assert.deepEqual(gold, [...q.goldSessionIds].sort());
    const official = scoreQuestion(q, ranked, 5, receipt.mode, receipt.duration_ms);
    const official10 = scoreQuestion(q, ranked, 10, receipt.mode, receipt.duration_ms);
    const goldSet = new Set(q.goldSessionIds);
    const dcg = ranked.slice(0, 10).reduce((sum: number, r: any, i: number) => sum + (goldSet.has(r.sessionId) ? 1 / Math.log2(i + 2) : 0), 0);
    const ideal = Array.from({ length: Math.min(10, goldSet.size) }, (_, i) => 1 / Math.log2(i + 2)).reduce((a, b) => a + b, 0);
    const fields = [
      ['recall_all_at_5', official.recallAllAtK],
      ['fractional_at_5', official.recallAtK],
      ['any_hit_at_5', official.hit === null ? null : Number(official.hit)],
      ['recall_all_at_10', official10.recallAllAtK],
      ['ndcg_at_10', official.recallAtK === null ? null : dcg / ideal],
    ] as const;
    for (const [key, want] of fields) {
      assert(want === null ? receipt[key] === null : Math.abs(receipt[key] - want) < 1e-12, `${q.id} ${key}`);
    }
    upstreamScores.push(official);
  }
  const scored = upstreamScores.filter(r => r.recallAtK !== null).length;
  if (!partial) assert.equal(scored, 470);
  const audit = {
    upstream_revision: pinned, source_sha256: sourceHashes, dataset_sha256: sha(data),
    checked: scores.length, scored, unanswerable: scores.length - scored,
    mismatches: 0, scores_sha256: sha(join(results, 'scores.ndjson.gz')),
    evidence_sha256: sha(join(results, 'evidence.ndjson.gz')),
    upstream_aggregate: aggregate(upstreamScores),
    limits: 'Same source-session scorer. Event transforms and pack budgets differ from whole-session indexing. No answer-text completeness or end-to-end QA claim.',
  };
  writeFileSync(join(results, 'agentmemory-score-audit.json'), JSON.stringify(audit, null, 2) + '\n', { mode: 0o600 });
  console.log(JSON.stringify({ results, checked: scores.length, scored, mismatches: 0 }));
}
main().catch(e => { console.error(e); process.exitCode = 1; });
