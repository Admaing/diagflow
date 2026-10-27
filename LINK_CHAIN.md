# DiagFlow 完整链路与设计原理

> 本文描述 **Go 实现**（`go-rewrite` 分支）。Python 原版已从仓库移除。

## 一、总览

DiagFlow 有两条诊断路径，共用工具层、证据池和合成器：

| 路径 | 入口 | 适用场景 |
| :-- | :-- | :-- |
| `Diagnose` | `Agent.Diagnose()` / `POST /api/v1/diagnose` | 已知组件 + 问题类型的标准诊断 |
| `Investigate` | `Agent.Investigate()` / `POST /api/v1/investigate` | 告警驱动的开放式内部排查 |

`Diagnose` 是 **5 个 Phase** 的流水线：

```
Phase 1  KB 指纹快路径       0 LLM
Phase 2  YAML 策略确定性取证 0 LLM（llm_decide 分支除外）
Phase 3  SDK ReAct           LLM
Phase 4  结构化合成 + 校验    LLM + 确定性检查
Phase 5  高置信度自动归档     0 LLM
```

核心设计哲学：**能用确定性逻辑解决的问题，绝不浪费 LLM 调用**。

> 历史上的 **Phase 2.5（证据驱动 MD5 匹配）已删除**。原 Python 版在 Phase 2
> 之后再做一次指纹匹配；Go 版把指纹匹配统一放在 Phase 1，避免同一能力出现在
> 两个阶段。`diag_agent.go` 中保留了一条说明注释。

---

## 二、全景数据流

```
浏览器 / curl / CLI
  │  POST /api/v1/diagnose        （或 /diagnose/stream、/investigate）
  ▼
internal/server
  ├─ authMiddleware        静态 Bearer token（配置了 DIAGFLOW_AUTH_TOKEN 时）
  ├─ recover + requestID   崩溃恢复、X-Request-ID
  └─ runDiagnosis()
       ├─ resolveCluster()             demo → simulated.Cluster
       │                               production → infra.RealCluster
       ├─ knowledgeBase()              进程级 KB（首次调用时创建）
       ├─ BuildV3Tools(cluster, kb, mcpClient, deepwikiClient)
       ├─ Agent.New(...) + RegisterTools
       └─ Agent.Diagnose() / Investigate()
            ▼
       Report { status, root_cause, confidence, suggestions,
                evidence_summary, phases_run, llm_calls, ... }
            ▼
       report.Render() → Markdown
       store.EntryFromReport() → session cache + 可选 MySQL（异步）
```

关键点：

- **每个请求新建一个 Agent 和工具集**，所以 `eventID`、`llmCalls` 等状态不会
  在并发请求之间共享。
- **KB 是进程级的**（`Server.knowledgeBase()` 用 `sync.Once` 创建），跨请求
  共享，但只存在内存中，重启即丢失。
- **CLI 路径**（`cmd/diagflow/main.go` 无 `-serve`）使用同样的 Agent，只是
  数据源换成 `simulated.Cluster` 或一次性的 `RealCluster`。

---

## 三、Phase 1 — KB 指纹快路径

```
输入: context = {component:"flink", problem:"job_failure",
                 error:"java.lang.OutOfMemoryError", version:"1.14.3"}

┌──────────────────────────────────────────────────────────┐
│ fingerprintHit(component, context)                       │
│                                                          │
│  errorPatternFromContext()                               │
│    优先取 error_pattern / error / exception              │
│    否则在 problem/detail/message 中匹配 RAG.ErrorKeywords │
│         ↓                                                │
│  kb.FingerprintMatch(component, pattern, version)        │
│    fp = MD5("component:pattern:version")[:16]            │
│    dict 精确查找 → root_cause + suggestions              │
│         ↓                                                │
│  命中且数据完整 → 直接返回 Report（0 LLM）                │
│  未命中 → 进入 Phase 2                                    │
└──────────────────────────────────────────────────────────┘
```

**语义检索目前不可用**：`KnowledgeBase.HasRealEmbeddings()` 恒为 `false`
（`internal/rag/embedder` 的 provider 是 stub），所以 `kbMatch()` 这一段
永远不会执行。源码里的门控是真实的，不是遗漏。

