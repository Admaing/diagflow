# DiagFlow 完整链路与设计原理

## 一、总览

一次诊断从用户输入到输出报告，经过 **5 个 Phase**。

核心设计哲学：**能用确定性逻辑解决的问题，绝不浪费 LLM 调用**。

---

## 二、全景数据流

```
用户: "uhadoop-1rz1p 的 Flink 任务一直挂，OOM"
  │
  ▼
┌─ ConversationManager ─────────────────────────────────────┐
│ 1. _extract_info()  → component=flink, problem=job_failure│
│ 2. 缺失字段追问 → version=1.17, cluster_id=uhadoop-1rz1p │
│ 3. 组装 context:                                           │
│    {component:"flink", problem:"job_failure",              │
│     cluster_id:"uhadoop-1rz1p", version:"1.17",            │
│     detail:"任务一直挂 OOM"}                                │
└──────────────────────┬────────────────────────────────────┘
                       ▼
┌─ DiagAgent.diagnose(context) ────────────────────────────┐
│                                                          │
│  context → Phase 1 → Phase 2 → Phase 2.5                  │
│                      ↓ (未命中)                            │
│            Phase 3 → Phase 4 → Phase 5                    │
│                                                          │
│  输出: DiagnosisReport {                                 │
│    root_cause: "TaskManager 2G heap 配 4 slots 导致 OOM", │
│    confidence: "high",                                   │
│    suggestions: ["增大 heap 到 4G", "减少 slot 到 2"],    │
│    evidence_summary: [...],                              │
│  }                                                       │
│                                                          │
│  呈现: render_report() → Markdown                        │
│         → Web UI / CLI 显示给用户                         │
└──────────────────────────────────────────────────────────┘
```

---

## 三、Phase 1 — KB 语义检索

```
输入: context = {component:"flink", problem:"job_failure", detail:"一直挂 OOM"}

┌──────────────────────────────────────────────────────────┐
│ _kb_match(context)                                       │
│                                                          │
│  if embedder 没有 API key:                                │
│    → return None  (跳过，fallback hash 向量没有语义)       │
│                                                          │
│  query = "flink job_failure 一直挂 OOM"                   │
│    ↓                                                     │
│  kb.search(query, n=3)                                   │
│    ├─ ChromaDB 语义: cosine 相似度                        │
│    ├─ BM25 关键词: "OOM", "flink" 词频匹配               │
│    └─ RRF 融合: 两个排名加权合并                          │
│    ↓                                                     │
│  fusion_score > 0.01 ?                                   │
│    ✅ → 返回缓存的 root_cause + suggestions               │
│    ❌ → return None                                      │
└──────────────────────────────────────────────────────────┘

LLM 调用: 0
耗时: <10ms (有真实 embedder) / 0ms (跳过)
```

**设计原因**: 很多生产问题会重复出现。上次诊断过的 OOM 问题，下次用户描述类似现象时应该直接返回已知结论。用自然语言语义检索而非精确关键词，因为用户描述不规范（"挂了"、"报错"、"一直重启"都有可能是同一个问题）。

---

## 四、Phase 2 — YAML 策略确定性执行

```
输入: strategy = load_strategy("flink", "job_failure")

┌─ 策略文件 data/strategies/flink_job_failure.yaml ──────┐
│ steps:                                                  │
│   - action: fingerprint_match    priority: 0  (串行)    │
│   - tool: query_yarn             priority: 1  (并行)    │
│     params: {action: list_apps}                         │
│   - tool: ssh_exec               priority: 1  (并行)    │
│     params: {node_name: "{{ context.cluster_id }}-master1",│
│              cmd: "grep ERROR .../taskmanager.log"}    │
│   - tool: call_umr_agent         priority: 2  (并行)    │
│     params: {action: GetBaseInfo}                       │
│   - tool: deepwiki_query         priority: 3  (串行)    │
│     params: {component: flink,                          │
│              question: "OOM known bugs"}                │
└──────────────────────────────────────────────────────────┘

执行流程:
  Priority 0:  fingerprint_match → 跳过 (Phase 1 已覆盖)
  Priority 1:  query_yarn ────┐
              ssh_exec ───────┤ asyncio.gather 并行
                              ↓
  Priority 2:  call_umr_agent → 获取节点基础信息
  Priority 3:  deepwiki_query → 查已知 bug

每步结果 → EvidencePool.add(Evidence(...))

LLM 调用: 0
耗时: 取决于网络/SSH，同 priority 并行所以只算最慢的那个
```

**设计原因**: 确定性步骤不需要 AI 判断——查 YARN 列表、grep 日志、获取节点信息，这些是**机械操作**。用 YAML 描述策略允许运维人员在不改代码的情况下调整排查流程。同优先级并行减少等待时间。

---

