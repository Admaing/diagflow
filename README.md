# DiagFlow — AI-Powered Diagnostic Agent for Big Data Platforms

[![Go](https://img.shields.io/badge/go-1.26-00ADD8.svg)]()
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)]()

**DiagFlow** is an AI agent system that automates root cause analysis for big data platform failures. Given a user-reported problem (e.g., "Flink job FAILED", "HDFS no space left"), DiagFlow systematically gathers evidence from logs, metrics, and configuration, then produces a structured diagnosis with actionable suggestions.

Inspired by [得物技术团队的 Troubleshooter](https://cloud.tencent.com/developer/article/2682409), DiagFlow uses a **single Agent** (`DiagAgent`) with SDK-native tool use, a YAML-driven deterministic strategy layer, and hybrid-search RAG.

## Architecture

DiagFlow v3 is a **single Agent** with a five-phase pipeline. The LLM is only invoked
when deterministic logic cannot already answer the question.

```
                    User Input (problem + cluster context)
                                    │
                                    ▼
                    ┌─────────────────────────────┐
                    │   DiagAgent.diagnose()      │
                    │                             │
                    │  Phase 1  KB fingerprint     │ ← 0 LLM
                    │           (semantic if wired)│
                    │  Phase 2  YAML strategy exec │ ← 0 LLM (parallel by priority)
                    │  Phase 3  SDK ReAct          │ ← LLM (tool_use loop)
                    │  Phase 4  Synthesize + verify│ ← LLM (deterministic checks + retry)
                    │  Phase 5  Auto-index to KB   │ ← 0 LLM (ok + high-confidence only)
                    └──────────────┬──────────────┘
                                   ▼
                     DiagnosisReport → Markdown
```

- **Phase 1–2** cover the deterministic fast paths — exact fingerprint matching,
  optional semantic search, and YAML-driven evidence collection.
- **Phase 3** hands the remaining unknown to the LLM via the Anthropic SDK's native
  tool loop (translated to DeepSeek by the modelverse proxy).
- **Phase 4** synthesizes a structured conclusion and validates it through the
  deterministic layers plus one feedback retry. A failed LLM call is never
  converted into a root cause: the API returns a dependency error instead.

## Features

### Single-Agent ReAct + YAML Strategy

- **SDK-native tool use**: `DiagAgent` calls `Anthropic().messages.create(...)` and runs
  the returned `tool_use` blocks in an explicit loop with turn, tool-call, and stall
  budgets (`internal/diagagent/convergence.go`), rather than parsing a text-based ReAct
  transcript.
- **YAML-driven deterministic phase**: `data/strategies/*.yaml` defines *what* to collect
  (which tools, in what order, at what priority). Same-priority steps run in parallel.
  Steps support conditional branching via `llm_decide` + `if_decision`.
- **Evidence pool**: findings are deposited in a thread-safe `EvidencePool` and shared
  across phases; specialist steps never talk to each other directly.

### Tools (`internal/tools/v3tools`)

| Tool group | Tools |
|------------|-------|
| Cluster/log evidence | `query_node_log`, `list_logs`, `search_logs`, `query_metrics`, `query_yarn` |
| Cluster metadata | `get_cluster_info`, `list_nodes` (via the node-log MCP gateway when configured) |
| Node commands | `ssh_exec` (read-only allowlist), `call_umr_agent` |
| Knowledge | `fingerprint_match`, `deepwiki_query` |

### Knowledge Base and Retrieval (in-memory)

- **Fingerprint Match**: MD5 over `component:error_pattern:version` — exact match for
  known issues (fast path, zero LLM cost). This is the only KB path active today.
- **BM25 + vector retrieval**: both are implemented in `internal/rag/retriever` and
  `internal/rag/vectorstore`, but Phase 1 gates the whole search path on
  `HasRealEmbeddings()`, which is always false while the embedder is a stub — so
  neither semantic nor BM25 retrieval runs during a diagnosis.
- **Vector store**: the only backend is an in-memory store. The `chromadb` /
  `milvus` config fields and `DIAGFLOW_VECTOR_STORE__BACKEND` are accepted but do
  not select a backend yet.
- **Current limitation**: the KB is process-scoped and non-durable — it is lost on
  restart. Auto-indexing (Phase 5) writes into that in-memory KB.

### Validation and failure semantics

| Layer | Method | LLM? |
|-------|--------|------|
| 1 | Format validation (root cause length, non-empty suggestions/evidence) | ❌ |
| 2 | Cross-source consistency (root cause references evidence keywords) | ❌ |
| 3 | Retry synthesis once with validation feedback | ✅ |

An independent LLM review layer is **not implemented yet**. A report with
`Status: degraded` means part of the pipeline failed but the conclusion came
from valid evidence; a dependency failure returns `502 llm_unavailable`, and an
unvalidated model response returns `422 no_conclusion`. Such responses contain
no `report` object, so operators cannot mistake them for a diagnosis.

### Pre-built Fault Scenarios

| Scenario | Component | Fault | Expected Root Cause |
|----------|-----------|-------|---------------------|
| `flink_oom` | Flink 1.14.3 | TaskManager OOM | Java heap space OutOfMemoryError |
| `flink_checkpoint_fail` | Flink 1.16.0 | Checkpoint failure | Checkpoint alignment timeout due to backpressure |
| `hdfs_disk_full` | HDFS 3.3.4 | Disk full | DataNode volume out of space |
| `yarn_queue_stuck` | YARN 3.3.4 | Queue congestion | Production queue at 100% capacity |

## Quick Start

### Prerequisites

- Go 1.26+
- `DEEPSEEK_API_KEY` (or `LLM_API_KEY`) for LLM-powered mode; mock mode works without it.

### Installation

```bash
git clone https://github.com/your-username/diagflow.git
cd diagflow
go build -o diagflow ./cmd/diagflow
```

### Run Demo

```bash
# List available scenarios
./diagflow --list

# Run Flink OOM diagnosis (mock mode — no API key needed)
./diagflow --scenario flink_oom

# Run with real LLM (via modelverse proxy → DeepSeek)
export DEEPSEEK_API_KEY=sk-...
./diagflow --scenario flink_oom

# Run all scenarios
./diagflow --all
```

### HTTP API Server (production mode)

```bash
./diagflow -serve            # listens on :8080 (DIAGFLOW_SERVER__PORT)
```

Endpoints:

| Method | Path                | Description |
|--------|---------------------|-------------|
| GET    | `/healthz`          | Liveness |
| GET    | `/readyz`           | Readiness: dependency matrix (`llm`, `cluster_discovery`, `mcp`, `persistence`, `api_auth`) |
| GET    | `/metrics`          | Prometheus metrics |
| POST   | `/api/v1/diagnose`  | Run a diagnosis |
| POST   | `/api/v1/investigate` | Intent-driven investigation (forces `investigate=true`) |
| POST   | `/api/v1/diagnose/stream` | Same diagnosis, narrated as SSE step events |
| GET    | `/`                 | Embedded single-page operator console |

```bash
curl -s localhost:8080/api/v1/diagnose \
  -H 'Authorization: Bearer <DIAGFLOW_AUTH_TOKEN>' \
  -d '{"component":"flink","problem":"Job FAILED","cluster_id":"uhadoop-...","job_id":"application_..."}' | jq .
```

`/readyz` returns `200` for both `ready` and `degraded`, and `503` only when a
critical dependency (LLM configuration, ZooKeeper discovery, or a configured
MCP gateway) is unavailable. Optional MySQL persistence and missing API auth
are reported as `degraded` instead of hiding behind a green health check.

> **KB limitation**: semantic embeddings are not wired yet. Exact fingerprints
> work and are shared for the lifetime of the process; vector similarity is
> disabled until `HasRealEmbeddings()` has a real provider behind it.

### Node-log MCP Server (shared by multiple agents)

```bash
export DIAGFLOW_MCP_TOKENS="diagflow=token1,opsbot=token2"
export DIAGFLOW_ZK_HOSTS="10.72.136.107:2181,10.72.136.108:2181"
export DIAGFLOW_ZONE=test03-01
export DIAGFLOW_INFRA__ZK_ZONE_ID=666003001   # ZK set id, scopes discovery
# optional policy
export DIAGFLOW_MCP_ACL="diagflow=uhadoop-a,uhadoop-b;opsbot=*"
export DIAGFLOW_MCP_RATE_LIMIT=20             # per-agent req/s, 0 disables
export DIAGFLOW_MCP_RATE_BURST=40
export DIAGFLOW_MCP_MAX_SEARCH_NODES=32
./diagflow-mcp -port 8090
```

When `DIAGFLOW_MCP_BASE_URL` is configured, the MCP server is the **single
cluster-access gateway**: DiagFlow agents hold no cluster credentials, only a
bearer token. Without it, the agent falls back to the in-process adapter in
`internal/infra` and therefore uses the cluster credentials itself. The MCP
server exposes its tools over Streamable HTTP (`POST /mcp`, JSON-RPC 2.0), with
per-agent bearer-token auth and audit logging.

It has two data planes:

| MCP tool | Backend |
|----------|---------|
| `get_cluster_info` | access API `describe_cluster` (via ZooKeeper discovery) |
| `list_nodes` | access API `list_node` |
| `get_node_info` | access API `describe_node` (includes per-node services) |
| `get_cluster_services` | access API `get_cluster_service` |
| `get_node_log` | read-only SSH `tail`/`grep` on the node |
| `list_logs` | read-only SSH: list log files under the standard `/var/log` roots |
| `search_logs` | read-only SSH, fanned out across nodes in parallel |
| `get_node_metrics` | read-only SSH (`uptime`, `free`, `df`) |
| `get_base_info` | read-only SSH (`hostname`, `uname`, `nproc`, `free`) |
| `check_process` | read-only SSH (`ps`) |
| `query_yarn` | read-only SSH (`yarn application`) |
| `ssh_exec` | read-only SSH, command allowlist enforced (incl. inline read-only `mysql -e`) |

**Cluster metadata** is discovered, never hard-coded: the server resolves each
zone's `UHADOOPAccessHTTP` instances under `/NS/uhadoop/set<zone>/` in
ZooKeeper (protobuf registration payload) and calls the access HTTP action API.
**Node operations** use the `access_user` + base64 `passwd` that
`describe_cluster` returns, over SSH to the node's IPv6 address (cluster IPv4
addresses are not routable from Kubernetes). Credentials never leave the
server: `passwd` and `access_user` are redacted from every tool result.