LLM 调用：0
耗时：亚毫秒级（字典查找）

---

## 四、Phase 2 — YAML 策略确定性执行

```
strategy.Load(component, problemType, strategiesDir)
  ├─ <component>_<problem>.yaml
  ├─ <component>_default.yaml
  └─ 内置 defaultStrategy()（文件都不存在时）
```

策略示例（`data/strategies/flink_checkpoint_failure.yaml`）：

```yaml
steps:
  - action: fingerprint_match
    priority: 0

  - action: tool_call
    tool: ssh_exec
    params:
      node_name: "master1"
      cmd: "grep -E 'Checkpoint|barrier' /data/flink/log/jobmanager.log | tail -100"
    priority: 1

  - action: tool_call
    tool: deepwiki_query
    priority: 2

knowledge_base:
  fingerprint: true
  semantic_search: true
  bm25_search: true
```

执行流程（`Agent.runStrategy`）：

1. `GroupByPriority()` 把步骤按 `priority` 分组。
2. 同一优先级内的 `tool_call` 步骤用 **goroutine 并发执行**（`sync.WaitGroup`）。
3. 每个批次结束后处理 `llm_decide` 步骤，最多产生一个 `branch_decision`；
   后续步骤通过 `if_decision` 决定是否执行。
4. 每步结果写入 `memory.EvidencePool`：
   - 成功 → confidence 0.8
   - 工具返回失败 → confidence 0.2（降级证据，不中断流程）
   - 工具 panic → confidence 0.0，并记录一条错误证据

参数支持 `{{ context.x }}` 模板渲染（`Step.RenderParams`）。

LLM 调用：0（`llm_decide` 分支会调用 1 次）

---

## 五、Phase 3 — SDK ReAct（LLM 自由探索）

`Agent.runReact()` 直接使用 Anthropic SDK 的 `tool_use` block 循环，预算如下：

| 限制 | 值 | 位置 |
| :-- | :-- | :-- |
| 最大轮数 | 12 | `Diagnose` 调用参数 |
| 最大工具调用数 | 20 | `maxReActToolCalls` |
| 连续无新证据轮数 | 3 | 停滞检测 |
| 单条工具结果回灌上限 | 8000 字符 | `maxReActResultChars` |
| 单次工具执行超时 | 30s | `toolTimeout` |

另外两项收敛机制（`convergence.go`）：

- **重复调用短路**：同一 `(tool, canonical args)` 再次出现时不再打到集群，
  直接把已有的重复提示回灌给模型。
- **结果指纹**：只有内容指纹变化才算"新证据"，避免"重复读同一个日志"
  无限重置停滞计数。

ReAct 阶段的输出只有在 `meaningfulConclusion()` 为真时才作为一条
confidence 0.7 的证据入池。

LLM 调用：1~12 次（通常 3~6 次）

---

## 六、Phase 4 — 结构化合成 + 确定性校验

```
synthesize(pool, context)
  ├─ 把 EvidencePool.Summary() + context 发给 LLM
  ├─ 只暴露一个工具 report_diagnosis
  ├─ ToolChoice = "any"（thinking 模型不接受强制指定某个 tool）
  └─ 解析 root_cause / confidence / suggestions
        ↓
validator.ValidateLayer()
  ├─ Layer 1（0 LLM）：根因非空且 ≥10 字符、建议非空、证据数 > 0
  └─ Layer 2（0 LLM）：根因必须引用证据关键词（中英文都覆盖）
        ↓
  失败 → 带 feedback 重试 synthesize() 一次
  仍失败 → 返回 ErrNoConclusion
```

**没有 Layer 3（独立 LLM 复核）**。`README` 和旧版 `LINK_CHAIN` 曾把它描述为
"四层校验"；实际代码只有确定性两层 + 一次重试。`validator.go` 的注释已按此
校准。

失败语义：

| 情况 | 返回值 | HTTP |
| :-- | :-- | :-- |
| LLM 调用失败 | `ErrLLMUnavailable` | 502 `llm_unavailable` |
| 合成不出可验证结论 | `ErrNoConclusion` | 422 `no_conclusion` |
| ReAct 失败但证据足够 | `status=degraded` + warnings | 200 |
| 请求超时 | `context.DeadlineExceeded` | 504 `diagnosis_timeout` |

