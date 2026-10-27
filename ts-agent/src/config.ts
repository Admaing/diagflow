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

function providerApiKey(provider: string): string | undefined {
  switch (provider) {
    case "deepseek":
      return env("DEEPSEEK_API_KEY");
    case "anthropic":
      return env("ANTHROPIC_API_KEY");
    case "openai":
      return env("OPENAI_API_KEY");
    default:
      return env("TS_AGENT_LLM_API_KEY");
  }
}

export function loadConfig(): AgentConfig {
  const provider = env("PI_MODEL_PROVIDER") ?? "deepseek";
  const modelId = env("PI_MODEL_ID") ?? "deepseek-flash";

  return {
    llm: {
      provider,
      modelId,
      apiKey: env("TS_AGENT_LLM_API_KEY") ?? providerApiKey(provider),
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
  return problems;
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