Node-log transport is pluggable (`infra.RemoteExec`); the SSH implementation is
first, so an umrAgent/agent_key transport can be swapped in later without
changing the tool surface.

**Safety**: every command is assembled from a fixed read-only template
(`tail`, `grep`, bounded scan) with shell-quoting; `log_path` must match an
absolute-path allowlist; `ssh_exec` additionally enforces a command allowlist
and rejects redirection, substitution, and mutating commands. Output is
truncated and every call is bounded by a timeout.

`ssh_exec` also accepts the `mysql` client so operators can inspect a cluster's
databases (the Hive metastore, for example) in the same command form. Every
`mysql` invocation is checked before it reaches a node: it must carry its query
inline with `-e`/`--execute`, that query must be a single
`SELECT`/`SHOW`/`DESC`/`EXPLAIN`/`WITH` statement, and writes, DDL,
multi-statement input, SQL comments, and client flags such as `--init-command`
are refused. Example: `mysql -h 127.0.0.1 -e "SHOW DATABASES"`.

Point DiagFlow at it and its `query_yarn` / `query_metrics` tools become thin
MCP clients instead of touching infra directly:

```bash
export DIAGFLOW_MCP_BASE_URL=http://diagflow-mcp:8090/mcp
export DIAGFLOW_MCP_TOKEN=token1
```

