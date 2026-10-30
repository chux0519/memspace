# 嵌入排队超时：复现、修复与本机部署

2026-10-30。保持查询 embedding deadline 1s、prompt hook deadline 1.5s。没有修改相似度阈值或用冻结查询替代本轮实时性能测试。

## 根因

本机 `embeddings_server` 的原实现用一个 mutex 包住整个 `batch_encode(inputs)`。MemSpace 默认后台批量为 8，8 个长输入会连续占用模型数秒；短查询即使自身很快，也必须等完整批次结束。重复查询超时又触发 MemSpace 的 30s 熔断，放大了损失。

复现输入是固定的公开合成中文文本，不使用私有历史：单条短查询约 46.5ms，单个长文本约 587.7ms，8 个长输入的批次约 4715.9ms。批次开始 120ms 后发起的 1s 查询超时。

## 改动

- `embeddings.cpp/src/inference_gate.h`：保持模型与 tokenizer 串行访问，但排队查询优先于后续后台输入；等待时检查连接是否断开。
- `embeddings.cpp/src/server.cpp`：后台 HTTP 批次在每个输入完成后让出模型。单条查询的计数与编码共用一次 lease，避免同一查询先后等两个后台输入。多输入自动视为后台；单输入后台任务可通过 `X-Embedding-Priority: background` 声明。
- `internal/embedding/client.go`、`service.go`：索引请求带 background 提示；调用方自己的 context 取消不再计为服务故障或触发熔断。其他服务可以忽略扩展 header，JSON body 仍是标准 embeddings 请求。

正在计算的一个输入不能中途抢占。这解决的是整批占锁和过期请求继续排队的问题，不承诺任意并发、任意硬件都能在 1s 内完成。

## 验证

同一机器，12 个推理线程，相同 GGUF、max_tokens=512、1s 查询 deadline。

| 场景 | 原实现 | 新实现 |
|---|---:|---:|
| 后台长批次期间，3 轮、24 次顺序查询 | 13/24 超时 | 0/24 超时 |
| 该场景 attempted latency p95 | 约 1002ms，超时截尾 | 约 693ms |
| 4 路前台并发，3 轮、12 次查询 | 未测 | 0/12 超时，最慢约 735ms |

原实现超时请求的时间在 1s 处截尾，不能解释成真实完成延迟仅有 1s。负载是固定一个后台批次，并非无限负载保证。

8 个混合长短、中文/英文输入逐条验证：**新旧单输入向量逐位一致**；新批量输出也与逐条输出一致。原来的 padded batch 路径本来就与单输入有小量差异，本例最小 cosine 为 0.99870057。HTTP 批次现在使用单输入路径，因此新旧批量向量不是逐位一致。此类量化批处理差异不能隐藏为“完全无数值变化”。生产已有向量不强制重建，本轮完整基准检查其质量影响。

OpenAI float/base64/维度截取、TEI `/embed`、无效请求和批量上限检查通过。模型无关的 C++ 调度测试覆盖前台优先、取消移出等待队列和后台继续执行。Go 测试覆盖 header 兼容、调用方取消不触发熔断；原有真实服务失败触发熔断的测试仍通过。全仓 Go 测试、embedding race、Python 测试通过。

## 实时基准与部署范围

先在隔离端口 8772 验证，再保留旧二进制并原子替换本机 `memspace-embeddings.service` 使用的二进制。当前版本为 `0.1.3+priority-local`，`/info.scheduling` 为 `queries_between_background_inputs`。MemSpace 服务与嵌入服务健康检查均通过。隔离测试进程已停止。

**此次上线仅替换嵌入服务。** 运行中的 MemSpace server 仍是原 v0.1.3；Go 侧取消请求修复、上一轮注入修复仍在工作区，尚未发布/替换运行中的 MemSpace server。原客户端的多输入请求也会被新嵌入服务自动识别为后台，因此无需等待 Go 版本发布即可避免整批占锁。

当前工作区构建的 benchmark runner 对**已部署的 8771** 跑 80 题、每题 3 次，共 1443 条 receipts：错误、超时、语义降级、scope 越界及来源标注错误均为 0，重复 hook 输出仍为 0。

- 搜索证据找齐 68/72（94.4%）；上次 batch 路径为 67/72（93.1%）。这是输入编码路径产生的小数值/排名变化，不宣称搜索算法提升。
- 最终注入证据找齐仍是 49/71（69.0%），负例误注入仍为 1/8。
- 首次 hook p50 / p95 约 100.9 / 115.5ms，token p50 / p95 为 121 / 208（cl100k_base）。这是小合成库、本机 loopback 条件下的本次测量。

可复核文件（本机，Git 忽略的 runs）：

- [单背景批次对照](runs/contention-priority.json)
- [并发查询结果](runs/contention-parallel.json)
- [隔离端口完整结果](runs/timeout-fixed-live/summary.json)
- [部署后实时结果](runs/timeout-fixed-deployed/summary.json)、[逐题 receipts](runs/timeout-fixed-deployed/receipts.jsonl)
- [部署版本、旧/新 binary SHA256 与备份位置](runs/timeout-deployment.json)

旧 binary 备份位于 `~/.local/opt/memspace/embeddings/backup-timeout-20261030T122655Z/embeddings_server`。如需回滚，先把它复制到新的临时文件，再原子替换目标 binary 并重启 `memspace-embeddings`，不要直接覆盖正在运行的文件。

复现争用：

```bash
python3 scripts/bench/contention.py \
  --before http://127.0.0.1:OLD_PORT/v1/embeddings \
  --after http://127.0.0.1:NEW_PORT/v1/embeddings \
  --out scripts/bench/runs/contention.json
```

已更新的 8771 不再是旧实现；重做前后对照应使用保留的旧 binary 在另一个隔离端口运行。
