# Usage: speed.py <db> <url> <n_docs> <ref.npz> [label]
# Embeds the first n docs of eval.py's corpus; prints docs/s, query latency and
# cosine vs. the reference vectors (same corpus order).
import sys, time, numpy as np, importlib.util, os
import types
src = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "eval.py")).read().rsplit("main()", 1)[0]
ev = types.ModuleType("ev"); ev.__file__ = os.path.join(os.path.dirname(os.path.abspath(__file__)), "eval.py"); exec(src, ev.__dict__)
db, url, n, ref = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
label = sys.argv[5] if len(sys.argv) > 5 else url
corpus = ([ev.DOCS[k] for k in ev.DOCS] + ev.distractors(2000))[:n]
ev.embed(url, corpus[:8])  # warm up
t0 = time.time(); D = ev.embed(url, corpus); dt = time.time() - t0
t0 = time.time(); ev.embed(url, [q for q, _ in ev.QUERIES[:20]], batch=1); q_ms = (time.time() - t0) / 20 * 1000
R = np.load(ref)["D"][:n]; cos = (D * R).sum(1)
print(f"{label}: {n/dt:.2f} doc/s  query {q_ms:.0f} ms  cos vs ref mean {cos.mean():.5f} min {cos.min():.5f}", flush=True)
