# 生产级 LLM Agent 可观测体系

## 架构总览

```
┌─────────────────────────────────────────────────────────────┐
│                      DiagFlow Pod                           │
│                                                             │
│  DiagAgent.diagnose()                                       │
│    │                                                        │
│    ├──→ /tmp/diagflow/<event_id>/  文件追溯 (已有)           │
│    │    保留用于单次诊断 debug                                │
│    │                                                        │
│    ├──→ Prometheus metrics        ← 新增: 聚合监控          │
│    │    diagnosis_total, phase_duration_seconds,             │
│    │    kb_hit_total, llm_token_total, ...                   │
│    │                                                        │
│    └──→ MetricsLogger              ← 新增: 结构化指标        │
│         JSON Lines → Loki/Elasticsearch                     │
│         按 event_id 串联全链路                                │
│                                                             │
├─────────────────────────────────────────────────────────────┤
│                        外部服务                              │
│                                                             │
│  Prometheus + Grafana  ─→ 聚合面板、告警规则                  │
│  Loki/ES              ─→ 全链路搜索、event_id 全文检索         │
│  Langfuse (可选)       ─→ LLM 专项: token 成本、prompt 版本    │
└─────────────────────────────────────────────────────────────┘
```

---

## Layer 1: Prometheus Metrics（必须）

解决"整体怎么样"的问题，在 diagflow/observability/ 下新增:

```python
# diagflow/observability/metrics.py

from prometheus_client import Counter, Histogram, Gauge, CollectorRegistry, generate_latest

registry = CollectorRegistry()

# 诊断结果
diagnosis_total = Counter(
    "diagflow_diagnosis_total", "Total diagnoses",
    ["component", "problem_type", "outcome"],  # outcome: kb_hit/react/skip
    registry=registry,
)
diagnosis_duration = Histogram(
    "diagflow_diagnosis_duration_seconds", "Diagnosis duration",
    ["component"],
    buckets=[1, 5, 10, 30, 60, 120, 300],
    registry=registry,
)

# 各 Phase 耗时
phase_duration = Histogram(
    "diagflow_phase_duration_seconds", "Per-phase duration",
    ["phase"],  # phase_1, phase_2, phase_3, phase_4
    buckets=[0.001, 0.01, 0.1, 1, 5, 10, 30, 60],
    registry=registry,
)

# KB 命中
kb_hit_total = Counter(
    "diagflow_kb_hit_total", "KB hit count",
    ["match_type"],  # semantic (Phase 1), fingerprint (Phase 2.5)
    registry=registry,
)

# LLM 调用
llm_call_total = Counter(
    "diagflow_llm_call_total", "LLM call count",
    ["phase", "model"],  # phase_3_react, phase_4_synthesis, phase_4_validate
    registry=registry,
)
llm_token_total = Counter(
    "diagflow_llm_token_total", "Token consumption",
    ["phase", "direction"],  # direction: input/output
    registry=registry,
)

# 工具结果
tool_result_total = Counter(
    "diagflow_tool_result_total", "Tool call results",
    ["tool", "status"],  # status: success/error/timeout
    registry=registry,
)

# 置信度分布
confidence_distribution = Counter(
    "diagflow_confidence_total", "Confidence distribution",
    ["confidence"],  # high/medium/low
    registry=registry,
)
```

在 DiagAgent 中使用:

```python
# diag_agent.py 关键节点埋点

async def diagnose(self, ...):
    start = time.monotonic()

    # Phase 1
    t0 = time.monotonic()
    hit = self._kb_match(context)
    phase_duration.labels(phase="phase_1").observe(time.monotonic() - t0)
    if hit:
        kb_hit_total.labels(match_type="semantic").inc()
        diagnosis_total.labels(component, problem_type, "kb_hit").inc()
        diagnosis_duration.labels(component).observe(time.monotonic() - start)
        return ...

    # Phase 2
    t0 = time.monotonic()
    await self._run_strategy(...)
    phase_duration.labels(phase="phase_2").observe(time.monotonic() - t0)

    # Phase 3 — 每次 LLM 调用
    # 在 _run_react 里:
    #   llm_call_total.labels(phase="phase_3_react", model=self.model).inc()
    #   llm_token_total.labels(phase="phase_3_react", direction="input").inc(input_tokens)

    # Phase 4
    #   llm_call_total.labels(phase="phase_4_synthesis", model=self.model).inc()
    #   confidence_distribution.labels(confidence=confidence).inc()

    # 收尾
    diagnosis_duration.labels(component).observe(time.monotonic() - start)
```

