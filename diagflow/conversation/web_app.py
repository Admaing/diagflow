"""
Streamlit web chat interface for DiagFlow — with live ReAct step display.
"""

import os, sys, queue, threading, asyncio

import streamlit as st

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))

from diagflow.conversation.manager import ConversationManager
from diagflow.core.llm import LLMClient
from diagflow.core.engine import DiagnosisEngine
from diagflow.core.agent import Agent
from diagflow.core.validator import ConclusionValidator, make_default_verify_llm
from diagflow.core.memory import EvidencePool
from diagflow.tools.registry import build_tool_registry
from diagflow.simulated.cluster import SimulatedCluster
from diagflow.observability.report import render_report
from diagflow.infra import RealCluster, UCloudServiceDiscovery, NodeInfoClient


def init_session():
    if "manager" not in st.session_state:
        api_key = os.environ.get("DEEPSEEK_API_KEY") or os.environ.get("LLM_API_KEY")
        st.session_state.manager = ConversationManager(api_key=api_key)
        st.session_state.event_queue = queue.Queue()
    if "messages" not in st.session_state:
        st.session_state.messages = []
        has_key = bool(api_key)
        st.session_state.messages.append({
            "role": "assistant",
            "content": (
                "# 🩺 DiagFlow — AI 排障助手\n\n"
                "描述您遇到的大数据平台问题，我会帮您诊断根因。\n\n"
                "**例如**: `c-uhadoop-001 任务报错`\n\n"
                f"{'🟢 **LLM 模式已就绪**' if has_key else '🟡 **Mock 模式**（设置 DEEPSEEK_API_KEY）'}"
            ),
        })


def _run_diagnosis_thread(scenario: str, manager, event_queue, api_key, cluster_id):
    """Run diagnosis in a background thread and push events to queue."""
    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)

    def on_event(msg: str):
        event_queue.put(("step", msg))

    async def _diagnose():
        # Determine mode
        if (os.environ.get("DIAGFLOW_MODE") == "production"
                and cluster_id and cluster_id not in ("未指定", "unknown")):
            return await _prod_diagnose(cluster_id, manager, api_key, event_queue, on_event)
        else:
            return await _demo_diagnose(scenario, manager, api_key, event_queue, on_event)

    try:
        result = loop.run_until_complete(_diagnose())
        event_queue.put(("result", result))
    except Exception as e:
        event_queue.put(("error", str(e)))


async def _demo_diagnose(scenario, manager, api_key, event_queue, on_event):
    cluster = SimulatedCluster(scenario)
    context = dict(cluster.context)
    context["cluster_id"] = context.get("cluster_id", "")
    tool_registry = build_tool_registry(cluster)
    if not api_key:
        event_queue.put(("step", "Demo 模式: 模拟诊断流程"))
        from diagflow.observability.report import render_report
        event_queue.put(("result", f"## 🔍 诊断场景: {scenario}\n\n**根因（预期）**: {cluster.expected_root_cause}\n\n> 设置 DEEPSEEK_API_KEY 可运行真实 LLM 诊断。"))
        return
    return await _run_engine(context, tool_registry, api_key, event_queue, on_event)


async def _prod_diagnose(cluster_id, manager, api_key, event_queue, on_event):
    discovery = UCloudServiceDiscovery.from_env()
    discovery.start()
    node_client = NodeInfoClient.from_discovery(discovery, os.environ.get("REGION", "test03"))
    cluster = RealCluster(cluster_id, node_client=node_client, discovery=discovery)
    await cluster._ensure_node_data()
    tool_registry = build_tool_registry(cluster)
    context = cluster.context
    return await _run_engine(context, tool_registry, api_key, event_queue, on_event)


