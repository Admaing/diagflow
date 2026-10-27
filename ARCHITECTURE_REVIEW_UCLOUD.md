# DiagFlow 架构评审：与 UCloud 大数据平台场景的匹配度

> 评审日期：2026-10-21
> 评审对象：`/Users/user/Prj/diagflow`（分支 `go-rewrite`，HEAD `c41cb11`）
> 对照对象：`/Users/user/Prj/ucloud/`（UHadoop 传统栈 + EMR/Go 新栈 + Flink Serverless）
> 方法：逐文件读代码 + 对照平台侧代码/配置；**未做真实集群联调**，涉及"线上行为"的推断均已标注。
> 结论口径：**架构设计**、**UCloud 场景匹配度**、**部署模式**、**排查流程易用性**四件事分开评。
>
> **文档一致性更新（2026-10-27）**：`README.md`、`LINK_CHAIN.md`、`OBSERVABILITY.md`
> 已按当前代码校准，§2.3 记录的"文档夸大"已随此次更新修复；能力缺口本身仍然
> 存在。本文其余内容保留为 2026-10-21 的评审快照，文中行号基于当时的代码。

---

## 0. 一句话结论

**诊断内核（单 Agent + 确定性前置 + 证据池 + 结构化收口）设计正确、工程质量不错，是一个合格的"二线 SRE 单集群辅助诊断原型"；但作为 UCloud 平台级能力尚不合格——部署形态与平台两代架构错位，排查证据只有"当下日志"，告警/工单/指标/变更四类入口全缺，知识闭环在运行态实际不生效。**

用一句话概括匹配度：**接入层对了一半，证据层缺一半，交付层基本没做。**

---

## 1. 结论与评分

### 1.1 分维度评分（1–5，5 = 可直接生产）

| 维度 | 评分 | 判断依据 |
| :-- | :--: | :-- |
| 诊断内核设计 | **4.0** | 单 Agent、确定性策略前置、证据池、结构化 `tool_choice` 收口；取舍正确（项目文档称比纯 ReAct 省约 60% 调用，本次未复测） |
| 代码工程质量 | **4.0** | 约 1.3 万行 Go（含测试），分层清晰，`go vet` 干净、`go test ./...` 全绿；预算/去重/降级/只读边界细节扎实 |
| 与 UCloud 元数据接入 | **4.5** | ZK 发现 `UHADOOPAccessHTTP`、protobuf `NameNodeContent` 解码、access action API `{"action":...}` → `{RetCode,Data}` **全部与平台代码一致**（详见 §3.1） |
| 节点 / 日志接入 | **2.5** | 绕开平台主通道 umrAgent，改用 access API 下发的 **root 密码 SSH**；host key 不校验 |
| 证据维度 | **1.5** | 只有日志 + `uptime/free/df` 瞬时快照；无指标时序、无告警、无集群事件、无变更/部署记录 |
| 部署交付 | **1.5** | 4 份互不一致的清单；`deploy.sh` 按现状**必然失败**；未遵循 KUN/EMR 的 per-region 配置约定 |
| 多地域 / 多租户 | **1.0** | 单地域写死在 manifest；无租户/集群级鉴权、无配额、无路由 |
| 知识闭环 | **1.0** | KB 每个请求新建内存实例；embedding 是桩函数；反馈无 API、`ConfirmCorrect` 是死代码 |
| 安全 | **2.0** | 明文 token 入库、SSH root、host key 不校验、日志片段外发公网 DeepWiki |
| 排查易用性 | **2.5** | 流式过程 + 证据链确实省时间；但要人工填 taxonomy、不能指定时间窗、无历史可查 |
| **综合（生产就绪）** | **≈2.5** | 原型 4 分，平台化 2 分 |

### 1.2 三个最重要的判断

1. **它已经不是一个 demo 了**：代码里有大量"踩过事故"的痕迹（思考模式 token 预算、重复调用去重、内容指纹防假进展、`STARTUP_MSG` 噪声过滤、行宽裁剪、状态枚举翻译、SSE 心跳）。从注释看，作者应当真的跑过真实故障（多处注释直接引用了具体事故现象）。
2. **但它仍然是一个"单机工具"，不是"平台能力"**：没有告警触发、没有工单闭环、没有多地域交付、没有知识沉淀、没有质量评测。UCloud 的故障是**告警驱动**的（UNOC/微信），不是"人打开网页输入集群 ID"驱动的。
3. **部署形态与平台正在分叉**：diagflow 按"可用区（AZ）单实例"设计，这匹配 **传统 Node 栈**（每 AZ 2 实例）；而 UCloud 新栈（EMR、flink-serverless、uhadoop-go）是**按 region 单副本 + Consul + leader 选举**。diagflow 恰好部署在新栈的 namespace `prj-diaflow` 里，却只适配了老栈的接入方式。

---

## 2. 事实基线：DiagFlow 现在是什么

### 2.1 运行形态

两个二进制，一套镜像：

| 组件 | 入口 | 作用 | 状态 |
| :-- | :-- | :-- | :-- |
| `diagflow -serve` | `cmd/diagflow/main.go` | HTTP API + 内嵌 Web 控制台 + SSE | 主服务 |
| `diagflow`（无 flag） | 同上 | CLI demo / 生产单次诊断 | 运维自测用 |
| `diagflow-mcp -port 8090` | `cmd/diagflow-mcp/main.go` | 集群访问网关（MCP over HTTP） | 独立 Deployment |

代码分层（依赖单向，没有循环）：

