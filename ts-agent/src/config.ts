import "dotenv/config";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const moduleDir = path.dirname(fileURLToPath(import.meta.url));

/** ts-agent/ — works from both src/ (tsx) and dist/ (compiled). */
export const PACKAGE_ROOT = path.resolve(moduleDir, "..");

export type ThinkingLevel = "off" | "low" | "medium" | "high";

export interface AgentConfig {
  llm: {
    provider: string;
    modelId: string;
    apiKey?: string;
    /** Where `config.llm.apiKey` came from, for error messages that name an env var. */
    apiKeySource?: string;
    /** Explicit Pi models.json (custom providers). Empty means "use the defaults". */
    modelsPath?: string;
    thinkingLevel: ThinkingLevel;
  };
  diagflow: {
    mcpUrl?: string;
    token?: string;
    allowSshExec: boolean;
    /** How long to wait for MCP servers to register their tools. */
    startupWaitMs: number;
  };
  prometheus: {
    url?: string;
    alertmanagerUrl?: string;
    basicAuth?: string;
    bearerToken?: string;
  };
  server: {
    port: number;
    authToken?: string;
    requestTimeoutMs: number;
    stageTimeoutMs: number;
    maxConcurrent: number;
  };
  limits: {
    /** Per MCP tool call, in seconds (Pi MCP config uses seconds). */
    toolTimeoutS: number;
    maxToolResultChars: number;
  };
  paths: {
    agentDir: string;
    workspaceDir: string;
    promServerTs: string;
    promServerJs: string;
  };
  language: string;
  debug: boolean;
}

function env(name: string): string | undefined {
  const value = process.env[name]?.trim();
  return value ? value : undefined;
}