## 五、Phase 2.5 — 证据驱动的 MD5 精确匹配

```
证据收集完后，EvidencePool 里有具体错误信息:
  "java.lang.OutOfMemoryError: Java heap space"
  "gc overhead 95.5%"
  "exit code 137"

┌──────────────────────────────────────────────────────────┐
│ _kb_evidence_match(context, evidence)                    │
│                                                          │
│  for ev in evidence.all():                               │
│    for kw in ["OutOfMemoryError","OOM","FATAL",...]:     │
│      if kw in ev.detail:                                 │
│        hit = kb.fingerprint_match("flink", kw, "1.17")   │
│          ↓ MD5("flink:OutOfMemoryError:1.17") = exact    │
│        if hit → ✅ 首次真正有意义的精确匹配                │
│                                                          │
│  匹配到 → 返回 DiagnosisReport (跳过 LLM)                 │
│  未匹配 → 进入 Phase 3                                    │
└──────────────────────────────────────────────────────────┘

LLM 调用: 0
耗时: <1ms (dict 查找)
```

**设计原因**: Phase 1 用自然语言找相似，Phase 2.5 用**具体错误类名**找精确匹配。两个阶段互补：Phase 1 宽松召回，Phase 2.5 精确确认。因为此时日志已经收集到，错误关键词是确定的，MD5 精确匹配才有意义。

---

## 六、Phase 3 — SDK ReAct（LLM 自由探索）

```
┌──────────────────────────────────────────────────────────┐
│ _run_react(task, evidence, tools, max_turns=12)          │
│                                                          │
│ system prompt: 注入 topology + evidence summary + playbook│
│                                                          │
│ Loop:                                                    │
│   LLM 收到 messages + tool schemas                       │
│   → LLM 决定调用哪个 tool                                 │
│   → 执行 tool.handler(**args)                             │
│   → tool result 追加到 messages                           │
│   → 3 轮无新证据? → 强制输出结论                           │
│   → 12 轮上限                                            │
│                                                          │
│ Tool schemas 暴露给 LLM:                                  │
│   query_yarn:     "查 YARN 应用状态"                      │
│   ssh_exec:       "SSH 到节点执行只读命令" ✅安全过滤      │
│   deepwiki_query: "查开源组件已知 bug"                    │
│   fingerprint_match: "查历史案例"                         │
│   call_umr_agent: "调 umrAgent 获取日志"                  │
│                                                          │
│ LLM 自主决定: 先查 YARN 找节点 → SSH 看日志 → DeepWiki    │
│              验证是不是已知 bug → 输出分析                 │
└──────────────────────────────────────────────────────────┘

LLM 调用: 1~12 次 (通常 3-6 次)
耗时: 取决于 LLM API 延迟和工具执行时间
```

**设计原因**: Phase 2 是预先规划的确定性路径，但实际排查中常有意想不到的情况——日志路径不标准、YARN 没注册、错误在另一个服务上。Phase 3 让 LLM **自由探索**，它可以根据返回结果动态调整下一步。

**3-turn 停滞检测**: 如果连续 3 次工具调用都没发现新证据，说明可能网络不通、服务未安装、或日志格式异常，强制 LLM 基于现有证据输出结论而不是无限循环。

---

## 七、Phase 4 — 结构化合成 + 四层验证

```
┌─ _synthesize() ─────────────────────────────────────────┐
│                                                          │
│  发送 EvidencePool 摘要 + context 给 LLM                  │
│  tool_choice = {"type": "tool", "name": "report_diagnosis"}│
│  强制 LLM 调用结构化 tool:                                │
│    {                                                     │
│      "root_cause": "TaskManager 2G heap 4 slots → OOM",  │
│      "confidence": "high",                               │
│      "suggestions": ["增大 heap 4G","减少 slots 到 2"],   │
│      "missing_evidence": ["未获取 JM 日志"],              │
│      "evidence_citations": ["strategy: OOM in taskmanager"]│
│    }                                                     │
│                                                          │
│  ✅ 结构化输出 → 不需要字符串解析 → 100% 可靠提取          │
└──────────────────────────────────────────────────────────┘
                         ↓
┌─ ConclusionValidator.validate() ─────────────────────────┐
│                                                          │
│  Layer 1 (0 LLM): 格式检查                                │
│    root_cause < 10 字符? → FAIL                           │
│    suggestions 为空? → FAIL                               │
│    evidence_count == 0? → FAIL                            │
│                                                          │
│  Layer 2 (0 LLM): 交叉验证                                │
│    root_cause 里有没有引用证据关键词(error/log/oom/...)?   │
│    没有 → 可能是编造的(幻视) → FAIL                        │
│                                                          │
│  Layer 3 (1 LLM): AI 验证                                 │
│    独立 LLM 检查: root_cause 够不够具体?                   │
│                  suggestions 可不可执行?                   │
│    tool_use 结构化输出 {passes: bool, reason: str}         │
│                                                          │
│  Layer 4 (max 2): 重试带反馈                              │
│    FAIL → 把 feedback 传回 _synthesize() 重新生成          │
└──────────────────────────────────────────────────────────┘

LLM 调用: 1 (合成) + 1 (验证) = 2
额外重试: 0~2 (仅验证失败时)
```