```
cmd/                入口
internal/config     环境变量配置（无配置文件）
internal/cluster    集群抽象接口（simulated / real 两实现）
internal/infra      ZK 发现、access API、SSH 日志、umrAgent、MySQL、状态枚举
internal/mcpserver  集群访问网关（对 agent 暴露 12 个只读工具）
internal/mcpclient  MCP 客户端
internal/tools      v3 工具面（query_yarn/ssh_exec/...）+ 命令安全校验
internal/strategy   YAML 策略解析与参数渲染
internal/diagagent  单 Agent 五阶段流水线 + 意图排查
internal/rag        embedder / vectorstore / retriever / knowledgebase
internal/validator  结论校验（实际只有确定性两层）
internal/observability  报告渲染 + MySQL 摘要落库
```

### 2.2 一次诊断的真实链路

```
浏览器/curl
  │  POST /api/v1/diagnose/stream  {component, problem, cluster_id, detail, investigate}
  ▼
server.runDiagnosis                         server.go:256-302
  ├─ resolveCluster: production 必须带 cluster_id           :307-326
  │    └─ ZK 发现 UHADOOPAccessHTTP → POST / {"action":"describe_cluster"}
  │       （结果缓存 30s）
  ├─ 进程级 KB：server.knowledgeBase() 首次调用时 vectorstore.NewMemory() + knowledgebase.New
  ├─ BuildV3Tools(cluster, kb, mcpClient, deepwikiClient)
  ▼
Agent.Diagnose                              diag_agent.go:166-259
  ├─ Phase 1  KB 语义检索      ← 实际永远跳过（embedder.HasRealEmbeddings()==false）
  ├─ Phase 2  YAML 策略并行取证 ← 默认策略：cluster_info→list_logs→search_logs→metrics
  ├─ Phase 3  SDK ReAct        ← ≤12 轮 / ≤20 次工具调用 / 3 轮无新证据强制收口
  ├─ Phase 4  结构化合成 + L1/L2 校验（失败重试 1 次）
  └─ Phase 5  high 置信度写 KB ← 写进程级内存 KB，重启即丢
  ▼
report.Render → JSON + Markdown；SSE 逐步 narrate；MySQL 只存摘要
```

关键边界（做得好的地方）：

- 单次工具调用 30s 超时（`toolTimeout`），ReAct 结果回灌上限 8000 字符（`maxReActResultChars`）。
- 相同调用直接短路、相同结果不计为新证据（`convergence.go`），避免"读同一个日志算进展"。
- 工具失败降级为低置信度证据，不中断整体流程。
- 命令只读白名单 + 拒绝重定向/命令替换 + `mysql` 仅允许单条 SELECT/SHOW/DESC/EXPLAIN/WITH（`sshlog.go`）。
- 凭据 `passwd`/`access_user` 在 MCP 返回前被脱敏。

### 2.3 四处"文档曾经夸大、运行态不具备"的能力（文档已于 2026-10-27 校准）

这是评审中最需要澄清的部分——**不是实现得不好，而是根本没有生效**。下表中的
"文档/UI 的表述"记录的是评审时（2026-10-21）的旧文档；这些表述已经在 2026-10-27
的文档校准中删除或改写，但**能力缺口本身没有变化**：

| 能力 | 文档/UI 的表述 | 代码实际 | 证据 |
| :-- | :-- | :-- | :-- |
| 知识库语义检索（Phase 1） | README/LINK_CHAIN 大幅介绍"历史案例命中，0 LLM" | `Embedder.HasRealEmbeddings()` 恒为 `false`，Phase 1 永不进入 | `internal/rag/embedder/embedder.go:39-49`；`diag_agent.go:181` |
| MD5 指纹精确匹配（Phase 2.5） | LINK_CHAIN 有独立章节 | 代码里已删除，注释写明"removed" | `diag_agent.go:206-207` vs `LINK_CHAIN.md:115-141` |
| 四层幻觉校验中的 Layer 3（独立 LLM 复核） | README 表格列出 4 层 | `ValidateLayer` 只做 L1 格式 + L2 关键词；L3 从未被调用 | `internal/validator/validator.go:26-56` |
| 知识自动归档（Phase 5） | "下次相同错误 → 0 LLM" | 写入进程级内存 KB，重启即丢；持久化还要求 `HasRealEmbeddings()`（恒 false） | `diag_agent.go`；`server.go` `knowledgeBase()` |

补充两处"接口在、行为不对"：

- `POST /api/v1/investigate` 现在通过 `forceInvestigate` 强制 `Investigate=true`，
  与 `/diagnose` 区分（2026-10-27 已修正；评审时两者绑的是同一个 handler）。
- `ConfirmCorrect`（把确认过的排查思路写回 KB）没有任何 HTTP 入口，生产环境是死代码（`internal/diagagent/investigate_plan.go`）。

---

## 3. 与 UCloud 平台现状的对照

先给平台侧的"地面真相"（均已核对代码，非仅文档）：

- **两代部署模型并存**：传统 Node 栈（task/manage/config/access/monitor/message）跑在 VM + pm2，**每可用区 2 实例**，ZK 路径 `/NS/uhadoop/set<zoneID>/...`；新栈（EMR、uhadoop-go、flink-serverless）跑在 K8s，**每 region 单副本 + Consul + leader 选举**。
- **接入平面**：`go_framework` 单路由 `POST /`，请求 `{"action": ...}`，响应 `{"RetCode", "Data"}`，**内部无鉴权**；`UHADOOPAccessHTTP` 注册在 ZK，负载为 protobuf `NameNodeContent{Ip,Port}`，临时节点。
- **节点主通道**：`umrAgent :65431`，HMAC-SHA1（per-node `umr_agent_key`），能力覆盖 `RunCmd`/`ExecShell`/`GetXMLConfig`/`NameNodeJMX` 等；SSH 仅用于 bootstrap/升级。
- **已有诊断/自愈**：`uhadoop-monitor`（老栈，源码不在工作区）配置里就有 `CHECK_CLUSTER_AUTO:1`、`DEAL_AUTO:1`、表 `t_uhadoop_diagnose`；新栈有 cron 巡检 + UNOC/微信告警。**全平台没有任何 LLM 诊断**。
- **告警通道**：UNOC（`AddUnomInfo`，`hadoop-告警-p1/p2`）+ 微信 webhook；`uhadoop-message-system` 是**站内消息收件箱**，不是告警总线。

