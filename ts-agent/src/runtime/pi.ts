import {
  createAgentSession,
  createMcpExtension,
  defineTool,
  DefaultResourceLoader,
  ModelRuntime,
  SessionManager,
  SettingsManager,
  type AgentSession,
  type AgentSessionEvent,
  type LoadedMcpConfig,
  type McpServerEntry,
  type ToolDefinition,
} from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import fs from "node:fs";
import path from "node:path";
import { ensureRuntimeDirs, PACKAGE_ROOT, type AgentConfig } from "../config.js";
import { ConfigError, LlmUnavailableError, NoConclusionError, StageTimeoutError } from "../errors.js";
import { EventEmitter, type EventSink } from "../events.js";
import type {
  Intent,
  IntentResult,
  InvestigationInput,
  ReportDraft,
  ToolAction,
} from "../report/types.js";
import { errorMessage, extractJsonObject, truncate } from "../util.js";
import {
  buildIntentExecutionPrompt,
  buildIntentPrompt,
  buildSynthesisPrompt,
  systemPrompt,
} from "../workflow/prompts.js";
import type { AgentRuntime, ReactResult, RuntimeStats } from "./types.js";
import type { DecideChoice } from "../strategy/types.js";

const INTERNAL_TOOLS = new Set([
  "propose_intents",
  "report_intent",
  "report_diagnosis",
  "branch_decision",
]);

const ALLOWED_TOOLS = [
  "propose_intents",
  "report_intent",
  "report_diagnosis",
  "branch_decision",
  "mcp__diagflow__*",
  "mcp__prom__*",
];

interface Captured {
  intents?: Intent[];
  intent?: { judgment: string; suspect_component_bug?: boolean };
  report?: ReportDraft;
  decision?: string;
}

interface ToolContent {
  type?: string;
  text?: string;
}

function extractToolResult(result: unknown): string {
  if (typeof result === "string") return result;
  if (result && typeof result === "object") {
    const content = (result as { content?: ToolContent[] }).content;
    if (Array.isArray(content)) {
      return content
        .filter((item) => item.type === "text" && typeof item.text === "string")
        .map((item) => item.text ?? "")
        .join("\n");
    }
  }
  try {
    return JSON.stringify(result);
  } catch {
    return String(result);
  }
}

function extractMessageText(message: unknown): string {
  if (!message || typeof message !== "object") return "";
  const content = (message as { content?: unknown }).content;
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content
      .map((item) => {
        if (!item || typeof item !== "object") return "";
        const block = item as { type?: string; text?: string };
        return block.type === "text" && typeof block.text === "string" ? block.text : "";
      })
      .filter(Boolean)
      .join("\n");
  }
  return "";
}

/**
 * Pi records a provider failure as an assistant message with
 * `stopReason: "error"` and resolves the prompt. Left unhandled, that looks
 * like "the model found nothing" and would silently degrade a run. Surface it
 * as LLM-unavailable so the API returns 502 instead of a weak report.
 */
export function llmErrorFromMessages(messages: readonly unknown[]): string | undefined {
  for (let index = messages.length - 1; index >= 0; index--) {
    const message = messages[index] as {
      role?: string;
      stopReason?: string;
      errorMessage?: string;
    };
    if (message.role !== "assistant") continue;
    if (message.stopReason === "error" || message.errorMessage) {
      return message.errorMessage ?? "provider returned stopReason=error";
    }
    return undefined;
  }
  return undefined;
}

function parseIntents(text: string | undefined): Intent[] {
  if (!text) return [];
  const parsed = extractJsonObject(text);
  if (!parsed || typeof parsed !== "object") return [];
  const raw = (parsed as { intents?: unknown }).intents;
  if (!Array.isArray(raw)) return [];
  const out: Intent[] = [];
  for (const item of raw) {
    if (!item || typeof item !== "object") continue;
    const intent = (item as { intent?: unknown }).intent;
    if (typeof intent !== "string" || !intent.trim()) continue;
    const reason = (item as { reason?: unknown }).reason;
    out.push({
      intent: intent.trim(),
      ...(typeof reason === "string" && reason.trim() ? { reason: reason.trim() } : {}),
    });
  }
  return out;
}