**设计原因**: 
- **结构化输出**替代字符串解析 — 之前用 `if line.startswith("ROOT_CAUSE:")` 解析，输出格式稍微变化就丢失结果。`tool_choice` 强制模型输出 JSON，100% 可靠。
- **四层验证** — LLM 的诊断可能"说得有道理但完全不对"(幻视)。Layer1-2 是确定性规则，零成本拦截明显问题；Layer3 让另一个 LLM 独立审查；Layer4 给修正机会。

---

## 八、Phase 5 — KB 自动归档（带质量门控）

```
仅当 confidence == "high" 才入库:

┌─ _kb_index() ───────────────────────────────────────────┐
│                                                          │
│  提取错误关键词:                                          │
│    for ev in evidence.all():                             │
│      for kw in error_keywords:                          │
│        if kw in ev.detail → error = kw                   │
│                                                          │
│  kb.add_case(                                            │
│    component="flink",                                    │
│    error_pattern="OutOfMemoryError",  ← 精确错误关键词    │
│    version="1.17",                                       │
│    root_cause="TaskManager 2G heap ...",                 │
│    suggestions=["增大 heap", "减少 slots"],              │
│  )                                                       │
│    ↓                                                     │
│    ├─ MD5("flink:OutOfMemoryError:1.17") → _fingerprints │
│    └─ ChromaDB.upsert(embedding) → 持久化                 │
│                                                          │
│  下次有相同的 "flink OOM 1.17" → Phase 2.5 精确命中       │
└──────────────────────────────────────────────────────────┘

LLM 调用: 0
```

**设计原因**: 
- **门控** — 只归档高置信度的诊断。`confidence=low` 的推测性结论入库会污染知识库，导致后续诊断被错误结果误导。
- **双写** — 同时写入内存 dict（Phase 2.5 精确匹配用）和 ChromaDB（Phase 1 语义检索用 + 重启恢复）。

---

## 九、LLM 调用次数汇总

| Phase | 说明 | LLM 调用 |
|-------|------|:--------:|
| 1 | KB 语义检索 | 0 |
| 2 | YAML 策略执行 | 0 |
| 2.5 | MD5 精确匹配 | 0 |
| 3 | SDK ReAct | 3~6 (通常) |
| 4 | 合成 + 验证 | 2 |
| 5 | KB 归档 | 0 |
| **合计 (新增案例)** | | **5~8** |
| **合计 (命中缓存)** | | **0** |

对比纯 ReAct 方案（每个工具调用都经过 LLM，通常 15+ 次），减少了 ~60% 的 LLM 调用和 ~58% 的 token 消耗。

---

## 十、为什么这样设计？四个核心原则

### 1. 确定性优先于智能

```
Phase 1 (语义搜索) → Phase 2 (YAML执行) → Phase 2.5 (MD5匹配)
         ↓                    ↓                  ↓
      0 LLM               0 LLM              0 LLM
```

大部分生产问题是重复的。历史案例 + 确定性证据收集能覆盖 80% 的场景，根本不需要 LLM。

### 2. 结构化优于自由文本

```
旧版: LLM 输出自由文本 → 正则/字符串解析 → 容易失败
新版: tool_choice 强制 JSON → 100% 可靠
```

LLM 的输出不可控——换行位置、大小写、格式都会有变化。`tool_choice` 让模型输出 JSON 而不是自然语言，消除了所有的解析不确定性。

### 3. 分层验证，越往前成本越低

```
Layer 1 (格式规则) → 0ms, 0 LLM
Layer 2 (交叉验证) → 0ms, 0 LLM
Layer 3 (AI 审查)  → ~500ms, 1 LLM
Layer 4 (重试)     → 额外 LLM 调用
```

90% 的问题在 Layer 1-2 就被拦截了（根因太短、没建议、没证据）。不需要把每种情况都送到 LLM 去验证。

### 4. 知识沉淀，越用越聪明

```
Phase 5: 每次成功诊断 → 自动入库
         ↓
Phase 1: 下次类似问题 → 语义检索命中 (0 LLM)
Phase 2.5: 下次相同错误 → MD5 精确命中 (0 LLM)
                      ↓
              系统积累的诊断越来越多
              LLM 调用越来越少
```

这是一个**闭环**——诊断→归档→复用→无需 LLM。系统运行时间越长，历史案例越多，LLM 调用的比例越低。
