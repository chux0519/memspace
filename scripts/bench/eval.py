"""Compare embedding services on MemSpace-style retrieval.

Labeled docs (Chinese / English agent history) are mixed with real distractor
documents; each query is ranked against the whole corpus.

Usage: eval.py <memspace.db> <n_distractors> <name> <embeddings-url> [query-prefix]

Distractors are real search documents from a MemSpace database. Corpus
vectors are cached as <name>-<n>.npz next to this script (delete to re-run);
results go to results-<name>.json. Example (Snowflake needs "query: "):

    eval.py ~/.local/share/memspace/memspace.db 2000 bge-m3-q8 http://127.0.0.1:8771/v1/embeddings
"""
import json, sqlite3, sys, time, urllib.request, os, random
import numpy as np

OUT = os.path.dirname(os.path.abspath(__file__))
os.environ["NO_PROXY"] = "*"

DOCS = {
 "auth_timeout": "已修复认证模块的超时问题：数据库连接池耗尽导致登录请求排队，调大连接池并加了超时控制。",
 "jwt_refresh": "重构认证模块，把 JWT 刷新令牌逻辑拆出去，TestRefresh 还在失败。",
 "stripe_idem": "实现 Stripe webhook 的幂等处理，用 event_id 去重，重复回调不会重复扣款。",
 "payment_retry": "Payment retries are now idempotent via a unique processed_events table.",
 "migration": "重构数据库迁移脚本，新增 users.last_login 列并回填历史数据。",
 "canary": "灰度发布按 5%/25%/100% 推进；熔断后降级到只读缓存。",
 "k8s_rollout": "kubectl rollout status hangs because the readiness probe points at the wrong port.",
 "react_state": "Moved the shopping cart state from Redux to React context and removed prop drilling.",
 "goroutine_leak": "Worker pool leaked goroutines: the result channel was never closed when ctx was cancelled.",
 "snapshot_test": "go test ./ui failed: snapshot mismatch in the header component after the font change.",
 "fts_cn": "SQLite FTS5 默认分词器不支持中文，换成 simple + jieba 后 幂等、去重 可以检索到。",
 "embed_bug": "embeddings.cpp 的 GTE mean pooling 在 reshape 时断言失败，CLS 不受影响。",
 "docker_cache": "Docker build cache was invalidated every time because COPY . . came before go mod download.",
 "rate_limit": "给 API 加了基于令牌桶的限流，单用户每秒 20 次。",
 # cross-language technical terms
 "deadlock": "Fixed a deadlock: two goroutines acquired mu1 and mu2 in opposite order; now locks are always taken in id order.",
 "mem_leak": "Node service RSS grew to 4GB overnight; the event listener was registered on every request and never removed.",
 "race": "go test -race reported a data race on the cache map; wrapped it with sync.RWMutex.",
 "tls_expired": "x509: certificate has expired or is not yet valid — renewed the Let's Encrypt cert and fixed the certbot cron.",
 "timezone": "订单统计少了一天：服务器用 UTC，报表按北京时间切日，改为统一用 Asia/Shanghai 计算日期边界。",
 "cors": "浏览器报 No 'Access-Control-Allow-Origin' header，后端补上 CORS 中间件并允许携带 cookie。",
 "serialization": "JSON marshal dropped the field because it was unexported (lowercase); renamed it and added a json tag.",
 "backoff": "调用第三方短信接口失败后改为指数退避重试，最多 5 次并加随机抖动。",
 "cache_stampede": "Hot key expiry caused thousands of requests to hit MySQL at once; added singleflight and jittered TTLs.",
 "dep_inject": "把全局单例改成构造函数注入，测试里可以替换成 mock 仓储。",
 "rollback": "Deployment v2.3 broke checkout, so we rolled back to v2.2 with helm rollback and pinned the image tag.",
 "oom_kill": "容器被 OOMKilled，原因是 JVM 堆上限超过 cgroup 内存限制，加了 -XX:MaxRAMPercentage=75。",
 "slow_query": "Orders page took 8s; EXPLAIN showed a full table scan, added a composite index on (user_id, created_at).",
 "flaky_test": "集成测试偶发失败：依赖测试执行顺序共享了全局状态，改成每个用例独立初始化数据库。",
 "ws_reconnect": "WebSocket 断线后前端没有重连，加了心跳检测和断线自动重连。",
 "i18n": "Added zh-CN and en locale files and replaced hard-coded strings with t() calls in the settings page.",
 "ssh_key": "CI 拉取私有仓库失败：deploy key 没有读权限，换成只读的 deploy key 并配置 known_hosts。",
 "pagination": "List API returned duplicates across pages with OFFSET; switched to keyset pagination on (created_at, id).",
 "upload_limit": "上传大文件返回 413，nginx client_max_body_size 默认 1m，调到 100m。",
 "env_secret": "Moved the database password out of config.yaml into an environment variable loaded from the secret manager.",
 "log_rotate": "磁盘被日志写满，配置 logrotate 每天轮转并保留 7 天。",
 "unicode": "用户名里有 emoji 时截断出错，按字节切导致乱码，改为按 rune 截断。",
 "gc_pause": "P99 latency spikes every few minutes were GC pauses; reduced allocations in the hot path with sync.Pool.",
 "dns": "Pods could not resolve the external API intermittently; CoreDNS was rate limited, enabled NodeLocal DNSCache.",
 "csv_bom": "导出的 CSV 用 Excel 打开中文乱码，文件头加上 UTF-8 BOM 解决。",
 "feature_flag": "New search ranking is behind a feature flag so it can be enabled per tenant without a deploy.",
}

