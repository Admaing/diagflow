import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { PACKAGE_ROOT, type AgentConfig } from "../config.js";
import { errorMessage } from "../util.js";
import type { ToolCallOptions, ToolExecutor, ToolResult } from "../tools/executor.js";

type ServerName = "diagflow" | "prom";

interface ResolvedTool {
  server: ServerName;
  tool: string;
  args: Record<string, unknown>;
}

function str(value: unknown, fallback = ""): string {
  return typeof value === "string" ? value : fallback;
}

function number(value: unknown): number | undefined {
  if (typeof value === "number") return value;
  if (typeof value === "string" && value.trim()) {
    const parsed = Number.parseInt(value, 10);
    if (Number.isFinite(parsed)) return parsed;
  }
  return undefined;
}

function withClusterId(
  args: Record<string, unknown>,
  options?: ToolCallOptions,
): Record<string, unknown> | string {
  if (!options?.clusterId) {
    return "cluster_id is required for diagflow tools";
  }
  return { cluster_id: options.clusterId, ...args };
}

/**
 * Map a YAML strategy tool name to the MCP server that actually exposes it.
 * The strategy format predates the gateway, so names like `call_umr_agent`
 * and `query_metrics` need translating rather than renaming the YAML.
 */
export function resolveStrategyTool(
  tool: string,
  params: Record<string, unknown>,
  options?: ToolCallOptions,
): ResolvedTool | string {
  if (tool.startsWith("prom_")) {
    return { server: "prom", tool, args: params };
  }

  if (tool === "deepwiki_query") {
    return "deepwiki_query is not wired into the TS agent yet";
  }

  if (tool === "ssh_exec") {
    const command = str(params.cmd ?? params.command);
    if (!command) return "ssh_exec: cmd is required";
    const args = withClusterId(
      { command, ...(params.node_name ? { node_ref: params.node_name } : {}) },
      options,
    );
    return typeof args === "string" ? args : { server: "diagflow", tool: "ssh_exec", args };
  }

  if (tool === "call_umr_agent") {
    const action = str(params.action);
    const nodeName = str(params.node_name);
    const nested = params.params && typeof params.params === "object"
      ? (params.params as Record<string, unknown>)
      : {};
    if (!nodeName) return "call_umr_agent: node_name is required";

    if (action === "GetLogs") {
      const args = withClusterId(
        {
          node_ref: nodeName,
          log_path: str(nested.Path),
          ...(nested.Keywords ? { keywords: str(nested.Keywords) } : {}),
          ...(number(nested.MaxLines) ? { max_lines: number(nested.MaxLines) } : {}),
        },
        options,
      );
      return typeof args === "string" ? args : { server: "diagflow", tool: "get_node_log", args };
    }
    if (action === "GetBaseInfo") {
      const args = withClusterId({ node_ref: nodeName }, options);
      return typeof args === "string" ? args : { server: "diagflow", tool: "get_base_info", args };
    }
    if (action === "CheckProcess") {
      const args = withClusterId(
        { node_ref: nodeName, process_name: str(nested.ProcessName) },
        options,
      );
      return typeof args === "string" ? args : { server: "diagflow", tool: "check_process", args };
    }
    if (action === "GetAppList") {
      const args = withClusterId({ action: "list_apps" }, options);
      return typeof args === "string" ? args : { server: "diagflow", tool: "query_yarn", args };
    }
    return `call_umr_agent: unsupported action ${action || "(empty)"}`;
  }

  if (tool === "query_yarn") {
    const args = withClusterId(
      {
        ...(params.action ? { action: str(params.action) } : {}),
        ...(params.app_id ? { app_id: str(params.app_id) } : {}),
        ...(params.app_name ? { states: str(params.app_name) } : {}),
        ...(params.node_name ? { node_ref: str(params.node_name) } : {}),
      },
      options,
    );
    return typeof args === "string" ? args : { server: "diagflow", tool: "query_yarn", args };
  }

  if (tool === "query_metrics") {
    const args = withClusterId(
      { ...(params.node_name ? { node_ref: str(params.node_name) } : {}) },
      options,
    );
    return typeof args === "string" ? args : { server: "diagflow", tool: "get_node_metrics", args };
  }

  // Direct gateway tools: get_cluster_info, list_nodes, get_node_info,
  // get_cluster_services, get_node_log, list_logs, search_logs,
  // get_node_metrics, get_base_info, check_process.
  const args = withClusterId(params, options);
  if (typeof args === "string") return args;
  return { server: "diagflow", tool, args };
}