Not in this gateway, by design: `deepwiki_query` targets the public
`mcp.deepwiki.com` MCP server, so it uses its **own** MCP connection (different
trust/network boundary). It is **disabled by default** because log snippets and
problem descriptions leave the cluster; enable it explicitly:

```bash
export DIAGFLOW_DEEPWIKI_MCP_URL=https://mcp.deepwiki.com/mcp
```

Node operations are read-only by construction: there is no path in the MCP
server that runs a mutating command.

**Endpoints**:

| Endpoint | Purpose |
|----------|---------|
| `POST /mcp` | JSON-RPC 2.0: `initialize`, `tools/list`, `tools/call`, `ping` |
| `GET /healthz` | Liveness (process is up) |
| `GET /readyz` | Readiness: ZooKeeper resolves `UHADOOPAccessHTTP` **and** its HTTP port accepts a connection |
| `GET /metrics` | Prometheus text format (`mcp_tool_calls_total`, `mcp_tool_duration_ms`, `mcp_rejected_total`, …) |

Every request carries an `X-Request-ID` (generated or propagated) that is
echoed in the response header and every audit line, so a tool call can be
correlated with the diagnosis that triggered it.

**Policy** (all optional, enforced server-side):

- **Auth**: `agent=token` pairs; the token identifies the agent in ACL checks
  and audit logs.