QUERIES = [
 ("登录很慢是什么原因", ["auth_timeout"]),
 ("why are sign-ins slow", ["auth_timeout"]),
 ("怎么避免 webhook 重复扣款", ["stripe_idem", "payment_retry"]),
 ("how did we make payments safe to retry", ["payment_retry", "stripe_idem"]),
 ("令牌过期的测试", ["jwt_refresh"]),
 ("数据库加字段并回填", ["migration"]),
 ("deployment is stuck and never finishes", ["k8s_rollout"]),
 ("协程泄漏", ["goroutine_leak"]),
 ("中文搜索不到", ["fts_cn"]),
 ("向量模型崩溃", ["embed_bug"]),
 ("镜像构建很慢每次都重新下载依赖", ["docker_cache"]),
 ("流量控制 每秒请求数", ["rate_limit"]),
 ("逐步放量上线", ["canary"]),
 ("前端状态管理", ["react_state"]),
 ("UI 快照测试失败", ["snapshot_test"]),
 # cross-language
 ("死锁怎么解决的", ["deadlock"]),
 ("内存泄漏 内存一直涨", ["mem_leak"]),
 ("并发读写 map 竞态", ["race"]),
 ("证书过期", ["tls_expired"]),
 ("timezone bug in daily report", ["timezone"]),
 ("CORS error from the browser", ["cors"]),
 ("字段序列化后丢失", ["serialization"]),
 ("exponential backoff for SMS provider", ["backoff"]),
 ("缓存击穿 数据库被打爆", ["cache_stampede"]),
 ("dependency injection for testing", ["dep_inject"]),
 ("版本回滚", ["rollback"]),
 ("container killed out of memory", ["oom_kill"]),
 ("慢查询加索引", ["slow_query"]),
 ("flaky integration tests", ["flaky_test"]),
 ("websocket reconnect", ["ws_reconnect"]),
 ("国际化 多语言", ["i18n"]),
 ("CI 克隆私有仓库权限", ["ssh_key"]),
 ("分页结果重复", ["pagination"]),
 ("file upload too large error", ["upload_limit"]),
 ("密码不要写在配置文件里", ["env_secret"]),
 ("disk full because of logs", ["log_rotate"]),
 ("emoji 截断乱码", ["unicode"]),
 ("垃圾回收导致延迟抖动", ["gc_pause"]),
 ("域名解析偶尔失败", ["dns"]),
 ("excel 打开 csv 中文乱码", ["csv_bom"]),
 ("灰度开关 按租户开启", ["feature_flag"]),
 # off-topic
 ("写一首关于秋天的诗", []),
 ("今天天气怎么样", []),
 ("explain quantum entanglement simply", []),
 ("推荐几部好看的电影", []),
 ("how to bake sourdough bread", []),
]

