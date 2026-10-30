# LongMemEval-S 同口径会话检索

2026-10-30。目标是复现并超过 [agentmemory 的公开会话检索基线](https://www.agent-memory.dev/benchmarks)。
这里比较的是**给定原始会话后的检索组件**，不运行问答 LLM、判分 LLM、捕获、
摘要生成或注入 hook。不能用这个数字替代实际注入找齐率或编码任务完成率。

## 固定方法

直接复用 agentmemory 未修改的 loader、输入变换、适配器和 scorer：

- 实际评估源码 revision：`3f8784d10c2e996873d4392b85226a5d042def0e`；
  [公布结果的报告](https://github.com/rohitg00/agentmemory/blob/c6c476d2905926727fa5848a134d7a3b38a7fa98/docs/benchmarks/2026-10-28-retrieval-evaluation.md)。
- loader/scorer/source tree SHA256：
  `25c0fc4f610b612675600ebac66da592716081b7cf97b34c018b417f1b34520f`，与公布清单一致。
- 数据：[LongMemEval-S cleaned](https://huggingface.co/datasets/xiaowu0162/longmemeval/tree/98d7416c24c778c2fee6e6f3006e7a073259d48f)，
  revision `98d7416c24c778c2fee6e6f3006e7a073259d48f`，277,383,467 bytes；
  SHA256 `d6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442`。
- 每系统完整执行 500 题，470 道可回答题进入质量分母；30 道不可回答题只报告执行覆盖，
  不伪造空答案准确率。前 **5 个不同会话**必须包含全部标准来源才得 1 分。
- 完全保留官方 role flatten、日期前缀、13 个重复会话 ID 的相同原文合并和不透明来源 ID。
  未选择、删除或修改题目。每题重建独立索引。
- MemSpace worker 只接收来源原文/日期、不透明 ID、问题、k、检索模式；
  协议拒绝 answer/gold 等额外字段。标准来源 ID、题型和答案只在检索结束后进入官方 scorer。
- 无 LLM、无 embedding、无 reranker、无外部推理 API 调用。全程本地 CPU。

两边均直接索引完整 supplied session：agentmemory 调 SearchIndex.add，MemSpace 调
search.Upsert。这绕过了实际事件捕获和派生记录流程；MemSpace 的 DocFromEvent 仍有
4KiB 正文上限，本成绩不证明该上限之下的端到端来源完整性。

## 结果

| 方法 | 找齐题数 / 470 | 严格找齐率 |
|---|---:|---:|
| 官方 grep，原样复现 | 339 | 72.13% |
| agentmemory BM25 组件，原样复现 | 398 | 84.68% |
| MemSpace 关键词覆盖排序 + Porter | 375 | 79.79% |
| MemSpace BM25 + Porter，保留前缀匹配 | 396 | 84.26% |
| MemSpace 完整会话 + 片段 RRF，保留前缀匹配 | 388 | 82.55% |
| MemSpace BM25 + Porter，英文完整词匹配，最终复跑一致 | **403** | **85.74%** |

最后一行的英文词形由 `porter simple` 归一化，例如 dependencies/testing 能匹配
dependency/test 的英文词形；英文 BM25 查询采用完整词，避免把 test 的前缀扩到 testament。
原有关键词入口继续使用中文、拼音 simple/jieba 路径。关键词覆盖优先和默认混合模式的查询规则
保留，另提供 `mode=bm25`，不强行将所有搜索入口改成单一排名。

403 对 398 是净增 **5 题、1.06 个百分点**。配对明细是 11 胜、6 负；
McNemar 双侧精确检验 p=0.332，固定种子 10,000 次配对 bootstrap 的差值 95% 区间
约为 **-0.64～+2.77 个百分点**。因此可以说本数据集点估计更高，不能说稳定、显著
或全面超越。公共题集已用于比较这些方案，结果是开发集上的回归，不是未见测试集。
没有按具体题目、实体或标准答案写检索规则，也没有训练模型。

按题型，英文完整词版本 vs agentmemory：

| 题型 | MemSpace | agentmemory |
|---|---:|---:|
| 单会话用户信息 | 64/64 | 63/64 |
| 单会话助手信息 | 56/56 | 55/56 |
| 偏好 | 25/30 | 25/30 |
| 跨会话 | 87/121 | 86/121 |
| 知识更新 | 71/72 | 70/72 |
| 时间推理 | 100/127 | 99/127 |

查询时间初次完整词版本 p50/p95 约 37/95ms；agentmemory 约 3.1/3.6ms。
最终源码复跑 p50/p95 为 **26.6/71.6ms**，500题的返回排名、分数和内容哈希与初次结果逐项一致。
这是同机单进程调用的诊断时间，包含 worker 通信/SQLite 搜索，与内存 JS 索引的
持久化、启动、建库行为不同，不是生产延迟 SLA，也不包含答案生成。
**当前成绩的优势在找齐率，不在查询速度。** 最终复跑的完整时间见聚合 JSON。

另做“完整会话 + 现有分块器片段”的等权 RRF 实验，rrfK=60，每一路每会话只投一票，
仍只返回 5 个会话；它只有388/470=82.55%，查询 p95 约3.04s，不作为默认注入策略。完整会话放入 summary
记录，片段放 reply 记录，二者共用 FTS 统计；分块沿用 380 估算 token、40 重叠、最多32块。
即使尾部超过分块上限，完整原文仍保留。这项实验的全量结果与前两版一起保留，不能只
公开最好的一次。

## 与实际注入的关系

本轮产品改动还包括：候选预算前按**完整来源及时间/角色/设备/agent**去重，保留不同
来源事实；扩大检索候选窗口以补充被重复记录占掉的位置。最终注入仍最多6条/6000字符，
同会话可以填补空位。相同预览但不同正文不会被误删。数据库迁移仅重建派生 FTS，
原始记录、doc_id 和 embeddings 不变；已有库升级与中英查询均有回归测试。

100 道内部合成题的真实 Build/hook 冻结查询回归仍为 **57/79=72.2%**，零正例得失；
context 的 cl100k_base token p50/p95 仍为 **125/488.2**。范围、来源校验、错误和超时均为0。
这不是此前官方 Python reranker 离线回放的 67/79=84.8%；本轮未将神经重排部署到服务。

新增 `coding-v2-relevance-v1.json` 为14题逐一检查全部72条语料的相关性补充标注，绑定
原题集 SHA；未标注的86题不计算 precision。标准是直接回答/支持目标实体问题，单纯
话题相近不算；q026 的回滚验收来源也算相关。这是事后 agent 标注，供诊断，非独立人工
标注，不用于选择参数。当前 context 的14题共返回19个不同引用，其中6个无关，
诊断 precision **68.4%**；不能据此声称整体噪声已解决。空返回的 precision 为 null。

内部复跑：

```bash
CGO_ENABLED=1 go run -tags sqlite_fts5 ./scripts/bench/retrieval \
  -corpus scripts/bench/data/coding-v2-evidence.json \
  -relevance-labels scripts/bench/data/coding-v2-relevance-v1.json \
  -out /tmp/memspace-lexical-labeled \
  -embedding-url http://127.0.0.1:8771/v1/embeddings \
  -query-vectors scripts/bench/runs/evidence-v2-before/query-vectors.json -repeats 1
python3 scripts/bench/tokenize_report.py /tmp/memspace-lexical-labeled --encoding cl100k_base
```

## 复现公开比较

Node 本次为 v24.21.0；固定依赖为 tsx4.23.15、zod4.6.7、@node-rs/jieba2.0.4、
tiny-segmenter0.2.0。原报告 Node25.6.1；本次计分结果精确复现398/339，时间不与原机比较。
准备外部源码/依赖目录，避免修改第三方 repo：

```bash
export HTTP_PROXY=http://127.0.0.1:7890
export HTTPS_PROXY=http://127.0.0.1:7890
export ALL_PROXY=http://127.0.0.1:7890
export NO_PROXY=127.0.0.1,localhost

git clone https://github.com/rohitg00/agentmemory.git /tmp/agentmemory-source
git -C /tmp/agentmemory-source fetch --depth 1 origin 3f8784d10c2e996873d4392b85226a5d042def0e
mkdir -p /tmp/agentmemory-eval /tmp/agentmemory-deps
git -C /tmp/agentmemory-source archive 3f8784d10c2e996873d4392b85226a5d042def0e \
  src eval/runner LICENSE package.json AGENTS.md | tar -x -C /tmp/agentmemory-eval
npm install --prefix /tmp/agentmemory-deps --ignore-scripts --no-audit --no-fund \
  tsx@4.23.15 zod@4.6.7 @node-rs/jieba@2.0.4 tiny-segmenter@0.2.0
ln -s /tmp/agentmemory-deps/node_modules /tmp/agentmemory-eval/node_modules

curl -L --retry 5 -o /tmp/longmemeval_s_cleaned.json \
  https://huggingface.co/datasets/xiaowu0162/longmemeval/resolve/98d7416c24c778c2fee6e6f3006e7a073259d48f/longmemeval_s_cleaned.json
# runner 校验上面的 SHA256，不符直接退出。
CGO_ENABLED=1 go build -tags sqlite_fts5 -o /tmp/memspace-lme ./scripts/bench/longmemeval/worker
/tmp/agentmemory-deps/node_modules/.bin/tsx scripts/bench/longmemeval/run.mjs \
  --vendor /tmp/agentmemory-eval --data /tmp/longmemeval_s_cleaned.json \
  --out /tmp/memspace-lme-run --binary /tmp/memspace-lme --modes bm25
python3 scripts/bench/longmemeval/compare.py /tmp/memspace-lme-run --out /tmp/memspace-lme-comparison.json
```

输出目录必须不存在。`--limit 5` 可冒烟测试，不能用于正式计分；compare.py 拒绝不足500题、
重复题、缺失结果、错误、数据 SHA 或官方源码 SHA 改变。正式运行同时保存每题排名和
内容哈希、得分、manifest、worker SHA、runner副本与运行前源码快照。

本次原始输出位于被 gitignore 的 `scripts/bench/runs/longmemeval-*`；不要顺手提交真实
会话或数据集。可审查的聚合结果与各原始文件 SHA 在
[results/longmemeval-2026-10-30.json](results/longmemeval-2026-10-30.json)。
后续优先改跨会话/时间推理和延迟，用新题确认泛化，同时继续检查实际注入的相关性。

验证：全仓 `go test -tags sqlite_fts5 ./...` 通过；db/contextpack/search/两个runner 的 race、vet 通过；7项 Python 回归通过。改动仅本地，未推送、未部署。
