# 高分项目如何保留注入证据

调研日期：2026-10-30。先完成 [超时修复及本机部署](TIMEOUT.md)，再核查高分公开报告及可读实现。初次调研未运行付费 LLM benchmark。

后续已在 `a3e2709` 实现原文证据回填及同会话最多两条，见 [100 题回归](EVIDENCE.md)。下方保留当时调研；更后续的官方 Python 重排、agentmemory 软配额源码核查及取消两条硬截止的实测见 [RERANK.md](RERANK.md)。

## 先核对“最高分”口径

| 项目 | 公开成绩及其含义 | 核查限制 |
|---|---|---|
| Supermemory | 官方报告自报 97%，标注 LongMemEval-S Recall@20 with aggregation，同时有回答模型和 GPT-4o judge | 不是我们 top-5 文档找齐或可见注入证据找齐的同口径分数；该报告链接组织主页，未绑定可复核实现 commit |
| Mastra OM | 自报 94.87%，六类题型准确率的宏平均；按该页公开分项计数合并为 468/500，即 93.6% | 依赖 Actor、Observer、Reflector 模型；报告平均整个上下文约 30k tokens，不能与我们的 memory-only token 分布直接比较 |
| Hindsight | 公开 benchmark 自报 Gemini-3 配置 91.4% | 问答分，使用模型与上下文策略不同；本轮查看代码用于机制参考，未独立复跑 |

不存在可据以上页面直接确认的“最终注入找齐率全球第一”。下面重点看高分实现保留证据的机制。

Mastra 的宏平均是代码明确定义的：`calculateMetrics` 对 `accuracy_by_type` 的 accuracy 求平均；不是 `correct_answers / total_questions`。其 GPT-5-mini 报告列出六类正确数 75、116、53、30、67、127，共 468/500。宏平均 94.87% 与按题 93.6% 都可成立，但不能与其他项目的微平均不加说明地排序。

来源：