Grafana 面板:

```
┌─ DiagFlow Dashboard ─────────────────────────────────┐
│                                                      │
│  📊 诊断量 (每小时)      📊 KB 命中率趋势             │
│  📊 各 Phase P99 延迟    📊 LLM Token 消耗 (日/周)    │
│  📊 工具错误率            📊 置信度分布                │
│  📊 诊断量 Top-N 故障类型                              │
│                                                      │
│  告警规则:                                            │
│  - diagnosis_total 骤降 >50% → 服务异常                │
│  - tool_result_total{status="error"} >10% → 工具故障   │
│  - llm_token_total 日消耗超预算 → 成本告警              │
│  - phase_duration P99 >120s → 诊断超时                 │
└──────────────────────────────────────────────────────┘
```

---

## Layer 2: 结构化日志 → Loki/ES（强烈建议）

解决"某次诊断为什么失败"的检索问题:

```python
# 在 configure_logging() 中输出 JSON 格式
{
  "ts": "2026-07-28T10:30:00Z",
  "level": "INFO",
  "event_id": "diag-1753698600-1234",  # ← 贯穿全链路
  "phase": "phase_3",
  "msg": "yarn_logs returned 3 FAILED apps",
  "tool": "query_yarn",
  "duration_ms": 234
}
```

这样在 Loki 里 `{event_id="diag-1753698600-1234"}` 就能看到完整链路，不需要 SSH 到 Pod 上翻文件。

---

## Layer 3: Langfuse（可选，按需开启）

解决"LLM 专项分析":

| 需求 | Prometheus 能否满足 | Langfuse 价值 |
|------|:---:|------|
| 今天 token 花了多少钱 | ✅ Counter | 更精细的 per-session 分解 |
| 哪个 prompt 版本更好 | ❌ | ✅ A/B 测试 + 评估 |
| 用户评价"这个诊断不对" | ❌ | ✅ 反馈收集 + 关联 trace |
| LLM 输出质量退化检测 | ❌ | ✅ 评估数据集自动跑分 |
| 单次 LLM 调用的完整 I/O | ⚠️ 太详细不适合 Prometheus | ✅ Trace 详情页 |

**建议**: 先上 Prometheus + Loki，Langfuse 留集成接口但不作为依赖。代码中只加一个可配置的回调:

```python
# config.py
@dataclass
class ObservabilityConfig:
    langfuse_enabled: bool = False
    langfuse_public_key: str = ""
    langfuse_secret_key: str = ""

# diag_agent.py — LLM 调用埋点
async def _call_llm(self, ...):
    if self._langfuse:
        trace = self._langfuse.trace(name="diagnosis", metadata={"event_id": event_id})
        generation = trace.generation(name="react_step", model=self.model, ...)
        # ... LLM call ...
        generation.end(output=response)

    # 始终记录 Prometheus metrics (不依赖 Langfuse)
    llm_call_total.labels(...).inc()
```

---

## 优先级

| 优先级 | 组件 | 几天能上线 | 解决什么 |
|:---:|------|:---:|------|
| P0 | Prometheus metrics | 1 天 | "系统还正常吗？" |
| P0 | JSON 结构化日志 + event_id | 0.5 天 | "这次诊断为什么失败？" |
| P1 | Grafana Dashboard | 0.5 天 | "趋势怎样？" |
| P1 | Prometheus 告警规则 | 0.5 天 | "出问题了通知谁？" |
| P2 | Langfuse 集成 (可选开关) | 1 天 | "LLM 成本/AI 质量细节" |

**Langfuse 不是不需要，但不是第一优先级。** Prometheus metrics 覆盖了 80% 的生产监控需求，Langfuse 解决的是那 20% 的 LLM 专项分析。先上线跑起来，等真实流量把前两层验证过了，再按需开 Langfuse。
