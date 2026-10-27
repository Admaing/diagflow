import type { IntentResult, InvestigationInput } from "../report/types.js";
import { safeJson, truncate } from "../util.js";

function languageDirective(language: string): string {
  return language.toLowerCase().startsWith("en")
    ? "Write all user-facing text in English."
    : "所有面向用户的文本使用简体中文。";
}

function contextBlock(input: InvestigationInput): string {
  const parts: string[] = [];
  parts.push(`组件: ${input.component}`);
  parts.push(`问题: ${input.problem}`);
  if (input.clusterId) parts.push(`集群: ${input.clusterId}`);
  if (input.jobId) parts.push(`任务: ${input.jobId}`);
  if (input.detail) parts.push(`现象: ${input.detail}`);
  if (input.alert) parts.push(`告警: ${safeJson(input.alert)}`);
  if (input.timeRange) parts.push(`时间范围: ${input.timeRange.start} ~ ${input.timeRange.end}`);
  if (input.context && Object.keys(input.context).length > 0) {
    parts.push(`附加上下文: ${safeJson(input.context)}`);
  }
  return parts.join("\n");
}

export function systemPrompt(language: string): string {
  return `你是一名大数据平台（Flink/HDFS/YARN/Spark/Hive/Kafka）的资深 SRE。
你的任务是用只读工具排查故障，并给出有证据支持的根因诊断。

工作规则：
1. 证据优先。没有证据时明确说“证据不足”，绝不编造根因。
2. 工具只读。不要尝试写操作、修改配置或执行破坏性命令。
3. 每个排查意图最多调用 4 次工具；优先用最少的数据回答最关键的问题。
4. diagflow 工具用于集群元数据、节点日志、YARN 和资源快照。
5. prom 工具用于 Prometheus 指标和 Alertmanager 告警。
6. 需要提交结构时，必须调用对应的工具，不要在正文里输出 JSON。
7. ${languageDirective(language)}`;
}

export function buildIntentPrompt(
  input: InvestigationInput,
  context: Record<string, unknown>,
  language: string,
): string {
  return `正在排查以下问题：
${contextBlock(input)}

可用数据源：
- diagflow MCP：集群元数据、节点日志、YARN 应用、资源快照（只读）
- prom MCP：Prometheus 指标查询、Alertmanager 告警

请提出 2-5 个相互独立、可验证的排查意图。每个意图必须能通过上述工具验证，
不要提出“重启服务”“修改配置”这类写操作。

只调用 propose_intents 工具提交结果，不要输出其他内容。
${languageDirective(language)}`;
}

export function buildIntentExecutionPrompt(
  input: InvestigationInput,
  intent: { intent: string; reason?: string },
  context: Record<string, unknown>,
  language: string,
): string {
  return `执行下面这个排查意图：
意图: ${intent.intent}
${intent.reason ? `原因: ${intent.reason}` : ""}

问题上下文：
${contextBlock(input)}

规则：
- 最多调用 4 次工具。
- 只做只读查询。
- 证据不足时明确说“证据不足”，不要猜测。
- 完成后调用 report_intent 工具提交：
  - judgment: 一句话判断（${language === "en" ? "English" : "简体中文"}）
  - suspect_component_bug: 是否怀疑是组件自身 Bug
${languageDirective(language)}`;
}

function traceBlock(trace: IntentResult[]): string {
  const lines: string[] = [];
  trace.forEach((step, index) => {
    lines.push(`### 意图 ${index + 1}: ${step.intent}`);
    if (step.reason) lines.push(`原因: ${step.reason}`);
    lines.push(`判断: ${step.judgment}`);
    lines.push(`怀疑组件 Bug: ${step.suspect_component_bug ? "是" : "否"}`);
    const snippets = step.snippets.slice(0, 3);
    for (const snippet of snippets) {
      lines.push(`证据: ${truncate(snippet, 800)}`);
    }
    const tools = step.actions.map((action) => action.tool);
    if (tools.length > 0) lines.push(`使用工具: ${tools.join(", ")}`);
    lines.push("");
  });
  return lines.join("\n");
}

export function buildSynthesisPrompt(
  input: InvestigationInput,
  trace: IntentResult[],
  context: Record<string, unknown>,
  language: string,
): string {
  return `根据以下排查记录，给出最终根因诊断：
${contextBlock(input)}

排查记录：
${traceBlock(trace)}

要求：
- 根因必须由上面的证据支持。
- 如果证据不足，root_cause 写“证据不足，无法给出确定结论”，confidence 用 "low"。
- suggestions 给出 2-4 条可执行建议。
- 只调用 report_diagnosis 工具提交，不要在正文里输出 JSON。
${languageDirective(language)}`;
}