function parseReport(text: string | undefined): ReportDraft | undefined {
  if (!text) return undefined;
  const parsed = extractJsonObject(text);
  if (!parsed || typeof parsed !== "object") return undefined;
  const value = parsed as Record<string, unknown>;
  const rootCause = value.root_cause;
  const confidence = value.confidence;
  const suggestions = value.suggestions;
  if (typeof rootCause !== "string" || !rootCause.trim()) return undefined;
  if (!Array.isArray(suggestions)) return undefined;
  const cleaned = suggestions.filter((item): item is string => typeof item === "string" && item.trim().length > 0);
  if (cleaned.length === 0) return undefined;
  const level = confidence === "high" || confidence === "medium" || confidence === "low" ? confidence : "low";
  return { root_cause: rootCause.trim(), confidence: level, suggestions: cleaned };
}

function suspectBug(judgment: string): boolean {
  const lower = judgment.toLowerCase();
  return ["bug", "已知问题", "缺陷", "regression", "upstream", "修复版本", "patch"].some((keyword) =>
    lower.includes(keyword),
  );
}

function defaultIntent(input: InvestigationInput): Intent {
  return {
    intent: `定位 ${input.component} 的失败任务，收集错误日志与相关监控指标`,
  };
}

function buildMcpConfig(config: AgentConfig): LoadedMcpConfig {
  const servers: McpServerEntry[] = [];

  if (config.diagflow.mcpUrl) {
    servers.push({
      name: "diagflow",
      source: "ts-agent",
      config: {
        type: "http",
        url: config.diagflow.mcpUrl,
        ...(config.diagflow.token
          ? { headers: { Authorization: `Bearer ${config.diagflow.token}` } }
          : {}),
        exposure: "direct",
        ...(config.diagflow.allowSshExec
          ? {}
          : { toolExposure: { ssh_exec: "hidden" as const } }),
        timeout: config.limits.toolTimeoutS,
        description: "UHadoop cluster metadata, node logs, YARN, and resource snapshots (read-only)",
      },
    });
  }

  if (config.prometheus.url || config.prometheus.alertmanagerUrl) {
    const useSource = import.meta.url.endsWith(".ts");
    const promPath = useSource ? config.paths.promServerTs : config.paths.promServerJs;
    servers.push({
      name: "prom",
      source: "ts-agent",
      config: {
        type: "stdio",
        command: process.execPath,
        args: useSource ? ["--import", "tsx", promPath] : [promPath],
        cwd: PACKAGE_ROOT,
        env: {
          PATH: process.env.PATH ?? "",
          HOME: process.env.HOME ?? "",
          PROMETHEUS_URL: config.prometheus.url ?? "",
          ALERTMANAGER_URL: config.prometheus.alertmanagerUrl ?? "",
          PROMETHEUS_BASIC_AUTH: config.prometheus.basicAuth ?? "",
          PROMETHEUS_BEARER_TOKEN: config.prometheus.bearerToken ?? "",
        },
        exposure: "direct",
        timeout: config.limits.toolTimeoutS,
        description: "Prometheus metrics and Alertmanager alerts (read-only)",
      },
    });
  }

  return { servers, errors: [] };
}

function configuredMcpServerNames(config: AgentConfig): string[] {
  const names: string[] = [];
  if (config.diagflow.mcpUrl) names.push("diagflow");
  if (config.prometheus.url || config.prometheus.alertmanagerUrl) names.push("prom");
  return names;
}

/**
 * Wait until every configured MCP server has registered at least one tool.
 *
 * Returns quietly on timeout: a missing data source degrades the diagnosis but
 * must not stop the agent from starting (the server's /readyz reports it).
 */
async function waitForMcpTools(
  session: AgentSession,
  servers: readonly string[],
  timeoutMs: number,
): Promise<void> {
  if (servers.length === 0) return;
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const active = session.getActiveToolNames();
    const ready = servers.every((server) => active.some((name) => name.startsWith(`mcp__${server}__`)));
    if (ready) return;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
}

export class PiAgentRuntime implements AgentRuntime {
  private currentActions: ToolAction[] = [];
  private currentSnippets: string[] = [];
  private currentAction: ToolAction | undefined;
  private llmCalls = 0;
  private toolCalls = 0;
  private closed = false;

  private constructor(
    private readonly session: AgentSession,
    private readonly config: AgentConfig,
    private readonly emitter: EventEmitter,
    private readonly captured: Captured,
  ) {}

