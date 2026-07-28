"""
Streamlit web chat interface for DiagFlow v3 — DiagAgent powered.
"""

import os, sys, queue, threading, asyncio, logging

import streamlit as st

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))

from diagflow.core.diag_agent import DiagAgent
from diagflow.core.validator import ConclusionValidator
from diagflow.simulated.cluster import SimulatedCluster
from diagflow.simulated.scenarios import ALL_SCENARIOS
from diagflow.tools.v3tools import build_v3_tools
from diagflow.observability.report import render_report
from diagflow.infra import RealCluster, UCloudServiceDiscovery, NodeInfoClient

logger = logging.getLogger(__name__)


def init_session():
    api_key = os.environ.get("DEEPSEEK_API_KEY") or os.environ.get("LLM_API_KEY") or ""
    auth_token = os.environ.get("DIAGFLOW_AUTH_TOKEN", "")

    # Auth gate (Phase 3.3)
    if auth_token:
        if "authenticated" not in st.session_state:
            st.session_state.authenticated = False
        if not st.session_state.authenticated:
            with st.sidebar:
                st.title("🩺 DiagFlow v3")
                token_input = st.text_input("Access Token", type="password")
                if st.button("登录"):
                    if token_input == auth_token:
                        st.session_state.authenticated = True
                        st.rerun()
                    else:
                        st.error("Token 错误")
            st.stop()

    if "manager" not in st.session_state:
        st.session_state.api_key = api_key
        st.session_state.event_queue = queue.Queue()
    if "messages" not in st.session_state:
        st.session_state.messages = []
        st.session_state.messages.append({
            "role": "assistant",
            "content": (
                "# 🩺 DiagFlow v3 — AI 排障助手\n\n"
                "描述集群问题，Agent 自动采集证据、验证已知 bug、输出诊断报告。\n\n"
                "**示例**: `uhadoop-1rz1ph4krlks 任务报错`\n\n"
                f"{'🟢 LLM 模式' if api_key else '🟡 Mock 模式'}"
            ),
        })


def _run_thread(scenario, cluster_id, api_key, event_queue):
    def on_event(msg):
        event_queue.put(("step", msg))

    async def _go():
        if api_key and (os.environ.get("DIAGFLOW_MODE") == "production") and cluster_id:
            return await _prod_run(cluster_id, api_key, on_event)
        elif api_key:
            return await _demo_llm_run(scenario, api_key, on_event)
        else:
            c = SimulatedCluster(scenario or "flink_oom")
            return f"## 🔍 Demo: {scenario}\n\n**预期根因**: {c.expected_root_cause}\n\n> 设置 DEEPSEEK_API_KEY 启用 LLM"

    try:
        result = asyncio.run(_go())
        event_queue.put(("result", result))
    except Exception as e:
        event_queue.put(("error", str(e)))


async def _demo_llm_run(scenario, api_key, on_event):
    from diagflow.config import get_config
    cfg = get_config()
    c = SimulatedCluster(scenario)
    tools = build_v3_tools(c)
    agent = DiagAgent(
        api_key=api_key,
        strategies_dir=os.path.join(
            os.path.dirname(os.path.dirname(os.path.dirname(__file__))),
            "data", "strategies",
        ),
        validator=ConclusionValidator.standalone(api_key),
        on_event=on_event,
    )
    agent.register_tools(tools)
    report = await agent.diagnose(
        component=c.context.get("component", "flink"),
        problem_type=c.context.get("problem", "unknown"),
        context=dict(c.context),
    )
    return render_report(report)


async def _prod_run(cluster_id, api_key, on_event):
    discovery = UCloudServiceDiscovery.from_env()
    discovery.start()
    node_client = NodeInfoClient.from_discovery(discovery, os.environ.get("REGION", "test03"))
    cluster = RealCluster(cluster_id, node_client=node_client, discovery=discovery)
    await cluster._ensure_node_data()
    tools = build_v3_tools(cluster)
    agent = DiagAgent(
        api_key=api_key,
        strategies_dir=os.path.join(
            os.path.dirname(os.path.dirname(os.path.dirname(__file__))),
            "data", "strategies",
        ),
        validator=ConclusionValidator.standalone(api_key),
        on_event=on_event,
    )
    agent.register_tools(tools)
    report = await agent.diagnose(
        component=cluster.context.get("component", "flink"),
        problem_type=cluster.context.get("problem", "unknown"),
        context=cluster.context,
    )
    return render_report(report)


def main():
    st.set_page_config(page_title="DiagFlow v3", page_icon="🩺", layout="wide")
    init_session()

    with st.sidebar:
        st.title("🩺 DiagFlow v3")
        st.markdown("AI 排障 Agent")
        api_key = st.session_state.api_key
        st.markdown(f"{'🟢 LLM' if api_key else '🟡 Mock'}")
        st.divider()
        if st.button("🔍 集群故障排查"):
            _send_message("uhadoop-1rz1ph4krlks 任务报错")

    for msg in st.session_state.messages:
        with st.chat_message(msg["role"]):
            st.markdown(msg["content"])
            if msg.get("event_steps"):
                with st.expander("🔍 诊断过程", expanded=False):
                    for s in msg["event_steps"]:
                        st.text(s)

    if prompt := st.chat_input("描述问题，例如: uhadoop-1rz1ph4krlks 任务报错"):
        _send_message(prompt)
        st.rerun()


def _send_message(prompt: str):
    import re
    match = re.search(r'(?:uhadoop|c-|c_)[\w-]+', prompt)
    cluster_id = match.group(0) if match else None

    scenario = None
    if not cluster_id:
        for key in ALL_SCENARIOS:
            if key in prompt.lower():
                scenario = key; break
        if not scenario:
            scenario = "flink_oom"

    api_key = st.session_state.api_key
    event_queue = st.session_state.event_queue

    thread = threading.Thread(
        target=_run_thread,
        args=(scenario, cluster_id, api_key, event_queue),
        daemon=True,
    )
    thread.start()

    steps, result_text, error_text = [], None, None
    with st.spinner("🔍 诊断中..."):
        while thread.is_alive() or not event_queue.empty():
            try:
                t, c = event_queue.get(timeout=0.5)
                if t == "step": steps.append(c)
                elif t == "result": result_text = c
                elif t == "error": error_text = c
            except queue.Empty:
                continue

    while not event_queue.empty():
        try:
            t, c = event_queue.get_nowait()
            if t == "step": steps.append(c)
            elif t == "result": result_text = c
        except queue.Empty:
            break

    st.session_state.messages.append({"role": "user", "content": prompt})
    if error_text:
        st.session_state.messages.append({"role": "assistant", "content": f"❌ {error_text}", "event_steps": steps})
    elif result_text:
        st.session_state.messages.append({"role": "assistant", "content": result_text, "event_steps": steps})
    else:
        st.session_state.messages.append({"role": "assistant", "content": "⚠️ 未返回结果", "event_steps": steps})
    st.rerun()


if __name__ == "__main__":
    main()