### 3.1 对得上的部分（这是加分项）

| 平台事实 | DiagFlow 实现 | 判定 |
| :-- | :-- | :-- |
| ZK 路径 `/NS/uhadoop/set<zoneID>/UHADOOPAccessHTTP` | `fmt.Sprintf("%s/set%s/%s", root, zoneID, service)` | ✅ 一致 |
| znode 负载 = protobuf `NameNodeContent`（ip=1，port=10） | 手写 varint 解码，字段 1/10 | ✅ 与 `go_framework/framework/configor.go:132-135` 完全对应 |
| 注册临时节点、多实例 | `Children` + 轮询 `Pick`，30s TTL 缓存 | ✅ 可用（无 watch，ZK 变更最多延迟一个 30s TTL） |
| access API：`POST /` + `{"action":"describe_cluster"}` | `AccessClient.Call` 完全一致 | ✅ |
| 响应 `{"RetCode","Data"}`，Data 内是 `cluster_info` | 解析 `RetCode`/`Data`，容忍单元素数组 | ✅ |
| `describe_cluster` / `list_node` / `describe_node` / `get_cluster_service` | 四个都实现了 | ✅ |
| 集群/节点/应用状态是数字枚举 | `states.go` 全量翻译 + "该状态下能否读日志"的提示 | ✅ 很实用 |

**结论：元数据接入这块是认真做过功课的，不是猜的。** 这一点比大多数"对接平台"的项目强。

### 3.2 部署模型：与两代平台的错位

diagflow 的自我定位是"一个实例服务一个可用区"（README 的 “Zone-level deployment” 一节；`config.go` 的生产校验强制 `REGION`+`ZONE`+`ZK_HOSTS`）。对照平台：

| 平台代际 | 部署粒度 | 副本 | 服务发现 | diagflow 是否匹配 |
| :-- | :-- | :-- | :-- | :-- |
| 传统 Node 栈 | 每 **AZ** | 2 | ZK | ✅ 匹配（甚至副本数 2 也对） |
| EMR（新） | 每 **region** | 1 + leader 选举 | Consul | ❌ 不匹配（会变成每 AZ 一套，31 套） |
| flink-serverless | 每 **region** | 2 | Consul | ❌ 不匹配，且接入方式完全不同（K8s API） |

而 diagflow 的 namespace 恰恰是 `prj-diaflow`——`emr/devops/config/kubernetes/flink-serverless-statefulset.yaml:4-5` 显示 flink-serverless 也部署在这里。**同一命名空间里，一个是 region 级 K8s 原生服务，一个是 AZ 级 UHadoop 适配服务**，模型是拧着的。

更实际的问题是：真按 AZ 铺开，就是 31 个 AZ × (1 个 API Deployment + 1 个 MCP Deployment + 1 套 MySQL + 1 份 LLM key + 1 套 ZK 地址)。当前没有任何批量生成/发布工具（`deploy/` 只有 test03 一份），也没有跨 region 路由层。

### 3.3 节点访问：SSH root 密码 vs umrAgent

这是**架构上最值得改的一处**。

diagflow 的做法（`infra/sshlog.go`）：

- 从 `describe_cluster` 拿 `node_ipv6` + `access_user` + base64 `passwd`；
- 直接 `ssh.Dial` 到节点 22 端口，密码认证，**`HostKeyCallback: ssh.InsecureIgnoreHostKey()`**（`sshlog.go:81`）；
- 每个工具调用都可能新建 SSH 连接（无连接池），跨节点扇出并发 8。

平台的做法：

- `umrAgent :65431` + HMAC-SHA1，密钥 per-node，**操作可审计、可按 Action 授权、无需给调用方 root 密码**；
- SSH 只用于 bootstrap/升级，不作为查询通道。

风险不是"能不能用"，而是：

1. **爆破半径**：MCP 网关被盗即等于拿到该 AZ 所有集群的 root 密码（内存中），而 umrAgent 至少只有被授予 Action 的能力。
2. **审计缺失**：SSH 通道在节点侧只有 sshd 日志，平台侧无 Action 级审计；MCP 的审计只记录"调了 ssh_exec + 命令"，与平台账号体系无关。
3. **合规**：等保/专有云场景，"诊断系统持有客户集群 root 密码"几乎必然被挑战。
4. **可达性**：diagflow 假设 K8s Pod 能到节点 IPv6:22。平台 Pod 是双栈（`cni.networking.kun/ipv4Enabled: "true"`），管理面走 IPv6；`prj-diaflow` 是否有到租户 VPC 节点的 22 端口策略，**未经联调验证**（见 §9 未验证假设）。

**注意**：diagflow 并非没做 agent 通道——`infra.UmrAgentClient` 的 HMAC 签名实现是对的，`call_umr_agent` 工具也在。问题在于**它没有日志读取能力**：工作区 `software/umrAgent/methods/`（106 个方法）与 v15 离线包（109 个 `.js`）里都**没有 `GetLogs`/`ReadLog`**，只有一个 2026-06-24 新增的 `FindLogs.js`（返回路径，不返回内容）。而工具描述里写的 `GetLogs(Path,Keywords,MaxLines,Since)` 在 agent 侧并不存在——所以最终日志只能走 SSH。

