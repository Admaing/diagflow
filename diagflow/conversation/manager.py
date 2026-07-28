"""
Conversation manager — multi-turn diagnostic dialogue.

Handles the full lifecycle of a user-facing diagnostic conversation:
  1. Parse user intent from natural language
  2. Identify missing parameters and ask clarifying questions
  3. Launch diagnostic workflow when ready
  4. Handle follow-up questions about diagnosis results
  5. Maintain conversation state across turns
"""

from __future__ import annotations

import json
import os
import time
from dataclasses import dataclass, field
from typing import Any

from diagflow.core.diag_agent import DiagAgent, DiagnosisReport
from diagflow.core.memory import EvidencePool
from diagflow.core.validator import ConclusionValidator
from diagflow.simulated.cluster import SimulatedCluster
from diagflow.simulated.scenarios import ALL_SCENARIOS
from diagflow.tools.v3tools import build_v3_tools
from diagflow.observability.report import render_report


# Known runnable scenarios for demo
SCENARIO_LOOKUP = {
    "flink_oom": "Flink 任务 OOM 挂掉",
    "flink_checkpoint_fail": "Flink Checkpoint 连续失败",
    "hdfs_disk_full": "HDFS 磁盘写满",
    "yarn_queue_stuck": "YARN 队列拥堵",
}


@dataclass
class Turn:
    """A single conversation turn."""
    role: str  # "user" | "assistant"
    content: str
    timestamp: float = field(default_factory=time.time)


@dataclass
class ConversationState:
    """State maintained across a dialogue session."""
    status: str = "waiting"  # waiting | gathering_info | diagnosing | complete
    component: str | None = None
    problem: str | None = None
    cluster_id: str | None = None
    region: str | None = None
    version: str | None = None
    job_id: str | None = None
    scenario_match: str | None = None
    report: DiagnosisReport | None = None
    turns: list[Turn] = field(default_factory=list)

    REQUIRED_FIELDS = ["component", "problem"]

    def missing_info(self) -> list[str]:
        missing = []
        for f in self.REQUIRED_FIELDS:
            if getattr(self, f) is None:
                missing.append(f)
        return missing

    def is_ready(self) -> bool:
        return len(self.missing_info()) == 0


