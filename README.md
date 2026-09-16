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
                    │  Phase 1  KB semantic search │ ← 0 LLM
                    │  Phase 2  YAML strategy exec │ ← 0 LLM (parallel by priority)
                    │  Phase 2.5 (removed)         │
                    │  (semantic fast-path only)   │
                    │  Phase 3  SDK ReAct          │ ← LLM (tool_use loop)
                    │  Phase 4  Synthesize + verify│ ← LLM (4-layer validator)
                    │  Phase 5  Auto-index to KB   │ ← 0 LLM (high-confidence only)
                    └──────────────┬──────────────┘
                                   ▼
                     DiagnosisReport → Markdown
```

- **Phase 1–2.5** cover the deterministic fast paths — semantic search, YAML-driven
  evidence collection, and exact fingerprint matching.
- **Phase 3** hands the remaining unknown to the LLM via the Anthropic SDK's native
  tool loop (translated to DeepSeek by the modelverse proxy).
- **Phase 4** synthesizes a structured conclusion and validates it through four
  hallucination-control layers.

## Features

### Single-Agent ReAct + YAML Strategy

- **SDK-native tool use**: `DiagAgent` uses `Anthropic().messages.create(...)` with the
  tool schema, relying on the SDK's native `tool_use` loop rather than a hand-rolled ReAct.
- **YAML-driven deterministic phase**: `data/strategies/*.yaml` defines *what* to collect
  (which tools, in what order, at what priority). Same-priority steps run in parallel.
  Steps support conditional branching via `llm_decide` + `if_decision`.
- **Evidence pool**: findings are deposited in a thread-safe `EvidencePool` and shared
  across phases; specialist steps never talk to each other directly.

### 5 Tools (`tools/v3tools.py`)

| Tool | Purpose |
|------|---------|
| `query_yarn` | Query YARN RM for apps / node placement |
| `ssh_exec` | Read-only shell commands (allowlist + blacklist guarded) |
| `call_umr_agent` | HMAC-SHA1 signed umrAgent calls (GetLogs / CheckProcess / GetBaseInfo / GetAppList) |
| `deepwiki_query` | Verify error classes against upstream component repos via MCP |
| `fingerprint_match` | MD5 exact-match against known historical cases |

### Hybrid RAG (ChromaDB + BM25)

- **Fingerprint Match**: MD5-based exact match for known issues (fast path, zero LLM cost)
- **Semantic Search**: ChromaDB / Milvus Lite vector similarity for intent-level matching
- **BM25**: Keyword-level exact match for error codes and stack traces
- **RRF Fusion**: Reciprocal Rank Fusion to merge both result sets
- Falls back to a Milvus Lite backend via `DIAGFLOW_VECTOR_STORE__BACKEND=milvus`.

### Four-Layer Illusion Control

| Layer | Method | LLM? |
|-------|--------|------|
| 1 | Format validation (root cause length, non-empty suggestions/evidence) | ❌ |
| 2 | Cross-source consistency (root cause references evidence keywords) | ❌ |
| 3 | Independent LLM review (structured `validate_diagnosis` tool call) | ✅ |
| 4 | Retry with feedback (max 2) | ✅ |

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
| GET    | `/readyz`           | Readiness |
| GET    | `/metrics`          | Prometheus metrics |
| POST   | `/api/v1/diagnose`  | Run a diagnosis |
| POST   | `/api/v1/investigate` | Intent-driven investigation |

```bash
curl -s localhost:8080/api/v1/diagnose \
  -d '{"component":"flink","problem":"Job FAILED","scenario":"flink_oom"}' | jq .
```

> **Degraded mode note**: semantic KB embeddings are not yet wired; the KB
> fast-path stays disabled and only deterministic/fingerprint paths run.

### Node-log MCP Server (shared by multiple agents)

```bash
export DIAGFLOW_MCP_TOKENS="diagflow=token1,opsbot=token2"
export DIAGFLOW_ZK_HOSTS="10.72.136.107:2181,10.72.136.108:2181"
export DIAGFLOW_ZONE=666003001        # availability-zone id, scopes ZK discovery
./diagflow-mcp -port 8090
```

The MCP server is the **single cluster-access gateway**: DiagFlow agents hold
no cluster credentials, only a bearer token. It exposes, over Streamable HTTP
(`POST /mcp`, JSON-RPC 2.0), with per-agent bearer-token auth and audit logging.

It has two data planes:

| MCP tool | Backend |
|----------|---------|
| `get_cluster_info` | access API `describe_cluster` (via ZooKeeper discovery) |
| `list_nodes` | access API `list_node` |
| `get_node_info` | access API `describe_node` (includes per-node services) |
| `get_cluster_services` | access API `get_cluster_service` |
| `get_node_log` | read-only SSH `tail`/`grep` on the node |
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
trust/network boundary). It is enabled by default and calls DeepWiki's
`ask_question` tool:

```bash
# default: https://mcp.deepwiki.com/mcp
export DIAGFLOW_DEEPWIKI_MCP_URL=https://mcp.deepwiki.com/mcp
```

Node operations are read-only by construction: there is no path in the MCP
server that runs a mutating command.


## Project Structure

```
diagflow/
├── demo/run.py              # Entry point: CLI demo with scenario selection
├── diagflow/
│   ├── config.py            # Centralized env-driven config
│   ├── core/                # ★ Single-Agent diagnostic engine
│   │   ├── diag_agent.py    #   DiagAgent: 5-phase pipeline (798 lines)
│   │   ├── strategy.py      #   YAML strategy parsing + Step rendering
│   │   ├── memory.py        #   EvidencePool + SessionMemory
│   │   └── validator.py     #   4-layer hallucination control
│   ├── tools/v3tools.py     # 5 tools (query_yarn/ssh_exec/call_umr_agent/...)
│   ├── rag/                 # RAG system
│   │   ├── vector_store.py  #   ChromaDB / Milvus Lite factory
│   │   ├── embedder.py      #   OpenAI embeddings + offline hash fallback
│   │   ├── retriever.py     #   Hybrid search (semantic + BM25 + RRF)
│   │   └── knowledge_base.py#   Case lifecycle + fingerprint persistence
│   ├── infra/__init__.py    # Production adapter (ZK discovery + umrAgent + node API)
│   ├── conversation/        # CLI/Web conversation manager (multi-turn)
│   ├── simulated/           # Mock environment for demo
│   │   └── scenarios/       # 4 pre-built fault scenarios
│   └── observability/       # MySQL persistence + event tracker + Markdown report
├── tests/                   # pytest suite
└── data/
    ├── cases/               # Seeded historical cases (Markdown)
    └── strategies/          # Configurable strategy YAML files
```

## Production Deployment

DiagFlow ships with a **production adapter** (`diagflow/infra/`) that bridges the
agent to real UHadoop infrastructure. The Tool layer is identical in demo and
production — only the data source changes.

### Architecture: Demo vs Production

```
Demo Mode (default):
  Tools → SimulatedCluster (pre-built scenarios)
         ↓
  Local mock data (no network calls)

Production Mode (DIAGFLOW_MODE=production):
  Tools → RealCluster
         ↓
  ┌──────────────────────────────────────────────────┐
  │ UCloudServiceDiscovery → ZooKeeper (NameContainer) │
  │                 (find uhadoop-manage, monitor)     │
  │ NodeInfoClient → uhadoop-manage HTTP API           │
  │                 (cluster/node metadata — NO MySQL) │
  │ UmrAgentClient → http://[ipv6]:65431/?Action=...   │
  │                 (HMAC-SHA1 signed, port of util/agent.js) │
  └──────────────────────────────────────────────────┘
```

### Reused Infrastructure Patterns

The production adapter faithfully ports existing uhadoop-task patterns to Python:

| uhadoop-task (Node.js) | DiagFlow (Python) | Notes |
|----------------------|------------------|-------|
| `libs/name_container.js` | `UCloudServiceDiscovery` | ZK-based service discovery |
| `util/agent.js` | `UmrAgentClient` | HMAC-SHA1 signing replicated exactly |
| `controllor/describe_cluster_nodes.js` | `NodeInfoClient` | Node metadata via uhadoop-manage HTTP API |

### Deploy to Kubernetes

```bash
# Build image
docker build -t uhadoop/diagflow:v0.1.0 -f deploy/Dockerfile .

# Deploy
kubectl apply -f deploy/kubernetes/diagflow-statefulset.yaml

# Set secrets
kubectl create secret generic diagflow-secrets \
  --from-literal=DEEPSEEK_API_KEY=sk-... -n uhadoop
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

- **No direct MySQL access**: node/cluster metadata comes from the zone's UHadoop
  access HTTP action API, discovered via ZooKeeper. DiagFlow never touches
  cluster node tables directly.
- **No write operations on nodes**: the MCP server only ever runs read-only
  commands. Node log/metrics/base-info/process/YARN commands are built from
  fixed templates; `ssh_exec` runs through a command allowlist that rejects
  redirection, command substitution, and mutating commands. Credentials are
  redacted from every tool result.
- **Timeout isolation**: every tool call has a configurable timeout; failures
  degrade gracefully rather than blocking diagnosis.
- **Audit trail**: every diagnosis produces a step-by-step trace; optional MySQL
  persistence (via `observability/db.py`) records diagnosis history and step logs
  when the database is reachable.
