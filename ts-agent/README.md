# DiagFlow TS Agent (Pi SDK)

基于 [Pi Agent SDK](https://pi.dev/docs/latest/sdk) 的**监控排查**实现。它是现有 Go `diagflow`
的并行版本，第一版只覆盖 `Investigate`（意图驱动排查）路径，目标是逐步替代 Go 的
`internal/diagagent` 编排层，同时保留 Go 的 `diagflow-mcp` 作为集群只读网关。

## 状态

| 能力 | 状态 |
|---|---|
| CLI 排查 | ✅ |
| HTTP + SSE (`POST /api/v1/investigate`) | ✅ |
| `diagflow-mcp` 只读工具接入 | ✅ |
| Prometheus / Alertmanager MCP server | ✅ |
| 意图生成 → 执行 → 合成 | ✅ |
| 五阶段 `Diagnose`、YAML 策略、KB、validator | ❌（后续里程碑） |
| MySQL 持久化、DeepWiki、UCloud UNOC | ❌ |

## 架构

```
CLI / HTTP+SSE
      │
      ▼
workflow/investigate.ts
  ├─ proposeIntents()   2-5 个排查意图
  ├─ runIntent() × N    每个意图最多 4 次工具调用
  └─ synthesize()       结构化根因 + 建议
      │
      ▼
Pi AgentSession
  ├─ MCP: diagflow ──► diagflow-mcp ──► UHadoop 集群（只读）
  └─ MCP: prom     ──► Prometheus / Alertmanager（只读）
```

设计约束：

- **只用 MCP 工具**。Pi 的内置 `read` / `bash` / `edit` / `write` 通过 `tools` allowlist 全部禁用。
- **`ssh_exec` 默认 hidden**，只有 `TS_AGENT_ALLOW_SSH_EXEC=1` 才暴露。
- **TS 进程不持有集群凭据**，集群访问全部经 `diagflow-mcp`。
- **失败不伪造结论**：合成阶段没有可验证的根因时抛 `NoConclusionError`，HTTP 返回 422。

## 前置条件

- Node.js **>= 22.19**（Pi SDK 要求）
- 一个 Pi 支持的模型凭据（默认 DeepSeek：`DEEPSEEK_API_KEY`）
- 一个可达的 `diagflow-mcp`（可选，但生产环境应有）
- Prometheus / Alertmanager（可选）

## 安装

```bash
cd ts-agent
npm install
cp .env.example .env
# 编辑 .env
```

## 配置

| 变量 | 说明 |
|---|---|
| `DEEPSEEK_API_KEY` | DeepSeek 凭据；或 `TS_AGENT_LLM_API_KEY` |
| `PI_MODEL_PROVIDER` | 默认 `deepseek` |
| `PI_MODEL_ID` | 默认 `deepseek-flash`；可选 `deepseek-v4-pro` |
| `PI_THINKING_LEVEL` | `off` / `low` / `medium` / `high`，默认 `medium` |
| `DIAGFLOW_MCP_BASE_URL` | 例如 `http://diagflow-mcp:8090/mcp` |
| `DIAGFLOW_MCP_TOKEN` | MCP bearer token |
| `TS_AGENT_ALLOW_SSH_EXEC` | `1` 才向模型暴露 `ssh_exec` |
| `PROMETHEUS_URL` | 例如 `http://prometheus:9090` |
| `ALERTMANAGER_URL` | 例如 `http://alertmanager:9093` |
| `PROMETHEUS_BASIC_AUTH` | `user:pass`（可选） |
| `PROMETHEUS_BEARER_TOKEN` | Bearer token（可选，优先于 basic auth） |
| `TS_AGENT_PORT` | HTTP 端口，默认 `8099` |
| `TS_AGENT_AUTH_TOKEN` | HTTP API 的 bearer token |
| `TS_AGENT_REQUEST_TIMEOUT_MS` | 请求总超时，默认 `240000` |
| `TS_AGENT_STAGE_TIMEOUT_MS` | 单个 agent 阶段超时，默认 `120000` |
| `TS_AGENT_MAX_CONCURRENT` | 并发排查数，默认 `2` |
| `TS_AGENT_LANGUAGE` | `zh-CN`（默认）/ `en` |
| `TS_AGENT_MCP_STARTUP_WAIT_MS` | 等待 MCP 工具注册的上限，默认 `15000` |
| `TS_AGENT_DEBUG` | `1` 时把模型输出和会话消息打到 stderr |

如果要走 modelverse 等 Anthropic/OpenAI 兼容网关，在 `~/.pi/agent/models.json` 里配置
Pi 的 custom provider，然后设置 `PI_MODEL_PROVIDER` / `PI_MODEL_ID`。

## CLI

```bash
npm run investigate -- \
  --component flink \
  --problem job_failure \
  --cluster uhadoop-xxxx \
  --detail "Job FAILED, TaskManager OOM" \
  --alert ./alert.json
```

输出：Markdown 报告到 stdout，排查步骤到 stderr。加 `--json` 输出报告 JSON。

## HTTP

```bash
npm run server
```

| Method | Path | 说明 |
|---|---|---|
| GET | `/healthz` | 存活 |
| GET | `/readyz` | 数据源 / 模型 / 鉴权状态 |
| POST | `/api/v1/investigate` | 同步返回 `{report, markdown}` |
| POST | `/api/v1/investigate/stream` | SSE：`step` / `report` / `failed` / `done` |

```bash
curl -s localhost:8099/api/v1/investigate \
  -H "Authorization: Bearer $TS_AGENT_AUTH_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"component":"flink","problem":"job_failure","cluster_id":"uhadoop-xxxx",
       "detail":"Job FAILED","alert":{"labels":{"severity":"P1"}}}' | jq .
```

## 配置文件

| 文件 | 说明 |
|---|---|
| `models.example.json` | 自定义 provider 示例（modelverse）。复制到 `.pi-agent/models.json` 后生效 |
| `.env.example` | 全部环境变量 |

项目会在 `<TS_AGENT_AGENT_DIR>/models.json` 存在时用它加载模型目录，因此可以完全
不碰 `~/.pi/agent`。示例：

```bash
cp models.example.json .pi-agent/models.json
export MODELVERSE_API_KEY=sk-...
export PI_MODEL_PROVIDER=modelverse
export PI_MODEL_ID=deepseek-v4.1-flash
```

## 调试

`TS_AGENT_DEBUG=1` 会把每个阶段的模型输出和会话消息打到 stderr，用于排查
"为什么模型没调工具"。

## 测试

```bash
npm run typecheck
npm test
```

测试覆盖：

- 报告渲染
- 排查工作流（成功 / 无结论必须报错）
- Prometheus 客户端（查询参数、鉴权、HTTP 错误）
- Prom MCP server（工具清单、`readOnlyHint`、真实 MCP 调用）
- LLM 错误识别（provider 401 必须上抛，不能被当成"没找到证据"）

## 与 Go 的迁移路径

```
v1（当前）：TS 并行跑 Investigate，Go 不动
v2：TS 补 Diagnose 五阶段 + YAML 策略 + validator
v3：Go 的 /api/v1/investigate 反向代理到 TS；Go 保留 MCP 网关与 infra
v4：删除 Go diagagent，保留 diagflow-mcp
```

关键原则：**`diagflow-mcp` 不动**。它是安全边界，TS 只是新的编排层。

## 已知限制

- Pi SDK 的模型目录使用 `deepseek-flash` / `deepseek-v4-pro`；Go 版默认的
  `deepseek-v4.1-flash` 是 modelverse 的模型名，需要在 Pi 里配 custom provider。
- 每个 HTTP 请求创建一个新的 Pi session 和 MCP 子进程；并发上限默认 2。
- 没有历史 KB 匹配，`matched_knowledge` 恒为 `false`。
- 没有 MySQL 持久化；会话只在进程内。