class ConversationManager:
    """Manages a multi-turn diagnostic conversation."""

    def __init__(self, api_key: str | None = None):
        self.api_key = api_key or os.environ.get("DEEPSEEK_API_KEY") or os.environ.get("LLM_API_KEY") or ""
        self.state = ConversationState()
        self._diagnosis_result: str | None = None
        self._agent: DiagAgent | None = None
        self._kb = None
        self._init_kb()

    def _init_kb(self):
        """Initialize knowledge base with seed cases."""
        try:
            from diagflow.rag.knowledge_base import KnowledgeBase
            from pathlib import Path
            cases_dir = Path(__file__).parent.parent.parent / "data" / "cases"
            self._kb = KnowledgeBase()
            if cases_dir.exists():
                count = self._kb.load_cases_from_dir(str(cases_dir))
                if count:
                    print(f"  📚 KB loaded: {count} cases, {len(self._kb._fingerprints)} fingerprints")
        except Exception:
            self._kb = None

    def handle_message(self, message: str) -> str:
        """Process a user message and return a response.

        This is the main entry point for the conversational interface.
        """
        # Add user turn
        self.state.turns.append(Turn(role="user", content=message))

        # Route based on current state
        if self.state.status == "waiting":
            return self._handle_initial(message)
        elif self.state.status == "gathering_info":
            return self._handle_info_gathering(message)
        elif self.state.status == "diagnosing":
            return "正在诊断中，请稍候..."
        elif self.state.status == "complete":
            return self._handle_followup(message)
        return "抱歉，我不理解。请重新描述您的问题。"

    def _handle_initial(self, message: str) -> str:
        """First message — try to parse intent."""
        msg_lower = message.lower()

        # Check if they want a specific demo scenario
        for key, desc in SCENARIO_LOOKUP.items():
            if key in msg_lower or desc in message:
                self.state.scenario_match = key
                return self._start_diagnosis(key)

        # Extract info using simple keyword matching
        self._extract_info(message)
        self.state.status = "gathering_info"
        return self._ask_next_question()

    def _extract_info(self, message: str) -> None:
        """Simple keyword extraction from user message."""
        msg = message.lower()

        # Component detection
        if any(w in msg for w in ["flink", "实时计算"]):
            self.state.component = "flink"
        elif any(w in msg for w in ["hdfs", "hadoop", "存储"]):
            self.state.component = "hdfs"
        elif any(w in msg for w in ["yarn", "资源调度"]):
            self.state.component = "yarn"

        # Problem detection (values match strategy YAML filenames)
        if any(w in msg for w in ["oom", "挂", "fail", " killed"]):
            self.state.problem = "job_failure"
        elif any(w in msg for w in ["checkpoint", "快照"]):
            self.state.problem = "checkpoint_failure"
        elif any(w in msg for w in ["磁盘", "空间", "disk", "no space"]):
            self.state.problem = "disk_full"
        elif any(w in msg for w in ["队列", "queue", "拥堵", "accepted"]):
            self.state.problem = "queue_stuck"

        # Cluster info
        import re
        match = re.search(r'(?:uhadoop|c-|c_)[\w-]+', message)
        if match:
            self.state.cluster_id = match.group(0)

        # Region
        for r in ["北京", "上海", "广州", "香港"]:
            if r in message:
                self.state.region = r

        # Version
        match = re.search(r'(\d+\.\d+\.\d+)', message)
        if match:
            self.state.version = match.group(0)

        # Job ID
        match = re.search(r'job_[\w]+', message)
        if match:
            self.state.job_id = match.group(0)

    def _ask_next_question(self) -> str:
        """Ask for missing information."""
        if self.state.component is None:
            return "请问是哪个组件出了问题？(Flink / HDFS / YARN)"
        if self.state.problem is None:
            hints = {
                "flink": "常见的 Flink 问题有：任务挂掉(OOM)、Checkpoint 失败",
                "hdfs": "常见的 HDFS 问题有：磁盘空间满、写入超时",
                "yarn": "常见的 YARN 问题有：队列拥堵、容器分配失败",
            }
            return f"请描述一下具体现象。{hints.get(self.state.component, '')}"
        return self._confirm_and_start()

    def _confirm_and_start(self) -> str:
        """Confirm the understanding and start diagnosis."""
        parts = [
            f"好的，我来诊断这个 {self.state.component} 问题。",
            f"集群: {self.state.cluster_id or '未指定'}",
            f"地域: {self.state.region or '未指定'}",
            f"现象: {self.state.problem}",
        ]
        if self.state.job_id:
            parts.append(f"Job ID: {self.state.job_id}")
        if self.state.version:
            parts.append(f"版本: {self.state.version}")
        parts.append("\n开始诊断...\n")
        return "\n".join(parts) + self._start_diagnosis()

    def _handle_info_gathering(self, message: str) -> str:
        """Process additional info from user."""
        self._extract_info(message)
        if self.state.is_ready():
            return self._confirm_and_start()
        return self._ask_next_question()

    def _start_diagnosis(self, scenario: str | None = None) -> str:
        """Launch the diagnostic workflow.

        Auto-detects production vs demo mode:
          - If DIAGFLOW_MODE=production and we have a real cluster_id,
            use RealCluster (MySQL + ZK + umrAgent).
          - Otherwise, fall back to SimulatedCluster with a scenario.
        """
        self.state.status = "diagnosing"

        # Production mode: use real infrastructure
        if (os.environ.get("DIAGFLOW_MODE") == "production"
                and self.state.cluster_id
                and self.state.cluster_id not in ("未指定", "unknown")):
            return self._start_production_diagnosis()

        # Demo mode: use simulated cluster with a scenario
        s = scenario or self.state.scenario_match or "flink_oom"
        cluster = SimulatedCluster(s)

        # Merge state info with scenario context
        context = dict(cluster.context)
        if self.state.cluster_id:
            context["cluster_id"] = self.state.cluster_id
        if self.state.region:
            context["region"] = self.state.region
        if self.state.version:
            context["version"] = self.state.version
        if self.state.job_id:
            context["job_id"] = self.state.job_id

        evidence_pool = EvidencePool()

        if self.api_key:
            return self._run_llm_diagnosis(s, context, cluster, {}, evidence_pool)
        else:
            return self._run_mock_diagnosis(s, context, cluster)

    def _start_production_diagnosis(self) -> str:
        """Launch diagnosis against REAL UHadoop infrastructure.

        Data flow:
          Node info: DiagFlow → NodeInfoClient → uhadoop-manager HTTP API
          Logs:      Strategy → umrAgent → [ipv6]:65431
          LLM:       DeepSeek API (via KUN public internet)
          DeepWiki:  MCP over HTTPS (known-issue verification)
        """
        import os
        from diagflow.infra import RealCluster, UCloudServiceDiscovery, NodeInfoClient

        instance_id = self.state.cluster_id or ""
        lines = [
            "🏭 **生产模式** — 连接真实 UHadoop 基础设施",
            f"   集群: {instance_id}",
            f"   组件: {self.state.component}",
            f"   现象: {self.state.problem}",
            "",
            "数据流: Strategy → umrAgent [ipv6]:65431 → 日志/指标/配置",
            "       Agent → DeepSeek + DeepWiki → 根因 + 已知 bug 验证",
            "",
        ]

        if not self.api_key:
            lines.append("⚠️ 生产模式需要 DEEPSEEK_API_KEY 才能运行 LLM 诊断。")
            self.state.status = "complete"
            return "\n".join(lines)

        import asyncio
        try:
            loop = asyncio.get_event_loop()
        except RuntimeError:
            loop = asyncio.new_event_loop()
            asyncio.set_event_loop(loop)

        async def _build_and_diagnose():
            discovery = UCloudServiceDiscovery.from_env()
            discovery.start()
            # uhadoop-manage服务发现
            node_client = NodeInfoClient.from_discovery(
                discovery, os.environ.get("REGION", "test03")
            )
            cluster = RealCluster(instance_id, node_client=node_client, discovery=discovery)
            await cluster._ensure_node_data()

            from diagflow.tools.v3tools import build_v3_tools
            tools = build_v3_tools(cluster)
            agent = DiagAgent(
                api_key=self.api_key,
                model="deepseek-v4-flash",
                strategies_dir=os.path.join(
                    os.path.dirname(os.path.dirname(os.path.dirname(__file__))),
                    "data", "strategies",
                ),
                knowledge_base=self._kb,
                validator=ConclusionValidator.standalone(self.api_key),
                on_event=lambda m: print(f"  📋 {m}"),
            )
            agent.register_tools(tools)

            return await agent.diagnose(
                component=cluster.context.get("component", "flink"),
                problem_type=cluster.context.get("problem", "unknown"),
                context=cluster.context,
            )

        try:
            report = loop.run_until_complete(_build_and_diagnose())
            self.state.report = report
            result = render_report(report)
            self.state.status = "complete"
            self._diagnosis_result = result
            self.state.turns.append(Turn(role="assistant", content=result))
            return "\n".join(lines) + result
        except Exception as exc:
            self.state.status = "complete"
            err = f"❌ 诊断失败: {type(exc).__name__}: {exc}"
            self.state.turns.append(Turn(role="assistant", content=err))
            return "\n".join(lines) + err

    def _run_mock_diagnosis(self, scenario: str, context: dict, cluster) -> str:
        """Run diagnosis in mock mode."""
        lines = []
        lines.append(f"## 🔍 诊断场景: {SCENARIO_LOOKUP.get(scenario, scenario)}")
        lines.append("")
        lines.append(f"**根因（预期）**: {cluster.expected_root_cause}")
        lines.append("")
        lines.append("**上下文**:")
        for k, v in context.items():
            lines.append(f"  - {k}: {v}")
        lines.append("")
        lines.append("**排查步骤**:")
        lines.append("1. ✅ 指纹匹配 — 检查已知问题")
        lines.append("2. ✅ 日志分析 — 提取 ERROR 和 OOM 模式")
        lines.append("3. ✅ 指标查询 — 检查堆内存、GC 暂停、Checkpoint")
        lines.append("4. ✅ 配置检查 — 审查资源配置")
        lines.append("5. ✅ 综合判断 — 证据交叉验证")
        lines.append("")
        lines.append("> 设置 DEEPSEEK_API_KEY 可运行真实 LLM 诊断。")

        self.state.status = "complete"
        self._diagnosis_result = "\n".join(lines)
        self.state.turns.append(Turn(role="assistant", content=self._diagnosis_result))
        return self._diagnosis_result

    def _run_llm_diagnosis(self, scenario: str, context: dict, cluster,
                           tool_registry, evidence_pool: EvidencePool) -> str:
        """v3: DiagAgent (Anthropic SDK-powered ReAct + Strategy)."""
        import asyncio
        from diagflow.tools.v3tools import build_v3_tools
        from diagflow.core.validator import ConclusionValidator
        import os

        async def _run():
            tools = build_v3_tools(cluster)
            agent = DiagAgent(
                api_key=self.api_key,
                model="deepseek-v4-flash",
                strategies_dir=os.path.join(
                    os.path.dirname(os.path.dirname(os.path.dirname(__file__))),
                    "data", "strategies",
                ),
                knowledge_base=self._kb,
                validator=ConclusionValidator.standalone(self.api_key) if self.api_key else None,
                on_event=lambda m: print(f"  📋 {m}"),
            )
            agent.register_tools(tools)
            return await agent.diagnose(
                component=context.get("component", "flink"),
                problem_type=context.get("problem", "unknown"),
                context=context,
            )

        loop = asyncio.new_event_loop()
        asyncio.set_event_loop(loop)
        report = loop.run_until_complete(_run())
        self.state.report = report
        result = render_report(report)
        self.state.status = "complete"
        self._diagnosis_result = result
        self.state.turns.append(Turn(role="assistant", content=result))
        return result

    def _handle_followup(self, message: str) -> str:
        """Handle follow-up questions after diagnosis."""
        msg = message.lower()

        # Quick pattern matching for common follow-ups
        if any(w in msg for w in ["原因", "root cause", "根因"]):
            if self.state.report:
                return f"根因是: **{self.state.report.root_cause}**"
            return "请先完成一次诊断。"

        if any(w in msg for w in ["建议", "修复", "fix", "解决"]):
            if self.state.report:
                lines = ["修复建议:\n"]
                for s in self.state.report.suggestions:
                    lines.append(f"- {s}")
                return "\n".join(lines)
            return "请先完成一次诊断。"

        if any(w in msg for w in ["证据", "日志", "指标"]):
            if self.state.report:
                lines = ["证据链:\n"]
                for ev in self.state.evidence_summary:
                    lines.append(f"- [{ev.get('source_agent', '?')}] {ev.get('summary', '')}")
                return "\n".join(lines)
            return "请先完成一次诊断。"

        if any(w in msg for w in ["重新", "再来", "另一个"]):
            self.state = ConversationState()
            return "好的，请描述新的问题。"

        # Default: use LLM to answer the follow-up
        if self.api_key and self._diagnosis_result:
            import asyncio
            prompt = f"""Previous diagnosis:
{self._diagnosis_result[:2000]}

User follow-up question: {message}

Answer the question based on the diagnosis context above."""
            import asyncio
            async def _ask():
                from anthropic import Anthropic
                c = Anthropic(api_key=self.api_key, base_url="https://api.modelverse.cn")
                resp = c.messages.create(
                    model="deepseek-v4-flash", max_tokens=512, temperature=0.3,
                    messages=[{"role": "user", "content": prompt}],
                )
                return "\n".join(b.text for b in resp.content if b.type == "text")
            try:
                loop = asyncio.get_event_loop()
            except RuntimeError:
                loop = asyncio.new_event_loop()
                asyncio.set_event_loop(loop)
            return loop.run_until_complete(_ask()) or "抱歉，我无法回答这个问题。"
        return (
            "您可以追问:\n"
            "- 根因是什么？\n"
            "- 修复建议？\n"
            "- 有哪些证据？\n"
            "- 重新开始诊断另一个问题"
        )

    def get_history(self) -> list[dict[str, str]]:
        """Get conversation history for display."""
        return [
            {"role": t.role, "content": t.content}
            for t in self.state.turns
        ]


# ------------------------------------------------------------------
# Helpers
# ------------------------------------------------------------------

def _load_topology(component: str) -> str:
    """Load and format the service deployment topology for one component."""
    import yaml
    from pathlib import Path

    topo_path = Path(__file__).parent.parent.parent / "data" / "service_topology.yaml"
    if not topo_path.exists():
        return ""

    try:
        with open(topo_path) as f:
            data = yaml.safe_load(f) or {}
    except Exception:
        return ""

    comp_data = data.get(component)
    if not comp_data:
        return ""

    lines = [f"## {component.upper()} Deployment Topology"]
    for svc_name, svc_info in comp_data.items():
        role = svc_info.get("node_role", "?")
        paths = svc_info.get("log_paths", [])
        proc = svc_info.get("process_name", "")
        cfg = svc_info.get("config_path", "")
        lines.append(f"\n### {svc_name}")
        lines.append(f"  Node role: {role}")
        if proc:
            lines.append(f"  Process: {proc}")
        if paths:
            lines.append(f"  Logs: {', '.join(paths)}")
        if cfg:
            lines.append(f"  Config: {cfg}")
    return "\n".join(lines)