- **ACL**: `agent=cluster[,cluster];agent2=*`. When set, an agent may only
  query listed clusters and unknown agents are refused (`-32003`). Empty means
  any authenticated agent may query any cluster in the zone.
- **Rate limit**: per-agent token bucket for `tools/call` (HTTP 429, `-32005`).
- **Fan-out cap**: `search_logs` samples at most `DIAGFLOW_MCP_MAX_SEARCH_NODES`
  nodes and says so in the result, instead of spending the 90s call budget on a
  large cluster.
- **Protocol negotiation**: `initialize` echoes the client's MCP revision when
  it is one of `2024-11-05` / `2025-03-26` / `2025-06-18`.

**Deployment** is a single manifest — `deploy/kubernetes/diagflow-mcp.yaml`
(Deployment + Service + PDB). The token Secret is created out of band:

```bash
kubectl -n prj-diaflow create secret generic diagflow-mcp-tokens \
  --from-literal=DIAGFLOW_MCP_TOKENS="diagflow=<token>,opsbot=<token>" \
  --from-literal=DIAGFLOW_MCP_CLIENT_TOKEN="<token>"   # the API server's own token
export IMAGE_TAG=v0.6.0
envsubst < deploy/kubernetes/diagflow-mcp.yaml | kubectl apply -f -
```


## Project Structure

```
diagflow/
├── cmd/diagflow/            # CLI + HTTP API entry point
├── cmd/diagflow-mcp/        # Read-only node-log MCP gateway
├── internal/
│   ├── config/              # Env-driven config
│   ├── diagagent/           # Five-phase diagnostic engine + investigation path
│   ├── strategy/            # YAML strategy parsing + parameter rendering
│   ├── memory/              # EvidencePool + session memory
│   ├── validator/           # Deterministic conclusion checks
│   ├── tools/v3tools/       # Tool definitions and MCP-backed implementations
│   ├── rag/                 # Vector store, retriever, fingerprint knowledge base
│   ├── infra/               # ZK discovery + UHadoop access + read-only SSH
│   ├── mcpclient/           # Node-log MCP client (Streamable HTTP)
│   ├── mcpserver/           # Node-log MCP server + auth/ACL/rate limit
│   ├── server/              # HTTP API, SSE, embedded operator console
│   └── observability/       # MySQL history + Markdown report rendering
├── deploy/                  # Dockerfile, Kubernetes manifests, deploy scripts
└── data/
    └── strategies/          # YAML diagnostic strategies
```

## Production Deployment

DiagFlow ships with a **production adapter** (`internal/infra/`) that bridges the
agent to real UHadoop infrastructure. The tool layer is identical in demo and
production — only the data source changes.

### Architecture: Demo vs Production

```
Demo Mode (default):
  Tools → simulated.Cluster (pre-built scenarios)
         ↓
  Local mock data (no network calls)

Production Mode (DIAGFLOW_MODE=production):
  Tools → RealCluster (or the MCP gateway when DIAGFLOW_MCP_BASE_URL is set)
         ↓
  ┌────────────────────────────────────────────────────────────┐
  │ infra.ZoneClusterAccess → ZooKeeper                        │
  │   resolves /NS/uhadoop/set<zone>/UHADOOPAccessHTTP         │
  │ infra.AccessClient → access HTTP API                       │
  │   describe_cluster / list_node / describe_node /           │
  │   get_cluster_service   (metadata only — NO direct MySQL)  │
  │ infra.SSHExec / LogReader → read-only SSH to the node      │
  │   (IPv6 preferred; credentials come from describe_cluster) │
  └────────────────────────────────────────────────────────────┘

  UmrAgentClient (HMAC-SHA1, port 65431) exists but is not wired into
  RealCluster; node operations currently use read-only SSH.
```

### Reused Infrastructure Patterns