> 这其实是个"补洞"的合理选择，但补在了错误的一层：应该在 agent 增加一个只读 `ReadLog` Action（HMAC + 审计 + 长度上限），而不是让诊断服务持有 root。工作量不大（Node 侧一个 method + Go 侧换 transport，`RemoteExec` 接口已经预留好了）。

### 3.4 证据面缺口：只有"日志 + 瞬时快照"

平台已有的、排障真正需要的证据，diagflow 一个都没接：

| 证据类型 | 平台已有 | DiagFlow | 影响 |
| :-- | :-- | :-- | :-- |
| 指标时序（CPU/内存/磁盘/JVM/YARN 队列） | `uhadoop-monitor`(21003)、umon2、uhadoop-base(Prometheus) | 仅 `uptime; free -m; df -h` 瞬时值 | 无法回答"昨晚 2 点为什么失败"；看不到趋势、毛刺、饱和 |
| 告警/事件 | UNOC、微信、`t_uhadoop_diagnose` | 无 | 不知道"当时报了什么警"，无法对齐现象 |
| 部署/变更/任务状态 | `t_uhadoop_main_task`、manage `jobs/tasks`、exception-handler | 无 | 扩容失败、配置变更、重启中被误判为"组件 bug" |
| YARN 队列/应用历史 | YARN RM REST :23188、`yarn-resource-manager` | 仅 `yarn application -list/-status`（SSH） | 看不到 AM/NM 日志聚合、队列指标 |
| HDFS 健康 | `hdfs dfsadmin -report`（命令白名单里有） | 有命令能力，但无结构化工具 | LLM 要靠猜命令 |
| K8s（Flink Serverless） | K8s API、Operator CRD status、Pod events | 无 | 新形态完全盲区 |

后果很直接：**诊断的"深度"受限于证据的"宽度"**。当前最多能做到"在日志里找到一段 Exception，然后让模型解释它"，而不是"这个 OOM 是因为队列资源被挤压 + TM 内存配置 + checkpoint 放大导致"。

### 3.5 与既有诊断能力的关系没有定义

平台已经有 `uhadoop-monitor` 的自动巡检/自动处置（`CHECK_CLUSTER_AUTO`/`DEAL_AUTO`，表 `t_uhadoop_diagnose`），还有 exception-handler 做失败补偿。diagflow 的定位是"只读建议"，这是对的，但：

- 没有说明**谁在什么时候调用它**：是 NOC 手动、工单系统自动、还是告警触发？
- 没有和 `t_uhadoop_diagnose` 对齐：两套"诊断记录"表，未来必然要做数据合并。
- 没有"建议 → 一键执行 → 执行结果回验"的闭环，也没有"这个建议会不会和 exception-handler 重复处置"的判断。

### 3.6 如果目标是 Flink Serverless

`ucloud-flink-serverless/flink-serverless-design.md`（未入库，2026-10-20）把"智能诊断"放在二期，且底座是 **UK8S + Flink Operator + Prometheus**。若这是 diagflow 的下一站，当前架构的适配成本是**重写接入层**：

- 没有 kubeconfig / K8s client，看不到 Pod、Event、CRD `FlinkDeployment.status`；
- 没有 Flink REST（`/jobs/:id/vertices/:vid/backpressure`、checkpoint 统计）；
- 没有 Prometheus 查询（吞吐、延迟、checkpoint duration、TM 堆）；
- 没有 SQL Gateway / 作业元数据（谁提交的、并行度、状态后端）。

诊断内核（证据池、ReAct、合成、校验）可以复用，但 **`cluster.Cluster` 接口需要抽象成 `AccessProvider`**，否则会写第二套 if-else。好消息是：接口已经存在（`internal/cluster/cluster.go`），只是当前只有两个实现，且 MCP 工具名与 `cluster_id` 语义强绑定。

---

## 4. 部署模式逐项核对

### 4.1 现有清单盘点（4 份，互不一致）

| 文件 | 类型 | 副本 | 镜像 | 问题 |
| :-- | :-- | :--: | :-- | :-- |
| `deploy/kubernetes/diagflow-statefulset.yaml` | **Deployment**（名字叫 statefulset） | 2 | `${IMAGE_TAG}` | 见 §4.2 致命项 |
| `deploy/kubernetes/diagflow-go.yaml` | Deployment | 1 | `go-0.5.1` 固定 | **明文 MCP token**（:62） |
| `deploy/kubernetes/diagflow-mcp.yaml` | Deployment | 1 | `go-0.5.1` | **明文 token 写进 Secret 的 stringData**（:12） |
| `deploy/kubernetes/diagflow-mcp-deployment.yaml` | Deployment | 2 | `diagflow-mcp:${IMAGE_TAG}` | 镜像名与 Dockerfile 产物不一致（Dockerfile 只产出 `diagflow` 一个镜像，内含两个二进制） |
| `deploy/deploy.sh` | 脚本 | — | — | 按现状执行**必失败** |

### 4.2 致命项：`deploy.sh` 跑不起来

按脚本顺序执行会连续踩三个坑：

1. `envsubst < deploy/kubernetes/diagflow-statefulset.yaml` 渲染后 apply。该清单设置的是 **`ZK_HOSTS`**（:64），而代码只读 **`DIAGFLOW_ZK_HOSTS`**（`config.go:378-380`）。生产模式下 `Validate()` 要求 ZK 地址非空（`config.go:300-311`），进程启动即 `os.Exit(1)`，Pod 进入 CrashLoopBackOff。
2. 清单声明的是 `kind: Deployment`（:7），脚本却等 `rollout status statefulset/diagflow`（`deploy.sh:48`），会报 NotFound。
3. 脚本末尾提示 `port-forward svc/diagflow 8501:8501`（:54-55）——8501 是旧 Python/Streamlit 的端口，Go 服务是 8080。

