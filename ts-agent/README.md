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
| `health_check` 状态询问（区别于故障诊断） | ✅ |
| 控制台会话（会话隔离、结论回放） | ✅（需 MySQL） |
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

问题类型启用 `health_check` 时走“状态询问”模式：prompt 明确告诉模型这是状态问题，报告
渲染为“状态报告 / 当前状态 / 注意事项”，不写根因。适合“NameNode 现在什么情况”这类提问——
否则报告会出现“问题类型 job_failure、结论 NameNode 正常”这种对不上的组合。

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
- 一个模型凭据（默认 modelverse：`MODELVERSE_API_KEY`）
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
| `MODELVERSE_API_KEY` | modelverse 网关凭据（默认 provider 使用） |
| `TS_AGENT_LLM_API_KEY` | 通用凭据，**优先级最高**，任何 provider 都可用 |
| `DEEPSEEK_API_KEY` | 仅 Pi 内置 `deepseek` provider（`api.deepseek.com`）使用 |
| `PI_MODEL_PROVIDER` | 默认 `modelverse` |
| `PI_MODEL_ID` | 默认 `deepseek-v4.1-flash-1017` |
| `TS_AGENT_MODELS_JSON` | provider 目录，默认 `./models.json`（镜像内 `/app/models.json`） |
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
| `TS_AGENT_MYSQL_URL` | `mysql://user:pass@host:3306/diagflow`；不设则无会话 |
| `TS_AGENT_SESSION_MAX_TURNS` | 回放进 prompt 的历史轮数，默认 `5` |
| `TS_AGENT_MYSQL_POOL` | 连接池大小，默认 `5` |

### 模型与网关

默认走 UCloud modelverse（OpenAI 兼容，`https://api.modelverse.cn/v1`），provider 定义在
`ts-agent/models.json`，部署时以 ConfigMap 挂到 `/etc/diagflow-ts/models.json`：

```json
{ "providers": { "modelverse": { "baseUrl": "https://api.modelverse.cn/v1",
  "api": "openai-completions", "authHeader": true,
  "models": [{ "id": "deepseek-v4.1-flash-1017", "reasoning": true }] } } }
```

凭据解析顺序：`TS_AGENT_LLM_API_KEY` → provider 自己的变量（`MODELVERSE_API_KEY` /
`DEEPSEEK_API_KEY` …）→ modelverse。启动日志会打印实际使用的变量名：

```
[diagflow-ts] model=modelverse/deepseek-v4.1-flash-1017
[diagflow-ts] model key=MODELVERSE_API_KEY
```

**不要把网关 token 放进 `DEEPSEEK_API_KEY`**：那是 `api.deepseek.com` 的 key，网关会返回
`401 Authentication Fails`。混用时启动日志和 `/readyz` 会给出 `config warning`。

如果网关有 IP 白名单，看到的是 **Pod 出集群的 IP**（节点 IP 或 NAT 出口 IP，不是浏览器所在
机器的 IP），用 Pod 内 `fetch('https://api.ipify.org')` 或报错里的 request_id 让网关侧确认。

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

### 控制台会话

设置 `TS_AGENT_MYSQL_URL` 后，控制台左侧出现会话列表：

- **每个会话独立**：会话内的提问共享上下文，跨会话互不可见。
- **只回放结论**：后续提问只带上本会话最近 `SESSION_MAX_TURNS` 轮的结论摘要，
  原始工具输出不落库、不回放——诊断是对当前时刻的取证，回放旧日志会让模型把过期的
  “正常”当成现在。prompt 里也明确要求本轮事实必须来自本轮工具调用。
- **匿名身份**：`X-Diagflow-User` 由浏览器生成并存在 localStorage，只用于归组同一
  浏览器的会话，不是登录态。接入真实身份时改 `server.ts` 的 `callerId()` 即可。
- 没有该变量时会话接口返回 503，控制台退化为单次排查。

