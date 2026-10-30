# 高分项目如何保留注入证据

调研日期：2026-10-30。先完成 [超时修复及本机部署](TIMEOUT.md)，再核查高分公开报告及可读实现。本轮没有运行付费 LLM benchmark，也没有把竞品机制直接改进生产策略。

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
