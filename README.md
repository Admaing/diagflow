# DiagFlow — AI-Powered Diagnostic Agent for Big Data Platforms

[![Python 3.11+](https://img.shields.io/badge/python-3.11+-blue.svg)]()
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
                    │  Phase 2.5 MD5 fingerprint   │ ← 0 LLM
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

- Python 3.11+
- `DEEPSEEK_API_KEY` (or `LLM_API_KEY`) for LLM-powered mode; mock mode works without it.

### Installation

```bash
git clone https://github.com/your-username/diagflow.git
cd diagflow
python -m venv .venv && source .venv/bin/activate
pip install -e .
```

### Run Demo

```bash
# List available scenarios
python -m demo.run --list

# Run Flink OOM diagnosis (mock mode — no API key needed)
python -m demo.run --scenario flink_oom

# Run with real LLM (via modelverse proxy → DeepSeek)
export DEEPSEEK_API_KEY=sk-...
python -m demo.run --scenario flink_oom

# Run all scenarios
python -m demo.run --all
```

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
| `DIAGFLOW_MODE=production` | Enable real infrastructure (vs demo) |
| `ZK_HOSTS` | ZooKeeper connection for service discovery |
| `UHADOOP_MANAGE_HTTP_BASE` | Fallback uhadoop-manage HTTP endpoint (when ZK discovery fails) |
| `DEEPSEEK_API_KEY` | LLM API key (via modelverse proxy) |
| `REGION` | Region code for ZK name resolution |

### Safety Guarantees

- **No direct MySQL access**: node/cluster metadata comes via the `NodeInfoClient`
  → uhadoop-manage HTTP API. DiagFlow never touches cluster node tables directly.
- **No write operations on nodes**: `ssh_exec` commands pass an allowlist + blacklist
  filter (`config.security`), and umrAgent calls are restricted to `GetLogs`,
  `CheckProcess`, `GetBaseInfo`, `GetAppList` — no `Execute` or restart actions.
- **Timeout isolation**: every tool call has a configurable timeout; failures
  degrade gracefully rather than blocking diagnosis.
- **Audit trail**: every diagnosis produces a step-by-step trace; optional MySQL
  persistence (via `observability/db.py`) records diagnosis history and step logs
  when the database is reachable.