换句话说：**仓库里唯一"一键部署"的路径是不可用的**，实际能跑的只有手工 apply `diagflow-go.yaml`。这不是文档小瑕疵，而是"部署模式没有被验证过"的直接证据。

### 4.3 与 KUN/EMR 约定不一致

平台新栈的标准形态（`emr/devops/config/kubernetes/*/`）：

| 约定 | 平台做法 | DiagFlow | 影响 |
| :-- | :-- | :-- | :-- |
| 工作负载 | StatefulSet + `serviceName` + preStop 优雅停 | Deployment，无 preStop | 滚动更新时在途诊断直接断流 |
| 配置 | `/data/emr/config/<svc>.yaml` + `-config` 参数 + projected ConfigMap | 纯环境变量；`deploy/kubernetes/test03/application.yaml` 是**死配置**（无人读取） | 无法按 region 生成配置；配置变更要改镜像/清单 |
| 端口 | 190xx + pprof(+2)（task 19050、ops 19030、manager 19010） | 8080，无 pprof | 与平台端口规划/监控模板不符；排障无 pprof |
| 副本 | EMR 1 + leader；传统栈 2/AZ | 1 或 2 混用，无 leader/PDB | 多副本会话不一致，无驱逐保护 |
| 探针 | 依赖真实可用性 | `/readyz` 只检查配置项是否存在（`server.go:161-180`） | **假就绪**：ZK 不通、LLM key 没配、MCP 不可达时仍返回 200 |
| 暴露 | Console/API 网关统一入口 | 只有 ClusterIP | 外部无法访问；无统一鉴权/租户/限流 |
| 指标 | Prometheus 采集 | 有 `/metrics` 但无 ServiceMonitor/annotation | Prometheus 采不到，等于没有 |
| 存储 | PVC 存日志/状态 | `emptyDir` 挂 `/tmp`，只读根文件系统 | 任何本地状态（KB、缓存、会话）重启即失 |
| 密钥 | 平台密钥管理 | 明文写进 Git（两处 token、ZK IP） | 泄密；轮换困难 |
| 数据库 | 平台有 per-zone DB | `sql/schema.sql` 手工执行，无 Job/迁移；连不上就**静默禁用**（`store.go:44-70`） | 生产可能"看起来正常，实际没有历史" |

### 4.4 部署模式建议（结论）

- 若服务 **UHadoop 传统栈**：保持 per-AZ，但改成 **StatefulSet + per-AZ ConfigMap 生成器**（对齐 `deploy-manage-hosts/deploy/<zoneID>/` 的生成方式），副本 2，加 PDB，加 ServiceMonitor。
- 若服务 **EMR / Flink Serverless**：改成 **per-region 单副本 + leader 选举**，走 Consul 或直接 K8s Service，别再用 AZ 概念。
- 无论哪种：**一份清单**（删掉其余三份）、**密钥进 Secret 且不入库**、**Ingress 走平台网关**、**探针探测真实依赖**、**加 schema init Job**。

---

## 5. 排查流程是否方便

### 5.1 一条典型故障的走查（Flink 作业 OOM，UHadoop 集群）

| 步骤 | 现状会发生什么 | 结论 |
| :-- | :-- | :-- |
| 1. 感知 | 值班先从 UNOC/微信收到告警，再到 diagflow 网页**手工输入** `cluster_id`、`flink`、`job_failure` | ⚠️ 两套系统割裂，告警里的 app_id/时间窗全部要人肉搬运 |
| 2. 策略 | 命中 `flink_job_failure.yaml`；但其中 2 个步骤依赖 `{{ context.job_id }}`（:29、:37），而 **HTTP API 没有 job_id 字段、UI 也不收集** → 参数渲染为空字符串 | ❌ 出箱即残：`app_id=""` 被 MCP 拒绝；`yarn logs -applicationId  -logFiles stderr` 是坏命令 |
| 3. 兜底 | 退化到 LLM 自由探索，自己 `yarn application -list` 找 FAILED → 再查 status → 再找节点 → 再 grep | ⚠️ 能跑通，但 3–6 轮 LLM，1–2 分钟，且每次都重来 |
| 4. 证据 | 拿到 stderr 里的 `OutOfMemoryError`，加上 `free -m` 瞬时值 | ⚠️ 没有容器内存上限、没有 TM 堆使用曲线、没有队列资源、没有 checkpoint 统计 |
| 5. 结论 | 合成根因 + 3 条建议；置信度由模型自报 | ⚠️ 无法验证"2G heap / 4 slots"这类判断是否有数据支撑 |
| 6. 沉淀 | 报告只在本浏览器 localStorage；MySQL 只存摘要且 UI 不读；反馈无入口 | ❌ 下一个人遇到同样问题，从零再来 |
| 7. 复盘 | 无法导出工单，无法回看"当时跑了哪些命令、哪步失败" | ❌ Trace 只在 investigate 路径返回，不落库 |

### 5.2 分角色评价

**一线值班 / NOC（最需要它的人）——不方便**

- 必须知道内部英文组件名和 `problem_type` 文件名（`flink` + `job_failure`）。填 `Flink 任务失败` 会落到 default 策略，质量骤降且**界面无任何提示**（只有日志里一行 `strategy: N steps`）。
- 告警里的关键信息（app_id、时间窗、节点）没有入口带入。
- 结论不能一键转工单、不能分享链接、不能标注"对不对"。
- 结论可信度是模型自报的 high/medium/low，没有校准、没有依据强度。

**二线 SRE —— 部分方便**