function extractText(result: { content?: unknown }): string {
  if (!Array.isArray(result.content)) return "";
  return result.content
    .map((item) => {
      if (!item || typeof item !== "object") return "";
      const block = item as { type?: string; text?: string };
      return block.type === "text" && typeof block.text === "string" ? block.text : "";
    })
    .filter(Boolean)
    .join("\n");
}

export class McpToolExecutor implements ToolExecutor {
  private diagflow?: Client;
  private prom?: Client;
  private readonly connectErrors: string[] = [];

  private constructor(private readonly config: AgentConfig) {}

  static async create(config: AgentConfig): Promise<McpToolExecutor> {
    const executor = new McpToolExecutor(config);
    await executor.connect();
    return executor;
  }

  private async connect(): Promise<void> {
    if (this.config.diagflow.mcpUrl) {
      try {
        const transport = new StreamableHTTPClientTransport(
          new URL(this.config.diagflow.mcpUrl),
          this.config.diagflow.token
            ? { requestInit: { headers: { Authorization: `Bearer ${this.config.diagflow.token}` } } }
            : undefined,
        );
        const client = new Client(
          { name: "diagflow-ts-agent", version: "0.1.0" },
          { capabilities: {} },
        );
        await client.connect(transport);
        this.diagflow = client;
      } catch (error) {
        this.connectErrors.push(`diagflow: ${errorMessage(error)}`);
      }
    }

    if (this.config.prometheus.url || this.config.prometheus.alertmanagerUrl) {
      try {
        const useSource = import.meta.url.endsWith(".ts");
        const promPath = useSource ? this.config.paths.promServerTs : this.config.paths.promServerJs;
        const transport = new StdioClientTransport({
          command: process.execPath,
          args: useSource ? ["--import", "tsx", promPath] : [promPath],
          cwd: PACKAGE_ROOT,
          stderr: "inherit",
          env: {
            PATH: process.env.PATH ?? "",
            HOME: process.env.HOME ?? "",
            PROMETHEUS_URL: this.config.prometheus.url ?? "",
            ALERTMANAGER_URL: this.config.prometheus.alertmanagerUrl ?? "",
            PROMETHEUS_BASIC_AUTH: this.config.prometheus.basicAuth ?? "",
            PROMETHEUS_BEARER_TOKEN: this.config.prometheus.bearerToken ?? "",
          },
        });
        const client = new Client(
          { name: "diagflow-ts-agent", version: "0.1.0" },
          { capabilities: {} },
        );
        await client.connect(transport);
        this.prom = client;
      } catch (error) {
        this.connectErrors.push(`prom: ${errorMessage(error)}`);
      }
    }
  }

  async call(
    tool: string,
    params: Record<string, unknown>,
    options?: ToolCallOptions,
  ): Promise<ToolResult> {
    const resolved = resolveStrategyTool(tool, params, options);
    if (typeof resolved === "string") {
      return { success: false, data: "", error: resolved };
    }
    if (resolved.server === "diagflow" && tool === "ssh_exec" && !this.config.diagflow.allowSshExec) {
      return {
        success: false,
        data: "",
        error: "ssh_exec is disabled (set TS_AGENT_ALLOW_SSH_EXEC=1 to enable)",
      };
    }

    const client = resolved.server === "diagflow" ? this.diagflow : this.prom;
    if (!client) {
      const reason = this.connectErrors.length > 0
        ? this.connectErrors.join("; ")
        : `${resolved.server} MCP server is not configured`;
      return { success: false, data: "", error: reason };
    }

    try {
      const result = (await client.callTool({
        name: resolved.tool,
        arguments: resolved.args,
      })) as { content?: unknown; isError?: boolean };
      const text = extractText(result);
      return result.isError
        ? { success: false, data: "", error: text || `${resolved.tool} failed` }
        : { success: true, data: text };
    } catch (error) {
      return { success: false, data: "", error: errorMessage(error) };
    }
  }

  async close(): Promise<void> {
    await this.diagflow?.close().catch(() => undefined);
    await this.prom?.close().catch(() => undefined);
    this.diagflow = undefined;
    this.prom = undefined;
  }
}