The production adapter carries over existing uhadoop-task patterns in Go:

| uhadoop-task (Node.js) | DiagFlow (Go) | Notes |
|----------------------|----------------|-------|
| `libs/name_container.js` | `infra.ZoneClusterAccess` | ZK-based service discovery |
| `util/agent.js` | `infra.UmrAgentClient` (unwired) / `infra.SSHExec` / MCP node tools | Read-only node operations |
| `controllor/describe_cluster_nodes.js` | `infra.AccessClient` | Node metadata via uhadoop-access HTTP API |

### Deploy to Kubernetes

```bash
# Build image
docker build -t uhadoop/diagflow:v0.1.0 -f deploy/Dockerfile .

# Deploy
kubectl apply -f deploy/kubernetes/diagflow-statefulset.yaml

# Set secrets
kubectl create secret generic diagflow-secrets \
  --from-literal=DEEPSEEK_API_KEY=sk-... \
  --from-literal=DIAGFLOW_AUTH_TOKEN='<long-random-token>' -n prj-diaflow
```

Required environment variables (see `deploy/kubernetes/`):

| Variable | Purpose |
|----------|---------|
| `DIAGFLOW_MODE=production` | Talk to real infrastructure instead of the simulated demo cluster |
| `DIAGFLOW_REGION` | Region code (falls back to flat `REGION`) |
| `DIAGFLOW_ZONE` | **Availability zone this instance serves** (falls back to flat `ZONE`); the MCP server uses it as the ZooKeeper set id |
| `DIAGFLOW_ZK_HOSTS` | ZooKeeper ensemble for service discovery, e.g. `10.72.136.107:2181,...` |
| `DIAGFLOW_INFRA__ZONE_ENDPOINTS` | `zone1=http://...,zone2=http://...` — per-zone uhadoop-manage endpoints |
| `UHADOOP_MANAGE_HTTP_BASE` | Global uhadoop-manage endpoint (only used by `direct` discovery mode) |
| `DEEPSEEK_API_KEY` | LLM API key (via modelverse proxy) |
| `DIAGFLOW_AUTH_TOKEN` | Bearer token required by the HTTP API; keep it in a Kubernetes Secret |
| `DIAGFLOW_LLM__LANGUAGE` | Language for LLM-generated report fields (root cause, suggestions, judgments). Defaults to `zh-CN`; set `en` for English |

### Zone-level deployment

DiagFlow is deployed **per availability zone**. An instance is pinned to one
`(region, zone)` and resolves that zone's infrastructure endpoint; it will
never silently fall back to another zone's manager. In production, requests must
carry a `cluster_id`, and `zone` is echoed back by `GET /readyz`:

```bash
export DIAGFLOW_MODE=production
export DIAGFLOW_REGION=test03
export DIAGFLOW_ZONE=test03-01
export DIAGFLOW_INFRA__ZONE_ENDPOINTS="test03-01=http://uhadoop-manage:20141"
./diagflow -serve

./diagflow -cluster uhadoop-xxxx --investigate   # CLI, production
```

The MCP server is zone-scoped too: `DIAGFLOW_ZONE` (the ZooKeeper set id) and
`DIAGFLOW_ZK_HOSTS` are **required** when starting `diagflow-mcp`.

### Safety Guarantees

- **No direct MySQL metadata access**: node/cluster metadata comes from the zone's
  UHadoop access HTTP action API, discovered via ZooKeeper. DiagFlow never touches
  cluster node tables directly. (Operators can still run a single read-only
  `mysql -e "SELECT ..."` through `ssh_exec`; that is a node command, not a
  metadata source.)
- **No write operations on nodes**: the MCP server only ever runs read-only
  commands. Node log/metrics/base-info/process/YARN commands are built from
  fixed templates; `ssh_exec` runs through a command allowlist that rejects
  redirection, command substitution, and mutating commands. Credentials are
  redacted from every tool result.
- **Timeout isolation**: every tool call has a configurable timeout; failures
  degrade gracefully rather than blocking diagnosis.
- **Audit trail**: every diagnosis produces a step-by-step trace; optional MySQL
  persistence (`internal/observability/store`, schema in `sql/schema.sql`)
  records a diagnosis summary when the database is reachable. It degrades to
  disabled when MySQL is unset or unreachable.