- [Supermemory 完整报告](https://supermemory.ai/research/longmembench/)
- [Mastra OM 研究报告](https://mastra.ai/research/observational-memory)
- [Mastra 评分实现，固定 commit](https://github.com/mastra-ai/mastra/blob/624a0120d6ed9221bc8fdc2f3d66c5b74ccf90ce/explorations/longmemeval/src/cli.ts#L48)
- [Hindsight 公布的 benchmark](https://github.com/vectorize-io/hindsight-benchmarks)

## 1. Supermemory：命中的事实必须带回原文证据

其报告描述的流程是：按语义块导入会话，提取消除指代歧义的原子记忆，关联原文 source chunk；搜索命中记忆后把该 chunk 一起返回给作答模型。还区分文档时间与事件时间，用 updates / extends / derives 表达事实关系。

**对 MemSpace 的启发（推论）：** 派生索引用于定位，原文块用于证实。我们刚修复了短正文被 snippet 截掉的问题，但长正文仍退回展示 snippet；下一步可以让语义搜索的 best-matching chunk 穿过融合层进入内部证据打包，而不是在 `semanticResults` 截成 240 bytes 后丢掉完整 chunk。当前 80 题主要是短正文，不能用它证明长正文收益。

[架构及 source-chunk 回填说明](https://supermemory.ai/research/longmembench/#architecture)

## 2. Mastra：后台维护信息密集的稳定观测日志

报告中的配置不是每题检索 top-5 再注入几个片段：Observer 压缩历史、Reflector 整理日志，Actor 读取稳定的 observations 与近期消息。

已查看主干 commit `624a0120d6ed9221bc8fdc2f3d66c5b74ccf90ce`：

- `formatObservationsForContext` 格式化 active observations，并加入 current-task、suggested-response 等续接字段。
- Observer 提示要求保留区分性细节，特别是名称、标识符、数值和关键原话。
- Reflector 重写日志时强调保留重要事实，压缩工具调用过程，而不是只留下“处理过某问题”。

主干已有更多可选 retrieval 功能，不能把当前主干全部行为都当成 2 月报告的实验配置。

**对 MemSpace 的启发（推论）：** 完整保留错误标识、修复动作、验收结果与下一步之间的联系，比笼统压缩成一句结论更有价值。我们已有规则摘要和续接卡片，可先验证其字段是否漏掉关键证据。OM 使用 LLM 和较大的总上下文预算，整体照搬会改变我们的成本与产品定位。

固定源码：

- [context formatter](https://github.com/mastra-ai/mastra/blob/624a0120d6ed9221bc8fdc2f3d66c5b74ccf90ce/packages/memory/src/processors/observational-memory/observational-memory.ts#L2082)
- [Observer 的细节保留规则](https://github.com/mastra-ai/mastra/blob/624a0120d6ed9221bc8fdc2f3d66c5b74ccf90ce/packages/memory/src/processors/observational-memory/observer-agent.ts#L169)
- [Reflector](https://github.com/mastra-ai/mastra/blob/624a0120d6ed9221bc8fdc2f3d66c5b74ccf90ce/packages/memory/src/processors/observational-memory/reflector-agent.ts)

## 3. Hindsight：候选召回与独立相关性判断分开

查看主干 commit `0b45cb942b160d4f2204d7a0b992bcb6a70a1e29` 的 fusion/reranking 实现：融合接受 semantic、BM25、graph、temporal 多路结果，保留各路排名及原始 semantic/keyword 信号；支持每路候选上限，避免一路挤满候选池；另有 cross-encoder reranker。部署也支持 passthrough reranker，因此不能说每种配置都执行神经重排。

**对 MemSpace 的启发（推论）：** 不只依赖一个 cosine 阈值判定最终相关性。我们的剩余 22 道漏题中，18 道已找到相关候选但未通过门槛；可以在开发分区评估独立相关性判断，再决定是否接纳弱语义候选。先测中英混合质量、困难负例及 CPU 耗时，不能直接接入一个英文 reranker，或引入另一条超时链路。

固定源码：

- [fusion：各路信号与候选预算](https://github.com/vectorize-io/hindsight/blob/0b45cb942b160d4f2204d7a0b992bcb6a70a1e29/hindsight-api-slim/hindsight_api/engine/search/fusion.py)
- [reranking：独立相关性与可配置 provider](https://github.com/vectorize-io/hindsight/blob/0b45cb942b160d4f2204d7a0b992bcb6a70a1e29/hindsight-api-slim/hindsight_api/engine/search/reranking.py)

## 对我们下一步最有用的实验

1. **先补评测缺口：** 同会话分别保存“问题、决策、失败、修复、验收”，要求组合多条证据；补长正文关键事实在中间/尾部的题，以及更多近义但无关、过期与跨项目负例。保留 coding-v1，不改旧答案来追分。
2. **证据打包：** 尝试回填命中的原文 chunk、把同会话多条必要来源按证据组打包。当前“一会话一条”仍可能挡住分散在同会话里的多条事实，原 80 题把答案分在不同会话，掩盖了这个风险。
3. **相关性校准：** 在开发集评估弱候选的独立重排/校准，再在测试分区检查找齐率、误注入和延迟。优先针对剩余 18 道门槛问题；另外 4 道未召回的问题另测中英跨语言候选召回。

以上是基于代码和现有失败题的实验建议，尚无新实验成绩。不会把竞品问答高分当成我们注入机制已经落后的证据。

## 追加调研：两条上限是否合理

2026-10-30，再查官方当前文档，并检查 `a3e2709` 的剩余漏题。

`a3e2709` 的每会话 **2 条**确实写死；prompt 模式总共最多 **6 条**、检索最多 **12 个候选**，另有 **6000 字符**总体积限制。这是防止一个会话占满上下文的启发式规则，没有实验证明两条足够。当时新增正例每会话只要求两条证据，不能验证三条及以上的完整性；后续已补独立的 3/4 条专项夹具并修复，见 RERANK.md。

| 项目 | 公开的数量/体积控制 | 完整性相关机制 | 与我们有何不同 |
|---|---|---|---|
| Hindsight | `budget` 控制搜索深度；`max_tokens` 单独控制返回事实体积，默认 4096；原文块有独立预算 | 多路召回、独立重排，随后按相关性和预算装入；单条放不下会尝试后面的较短事实 | 候选深度与最终注入体积分开。公开 API 没有我们这种“每会话两条”参数 |
| Supermemory 当前 v5 | `limit` 默认 10、最大 100；支持 `threshold` | 同时搜索抽取记忆与原文块；可选多次查询改写、重排、聚合以及相关事实附件 | 也有条数上限，不是无上限动态找齐；参数可由调用方设定 |
| Mem0 | `top_k`、`threshold` 配置返回数量和筛选；可选 `rerank` | 查询处理、作用域过滤和第二次相关性排序 | 是检索 API；调用方还需负责最终上下文打包，不能把 top_k 当最终注入策略 |
| claude-mem | 官方搜索说明采用目录→时间线→选定记录详情的三步读取，各步有数量/深度约束 | agent 看过前一步后决定读哪些详情，可补充周边事件 | 通过工具逐步读取，与一次自动 hook 注入是不同成本和交互模式 |

来源（本次核查）：

- [Hindsight recall API](https://hindsight.vectorize.io/developer/api/recall)：事实 token 预算只数事实正文，不含元数据；最低一条可能超预算，不能照搬为我们的硬 hook 上限。
- [Hindsight 检索流水线](https://hindsight.vectorize.io/developer/retrieval)：候选深度、多路融合与 cross-encoder；没有 cross-encoder 时也支持回退，因此不是所有部署都执行神经重排。
- [Supermemory 当前 Search](https://supermemory.ai/docs/recall/search) / [v5 参数](https://supermemory.ai/docs/v5/api-reference/recall-search/search-memories)：这里的 hybrid 是记忆与 chunks 两种检索表面，不据此断言是 BM25+向量融合；v5 默认值与先前版本不同，不用于倒推其 benchmark 配置。
- [Mem0 Search](https://docs.mem0.ai/core-concepts/memory-operations/search) / [Advanced Retrieval](https://docs.mem0.ai/platform/features/advanced-retrieval)：文档自报重排额外 150–200ms，尚非我们机器上的实测成本；不同版本/平台默认值不混用。
- [claude-mem 搜索工作流](https://docs.claude-mem.ai/usage/search-tools)：以当前说明的三步 agent 读取为准。10 月 9 日 changelog 还描述 guided/auto 与 `maxDetails`，当前使用文档已称无这两种模式，因此不把旧默认详情条数作为当前机制。其节省 token 的宣传数字没有在这里当作实测结果。

这些机制都不能保证“证据已经找齐”；它们提供的是查找深度、相关性、上下文体积之间的控制方式。

### 我们实际卡在哪里

在独立临时 worktree 中，只将每会话上限从 `2` 改为请求总上限（prompt 为 `6`），其余源码、语料与冻结查询向量不变，100 题跑一遍：

- 注入找齐仍为 **57/79，72.2%**，和当前实现首遍逐题比较，601 条回执的评分及 context/hook 实际投递引用均无变化。
- 所有 22 道漏题都来自旧 80 题；对应答案不共享会话，因此不是两条上限造成。
- 重新检查当前 12 候选及当前资格规则：**18 题**有正确来源但不符合门槛，**4 题**正确来源未进入候选。这里重新计算当前规则，没有使用诊断文件里保留的旧版 `baseline_gate_reason`。
- 例：登录慢正确来源相似度 0.591、死锁 0.599，都低于 0.60；这不意味着统一降阈值就是正确方案。另有 map 竞态、缓存方案的部分关键词覆盖不足。
- 4 道候选漏题是协程泄漏、向量模型崩溃、前端状态管理、字段序列化丢失，其中 3 道是中文问题找英文记录。未进候选也可能源于 search floor，不能直接归因为“向量库没搜到”。

本机产物：

- [放宽会话上限的回执](runs/session-cap-unrestricted-study/receipts.jsonl) / [manifest](runs/session-cap-unrestricted-study/manifest.json)
- [当前诊断候选](runs/evidence-v2-current-diagnosis/diagnostics.json) / [按当前规则复核](runs/session-cap-unrestricted-study/review.json)

这是冻结向量的一遍质量对照，不是线上延迟或新策略的最终验收；没有修改当前工作区的生产策略。

### 建议的下一轮顺序（待实验，尚无收益结论）

1. **先做独立相关性判断。** 多找候选，用支持中英混合的 reranker 同时阅读“问题＋原文”，再决定是否注入。它必须发生在现有 0.60 注入筛选之前，否则那 18 题仍没有翻案机会。候选阶段保留宽松下限，最终用重排分数及负例校准；不把“换个分数阈值”当作已经解决相关性。
2. **补同义词和跨语言召回。** 比较更深候选与受控查询扩展，优先检查上述 4 题。改写与重排都可能增加耗时，分别报告实时 p95 和故障回退，不能复用冻结向量的耗时当线上指标。
3. **把会话条数变成预算内的软配额。** 先覆盖不同会话，再允许相关且不重复的第三、第四条补入；按相关性与新增信息价值选择，保留整体积硬上限。补同会话 3/4/更多证据、多个会话争抢预算、重复及无关邻居测试后，再决定是否修改当前两条上限。仅把 2 改成另一个常数无法验证找齐能力。
4. **自动注入之外提供按需补读。** hook 先带足够的紧凑证据，agent 再用明确来源 ID 读取更多原文或周边事件。单独测工具补读后的找齐率，不能与现有一次 hook 找齐率合并。

保持原题目、负例及预算作对照，同时报告找齐率、无关证据、token 与延迟；目前已经看过所有旧测试题，后续调参还需一批新标注题验证泛化。
