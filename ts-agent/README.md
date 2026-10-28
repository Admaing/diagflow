# DiagFlow TS Agent (Pi SDK)

基于 [Pi Agent SDK](https://pi.dev/docs/latest/sdk) 的**监控排查**实现。它是现有 Go `diagflow`
的并行版本，目标是逐步替代 Go 的 `internal/diagagent` 编排层，同时保留 Go 的
`diagflow-mcp` 作为集群只读网关。

## 状态

| 能力 | 状态 |
|---|---|
| CLI 排查 | ✅ |
| HTTP + SSE（`investigate` / `diagnose`） | ✅ |
| `diagflow-mcp` 只读工具接入 | ✅ |
| Prometheus / Alertmanager MCP server | ✅ |
| 意图生成 → 执行 → 合成（Investigate） | ✅ |
| 五阶段 `Diagnose` + YAML 策略 + L1/L2 校验 | ✅ |
| KB 指纹快路径 / 高置信度回写 | ✅（进程内存，不持久） |
| MySQL 持久化、DeepWiki、UCloud UNOC | ❌ |
| 语义检索 / BM25 | ❌（Pi 版未接 embedder） |

## 两条工作流

```
investigate（默认）                  diagnose
─────────────────                  ─────────
proposeIntents 2-5 个                Phase 1  KB 指纹快路径
  ↓                                    ↓
runIntent × N                        Phase 2  YAML 策略（data/strategies）
  ↓                                    ↓
synthesize                           Phase 3  ReAct 补证据
                                     Phase 4  合成 + L1/L2 + 一次重试
                                     Phase 5  high 置信度回写 KB
```

两条路径共用：

- Pi AgentSession 作为 LLM 编排器；
- `diagflow-mcp` 集群工具与 `prom` 监控工具；
- 结构化 `DiagnosisReport` 与 Markdown 渲染；
- 失败语义：`NoConclusionError` → HTTP 422，LLM 失败 → HTTP 502。

```
CLI / HTTP+SSE
      │
      ▼
workflow/{investigate,diagnose}.ts
      │
      ├─ strategy/  YAML 加载、渲染、优先级并发执行
      ├─ kb/        MD5 指纹（进程内存）
      ├─ validator/ L1 格式 + L2 证据引用
      ▼
Pi AgentSession ── MCP ──► diagflow-mcp / prom
```

设计约束：

- **只用 MCP 工具**。Pi 的内置 `read` / `bash` / `edit` / `write` 通过 `tools` allowlist 全部禁用。
- **`ssh_exec` 默认 hidden**，只有 `TS_AGENT_ALLOW_SSH_EXEC=1` 才暴露（策略层同样受此限制）。
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
| `TS_AGENT_STRATEGIES_DIR` | YAML 策略目录，默认仓库根 `data/strategies` |

如果要走 modelverse 等 Anthropic/OpenAI 兼容网关，在 `~/.pi/agent/models.json` 里配置
Pi 的 custom provider，然后设置 `PI_MODEL_PROVIDER` / `PI_MODEL_ID`。

## CLI

```bash
# 意图驱动排查
npm run investigate -- --mode investigate \
  --component flink \
  --problem job_failure \
  --cluster uhadoop-xxxx \
  --detail "Job FAILED, TaskManager OOM" \
  --alert ./alert.json

# 五阶段诊断（YAML 策略 + ReAct + 校验）
npm run investigate -- --mode diagnose \
  --component flink \
  --problem job_failure \
  --cluster uhadoop-xxxx
```

输出：Markdown 报告到 stdout，排查步骤到 stderr。加 `--json` 输出报告 JSON。

策略文件默认从仓库根的 `data/strategies/` 读取（`<component>_<problem>.yaml` →
`<component>_default.yaml` → 内置兜底），可用 `TS_AGENT_STRATEGIES_DIR` 覆盖。

`--mode diagnose` 必须提供 `--cluster`：策略里的 `get_cluster_info` / `query_yarn` /
`ssh_exec` 等工具都按集群作用域调用。

## HTTP

```bash
npm run server
```

| Method | Path | 说明 |
|---|---|---|
| GET | `/` | 内置 Web 控制台（单页，无外部依赖） |
| GET | `/healthz` | 存活 |
| GET | `/readyz` | 数据源可达性 / 模型 / 鉴权状态 |
| POST | `/api/v1/investigate` | 意图排查，同步返回 `{report, markdown}` |
| POST | `/api/v1/investigate/stream` | 意图排查 SSE |
| POST | `/api/v1/diagnose` | 五阶段诊断，同步返回 `{report, markdown}` |
| POST | `/api/v1/diagnose/stream` | 五阶段诊断 SSE |

SSE 事件：`step` / `report` / `failed` / `done`。

### Web 控制台

服务启动后打开 `http://localhost:8099/`（或 port-forward 后的本地地址）：

- 选择“五阶段诊断”或“意图排查”；
- 填组件、问题类型、集群 ID、现象、告警 JSON；
- 粘贴 Bearer Token（`DIAGFLOW_AUTH_TOKEN`，仅存本机 localStorage）；
- 结果以 SSE 流式步骤展示，最后渲染根因、置信度、建议、证据链和排查过程。

页面是内置的单个 `webui.html`，不加载任何外部脚本/字体；所有模型输出都用
`textContent` 渲染，不拼 HTML。

```bash
curl -s localhost:8099/api/v1/diagnose \
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
- 五阶段诊断（策略执行、指纹快路径、ReAct 降级、校验重试）
- YAML 策略（加载、优先级、模板渲染、分支过滤）
- 策略工具到 MCP 工具的映射（`call_umr_agent` / `query_metrics` 等）
- Prometheus 客户端（查询参数、鉴权、HTTP 错误）
- Prom MCP server（工具清单、`readOnlyHint`、真实 MCP 调用）
- LLM 错误识别（provider 401 必须上抛，不能被当成"没找到证据"）
- 结论校验 L1/L2

## Docker

在仓库根目录构建（需要把 `data/strategies` 一起打进镜像）：

```bash
docker build -f ts-agent/Dockerfile -t hub.ucloudadmin.com/diaflow/diagflow-ts:ts-0.1.3 .
docker push hub.ucloudadmin.com/diaflow/diagflow-ts:ts-0.1.3
```

运行时把 `DIAGFLOW_MCP_BASE_URL` / `DIAGFLOW_MCP_TOKEN` / `PROMETHEUS_URL` /
模型凭据通过环境变量或 Secret 注入。

## Kubernetes（prj-diaflow）

清单：[ts-agent/deploy/kubernetes/diagflow-ts.yaml](deploy/kubernetes/diagflow-ts.yaml)

```bash
cd ..   # repo root
export IMAGE_TAG=ts-0.1.3
envsubst < ts-agent/deploy/kubernetes/diagflow-ts.yaml | x6 apply -f -
x6 -n prj-diaflow rollout status deploy/diagflow-ts
```

依赖（namespace 内已存在）：

| 资源 | 用途 |
|---|---|
| `deploy/diagflow-mcp` + `svc/diagflow-mcp:8090` | 集群只读网关 |
| Secret `diagflow-mcp-tokens` / `DIAGFLOW_MCP_CLIENT_TOKEN` | MCP bearer token |
| Secret `diagflow-secrets` / `DEEPSEEK_API_KEY` | 模型凭据 |
| Secret `diagflow-secrets` / `DIAGFLOW_AUTH_TOKEN` | HTTP API bearer token |
| `imagePullSecrets: paasuhadoop` | 拉取 `hub.ucloudadmin.com` 镜像 |

`PROMETHEUS_URL` / `ALERTMANAGER_URL` 在清单里是注释状态；需要“监控”侧证据时
取消注释并填入可达地址。没有它们时，`diagflow` 工具仍可单独工作。

验证：

```bash
x6 -n prj-diaflow port-forward svc/diagflow-ts 8099:8099 &
curl -s localhost:8099/healthz
curl -s localhost:8099/readyz
curl -s -X POST localhost:8099/api/v1/diagnose \
  -H "Authorization: Bearer $TS_AGENT_AUTH_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"component":"flink","problem":"job_failure","cluster_id":"uhadoop-xxxx"}'
```

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
- `diagnose` 模式会额外建立一个直连 MCP client 执行 YAML 策略，因此一次请求会有
  两组 MCP 连接（Pi 一组、策略一组）。
- KB 是进程内存，重启即丢；没有语义检索和 BM25。
- `deepwiki_query` 尚未接入 TS 版；策略里遇到该工具会记录为低置信度证据。
- 策略里的 `ssh_exec` 只有 `TS_AGENT_ALLOW_SSH_EXEC=1` 时才会真正执行。
- 没有 MySQL 持久化；会话只在进程内。
- 校验只有 L1/L2 确定性层 + 一次重试，没有独立 LLM 复核。
