# DiagFlow 可观测性现状

> 本文按 **2026-10-27 的 Go 实现**校准。旧版本把本文件写成"设计提案、尚未实现"，
> 与代码不符——Prometheus 指标、JSON 结构化日志、MySQL 历史都已落地。下文先列
> 已实现部分，再列仍然缺失的部分。

## 一、已实现

### 1. Prometheus `/metrics`

两个二进制都暴露 `GET /metrics`（文本格式，`internal/metrics` 是零依赖实现）。

| 指标 | 类型 | 标签 | 来源 |
| :-- | :-- | :-- | :-- |
| `diagflow_diagnoses_total` | Counter | `component`, `status` | HTTP 诊断结束 |
| `diagflow_diagnosis_duration_ms` | Histogram | — | HTTP 诊断耗时 |
| `diagflow_tool_calls_total` | Counter | `tool`, `status` | 策略与 ReAct 工具调用 |
| `diagflow_llm_calls_total` | Counter | — | 成功的 LLM 调用 |
| `diagflow_llm_errors_total` | Counter | — | 失败的 LLM 调用 |
| `diagflow_llm_tokens_total` | Counter | — | input + output token |
| `mcp_tool_calls_total` | Counter | `agent`, `tool`, `status` | MCP 网关 |
| `mcp_tool_duration_ms` | Histogram | — | MCP 工具耗时 |
| `mcp_rejected_total` | Counter | `reason` | 鉴权 / ACL / 限流拒绝 |

直方图使用固定桶（100ms ~ 300s），详见 `internal/metrics/metrics.go`。

### 2. 结构化日志

两个 `main.go` 都用 `slog.NewJSONHandler` 输出 JSON。HTTP 层有：

- `requestIDMiddleware`：生成或透传 `X-Request-ID`，响应头回显；
- `recoverMiddleware`：panic 恢复，返回 500 并记录；
- 每次诊断结束记录一行 `diagnosis done`，带 `component`、`status`、
  `confidence`、`duration_ms`、`warnings`、`phases`。

MCP 网关的每条审计日志额外带 `agent`、`cluster_id`、`request_id`、
`duration_ms`、`result_bytes`。

### 3. 诊断历史（可选 MySQL）

`internal/observability/store`：

- `store.New` 在 `DIAGFLOW_MYSQL__HOST` 为空或连不上时返回 `nil`，服务以
  "持久化关闭"继续运行；
- `Save` 在后台 goroutine 执行，带 5s 超时，失败只记 warning；
- 写入 `diagnosis_history` 摘要（根因、置信度、建议、证据数、耗时、LLM 次数），
  **不写逐步追踪**；
- `ListBySession` 用于冷启动时回放会话历史。

表结构见 `sql/schema.sql`（`diagnosis_history`、`diagnosis_feedback` 等）。

### 4. 会话热缓存

`internal/server/session.go`：`session_id → 最近 50 条诊断`，TTL 2 小时，
每分钟清理过期项。未命中时从 MySQL 回放；`session_history` 会注入到下一次
诊断的 context，供模型参考。

### 5. SSE 步骤流

`POST /api/v1/diagnose/stream` 把 Agent 的 `onEvent` 消息转换成 SSE 事件：

- `step`：一条步骤叙述，`kind` 取 `log` / `tool_call` / `tool_result` /
  `tool_error` / `strategy` / `warn` / `phase` / `notice`；
- `report`：最终报告（JSON + Markdown）；
- `failed`：诊断失败的错误码与消息；
- `done`：结束，带总耗时。

另有 15s 心跳，防止代理/浏览器断开空闲连接。普通 `/api/v1/diagnose` 只返回
最终报告。

### 6. 健康检查

- `/healthz`：进程存活；
- `/readyz`：依赖矩阵 `llm`、`cluster_discovery`、`mcp`、`persistence`、
  `api_auth`。关键依赖不可用返回 503，可选依赖不可用返回 200 + `degraded`；
- MCP 网关 `/readyz`：ZK 能解析 `UHADOOPAccessHTTP` 且其端口可连。

### 7. Markdown 报告

`internal/observability/report` 把 `Report` 渲染成 Markdown（根因、置信度、
建议、证据链、降级警告），HTTP 响应和 CLI 共用。

---

## 二、尚未实现

以下能力**不在代码中**，不要按旧版本文档假设它们存在：

| 能力 | 现状 |
| :-- | :-- |
| OpenTelemetry tracing | 未接入，没有 span |
| Langfuse / LLM 专项分析 | 未接入 |
| 每 Phase 耗时直方图 | 只有整体诊断耗时，没有 `phase` 标签 |
| KB 命中率 / 置信度分布指标 | 只出现在报告字段和 MySQL 行里，没有 Prometheus 指标 |
| 工具级 token / 成本拆分 | 只有全局 `diagflow_llm_tokens_total` |
| 文件式 event tracker | 没有；事件只走结构化日志和报告 |
| MCP 审计落库 | 只有结构化日志，没有 MySQL 表 |
| Grafana Dashboard / 告警规则 | 仓库里没有 |
| 诊断步骤持久化 | MySQL 只存摘要，不存每步证据 |

---

## 三、建议的下一步（按优先级）

| 优先级 | 事项 | 解决什么 |
| :---: | :-- | :-- |
| P0 | 给 `diagflow_diagnoses_total` 增加 `outcome`（ok/degraded/error）并核对告警 | "服务还正常吗？" |
| P1 | 增加 Phase 耗时直方图 + KB 命中/置信度计数器 | 定位慢在哪、KB 是否有效 |
| P1 | 把 MCP 审计写入 MySQL 或 Loki | 跨 Pod 追溯 |
| P2 | OpenTelemetry（HTTP → Agent → 工具 → MCP） | 全链路 trace |
| P2 | Langfuse 可配置开关 | prompt 版本 / 成本 / 质量评估 |

所有新增埋点都应复用 `internal/metrics`，保持零外部依赖；日志继续用 `slog`，
不要把 token、密码、`passwd` 写进结构化字段。
