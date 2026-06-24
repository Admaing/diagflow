DiagFlow：大数据平台智能排障 Agent 系统

项目背景
针对 Flink、YARN、HDFS 等大数据组件故障排查需人工登录节点检索日志、跨多平台比对监控指标、高度依赖个人经验
的痛点，设计并实现 AI 驱动的自动化排障系统。参考得物技术团队的 Troubleshooter 架构，在 Agent 框架自研、
多 Agent 并行编排、混合检索 RAG、幻觉控制体系四个维度进行增强设计，实现"用户描述问题 → Agent 自动采集证据 →
结构化诊断报告"的全流程自动化。

核心成果
• 从零构建 ReAct Agent Runtime 与 Tool Framework：自行实现 Reasoning-Acting 推理循环、JSON Schema
  工具注册与超时隔离机制、多 Agent 共享 Evidence Pool 证据管理，核心代码约 500 行，不依赖 LangChain/
  CrewAI 等通用框架，实现对工具超时降级、重试策略、上下文窗口等关键路径的完全掌控。

• 设计 Supervisor + 3 Specialist Agent 并行诊断架构：将日志分析、指标分析、配置分析三类数据采集 Agent
  完全解耦并行执行，通过结构化 EvidencePool 统一汇合证据后由 Supervisor 综合研判。单次诊断发起 3 路并行
  Agent 调用，相比得物串行 ReAct 模式可缩短端到端采集耗时。

• 构建 BM25 + ChromaDB + RRF 混合检索 RAG 体系：以 Reciprocal Rank Fusion 融合语义检索与关键词
  检索两路结果；额外设计指纹匹配快速通道——将 (组件名, 错误模式, 版本号) 三元组 MD5 哈希后精确匹配历史
  案例，命中即毫秒级返回已知结论，无需启动完整 AI 诊断流程，实现历史经验的自动化沉淀与复用。

• 实现四层幻觉控制与诊断结论校验机制：Layer 1 — 规则格式校验（零 LLM 调用，毫秒级检查章节完整性与指标
  表格格式）；Layer 2 — 跨源证据交叉验证（日志时间线与指标异常时间点一致性比对）；Layer 3 — 独立
  Validation Agent 评审根因明确性与建议可执行性；Layer 4 — 反馈重试机制（格式问题不消耗配额，
  内容问题最多重试 2 次后从容兜底）。四层兜底确保每次诊断结论可审计、可复现。

• 设计基于 umrAgent 的安全执行链路：LLM 诊断过程中构造 (node_name, Action, params) 结构化指令，
  工具层将其翻译为 HMAC-SHA1 签名请求（跨语言复刻 Node.js util/agent.js 与 Go pkg/client/uagent
  签名算法），通过 HTTP 调用节点侧 umrAgent（[ipv6]:65431）获取日志与进程状态。全程禁止 SSH 直连与
  数据库直连，遵循生产环境最小权限原则。

• 基于 K8s（StatefulSet + PVC + ConfigMap）完成容器化部署，集成 ZooKeeper/Consul 服务发现与内部
  LLM Proxy 网关解决 Pod 无公网出口的网络隔离问题。数据获取层抽象为统一接口（SimulatedCluster vs
  RealCluster），Agent 与 Tool 层无感知底层差异，实现 Demo 模式（4 种预置故障场景的逼真日志模拟）
  与 Production 模式的一键切换。
