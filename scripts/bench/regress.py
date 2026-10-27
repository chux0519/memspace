# Compare embeddings from two embeddings_server builds on several models.
# Usage: regress.py <old embeddings_server> <new embeddings_server> <model.gguf,...>
# (models are read from $MODELS_DIR, default ./models)
import json, subprocess, sys, time, urllib.request, numpy as np, os, random
OLD, NEW, MODELS = sys.argv[1], sys.argv[2], sys.argv[3].split(",")
M = os.environ.get("MODELS_DIR", "models").rstrip("/") + "/"
rnd = random.Random(3)
base = ["登录很慢，认证模块超时", "SQLite FTS5 默认分词器不支持中文", "How do I fix a flaky integration test?",
        "def foo(x):\n    return x * 2", "数据库连接池耗尽导致登录请求排队，调大连接池并加了超时控制。", "a", "🙂 emoji 截断乱码"]
texts = [" ".join(rnd.choice(base) for _ in range(rnd.randint(1, 60))) for _ in range(48)] + base
def run(binary, model, port):
    p = subprocess.Popen([binary, "--model", M + model, "--port", str(port), "--threads", "8", "--max-tokens", "512"],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(120):
        try: urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=1); break
        except Exception: time.sleep(0.5)
    out = []
    for bs in (8, 1):
        for i in range(0, len(texts), bs):
            req = urllib.request.Request(f"http://127.0.0.1:{port}/v1/embeddings", json.dumps({"input": texts[i:i+bs]}).encode(), {"Content-Type": "application/json"})
            out.extend(d["embedding"] for d in json.load(urllib.request.urlopen(req, timeout=300))["data"])
    p.terminate(); p.wait()
    v = np.array(out, dtype=np.float64); return v / np.linalg.norm(v, axis=1, keepdims=True)
os.environ["no_proxy"] = "*"
for m in MODELS:
    a, b = run(OLD, m, 18130), run(NEW, m, 18131)
    cos = (a * b).sum(1); n = len(texts)
    batch_vs_single = (b[:n] * b[n:]).sum(1)
    print(f"{m:48} old-vs-new cos mean {cos.mean():.6f} min {cos.min():.6f} | new batch-vs-single min {batch_vs_single.min():.6f}", flush=True)
