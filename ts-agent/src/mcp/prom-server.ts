/**
 * Read-only Prometheus / Alertmanager MCP server, spoken over stdio.
 *
 * Pi spawns this as a child process and prefixes every tool with
 * `mcp__prom__`. stdout is the JSON-RPC channel: log only to stderr.
 */
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import {
  promAlerts,
  promQuery,
  promQueryRange,
  promRules,
  promTargets,
  prometheusConfigFromEnv,
} from "./prom-client.js";

const config = prometheusConfigFromEnv();

if (!config.prometheusUrl && !config.alertmanagerUrl) {
  console.error("prom-mcp: set PROMETHEUS_URL and/or ALERTMANAGER_URL");
  process.exit(1);
}

const server = new McpServer({ name: "diagflow-prom", version: "0.1.0" });

function text(value: unknown): string {
  return JSON.stringify(value, null, 2);
}

function ok(value: unknown) {
  return { content: [{ type: "text" as const, text: text(value) }] };
}

function fail(error: unknown) {
  const message = error instanceof Error ? error.message : String(error);
  return { isError: true, content: [{ type: "text" as const, text: message }] };
}

server.registerTool(
  "prom_query",
  {
    title: "Prometheus instant query",
    description:
      "Run a PromQL instant query against Prometheus. Read-only. Returns the raw Prometheus JSON.",
    inputSchema: {
      query: z.string().describe("PromQL expression, e.g. sum(rate(container_cpu_usage_seconds_total[5m]))"),
      time: z.string().optional().describe("Evaluation timestamp (RFC3339 or Unix seconds). Default: now"),
    },
    annotations: { readOnlyHint: true },
  },
  async ({ query, time }) => {
    try {
      return ok(await promQuery(config, query, time));
    } catch (error) {
      return fail(error);
    }
  },
);

server.registerTool(
  "prom_query_range",
  {
    title: "Prometheus range query",
    description:
      "Run a PromQL range query against Prometheus. Read-only. Use for trend and spike analysis.",
    inputSchema: {
      query: z.string().describe("PromQL expression"),
      start: z.string().describe("Start timestamp (RFC3339 or Unix seconds)"),
      end: z.string().describe("End timestamp (RFC3339 or Unix seconds)"),
      step: z.string().describe("Resolution step, e.g. '30s' or '1m'"),
    },
    annotations: { readOnlyHint: true },
  },
  async ({ query, start, end, step }) => {
    try {
      return ok(await promQueryRange(config, query, start, end, step));
    } catch (error) {
      return fail(error);
    }
  },
);

server.registerTool(
  "prom_alerts",
  {
    title: "Active Alertmanager alerts",
    description:
      "List active, non-silenced, non-inhibited alerts from Alertmanager. Read-only.",
    inputSchema: {},
    annotations: { readOnlyHint: true },
  },
  async () => {
    try {
      return ok(await promAlerts(config));
    } catch (error) {
      return fail(error);
    }
  },
);

server.registerTool(
  "prom_rules",
  {
    title: "Prometheus alerting rules",
    description: "List Prometheus alerting rules and their current state. Read-only.",
    inputSchema: {},
    annotations: { readOnlyHint: true },
  },
  async () => {
    try {
      return ok(await promRules(config));
    } catch (error) {
      return fail(error);
    }
  },
);

server.registerTool(
  "prom_targets",
  {
    title: "Prometheus scrape targets",
    description: "List active Prometheus scrape targets and their health. Read-only.",
    inputSchema: {},
    annotations: { readOnlyHint: true },
  },
  async () => {
    try {
      return ok(await promTargets(config));
    } catch (error) {
      return fail(error);
    }
  },
);

await server.connect(new StdioServerTransport());
console.error("prom-mcp: ready");
