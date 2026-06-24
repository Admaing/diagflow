#!/usr/bin/env python3
"""
CLI chat interface for DiagFlow — interactive diagnostic dialogue.

Usage:
  # Mock mode (no API key needed)
  python -m diagflow.conversation.cli_chat

  # LLM mode
  DEEPSEEK_API_KEY=sk-... python -m diagflow.conversation.cli_chat
"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))
sys.path.insert(0, os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))), "diagflow"))

from diagflow.conversation.manager import ConversationManager


def main():
    api_key = os.environ.get("DEEPSEEK_API_KEY") or os.environ.get("LLM_API_KEY")
    mode = "🟢 LLM" if api_key else "🟡 Mock"
    print("=" * 60)
    print("  DiagFlow — AI 排障助手")
    print(f"  模式: {mode}")
    print("=" * 60)
    print()
    print("描述您的大数据平台问题，我会帮您诊断根因。")
    print()
    print("例如:")
    print("  > Flink 任务挂了，c-uhadoop-001，北京二")
    print("  > HDFS 报 No space left on device")
    print("  > YARN 队列拥堵")
    print("  > flink_oom (直接跑演示场景)")
    print()
    if not api_key:
        print("💡 设置 DEEPSEEK_API_KEY 启用真实 LLM 诊断")
        print()
    print("输入 /quit 退出，/new 重新开始")
    print()

    manager = ConversationManager(api_key=api_key)

    while True:
        try:
            user_input = input(">>> ")
        except (EOFError, KeyboardInterrupt):
            print("\n再见！")
            break

        if not user_input.strip():
            continue
        if user_input.strip() == "/quit":
            print("再见！")
            break
        if user_input.strip() == "/new":
            manager = ConversationManager(api_key=api_key)
            print("\n已重置对话。描述新的问题：\n")
            continue

        print()
        response = manager.handle_message(user_input)
        print(response)
        print()


if __name__ == "__main__":
    main()