- 省掉了"SSH 上去 grep 一圈"的机械劳动，证据链和只读命令模板确实好用。
- 状态枚举翻译（`CS_SERVICE_EXCEPTION` + 能否读日志的提示）很贴心。
- 但：不能指定"昨晚 02:00–02:30"；没有指标/变更/告警上下文；跨组件因果靠模型自己串（`topology` 字段在 prompt 里还被 `stripTopology` 剥掉了）；同一问题第二次问，仍然全价重跑。
- 不能"接着上次继续查"——会话历史只在 MySQL 可用且 UI 未提供入口。

**平台研发（要排查 DiagFlow 自己）——不方便**

- 只有 stdout JSON 日志 + 几个 counter；**没有 phase 级耗时**、没有单次诊断的完整 trace 落库、没有 trace_id 串到 LLM。
- 工具入参/出参只在内存里，事后无法回答"当时到底执行了什么命令、返回了什么"。
- 文档与代码曾系统性漂移（README 是 Python 目录结构、OBSERVABILITY 说"指标未实现"但代码已有 `/metrics`、LINK_CHAIN 说 Phase 2.5 但代码已删）。**已于 2026-10-27 校准，排障时请以 README 为准。**

### 5.3 "方便"的三个真实亮点（不要改掉）

1. **SSE 逐步叙述**：能看见"正在 grep 哪个文件、哪一步失败"，比黑盒等 90 秒好得多。
2. **证据链可展开**：每条证据的 category/summary/detail/confidence 都在，能人工复核。
3. **只读命令模板 + 噪声过滤**：`STARTUP_MSG` 过滤、行宽 2000 字符截断、严重度直方图（`ERROR=2 WARN=5`）——这些是真正减少阅读成本的细节。

---

## 6. 风险清单

### P0（阻断上线 / 安全）

| # | 问题 | 证据 | 影响 |
| :--: | :-- | :-- | :-- |
| 1 | `deploy.sh` 部署必失败（`ZK_HOSTS` 变量名、statefulset 类型、8501 端口） | `diagflow-statefulset.yaml:64`；`deploy.sh:48,54`；`config.go:300-311` | 交付不可用，误以为"部署过了" |
| 2 | ~~MCP bearer token 明文入库（两处）~~（已改为 Secret 注入） | `deploy/kubernetes/*.yaml` | 评审时：拿到仓库即拿到集群只读访问权；现已修正 |
| 3 | SSH root 密码 + 不校验 host key | `sshlog.go:81`；`clusteraccess.go:127-141` | 中间人 + 集群接管；合规红线 |
| 4 | ACL 未配置时，任意有效 token 可读本 AZ **所有租户**集群日志 | `internal/mcpserver/policy.go`（ACL 已实现，默认关闭） | 越权/数据泄露 |
| 5 | ~~LLM 失败会把错误文本当根因输出~~（已修正：返回 502/422） | `diag_agent.go` `ErrLLMUnavailable`/`ErrNoConclusion`；`server.go` `writeDiagnosisError` | 评审时：报告出现错误文本；现已不会伪造结论 |
| 6 | KB 全链路失效但 UI/文档宣称"命中知识库" | `embedder.go:39-49`；`diag_agent.go:181,239` | 成本预期错误；用户误信"系统在学习" |
| 7 | ~~就绪探针不探测依赖~~（已修正） | `server.go` `handleReadyz` 依赖矩阵 | 故障期间不再误接流量 |

### P1（不影响启动，影响信任/规模）

| # | 问题 | 证据 | 影响 |
| :--: | :-- | :-- | :-- |
| 8 | ZK `DiscoverUnder` 在 zone 未命中时**扫描所有 set** | `zkdiscovery.go:179-205` | 与"绝不跨区回退"的注释矛盾；多 set 同 ZK 时可能串区 |
| 9 | MySQL 连不上静默禁用，无重连、无指标 | `store.go:44-70` | 历史/会话悄悄消失，没人知道 |
| 10 | 无 schema 迁移，`sql/schema.sql` 靠手工 | `sql/schema.sql`；`deploy/` 无 Job | 新环境静默无表 |
| 11 | DeepWiki 把日志片段发往公网，且强依赖出网 | `config.go:232-234`；`investigate_plan.go:121-127` | 专有云不可用；日志外泄 |
| 12 | 无告警/工单触发入口 | 全仓无 webhook/alarm 接入（`grep` 仅注释） | 只能人工发起，与 NOC 流程脱节 |
| 13 | ~~文档与代码系统性漂移~~（已于 2026-10-27 校准） | `README.md`、`OBSERVABILITY.md`、`LINK_CHAIN.md` | 曾误导新人/排障；现已与代码一致 |
| 14 | 4 份清单、2 种镜像标签、2 个 namespace 说法 | §4.1 | 发布不确定 |
| 15 | 240s 请求超时 vs thinking 模型多轮调用 | `config.go:216`；`diag_agent.go:343`；`investigate_plan.go:211-291` | 意图排查（2–5 intent × 4 轮）大概率超时；SSE 只报"连接中断" |
| 16 | 多副本无共享会话/KB | `server.go:68`（进程内 cache） | 多副本体验不一致 |

### P2（产品化/成本）

| # | 问题 | 影响 |
| :--: | :-- | :-- |
| 17 | 无 HPA/PDB/NetworkPolicy/ServiceMonitor | 容量与稳定性靠运气 |
| 18 | 无评测集（golden set）、无准确率指标 | 换模型/改 prompt 无法判断变好还是变坏 |
| 19 | 无 token 预算/限流/相同告警去重 | 告警风暴时成本失控 |
| 20 | 无 Flink Serverless / K8s 适配 | 新形态零覆盖 |
| 21 | 组件覆盖只有 flink/hdfs/yarn 三个策略 | HBase/Hive/Kafka/Spark/StarRocks 全部落到 default |

---

## 7. 改进路线

### P0：止血（约 1 周，不改架构）