**失败绝不会被包装成根因。**

---

## 七、Phase 5 — KB 自动归档

只有同时满足以下条件才归档：

- `confidence == "high"`
- `status == "ok"`（没有降级警告）

归档内容：

```
kb.AddCase(component, errorPattern, version, rootCause, suggestions)
  ├─ MD5 指纹 → 进程内 fingerprints map
  └─ 文本 + metadata → 进程内 vector store
```

**边界**：KB 是进程级内存，重启即丢；向量检索因 embedding stub 关闭，所以
"下次相同错误 → 0 LLM"只在同一进程生命周期内成立。跨重启的持久化只有 MySQL
诊断历史（`internal/observability/store`），它不参与 Phase 1 匹配。

LLM 调用：0

---

## 八、Investigate — 意图驱动排查

`Agent.Investigate()` 是另一条编排：

```
1. buildInvestigationPlan()
     LLM 结合告警上下文生成 2~5 个排查意图
     失败 → 回退到单个默认意图 + degraded 警告
2. 对每个意图：
     searchSimilarIntent()
       命中历史 → 直接复用历史根因，跳过工具执行
     否则 runIntent()
       工具调用 → 日志片段 → LLM 判断
       若怀疑组件 Bug：
         buildDeepwikiPrompt() → dispatchDeepwiki()
3. synthesizeFromTrace()
     任一意图命中历史 → 直接返回该历史根因（不再合成）
     否则把 trace 转成 EvidencePool，复用 synthesize()
```

输出报告额外带 `Trace`（每个意图的执行步骤、日志片段、判断、DeepWiki 结果）。

`Agent.ConfirmCorrect()` 可以把确认过的排查思路写回 KB，但目前**没有 HTTP
入口**，只有测试调用它。

---

## 九、LLM 调用次数汇总

| 阶段 | 说明 | LLM 调用 |
| :-- | :-- | :--: |
| Phase 1 | KB 指纹匹配 | 0 |
| Phase 2 | YAML 策略执行 | 0（`llm_decide` 除外） |
| Phase 3 | SDK ReAct | 3~6（通常） |
| Phase 4 | 合成 + 校验 + 最多一次重试 | 1~2 |
| Phase 5 | KB 归档 | 0 |
| **合计（新案例）** | | **4~8** |
| **合计（指纹命中）** | | **0** |

---

## 十、为什么这样设计？四个核心原则

### 1. 确定性优先于智能

指纹匹配、YAML 取证都是 0 LLM。生产问题大量重复，能用历史结论和固定步骤
回答的，不进入模型。

### 2. 结构化优于自由文本

合成阶段只暴露 `report_diagnosis` 一个工具，模型必须输出结构化字段；
文本回退只接受明确可解析的结论，任意散文不会被当作根因。

### 3. 分层验证，越往前成本越低

格式和证据引用检查是纯字符串操作（0 成本），先拦住明显不可用的输出；
只有它们通过后才值得考虑（尚未实现的）模型复核。

### 4. 知识沉淀，但当前是进程级

诊断 → 指纹归档 → 同进程内复用。真正跨重启的知识沉淀需要先接上持久化
向量库和真实 embedding provider，这两项目前都没有落地。

---

## 十一、关键代码索引

| 能力 | 文件 |
| :-- | :-- |
| 五阶段主流程 | `internal/diagagent/diag_agent.go` `Diagnose()` |
| 意图排查 | `internal/diagagent/investigate_plan.go` `Investigate()` |
| ReAct 预算 / 收敛 | `internal/diagagent/convergence.go` |
| YAML 策略 | `internal/strategy/strategy.go`、`data/strategies/*.yaml` |
| 证据池 | `internal/memory/memory.go` |
| 校验 | `internal/validator/validator.go` |
| 指纹 KB | `internal/rag/knowledgebase/knowledgebase.go` |
| HTTP 服务 | `internal/server/server.go`、`sse.go` |
| 工具定义 | `internal/tools/v3tools/v3tools.go` |
| 生产数据源 | `internal/infra/infra.go`、`clusteraccess.go`、`sshlog.go` |
| MCP 网关 | `internal/mcpserver/server.go` |
