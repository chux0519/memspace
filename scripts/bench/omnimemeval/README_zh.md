# MemSpace 的 OmniMemEval 适配器

适配固定版本7c79d46e，不会自动发布代码或成绩。先从MemSpace构建本地worker，
用install_adapter.py把薄Python client和分发注册应用到独立评测checkout。
配置env_examples/.env.memspace：worker路径、全新私有评测根目录、embedding服务，
以及统一作答/裁判模型。先用--lib memspace完成一题导入检索（--to-step 2），
再跑完整QA；检索smoke不是问答成绩。目前先验证LongMemEval，其他题集另行验收。

默认context_pack返回真实ModePrompt注入包的可见Text，不根据引用偷取全文。
真实来源最多6条、6000字符；小top_k额外限制来源数，但不缩小候选召回。
返回列表中的一个字符串是整个包，来源数量以收据里的Items为准。
search仅为单列诊断模式，返回真实搜索snippet；两种成绩不能混合。查询不写入记忆。

每个评测user使用独立worker进程和一次性SQLite库，避免进程级语义服务跨库污染。
原始角色、session和RFC3339时间必须保留；同一批次重试幂等，内容冲突报错。
必须安装session/time分发，不是只注册client。ready完成索引；embedding错误不能
静默降级；空embedding URL明确表示keyword-only。源会话标ended，并在add完成前运行产品规则摘要/记忆生成器；不生成虚构未完成
工作摘要；system/tool保留为源事件，仅生产支持的事件类型进入搜索。

清理只停止并删除对应user的已确认所有权目录，不影响其他user，不连接线上服务。
根目录拒绝已有无所有权数据及生产库路径/其祖先。私有逐次RPC收据和stderr保留用于
审计；不要提交凭据或私有数据。切换模式/模型须换评测根目录。

worker调用真实认证入库handler和共享检索/注入代码。JSONL通信不包含真实网络认证
开销；client耗时包含RPC/ready，worker duration_ms只计ready后的组包。
它们都不是线上hook延迟。add/ready入库与建索引耗时、worker冷启动应单列。
完整构建、离线测试、上游贡献及结果要求见同目录memspace.md（本源码目录README.md）。