1. **修部署**：统一为一份 StatefulSet 清单（对齐 KUN 约定），修正 `DIAGFLOW_ZK_HOSTS`、`rollout status`、端口；删除其余三份。
2. **密钥治理**：token 移出 Git，进 Secret 并轮换；MCP 按 agent/租户发不同 token；增加"token → 允许的 cluster 列表"校验。
3. **修 `/api/v1/investigate`**：handler 里置 `req.Investigate = true`，或直接删掉该路由（别留一个名不副实的接口）。
4. **LLM 失败不产出报告**：`runReact` 返回 error 而非字符串；`Diagnose` 感知后返回失败；`validator` 拦截包含 `LLM error`/`panic`/`timeout` 的根因文本。
5. **KB 二选一**：要么接真 embedding + 落库（MySQL/向量库）+ 跨副本共享；要么删掉 Phase 1/5 与 UI 的"命中知识库"，别让用户误信。
6. **探针真实化**：`/readyz` 探测 ZK 可连、MCP 可达、LLM key 存在；MySQL 配置了但不可达时置 not ready 并打点。

### P1：适配 UCloud（约 3–4 周）

7. **换节点通道**：给 `umrAgent` 增加只读 `ReadLog`（Path/Keywords/MaxLines/Since，HMAC + 审计 + 输出上限），MCP 的 `get_node_log`/`search_logs` 改走 agent；SSH 降级为 fallback 且禁止 root。
8. **补证据面**（按性价比排序）：
   - `umonitor2` / Prometheus 查询：（cluster, node, 时间窗）→ CPU/内存/磁盘/JVM/YARN 队列；
   - 部署/变更状态：`t_uhadoop_main_task` / manage `jobs`（避免把"扩容中"误判为组件故障）；
   - UNOC 告警：按 cluster + 时间窗拉当时的告警；
   - YARN RM REST、`hdfs dfsadmin -report` → 结构化工具，而不是让模型拼命令。
9. **告警驱动入口**：`POST /api/v1/investigate/alert`（接收 cluster_id/规则/时间窗/app_id），自动生成"诊断草稿 + 建议工单"，与 `uhadoop-monitor` 的 `t_uhadoop_diagnose` 对齐字段。
10. **按 region 交付**：配置生成器对齐 `emr/devops/config/kubernetes/<region>/`；`-config` 读 YAML；端口 190xx + pprof；StatefulSet + preStop + PDB。
11. **鉴权与租户**：入口走平台网关，携带 `org_id`/`user`；诊断结果按集群归属校验；审计日志落库。
12. **可观测性**：ServiceMonitor、phase 级 histogram、trace_id、诊断全链路（命令原文 + 返回码 + 耗时）落库，支持按 event_id 回放。

### P2：产品化（1–2 月）

13. **抽象 `AccessProvider`**：UHadoop（access + agent）与 UK8s（K8s API + Flink REST + Prometheus）并列实现，替换现在 `cluster_id` 强绑定的工具层。
14. **知识闭环**：反馈按钮 → `diagnosis_feedback` → 人工确认 → fingerprint/意图入库 → 下次 0 LLM；争议样本标记 `disputed`。
15. **评测体系**：收集 30–50 个历史故障（cluster/时间/真实根因），离线跑分（Top-1 命中率、证据命中率、平均耗时、token 成本），作为模型/prompt 变更的门禁。
16. **成本治理**：按租户/集群配额、并发限流、相同告警去重、结果缓存。

### 目标形态（建议）

```
UNOC/umonitor2 告警 ─┐
工单/控制台 ─────────┼─→ API 网关（鉴权/租户/配额）─→ DiagFlow（无状态，per-region）
OpenAPI/CLI ─────────┘                                   │
                                                         ├─ 证据代理 Evidence Broker
                                                         │    ├ UHadoop access（元数据）
                                                         │    ├ umrAgent ReadLog（只读，HMAC）
                                                         │    ├ umonitor2/Prometheus（时序）
                                                         │    ├ 任务/变更/告警（t_uhadoop_*）
                                                         │    └ K8s API + Flink REST（serverless）
                                                         ├─ Agent（ReAct + 策略 + 校验）
                                                         └─ 知识库（MySQL/向量）+ 反馈闭环
```

---

## 8. 建议的复核动作（半天内可完成）

在把结论当定论之前，建议按下面 5 条实测一次（我未做真实联调）：

1. `kubectl -n prj-diaflow get deploy,sts,svc`：确认线上到底跑的是哪份清单、几个副本。
2. 在 `prj-diaflow` 起一个临时 Pod，`nc -6 -vz <node-ipv6> 22` 与 `65431`：验证 SSH 通道可达性（这是整个日志链路的前提）。
3. 用真实 `cluster_id` 调 `POST /api/v1/diagnose`（component=`flink`, problem=`job_failure`）：观察 2 个依赖 `context.job_id` 的步骤是否如 §5.1 所述失败。
4. 检查 MySQL 是否真的建了 `diagnosis_history` 等表：`store.New` 失败是静默的，日志里只有一行 `mysql disabled`。
5. 用 `kubectl -n prj-diaflow logs` 看一次完整诊断：确认 LLM 调用次数、总耗时是否逼近 240s。

---

## 9. 未验证假设（明确声明）

1. **生产环境的 umrAgent 版本**可能与工作区 `software/umrAgent/` 不同；若线上版本已有 `GetLogs`，则 §3.3 的"日志能力缺失"不成立（但 SSH 仍然是事实通道）。
2. **`prj-diaflow` 到集群节点 22/65431 的网络策略**未验证；若 22 不通，当前实现完全不可用。
3. **Flink Serverless 是否是 diagflow 的目标场景**：仓库里出现 `ucloud-flink-serverless/flink-serverless-design.md`（未入库）且同 namespace 有 `flink-serverless` StatefulSet，我按"很可能"处理，但未获确认。
4. **线上真实流量/调用方**（NOC？工单？纯人工？）未知；这直接影响 P0 的优先级排序。
5. `uhadoop-monitor` 的自动处置细节（源码不在工作区）未验证，只核对了 `deploy-manage-hosts/deploy/uhadoop-monitor/gen_config.js` 中的开关与表名。