  static async create(config: AgentConfig, sink: EventSink): Promise<PiAgentRuntime> {
    ensureRuntimeDirs(config);
    const emitter = new EventEmitter(sink);

    // A project-local models.json lets the deployment declare a compatible
    // endpoint (modelverse, vLLM, ...) without touching ~/.pi/agent.
    const localModels = path.join(config.paths.agentDir, "models.json");
    const modelsPath = fs.existsSync(localModels) ? localModels : undefined;

    const modelRuntime = await ModelRuntime.create(
      modelsPath ? { modelsPath, authPath: path.join(path.dirname(modelsPath), "auth.json") } : {},
    );
    if (config.llm.apiKey) {
      try {
        await modelRuntime.setRuntimeApiKey(config.llm.provider, config.llm.apiKey);
      } catch (error) {
        emitter.step("warn", `无法注册 LLM 凭据: ${errorMessage(error)}`);
      }
    }

    const model = modelRuntime.getModel(config.llm.provider, config.llm.modelId);
    if (!model) {
      const available = modelRuntime
        .getModels(config.llm.provider)
        .map((item) => item.id)
        .slice(0, 20)
        .join(", ");
      throw new ConfigError(
        `unknown model ${config.llm.provider}/${config.llm.modelId}` +
          (available ? `; available: ${available}` : `; unknown provider ${config.llm.provider}`),
      );
    }

    const settingsManager = SettingsManager.inMemory();
    const resourceLoader = new DefaultResourceLoader({
      cwd: config.paths.workspaceDir,
      agentDir: config.paths.agentDir,
      settingsManager,
      noContextFiles: true,
      noSkills: true,
      noPromptTemplates: true,
      noThemes: true,
      systemPromptOverride: () => systemPrompt(config.language),
      appendSystemPromptOverride: () => [],
      extensionFactories: [
        createMcpExtension({
          loadConfig: () => buildMcpConfig(config),
          startupWaitMs: 15_000,
        }),
      ],
    });
    await resourceLoader.reload();

    const captured: Captured = {};
    const customTools = buildCustomTools(captured);

    const { session } = await createAgentSession({
      cwd: config.paths.workspaceDir,
      agentDir: config.paths.agentDir,
      modelRuntime,
      model,
      thinkingLevel: config.llm.thinkingLevel,
      settingsManager,
      sessionManager: SessionManager.inMemory(config.paths.workspaceDir),
      resourceLoader,
      customTools,
      tools: ALLOWED_TOOLS,
    });

    const runtime = new PiAgentRuntime(session, config, emitter, captured);
    session.subscribe((event) => runtime.handleEvent(event));
    await session.bindExtensions({});

    // MCP servers connect in the background; their tools appear a moment after
    // bindExtensions. Prompting before that declares only the internal tools to
    // the model, which then cannot gather any evidence.
    const expectedServers = configuredMcpServerNames(config);
    if (expectedServers.length > 0) {
      await waitForMcpTools(session, expectedServers, config.diagflow.startupWaitMs);
    }

    const activeTools = session.getActiveToolNames();
    emitter.step(
      "phase",
      `agent ready: ${config.llm.provider}/${config.llm.modelId} ` +
        `tools=[${activeTools.join(", ") || "none"}]`,
    );
    return runtime;
  }

  async proposeIntents(
    input: InvestigationInput,
    context: Record<string, unknown>,
  ): Promise<Intent[]> {
    this.resetTrace();
    this.captured.intents = undefined;
    await this.prompt(buildIntentPrompt(input, context, this.config.language), "生成排查意图");

    const intents = this.captured.intents as Intent[] | undefined;
    if (intents && intents.length > 0) return intents;
    const parsed = parseIntents(this.session.getLastAssistantText());
    if (parsed.length) return parsed;

    this.emitter.step("warn", "模型未调用 propose_intents，使用默认排查意图");
    return [defaultIntent(input)];
  }

  async runIntent(
    input: InvestigationInput,
    intent: Intent,
    context: Record<string, unknown>,
  ): Promise<IntentResult> {
    this.resetTrace();
    this.captured.intent = undefined;
    await this.prompt(
      buildIntentExecutionPrompt(input, intent, context, this.config.language),
      `执行意图: ${intent.intent}`,
    );

    const capturedIntent = this.captured.intent as
      | { judgment: string; suspect_component_bug?: boolean }
      | undefined;
    const judgment =
      capturedIntent?.judgment?.trim() ||
      this.session.getLastAssistantText()?.trim() ||
      "";
    if (!judgment) {
      throw new NoConclusionError(`intent "${intent.intent}" produced no judgment`);
    }

    return {
      intent: intent.intent,
      ...(intent.reason ? { reason: intent.reason } : {}),
      judgment,
      suspect_component_bug:
        capturedIntent?.suspect_component_bug ?? suspectBug(judgment),
      actions: [...this.currentActions],
      snippets: [...this.currentSnippets],
    };
  }