async def _run_engine(context, tool_registry, api_key, event_queue, on_event):
    from diagflow.conversation.manager import _load_topology
    llm = LLMClient(api_keys=[api_key])
    verify_llm = make_default_verify_llm(llm)
    topology = _load_topology(component=context.get("component", "flink"))
    agent = Agent(llm=llm, tool_registry=tool_registry, max_steps=12,
                  on_step=on_event, topology=topology)
    validator = ConclusionValidator(llm, verify_llm)
    engine = DiagnosisEngine(
        llm=llm, tool_registry=tool_registry, agent=agent, validator=validator,
        strategies_dir=os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(__file__))),
                                    "data", "strategies"),
        on_event=on_event,
    )
    report = await engine.diagnose(
        component=context.get("component", "flink"),
        problem_type=context.get("problem", "unknown"),
        context=context,
    )
    return render_report(report)


def main():
    st.set_page_config(page_title="DiagFlow - AI 排障助手", page_icon="🩺", layout="wide")
    init_session()

    # Sidebar
    with st.sidebar:
        st.title("🩺 DiagFlow")
        st.markdown("AI 驱动的**大数据平台排障助手**")
        api_key = os.environ.get("DEEPSEEK_API_KEY") or os.environ.get("LLM_API_KEY")
        st.markdown(f"**模式**: {'🟢 LLM' if api_key else '🟡 Mock'}")
        st.divider()
        st.markdown("### 快速入口")
        if st.button("🔍 集群故障排查"):
            _send_message("c-uhadoop-001 任务报错")

    # Main area: chat history
    for msg in st.session_state.messages:
        with st.chat_message(msg["role"]):
            st.markdown(msg["content"])
            # If this is a diagnosis result with event log, show expandable steps
            if msg.get("event_steps"):
                with st.expander("🔍 查看诊断过程", expanded=False):
                    for step in msg["event_steps"]:
                        st.text(step)

    # Live progress area
    progress_placeholder = st.empty()

    # Chat input
    if prompt := st.chat_input("描述您的问题，例如: c-uhadoop-001 任务报错"):
        _send_message(prompt)
        st.rerun()


def _send_message(prompt: str):
    manager = st.session_state.manager
    event_queue = st.session_state.event_queue
    api_key = os.environ.get("DEEPSEEK_API_KEY") or os.environ.get("LLM_API_KEY")

    # Extract cluster_id from prompt
    import re
    match = re.search(r'(?:uhadoop|c-|c_)[\w-]+', prompt)
    cluster_id = match.group(0) if match else None

    # Determine scenario for demo mode
    scenario = None
    if not cluster_id:
        msg_lower = prompt.lower()
        for key in ["flink_oom", "flink_checkpoint_fail", "hdfs_disk_full", "yarn_queue_stuck"]:
            if key in msg_lower:
                scenario = key
                break
        if not scenario:
            scenario = "flink_oom"

    # Start diagnosis in background thread
    thread = threading.Thread(
        target=_run_diagnosis_thread,
        args=(scenario, manager, event_queue, api_key, cluster_id),
        daemon=True,
    )
    thread.start()

    # Show progress while waiting
    steps = []
    result_text = None
    error_text = None

    with st.spinner("🔍 正在诊断..."):
        while thread.is_alive() or not event_queue.empty():
            try:
                msg_type, msg_content = event_queue.get(timeout=0.5)
                if msg_type == "step":
                    steps.append(msg_content)
                elif msg_type == "result":
                    result_text = msg_content
                elif msg_type == "error":
                    error_text = msg_content
            except queue.Empty:
                continue

    # Collect remaining events
    while not event_queue.empty():
        try:
            msg_type, msg_content = event_queue.get_nowait()
            if msg_type == "step":
                steps.append(msg_content)
            elif msg_type == "result":
                result_text = msg_content
        except queue.Empty:
            break

    # Display result
    st.session_state.messages.append({"role": "user", "content": prompt})

    if error_text:
        st.session_state.messages.append({
            "role": "assistant",
            "content": f"❌ 诊断失败: {error_text}",
            "event_steps": steps,
        })
    elif result_text:
        st.session_state.messages.append({
            "role": "assistant",
            "content": result_text,
            "event_steps": steps,
        })
    else:
        st.session_state.messages.append({
            "role": "assistant",
            "content": "⚠️ 诊断未返回结果",
            "event_steps": steps,
        })

    st.rerun()


if __name__ == "__main__":
    main()