function intEnv(name: string, fallback: number): number {
  const raw = env(name);
  if (!raw) return fallback;
  const parsed = Number.parseInt(raw, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

function boolEnv(name: string, fallback = false): boolean {
  const raw = env(name)?.toLowerCase();
  if (!raw) return fallback;
  return raw === "1" || raw === "true" || raw === "yes" || raw === "on";
}

function thinkingEnv(fallback: ThinkingLevel): ThinkingLevel {
  const raw = env("PI_THINKING_LEVEL")?.toLowerCase();
  if (raw === "off" || raw === "low" || raw === "medium" || raw === "high") return raw;
  return fallback;
}

function resolvePath(value: string | undefined, fallback: string): string {
  const target = value ?? fallback;
  return path.isAbsolute(target) ? target : path.resolve(PACKAGE_ROOT, target);
}

/** Credential env var a Pi built-in provider reads on its own. */
function providerApiKeyVar(provider: string): string | undefined {
  switch (provider) {
    case "deepseek":
      return "DEEPSEEK_API_KEY";
    case "anthropic":
      return "ANTHROPIC_API_KEY";
    case "openai":
      return "OPENAI_API_KEY";
    default:
      return undefined;
  }
}

/**
 * Resolve the API key and the env var it came from.
 *
 * An explicitly set TS_AGENT_LLM_API_KEY wins for every provider: the
 * deployment wires the gateway key there, and a release build may still see
 * DEEPSEEK_API_KEY set by the environment. Otherwise use the variable the
 * provider reads on its own. Callers use the returned name to say exactly
 * which variable to fix — a modelverse token sitting in DEEPSEEK_API_KEY fails
 * upstream with an opaque 401.
 */
function resolveLlmKey(provider: string): { apiKey?: string; source?: string } {
  const own = providerApiKeyVar(provider);
  const candidates = ["TS_AGENT_LLM_API_KEY", own, "MODELVERSE_API_KEY"].filter(
    (name): name is string => Boolean(name),
  );
  for (const name of candidates) {
    const value = env(name);
    if (value) return { apiKey: value, source: name };
  }
  return {};
}

export function loadConfig(): AgentConfig {
  const provider = env("PI_MODEL_PROVIDER") ?? "deepseek";
  const modelId = env("PI_MODEL_ID") ?? "deepseek-flash";
  const key = resolveLlmKey(provider);

  return {
    llm: {
      provider,
      modelId,
      apiKey: key.apiKey,
      ...(key.source ? { apiKeySource: key.source } : {}),
      ...(env("TS_AGENT_MODELS_JSON") ? { modelsPath: env("TS_AGENT_MODELS_JSON") } : {}),
      thinkingLevel: thinkingEnv("medium"),
    },
    diagflow: {
      mcpUrl: env("DIAGFLOW_MCP_BASE_URL"),
      token: env("DIAGFLOW_MCP_TOKEN"),
      allowSshExec: boolEnv("TS_AGENT_ALLOW_SSH_EXEC", false),
      startupWaitMs: intEnv("TS_AGENT_MCP_STARTUP_WAIT_MS", 15_000),
    },
    prometheus: {
      url: env("PROMETHEUS_URL"),
      alertmanagerUrl: env("ALERTMANAGER_URL"),
      basicAuth: env("PROMETHEUS_BASIC_AUTH"),
      bearerToken: env("PROMETHEUS_BEARER_TOKEN"),
    },
    server: {
      port: intEnv("TS_AGENT_PORT", 8099),
      authToken: env("TS_AGENT_AUTH_TOKEN"),
      requestTimeoutMs: intEnv("TS_AGENT_REQUEST_TIMEOUT_MS", 240_000),
      stageTimeoutMs: intEnv("TS_AGENT_STAGE_TIMEOUT_MS", 120_000),
      maxConcurrent: intEnv("TS_AGENT_MAX_CONCURRENT", 2),
    },
    limits: {
      toolTimeoutS: intEnv("TS_AGENT_TOOL_TIMEOUT_S", 90),
      maxToolResultChars: intEnv("TS_AGENT_MAX_TOOL_RESULT_CHARS", 8_000),
    },
    paths: {
      agentDir: resolvePath(env("TS_AGENT_AGENT_DIR"), ".pi-agent"),
      workspaceDir: resolvePath(env("TS_AGENT_WORKSPACE"), ".agent-workspace"),
      promServerTs: path.join(PACKAGE_ROOT, "src", "mcp", "prom-server.ts"),
      promServerJs: path.join(PACKAGE_ROOT, "dist", "mcp", "prom-server.js"),
    },
    language: env("TS_AGENT_LANGUAGE") ?? "zh-CN",
    debug: boolEnv("TS_AGENT_DEBUG", false),
  };
}

/** Problems that should stop the service from starting. */
export function validateConfig(config: AgentConfig): string[] {
  const problems: string[] = [];
  if (!config.diagflow.mcpUrl && !config.prometheus.url && !config.prometheus.alertmanagerUrl) {
    problems.push(
      "no data source configured: set DIAGFLOW_MCP_BASE_URL and/or PROMETHEUS_URL / ALERTMANAGER_URL",
    );
  }
  if (config.diagflow.mcpUrl && !/^https?:\/\//i.test(config.diagflow.mcpUrl)) {
    problems.push("DIAGFLOW_MCP_BASE_URL must be an http(s) URL");
  }
  if (config.prometheus.url && !/^https?:\/\//i.test(config.prometheus.url)) {
    problems.push("PROMETHEUS_URL must be an http(s) URL");
  }
  if (config.prometheus.alertmanagerUrl && !/^https?:\/\//i.test(config.prometheus.alertmanagerUrl)) {
    problems.push("ALERTMANAGER_URL must be an http(s) URL");
  }
  // A typo'd path would otherwise fall back to the built-in DeepSeek catalog
  // and resurface as a confusing "model not found" or an upstream 401.
  if (config.llm.modelsPath && !fs.existsSync(config.llm.modelsPath)) {
    problems.push(`TS_AGENT_MODELS_JSON does not exist: ${config.llm.modelsPath}`);
  }
  return problems;
}

/**
 * The one model config that catches the common misconfiguration: a custom
 * provider that no models.json declares, or an API key borrowed from a
 * different vendor. Empty when nothing is obviously wrong.
 */
export function llmConfigHint(config: AgentConfig): string | undefined {
  if (config.llm.modelsPath && !fs.existsSync(config.llm.modelsPath)) {
    return `TS_AGENT_MODELS_JSON points at a missing file: ${config.llm.modelsPath}`;
  }
  if (config.llm.apiKeySource === "DEEPSEEK_API_KEY" && config.llm.provider !== "deepseek") {
    return (
      `PI_MODEL_PROVIDER=${config.llm.provider} but the key came from DEEPSEEK_API_KEY; ` +
      "that variable is the DeepSeek platform key, not a gateway key. " +
      "Move it to TS_AGENT_LLM_API_KEY or MODELVERSE_API_KEY."
    );
  }
  return undefined;
}

export function ensureRuntimeDirs(config: AgentConfig): void {
  for (const dir of [config.paths.agentDir, config.paths.workspaceDir]) {
    fs.mkdirSync(dir, { recursive: true });
  }
}

export function hasLlmCredential(config: AgentConfig): boolean {
  if (config.llm.apiKey) return true;
  // Custom providers may keep credentials in ~/.pi/agent/auth.json or models.json.
  const authPath = path.join(config.paths.agentDir, "auth.json");
  const globalAuth = path.join(process.env.HOME ?? "", ".pi", "agent", "auth.json");
  return fs.existsSync(authPath) || (Boolean(process.env.HOME) && fs.existsSync(globalAuth));
}