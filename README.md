# DiagFlow — AI-Powered Diagnostic Agent for Big Data Platforms

[![Python 3.11+](https://img.shields.io/badge/python-3.11+-blue.svg)]()
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)]()

**DiagFlow** is an AI agent system that automates root cause analysis for big data platform failures. Given a user-reported problem (e.g., "Flink job FAILED", "HDFS no space left"), DiagFlow systematically gathers evidence from logs, metrics, and configuration, then produces a structured diagnosis with actionable suggestions.

Inspired by [得物技术团队的 Troubleshooter](https://cloud.tencent.com/developer/article/2682409), DiagFlow extends the concept with self-built agent framework, hybrid search RAG, and multi-agent parallel orchestration.

## Architecture

```
┌──────────────────────────────────────────────────────────┐
│                     User Input                            │
│         (problem description + cluster context)           │
└─────────────────────┬────────────────────────────────────┘
                      │
┌─────────────────────▼────────────────────────────────────┐
│                  Orchestrator                             │
│    Coordinates multi-agent diagnosis workflow             │
└────┬──────────┬──────────┬──────────┬────────────────────┘
     │          │          │          │
┌────▼───┐ ┌───▼────┐ ┌──▼────┐ ┌──▼──────────┐
│  Log   │ │ Config │ │Metric │ │ Knowledge   │
│ Analyst│ │ Analyst│ │Analyst│ │ Agent (RAG) │
└───┬────┘ └───┬────┘ └───┬───┘ └──┬─────────┘
    │          │          │        │
    │     ┌────▼──────────▼──┐     │
    │     │  Tool Registry   │     │
    │     │  (5 tools)       │     │
    │     └────────┬─────────┘     │
    │              │               │
┌───▼──────────────▼───────────────▼──────────────────┐
│              Simulated Environment                   │
│  (4 pre-built fault scenarios, realistic logs)       │
└─────────────────────────────────────────────────────┘
```

## Features

### Agent Framework (自建, ~500 行核心代码)
- **ReAct Loop**: Reasoning → Acting → Observation cycle (inspired by Yao et al. 2022)
- **Tool Abstraction**: Each tool has JSON Schema, timeout isolation, structured result
- **Evidence Pool**: Decoupled evidence sharing between specialist agents
- **Session Memory**: Sliding-window context management

### Multi-Agent Architecture
| Agent | Role | Tools |
|-------|------|-------|
| **Supervisor** | Orchestrates diagnosis, synthesizes final report | All tools |
| **Log Analyst** | Extracts error patterns from component logs | `query_node_log` |
| **Config Analyst** | Reviews configuration for misconfigurations | `read_config` |
| **Metrics Analyst** | Analyzes monitoring metrics for anomalies | `query_metrics` |

### Hybrid RAG (ChromaDB + BM25)
- **Fingerprint Match**: MD5-based exact match for known issues (fast path, zero LLM cost)
- **Semantic Search**: ChromaDB vector similarity for intent-level matching
- **BM25**: Keyword-level exact match for error codes and stack traces
- **RRF Fusion**: Reciprocal Rank Fusion to merge both result sets

### Four-Layer Illusion Control
| Layer | Method | LLM? | Latency |
|-------|--------|------|---------|
| 1 | Format validation (required sections, table structure) | ❌ | <1ms |
| 2 | Cross-source consistency (timeline, metric × log cross-ref) | ❌ | <1ms |
| 3 | Independent Validation Agent review | ✅ | ~1s |
| 4 | Retry with feedback (max 2, format issues don't count) | ✅ | controlled |

### Pre-built Fault Scenarios
| Scenario | Component | Fault | Expected Root Cause |
|----------|-----------|-------|-------------------|
| `flink_oom` | Flink 1.14.3 | TaskManager OOM | Java heap space OutOfMemoryError |
| `flink_checkpoint_fail` | Flink 1.16.0 | Checkpoint failure | Checkpoint alignment timeout due to backpressure |
| `hdfs_disk_full` | HDFS 3.3.4 | Disk full | DataNode volume out of space |
| `yarn_queue_stuck` | YARN 3.3.4 | Queue congestion | Production queue at 100% capacity |

## Quick Start

### Prerequisites
- Python 3.11+
- Anthropic API key (for LLM-powered mode; mock mode works without it)

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

# Run with real LLM
export ANTHROPIC_API_KEY=sk-...
python -m demo.run --scenario flink_oom

# Run all scenarios
python -m demo.run --all
```

## Project Structure

```
diagflow/
├── demo/run.py              # Entry point: CLI demo with scenario selection
├── diagflow/
│   ├── core/                # ★ Self-built Agent framework
│   │   ├── agent.py         #   ReAct loop base class
│   │   ├── tool.py          #   Tool abstraction with timeout isolation
│   │   ├── llm.py           #   LLM client (Anthropic, round-robin keys)
│   │   ├── memory.py        #   Session memory + EvidencePool
│   │   ├── orchestrator.py  #   Multi-agent orchestrator
│   │   └── validator.py     #   4-layer illusion control
│   ├── agents/              # Multi-agent implementations
│   │   ├── supervisor.py
│   │   ├── log_analyst.py
│   │   ├── metrics_analyst.py
│   │   └── validation_agent.py
│   ├── tools/               # Tool implementations (5 tools)
│   │   ├── node_log.py
│   │   ├── flink_api.py
│   │   ├── metrics_api.py
│   │   └── fingerprint.py
│   ├── rag/                 # RAG system
│   │   ├── vector_store.py  #   ChromaDB wrapper
│   │   ├── embedder.py      #   Embedding abstraction
│   │   ├── retriever.py     #   Hybrid search (semantic + BM25 + RRF)
│   │   └── knowledge_base.py#   Case lifecycle management
│   ├── workflows/           # Component-specific diagnostic workflows
│   ├── simulated/           # Mock environment for demo
│   │   └── scenarios/       # 4 pre-built fault scenarios
│   └── observability/       # Event tracking + report generation
└── data/
    ├── cases/               # Seeded historical cases (Markdown)
    └── strategies/          # Configurable strategy YAML files
```

## Comparison: DiagFlow vs. Troubleshooter (得物)

| Dimension | Troubleshooter | DiagFlow |
|-----------|---------------|----------|
| Agent Framework | Spring AI Alibaba | **Self-built** (ReAct ~500 lines) |
| Agent Count | Single Supervisor | **Multi-agent** (Supervisor + 3 specialists) |
| Agent Execution | Serial | **Parallel** (specialists run concurrently) |
| RAG | MD5 fingerprint only | **Hybrid** (fingerprint + ChromaDB + BM25 + RRF) |
| Target Systems | Microservices (Java) | **Big Data** (Flink / HDFS / YARN) |
| Language | Java | **Python** |
| Simulated Env | N/A (production only) | **4 pre-built scenarios** for demo |

## Examples

### Input
```
Component: flink
Problem: 任务挂掉，状态 FAILED
Cluster: c-uhadoop-001 (北京二)
Version: 1.14.3
Job ID: job_xxx
Detail: 最后一次 Checkpoint 失败
```

### Output
```markdown
## 根因分析
**根因**: TaskManager Java heap space OutOfMemoryError
**置信度**: 🟢 高

## 修复建议
1. 增加 taskmanager.memory.heap.size 至 4096m
2. 降低 parallelism.default 缓解内存争用
3. 配置 input_rate_mbps 告警阈值（>30 MB/s 触发）

## 证据链
- [log_analyst] TaskManager-1 OOM at 14:23:01 (conf=0.9)
- [metrics_analyst] Heap usage 95.5%, GC pause avg 850ms (conf=0.8)
- [config_analyst] 2GB heap for 4 slots (conf=0.7)
```

## Production Deployment

DiagFlow ships with a **production adapter** (`diagflow/infra/`) that bridges the
agent framework to real UHadoop infrastructure. The Tool layer is identical in
demo and production — only the data source changes.

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
  │ MySQLClient    → t_uhadoop_node + t_uhadoop_umragent │
  │                 (node IPs, ipv6, agent keys)        │
  │ UCloudServiceDiscovery → ZooKeeper (NameContainer)  │
  │                 (find uhadoop-go monitor, etc.)     │
  │ UmrAgentClient → http://[ipv6]:65431/?Action=GetLogs│
  │                 (HMAC-SHA1 signed, port of util/agent.js) │
  └──────────────────────────────────────────────────┘
```

### Reused Infrastructure Patterns

The production adapter faithfully ports existing uhadoop-task patterns to Python:

| uhadoop-task (Node.js) | DiagFlow (Python) | Notes |
|----------------------|------------------|-------|
| `libs/name_container.js` | `UCloudServiceDiscovery` | ZK-based service discovery |
| `libs/db.js` | `MySQLClient` | Connection pool, **read-only account** |
| `util/agent.js` | `UmrAgentClient` | HMAC-SHA1 signing replicated exactly |
| `logic/cluster_info.js` | `ClusterInfoRepository` | Read-only node/cluster lookups |

### Deploy to Kubernetes

```bash
# Build image
docker build -t uhadoop/diagflow:v0.1.0 -f deploy/Dockerfile .

# Deploy
kubectl apply -f deploy/k8s.yaml

# Set secrets
kubectl create secret generic diagflow-secrets \
  --from-literal=ANTHROPIC_API_KEY=sk-... \
  --from-literal=DB_UHADOOP_PASSWORD=... -n uhadoop
```

Required environment variables (see `deploy/k8s.yaml`):

| Variable | Purpose |
|----------|---------|
| `DIAGFLOW_MODE=production` | Enable real infrastructure (vs demo) |
| `ZK_HOSTS` | ZooKeeper connection for service discovery |
| `DB_UHADOOP_*` | MySQL (read-only account, shared uhadoop DB) |
| `ANTHROPIC_API_KEY` | LLM API key |
| `REGION` | Region code for ZK name resolution |

### Safety Guarantees

- **Read-only DB access**: DiagFlow uses a dedicated read-only MySQL account.
  The shared uhadoop DB is never mutated (per CLAUDE.md guidance).
- **No write operations on nodes**: umrAgent calls are restricted to `GetLogs`,
  `GetAppList` — no `Execute` or restart actions.
- **Timeout isolation**: every tool call has a configurable timeout; failures
  degrade gracefully rather than blocking diagnosis.
- **Audit trail**: every diagnosis produces a full step-by-step trace under
  `/tmp/diagflow/<event_id>/`.