  async synthesize(
    input: InvestigationInput,
    trace: IntentResult[],
    context: Record<string, unknown>,
    feedback?: string,
  ): Promise<ReportDraft> {
    this.resetTrace();
    this.captured.report = undefined;
    await this.prompt(
      buildSynthesisPrompt(input, trace, context, this.config.language, feedback),
      "合成诊断报告",
    );

    const capturedReport = this.captured.report as ReportDraft | undefined;
    const draft = capturedReport ?? parseReport(this.session.getLastAssistantText());
    if (!draft || !draft.root_cause.trim() || draft.suggestions.length === 0) {
      throw new NoConclusionError("synthesis returned no verifiable conclusion");
    }
    return draft;
  }

  async decide(prompt: string, choices: DecideChoice[]): Promise<string> {
    this.resetTrace();
    this.captured.decision = undefined;
    const choiceText = choices
      .map((choice) => `- ${choice.value}: ${choice.description ?? ""}`)
      .join("\n");
    await this.prompt(
      `${prompt}\n\n请调用 branch_decision 工具，从以下值中选择一个：\n${choiceText}`,
      "策略分支决策",
    );

    const decision =
      (this.captured.decision as string | undefined) ||
      this.session.getLastAssistantText()?.trim() ||
      "";
    if (!choices.some((choice) => choice.value === decision)) {
      this.emitter.step("warn", `无效的分支决策: ${truncate(decision, 80) || "(empty)"}`);
      return "";
    }
    return decision;
  }

  async react(
    task: string,
    _context: Record<string, unknown>,
    evidenceSummary: string,
  ): Promise<ReactResult> {
    this.resetTrace();
    await this.prompt(
      `${task}\n\n已收集的证据:\n${evidenceSummary || "(none)"}\n\n` +
        "只在确实需要时调用工具；完成后给出你的分析和根因假设。",
      "ReAct 探索",
    );
    return {
      text: this.session.getLastAssistantText()?.trim() ?? "",
      actions: [...this.currentActions],
      snippets: [...this.currentSnippets],
    };
  }

  getStats(): RuntimeStats {
    return { llmCalls: this.llmCalls, toolCalls: this.toolCalls };
  }

  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    this.session.dispose();
  }

  private resetTrace(): void {
    this.currentActions = [];
    this.currentSnippets = [];
    this.currentAction = undefined;
  }

  private async prompt(text: string, stage: string): Promise<void> {
    this.emitter.step("phase", stage);
    this.llmCalls++;
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      void this.session.abort();
    }, this.config.server.stageTimeoutMs);

    try {
      await this.session.prompt(text);
    } catch (error) {
      if (timedOut) {
        throw new StageTimeoutError(`stage "${stage}" exceeded ${this.config.server.stageTimeoutMs}ms`);
      }
      throw new LlmUnavailableError(`stage "${stage}" failed: ${errorMessage(error)}`, { cause: error });
    } finally {
      clearTimeout(timer);
    }

    if (timedOut) {
      throw new StageTimeoutError(`stage "${stage}" exceeded ${this.config.server.stageTimeoutMs}ms`);
    }

    const llmError = llmErrorFromMessages(this.session.messages);
    if (llmError) {
      throw new LlmUnavailableError(`stage "${stage}" failed: ${llmError}`);
    }

    if (this.config.debug) {
      const recent = this.session.messages.slice(-3);
      for (const message of recent) {
        this.emitter.step(
          "log",
          `msg ${(message as { role?: string }).role ?? "?"}: ${truncate(JSON.stringify(message), 1_200)}`,
        );
      }
    }
  }

  private handleEvent(event: AgentSessionEvent): void {
    if (event.type === "tool_execution_start") {
      if (INTERNAL_TOOLS.has(event.toolName)) {
        this.emitter.step("log", `internal tool: ${event.toolName}`, event.toolName);
        return;
      }
      const params = (event.args ?? {}) as Record<string, unknown>;
      this.currentAction = { tool: event.toolName, params };
      this.currentActions.push(this.currentAction);
      this.toolCalls++;
      this.emitter.step(
        "tool_call",
        `${event.toolName}(${truncate(JSON.stringify(params), 300)})`,
        event.toolName,
      );
      return;
    }

    if (event.type === "tool_execution_end") {
      if (INTERNAL_TOOLS.has(event.toolName)) {
        this.emitter.step("log", `internal tool done: ${event.toolName}`, event.toolName);
        return;
      }
      const text = extractToolResult(event.result);
      if (this.currentAction?.tool === event.toolName) {
        this.currentAction.result = truncate(text, 1_000);
      }
      this.currentSnippets.push(truncate(text, this.config.limits.maxToolResultChars));
      this.emitter.step(
        event.isError ? "tool_error" : "tool_result",
        `${event.toolName}: ${truncate(text, 300)}`,
        event.toolName,
      );
      return;
    }

    if (event.type === "auto_retry_start") {
      this.emitter.step(
        "warn",
        `LLM 重试 ${event.attempt}/${event.maxAttempts}: ${event.errorMessage}`,
      );
      return;
    }

    if (event.type === "auto_retry_end" && !event.success) {
      this.emitter.step("warn", `LLM 重试失败: ${event.finalError ?? "unknown error"}`);
      return;
    }

    if (this.config.debug && event.type === "message_end") {
      const text = extractMessageText(event.message);
      const role = (event.message as { role?: string }).role ?? "?";
      if (text.trim()) this.emitter.step("log", `${role}: ${truncate(text, 500)}`);
    }
  }
}