---

## 10. 附录：主要证据索引

DiagFlow：

- `internal/server/server.go:96-97`（两个路由同一 handler）、`:161-180`（readyz）、`:256-302`（runDiagnosis）、`:265-267`（每请求 KB）
- `internal/diagagent/diag_agent.go:181`（Phase 1 门控）、`:206-207`（Phase 2.5 已删）、`:227-233`（空结论回退）、`:239-242`（KB 归档）、`:355-359`（LLM 失败返回字符串）
- `internal/rag/embedder/embedder.go:39-49`（HasRealEmbeddings 恒 false）
- `internal/validator/validator.go:26-56`（只有 L1/L2）
- `internal/diagagent/investigate_plan.go`（Investigate、ConfirmCorrect 无入口）
- `internal/infra/zkdiscovery.go:179-205`（跨 set 回退）、`bootstrap.go:25-43`（protobuf 格式）
- `internal/infra/sshlog.go:81`（不校验 host key）、`:253-274`（/var/log 白名单）、`:471+`（命令白名单）
- `internal/mcpserver/server.go:47,118-141`（token 与审计）、`:172-174`（仅校验 cluster_id 存在）
- `internal/observability/store/store.go:44-70`（连不上即静默禁用）
- `deploy/kubernetes/*.yaml`、`deploy/deploy.sh`、`sql/schema.sql`
- `README.md`、`OBSERVABILITY.md`、`LINK_CHAIN.md`（评审时记录的文档漂移；已于 2026-10-27 校准）

UCloud 平台：

- `docs/UHADOOP_ARCHITECTURE_ATLAS.md:127`（部署拓扑）、`:1040`（服务发现）、`:1064`（节点 Agent 协议）、`:1161`（风险）
- `go_framework/business/http_router.go:32-46`（POST body）、`:227+`（HandlerMap）
- `go_framework/framework/configor.go:128-145`（protobuf NameNodeContent）、`framework/zookeeper.go:164`（ephemeral）
- `deploy-manage-hosts/deploy/go_framework/gen_config.js:35`（ZK 路径）、`:62`（SelfScope）
- `deploy-manage-hosts/deploy/uhadoop-monitor/gen_config.js:25-30`（CHECK_CLUSTER_AUTO/DEAL_AUTO/t_uhadoop_diagnose）
- `emr/devops/config/kubernetes/<region>/application.yaml`（per-region 配置：zk/consul/dbSources）
- `emr/devops/config/kubernetes/flink-serverless-statefulset.yaml:4-5`（namespace `prj-diaflow`）
- `software/umrAgent/methods/`（106 个方法，无日志读取）、`software/umrAgent/methods/FindLogs.js`（2026-06-24 新增，仅返回路径）

---

## 11. 评审后已落地的变更（2026-10-21，范围：MCP 服务）

按“先改 MCP 服务”的范围，已完成的改动（其余方案未动）：

| 项 | 改动 | 证据 |
| :-- | :-- | :-- |
| 缓存污染 / 数据竞争 | `dedupeNodes` 改为复制切片，不再原地压缩 `ClusterInfo.NodeInfo` | `internal/mcpserver/server.go` |
| 可观测 | 新增 `GET /metrics`（tool 调用数 / 耗时直方图 / 拒绝数）、`GET /readyz`（ZK 发现 + access 端口可连，3s 兜底） | 同上 |
| 审计 | 每条调用带 `X-Request-ID`，日志补 `cluster_id`/`request_id`/`duration_ms`/`result_bytes` | 同上 |
| 授权 | 每 agent 集群白名单 `DIAGFLOW_MCP_ACL`，越权返回 `-32003` | `internal/mcpserver/policy.go` |
| 限流 | 每 agent 令牌桶，超限返回 HTTP 429 + `-32005` + `Retry-After` | 同上 |
| 扇出治理 | `search_logs` 最多采样 `DIAGFLOW_MCP_MAX_SEARCH_NODES`（默认 32）个节点，并在结果中说明采样比例 | `internal/mcpserver/server.go` |
| 协议 | `initialize` 回显客户端 MCP 版本（2024-11-05 / 2025-03-26 / 2025-06-18） | 同上 |
| 语义收敛 | `ssh_exec` 不再把 `action` 当命令执行 | 同上 |
| 部署 | 合并为单份 `deploy/kubernetes/diagflow-mcp.yaml`（Deployment + Service + PDB），移除明文 token、探针改 `/readyz`、加 Prometheus 注解；删除 `diagflow-mcp-deployment.yaml` | `deploy/kubernetes/` |
| 测试 | 新增 ACL/限流/就绪/指标/采样/缓存不变性/协议协商用例；`go test ./... -race` 全绿 | `internal/mcpserver/policy_test.go`、`internal/config/config_test.go` |

仍未做（明确保留）：

1. **审计落库 MySQL**：需要 DSN 与表归属（当前只有结构化日志）。
2. **mTLS / NetworkPolicy / token 轮换**：ACL 是应用层兜底，网络层仍为明文 ClusterIP。
3. **SSH root → umrAgent 只读 Action**：属于“其他方案”，按约定暂不动。
4. **ACL 默认关闭**：需要先有“集群 → 租户/agent”的归属数据，否则开启会误伤。

> 注意：本仓库 `.gitignore` 第 14 行忽略 `deploy/`，因此清单改动不会进入版本库；评审里说的“4 份清单漂移”与此直接相关。