def embed(url, texts, batch=8):
    out = []
    for i in range(0, len(texts), batch):
        body = json.dumps({"input": texts[i:i + batch]}).encode()
        req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=600) as r:
            out.extend(x["embedding"] for x in json.load(r)["data"])
    v = np.array(out, dtype=np.float32)
    return v / np.linalg.norm(v, axis=1, keepdims=True)

def distractors(n, seed=7):
    db = sqlite3.connect(sys.argv[1])
    rows = db.execute("SELECT title, body FROM search_docs WHERE kind IN ('command','reply','failure','prompt') AND length(body) > 40").fetchall()
    random.Random(seed).shuffle(rows)
    # MemSpace chunks at ~380 estimated tokens; first chunk is enough here.
    return [(t + "\n" + b).strip()[:1200] for t, b in rows[:n]]

def main():
    n_distract = int(sys.argv[2]) if len(sys.argv) > 2 else 2000
    names = list(DOCS)
    corpus = [DOCS[k] for k in names] + distractors(n_distract)
    results = {}
    for svc in [sys.argv[3]]:
        url, prefix = sys.argv[4], (sys.argv[5] if len(sys.argv) > 5 else "")
        cache = os.path.join(OUT, f"{svc}-{n_distract}.npz")
        if os.path.exists(cache):
            z = np.load(cache); D, doc_s = z["D"], float(z["doc_s"])
        else:
            t0 = time.time(); D = embed(url, corpus); doc_s = time.time() - t0
            np.savez(cache, D=D, doc_s=doc_s)
        t0 = time.time()
        Q = embed(url, [prefix + q for q, _ in QUERIES], batch=1)
        q_ms = (time.time() - t0) / len(QUERIES) * 1000
        S = Q @ D.T
        top1 = r5 = mrr = 0; nlab = 0; misses = []; rel, irr, offtop = [], [], []
        for i, (q, good) in enumerate(QUERIES):
            order = np.argsort(-S[i])
            if not good:
                offtop.append(float(S[i, order[0]])); continue
            nlab += 1
            gidx = [names.index(g) for g in good]
            rank = min(int(np.where(order == g)[0][0]) for g in gidx) + 1
            top1 += rank == 1; r5 += rank <= 5; mrr += 1 / rank
            if rank > 1:
                top = order[0]
                misses.append((q, rank, corpus[top][:60].replace("\n", " "), float(S[i, top]), max(float(S[i, g]) for g in gidx)))
            for j in range(len(names)):
                (rel if j in gidx else irr).append(float(S[i, j]))
            irr.extend(S[i, len(names):].tolist())
        rel, irr = np.array(rel), np.array(irr)
        results[svc] = dict(top1=top1 / nlab, r5=r5 / nlab, mrr=mrr / nlab, doc_s=doc_s, docs=len(corpus), q_ms=q_ms,
            rel=np.percentile(rel, [10, 50, 90]).tolist(), irr=np.percentile(irr, [50, 90, 99, 99.9]).tolist(),
            irr_max=float(irr.max()), offtop_max=max(offtop),
            thr={t: (float((rel >= t).mean()), float((irr >= t).mean())) for t in np.arange(0.3, 0.75, 0.05).round(2)},
            misses=misses)
    json.dump(results, open(os.path.join(OUT, "results-" + sys.argv[3].replace(",", "_") + ".json"), "w"), ensure_ascii=False, indent=1)
    for svc, r in results.items():
        print(f"\n=== {svc}: top1 {r['top1']:.0%}  recall@5 {r['r5']:.0%}  MRR {r['mrr']:.3f}  | embed {r['docs']} docs in {r['doc_s']:.0f}s ({r['docs']/r['doc_s']:.1f} doc/s)  query {r['q_ms']:.0f} ms")
        print(f"    relevant sim P10/P50/P90 {[round(x,3) for x in r['rel']]}  irrelevant P50/P90/P99/P99.9 {[round(x,3) for x in r['irr']]}  max {r['irr_max']:.3f}  off-topic top {r['offtop_max']:.3f}")
        print("    threshold: " + "  ".join(f"{t}: keep {k:.0%} / noise {n:.3%}" for t, (k, n) in r['thr'].items()))
        for m in r["misses"]:
            print(f"    miss  {m[0]:28} rank {m[1]:4}  top={m[2]!r} {m[3]:.3f} vs relevant {m[4]:.3f}")

main()