接口：

| Method | Path | 说明 |
|---|---|---|
| GET | `/api/v1/sessions` | 本浏览器的会话列表 |
| POST | `/api/v1/sessions` | 新建会话 |
| GET | `/api/v1/sessions/:id` | 会话的所有轮次 |
| DELETE | `/api/v1/sessions/:id` | 删除会话（级联删除轮次） |

建表语句在 `ts-agent/sql/session.sql`；服务启动时也会自动建表（`CREATE TABLE IF NOT EXISTS`）。
作为参照，Go 版的表定义在仓库根的 `sql/schema.sql`——两套表名不同，互不冲突。

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
| `models.json` | provider 目录（默认 modelverse），`TS_AGENT_MODELS_JSON` 或 `.pi-agent/models.json` 生效 |
| `models.example.json` | 单模型精简示例 |
| `.env.example` | 全部环境变量 |

项目会在 `<TS_AGENT_AGENT_DIR>/models.json` 存在时用它加载模型目录，因此可以完全
不碰 `~/.pi/agent`。示例：

```bash
export MODELVERSE_API_KEY=...
export PI_MODEL_PROVIDER=modelverse
export PI_MODEL_ID=deepseek-v4.1-flash-1017
```

`~/.pi/agent/models.json` 仍然生效（`TS_AGENT_MODELS_JSON` 未设置且 `.pi-agent/models.json`
不存在时），项目内文件优先。

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
docker build -f ts-agent/Dockerfile -t hub.ucloudadmin.com/diaflow/diagflow-ts:ts-0.1.5 .
docker push hub.ucloudadmin.com/diaflow/diagflow-ts:ts-0.1.5
```

运行时把 `DIAGFLOW_MCP_BASE_URL` / `DIAGFLOW_MCP_TOKEN` / `PROMETHEUS_URL` /
模型凭据通过环境变量或 Secret 注入。

## Kubernetes（prj-diaflow）

清单：[ts-agent/deploy/kubernetes/diagflow-ts.yaml](deploy/kubernetes/diagflow-ts.yaml)

```bash
cd ..   # repo root
export IMAGE_TAG=ts-0.1.5
envsubst < ts-agent/deploy/kubernetes/diagflow-ts.yaml | x6 apply -f -
x6 -n prj-diaflow rollout status deploy/diagflow-ts
```

依赖（namespace 内已存在）：

| 资源 | 用途 |
|---|---|
| `deploy/diagflow-mcp` + `svc/diagflow-mcp:8090` | 集群只读网关 |
| Secret `diagflow-mcp-tokens` / `DIAGFLOW_MCP_CLIENT_TOKEN` | MCP bearer token |
| Secret `diagflow-secrets` / `MODELVERSE_API_KEY` | 模型网关凭据 |
| ConfigMap `diagflow-ts-models` | provider 目录，挂到 `/etc/diagflow-ts/models.json` |
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

- 模型名以 modelverse 目录为准（`deepseek-v4.1-flash-1017` 等）；Pi 内置的
  `deepseek-flash` / `deepseek-v4-pro` 属于 DeepSeek 官方平台，与网关不通用。
- 每个 HTTP 请求创建一个新的 Pi session 和 MCP 子进程；并发上限默认 2。
- `diagnose` 模式会额外建立一个直连 MCP client 执行 YAML 策略，因此一次请求会有
  两组 MCP 连接（Pi 一组、策略一组）。
- KB 是进程内存，重启即丢；没有语义检索和 BM25。
- `deepwiki_query` 尚未接入 TS 版；策略里遇到该工具会记录为低置信度证据。
- 策略里的 `ssh_exec` 只有 `TS_AGENT_ALLOW_SSH_EXEC=1` 时才会真正执行。
- 没有 MySQL 持久化；会话只在进程内。
- 校验只有 L1/L2 确定性层 + 一次重试，没有独立 LLM 复核。