function buildCustomTools(captured: Captured): ToolDefinition[] {
  const proposeIntents = defineTool({
    name: "propose_intents",
    label: "Propose investigation intents",
    description: "Submit 2-5 ordered investigation intents for the current problem.",
    parameters: Type.Object({
      intents: Type.Array(
        Type.Object({
          intent: Type.String({ description: "A self-contained investigation direction" }),
          reason: Type.Optional(
            Type.String({ description: "Why this intent may explain the problem" }),
          ),
        }),
        { minItems: 1, maxItems: 5 },
      ),
    }),
    execute: async (_toolCallId, params) => {
      captured.intents = params.intents.map((item) => ({
        intent: item.intent.trim(),
        ...(item.reason?.trim() ? { reason: item.reason.trim() } : {}),
      }));
      return {
        content: [
          {
            type: "text" as const,
            text: `已记录 ${params.intents.length} 个排查意图。`,
          },
        ],
        details: {},
      };
    },
  });

  const reportIntent = defineTool({
    name: "report_intent",
    label: "Report intent judgment",
    description: "Record the judgment for the current investigation intent.",
    parameters: Type.Object({
      judgment: Type.String({ description: "One-sentence judgment backed by evidence" }),
      suspect_component_bug: Type.Optional(
        Type.Boolean({ description: "Whether the evidence suggests an upstream component bug" }),
      ),
    }),
    execute: async (_toolCallId, params) => {
      captured.intent = {
        judgment: params.judgment.trim(),
        suspect_component_bug: params.suspect_component_bug ?? false,
      };
      return {
        content: [{ type: "text" as const, text: "已记录本意图的判断。" }],
        details: {},
      };
    },
  });

  const reportDiagnosis = defineTool({
    name: "report_diagnosis",
    label: "Report final diagnosis",
    description: "Submit the final root cause, confidence, and actionable suggestions.",
    parameters: Type.Object({
      root_cause: Type.String({ description: "Evidence-backed root cause" }),
      confidence: Type.Union([
        Type.Literal("high"),
        Type.Literal("medium"),
        Type.Literal("low"),
      ]),
      suggestions: Type.Array(
        Type.String({ description: "A concrete, actionable remediation step" }),
        { minItems: 1, maxItems: 5 },
      ),
    }),
    execute: async (_toolCallId, params) => {
      captured.report = {
        root_cause: params.root_cause.trim(),
        confidence: params.confidence,
        suggestions: params.suggestions.map((item) => item.trim()).filter(Boolean),
      };
      return {
        content: [{ type: "text" as const, text: "已记录诊断结论。" }],
        details: {},
      };
    },
  });

  const branchDecision = defineTool({
    name: "branch_decision",
    label: "Choose a strategy branch",
    description: "Choose one branch value for the current diagnostic strategy.",
    parameters: Type.Object({
      decision: Type.String({ description: "One of the offered branch values" }),
    }),
    execute: async (_toolCallId, params) => {
      captured.decision = params.decision.trim();
      return {
        content: [{ type: "text" as const, text: `已选择分支: ${params.decision}` }],
        details: {},
      };
    },
  });

  return [proposeIntents, reportIntent, reportDiagnosis, branchDecision];
}

export async function createPiRuntime(config: AgentConfig, sink: EventSink): Promise<AgentRuntime> {
  return PiAgentRuntime.create(config, sink);
}
