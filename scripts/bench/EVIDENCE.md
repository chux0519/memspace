# 原始证据打包：100 题回归

后续实验及修复见 [RERANK.md](RERANK.md)：官方 Python 重排回放，以及取消每会话
两条硬截止、在总预算内按会话轮流补齐。下文保留 `a3e2709` 阶段的历史结果。

2026-10-30。基线为 MemSpace `27d8c6b`；本机嵌入服务的调度修复另在
embeddings.cpp `0c06342`。本轮只改注入证据的选择与呈现，Inject 0.60、SearchFloor
0.45、RRF 排名及总预算不变。

## 受控对照

[coding-v2-evidence.json](data/coding-v2-evidence.json) 原样保留 v1 的 80 道题和
60 条答案，增加 20 道题、12 条答案。共 100 题、72 条答案、144 条索引文档，
34 dev / 66 test。新增题在实现改动前固定并跑过旧实现；它们是针对故障模式编写的
合成回归题，开发时也看过结果，不能视为独立泛化测试。

两版使用相同的语料、真实查询向量快照和模型 fingerprint。旧版生产源码快照与
`27d8c6b` 逐字节一致；900 次 search 回执的来源顺序、分数、similarity、Match
和预览逐项一致。以下增益来自注入打包，没有搜索排名提升。

| 项目 | 基线 | 改进后 |
|---|---:|---:|
| search/hybrid 找齐证据（80 正例） | 76/80，95.0% | 76/80，95.0% |
| context / 首次 hook 可见证据找齐（79 正例） | 49/79，62.0% | 57/79，72.2% |
| 新增同会话互补证据 | 0/4 | 4/4 |
| 新增长正文证据 | 0/4 | 4/4 |
| 首次 hook token p50 / p95 | 124 / 208 | 125 / 488.4 |
| 即刻重复 hook 非空 | 0/99 | 0/99 |

每题重复 3 次，两版各 1803 条回执；所有重复的正例均无退步。自动注入排除
1 道全项目查询，因此为 99 题、79 正例。找齐要求来源引用及可见的标注证据短语
同时存在，不经过作答模型或 LLM 判分。token 使用 `cl100k_base`，不是 Claude
账单。冻结向量对照只验证质量，不提供线上延迟。

## 改动与成本

- 同一会话先选一条，再轮流补充第二条不同内容的合格证据；仍最多 6 条历史记录。
  在来源内容读取后去重，保留标识符大小写差别。回答、失败或总结仍优先于提问。
- 搜索内部保留命中的原文 chunk；公开 JSON 继续只返回原有预览。注入侧按
  owner、project、doc ID 重新读取来源，并检查 chunk 仍属于当前正文。
- 短正文完整保留；较长的强匹配来源优先返回原文 chunk，或以关键词为锚的原文
  窗口，每条最多 600 字符。明确标出省略；无字面锚点的长语义块保留首尾。
  仅凭部分关键词通过的弱匹配保持紧凑预览，避免放大无关上下文。
- 全局 6000 字符预算、当前会话排除、项目范围和重复投递抑制保持原有约束。

长文题的 token p50 / p95 为 491.5 / 494，解释了总体 p95 的增加。这轮没有做
语义压缩或 LLM 摘要。更短窗口与证据完整性的取舍仍需单独验证。

## 负例与未解决的问题

| 题组 | 基线非空 | 改进后非空 | 解释 |
|---|---:|---:|---|
| v1 原 8 道负例 | 1/8 | 1/8 | 原有相近主题误注入未修复 |
| q089–096：缺失属性或核对前提 | 8/8 | 8/8 | 相关历史可用于反驳前提或说明信息不足，不能仅凭非空判错 |
| q097–100：范围及未知项目 | 1/4 | 1/4 | q097 返回 billing 内的 Draco 记录，不是所问 Orion；没有跨项目泄漏 |

新增 12 题沿用数据中的 `hard_negative` 空证据标签，因此通用报表得到 9/12，
合并全部负例为 10/20。这仅表示非空，不能宣传为回答错误率或经过人工审定的
误注入率。本轮没有调整标签来改变成绩。

仍有 22/79 正例未找齐；候选相关性校准、重排及全部长文证据恢复尚未解决。
所有已测 scope 越界、来源标注错误、错误、超时及语义降级为 0。四个 session-start
冒烟夹具仍为 3/4，不是交接成功率。跨设备同步和真实 agent 任务完成率尚未测量。

## 原 80 题实时回归

原始 v1、真实在线查询嵌入、3 次重复，共 1443 条回执：

- context / 首次 hook 仍为 49/71，69.0%；逐题比较原有线上基线，全部重复的正例结果未变。
- hybrid search 为 68/72，94.4%；原 8 道负例仍为 1/8 非空。
- 首次 hook p50 / p95 为 101.50 / 116.34 ms，token 为 121 / 208。
- 零错误、超时、降级、scope 越界及来源标注错误；即时重复输出全空。

这是共享机器上预热、顺序、loopback 的一次运行；启动时与 v2 索引构建有并行，
不据此主张延迟改善，也不代表 4.2 万事件生产库的性能。

## 复核与复跑

本机文件（`runs/` 被 Git 忽略，clone 后需重跑）：

- [v2 基线回执](runs/evidence-v2-before/receipts.jsonl) / [manifest](runs/evidence-v2-before/manifest.json)
- [v2 改进回执](runs/evidence-v2-reviewed-final/receipts.jsonl) / [汇总](runs/evidence-v2-reviewed-final/summary.json) / [manifest](runs/evidence-v2-reviewed-final/manifest.json)
- [v1 实时回执](runs/evidence-v1-reviewed-live/receipts.jsonl) / [汇总](runs/evidence-v1-reviewed-live/summary.json)

SHA256：

```text
v2 corpus       edca409435997beb9b4a789651b21895d000abae55db451c47476f2f0552025f
query vectors   e076d9b86cb10d04a35da0f5ca19f7e5d848bad11664f2774a09ea43901f1f64
before receipts 931a1340f78dd8eef33b26ffb2ba3266be654bbf512b926af73afb132362de0b
after receipts  e7de8f0b698195530ea9becdbfb31e477206e5bb19c89b8003189880dd6dfd7f
v1 live         549784f30c06a24f869f6df6dad317ea7988c29f5335aad1098fd74ceb104779
```

按 [README](README.md) 运行，指定 `-corpus scripts/bench/data/coding-v2-evidence.json`。
首次用 `-freeze-queries`，另一版本用 `-query-vectors` 重用快照。旧版 checkout
`27d8c6b` 后，仅复制新版 `scripts/bench/retrieval/` 和 v2 数据，以支持同会话多记录；
保留旧版 `internal/` 生产代码。对照 manifest、向量快照及 search 回执后再归因。
原始向量快照未发布；模型数值或硬件变化可能影响重新生成的向量及分数。

全仓 Go 测试、受影响包 race、vet 及 Python 报表测试通过。新增检查覆盖来源范围、
失效 chunk、原文窗口上界、弱匹配不展开、同会话内容去重、大小写标识符、重复抑制、
内部原文不进入公开 JSON，以及 v1 题目保持不变。

改进保留在本地提交，尚未推送或部署 MemSpace Go 服务；本机嵌入服务继续使用此前
已部署的调度修复。
