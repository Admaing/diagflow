#!/usr/bin/env node
import http from "node:http";
import { hasLlmCredential, ensureRuntimeDirs, loadConfig, validateConfig, type AgentConfig } from "./config.js";
import {
  ConfigError,
  LlmUnavailableError,
  NoConclusionError,
  StageTimeoutError,
} from "./errors.js";
import type { AgentEvent, EventSink } from "./events.js";
import type { InvestigationInput } from "./report/types.js";
import { createPiRuntime } from "./runtime/pi.js";
import type { AgentRuntime } from "./runtime/types.js";
import { errorMessage } from "./util.js";
import { runInvestigation } from "./workflow/investigate.js";

const MAX_BODY_BYTES = 1 << 20;
const HEARTBEAT_MS = 15_000;

interface InvestigateRequest {
  component?: string;
  problem?: string;
  cluster_id?: string;
  detail?: string;
  job_id?: string;
  alert?: Record<string, unknown>;
  time_range?: { start: string; end: string };
  context?: Record<string, unknown>;
}

class Semaphore {
  private active = 0;
  private readonly queue: Array<() => void> = [];

  constructor(private readonly max: number) {}

  async acquire(): Promise<void> {
    if (this.active < this.max) {
      this.active++;
      return;
    }
    await new Promise<void>((resolve) => this.queue.push(resolve));
    this.active++;
  }

  release(): void {
    this.active--;
    const next = this.queue.shift();
    if (next) next();
  }
}

function sendJson(response: http.ServerResponse, status: number, value: unknown): void {
  const body = JSON.stringify(value);
  response.writeHead(status, {
    "Content-Type": "application/json; charset=utf-8",
    "Content-Length": Buffer.byteLength(body),
  });
  response.end(body);
}

function errorStatus(error: unknown): { status: number; code: string; message: string } {
  if (error instanceof SyntaxError) {
    return { status: 400, code: "invalid_json", message: error.message };
  }
  if (error instanceof NoConclusionError) {
    return { status: 422, code: error.code, message: error.message };
  }
  if (error instanceof StageTimeoutError) {
    return { status: 504, code: error.code, message: error.message };
  }
  if (error instanceof LlmUnavailableError) {
    return { status: 502, code: error.code, message: error.message };
  }
  if (error instanceof ConfigError) {
    return { status: 500, code: error.code, message: error.message };
  }
  return { status: 500, code: "investigation_failed", message: errorMessage(error) };
}

async function readBody(request: http.IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of request) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    size += buffer.length;
    if (size > MAX_BODY_BYTES) {
      throw Object.assign(new Error("request body too large"), { statusCode: 413 });
    }
    chunks.push(buffer);
  }
  const raw = Buffer.concat(chunks).toString("utf8").trim();
  if (!raw) return {};
  return JSON.parse(raw) as unknown;
}

function toInput(body: InvestigateRequest): InvestigationInput {
  const component = body.component?.trim();
  const problem = body.problem?.trim();
  if (!component || !problem) {
    throw Object.assign(new Error("component and problem are required"), { statusCode: 400 });
  }
  return {
    component,
    problem,
    ...(body.cluster_id?.trim() ? { clusterId: body.cluster_id.trim() } : {}),
    ...(body.detail?.trim() ? { detail: body.detail.trim() } : {}),
    ...(body.job_id?.trim() ? { jobId: body.job_id.trim() } : {}),
    ...(body.alert ? { alert: body.alert } : {}),
    ...(body.time_range ? { timeRange: body.time_range } : {}),
    ...(body.context ? { context: body.context } : {}),
  };
}

function authorized(request: http.IncomingMessage, config: AgentConfig): boolean {
  if (!config.server.authToken) return true;
  const header = request.headers.authorization ?? "";
  return header === `Bearer ${config.server.authToken}`;
}

function writeSse(response: http.ServerResponse, event: AgentEvent): void {
  response.write(`event: ${event.type}\n`);
  response.write(`data: ${JSON.stringify(event)}\n\n`);
}

async function createRuntime(config: AgentConfig, sink: EventSink): Promise<AgentRuntime> {
  return createPiRuntime(config, sink);
}

function main(): void {
  const config = loadConfig();
  const problems = validateConfig(config);
  if (problems.length > 0) {
    for (const problem of problems) console.error(`config: ${problem}`);
    process.exit(1);
  }
  ensureRuntimeDirs(config);

  const semaphore = new Semaphore(config.server.maxConcurrent);
  const log = (message: string) => console.error(`[diagflow-ts] ${message}`);

  const server = http.createServer(async (request, response) => {
    try {
      const url = new URL(request.url ?? "/", "http://localhost");

      if (request.method === "GET" && url.pathname === "/healthz") {
        sendJson(response, 200, { status: "ok" });
        return;
      }

      if (request.method === "GET" && url.pathname === "/readyz") {
        const dataSource =
          Boolean(config.diagflow.mcpUrl) ||
          Boolean(config.prometheus.url) ||
          Boolean(config.prometheus.alertmanagerUrl);
        const llm = hasLlmCredential(config);
        const body = {
          status: dataSource ? (llm ? "ready" : "degraded") : "not ready",
          checks: {
            llm: llm ? "ok" : "degraded",
            diagflow_mcp: config.diagflow.mcpUrl ? "configured" : "disabled",
            prometheus: config.prometheus.url ? "configured" : "disabled",
            alertmanager: config.prometheus.alertmanagerUrl ? "configured" : "disabled",
            api_auth: config.server.authToken ? "ok" : "degraded",
          },
          model: `${config.llm.provider}/${config.llm.modelId}`,
        };
        sendJson(response, dataSource ? 200 : 503, body);
        return;
      }

      const isDiagnose = request.method === "POST" && url.pathname === "/api/v1/investigate";
      const isStream =
        request.method === "POST" && url.pathname === "/api/v1/investigate/stream";
      if (!isDiagnose && !isStream) {
        sendJson(response, 404, { error: { code: "not_found", message: "unknown route" } });
        return;
      }

      if (!authorized(request, config)) {
        sendJson(response, 401, {
          error: { code: "unauthorized", message: "missing or invalid bearer token" },
        });
        return;
      }

      await semaphore.acquire();
      try {
        if (isStream) {
          await handleStream(request, response, config, log);
        } else {
          await handleJson(request, response, config, log);
        }
      } finally {
        semaphore.release();
      }
    } catch (error) {
      const mapped = errorStatus(error);
      const explicitStatus = (error as { statusCode?: number }).statusCode;
      const statusCode = explicitStatus ?? mapped.status;
      const code =
        (error as { code?: string }).code ??
        (error instanceof SyntaxError ? "invalid_json" : mapped.code);
      sendJson(response, statusCode, {
        error: {
          code,
          message: errorMessage(error),
        },
      });
    }
  });

  server.requestTimeout = config.server.requestTimeoutMs;
  server.headersTimeout = 20_000;
  server.listen(config.server.port, () => {
    log(`listening on :${config.server.port}`);
    log(`model=${config.llm.provider}/${config.llm.modelId}`);
    log(
      `sources: diagflow=${config.diagflow.mcpUrl ?? "disabled"} ` +
        `prom=${config.prometheus.url ?? "disabled"} ` +
        `alertmanager=${config.prometheus.alertmanagerUrl ?? "disabled"}`,
    );
  });

  const shutdown = (signal: string) => {
    log(`${signal} received, draining`);
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(1), 10_000).unref();
  };
  process.on("SIGINT", () => shutdown("SIGINT"));
  process.on("SIGTERM", () => shutdown("SIGTERM"));
}

async function handleJson(
  request: http.IncomingMessage,
  response: http.ServerResponse,
  config: AgentConfig,
  log: (message: string) => void,
): Promise<void> {
  const body = (await readBody(request)) as InvestigateRequest;
  const input = toInput(body);
  const runtime = await createRuntime(config, () => undefined);
  try {
    const { report, markdown } = await runInvestigation(runtime, input, () => undefined);
    log(
      `done event=${report.event_id} component=${report.component} ` +
        `status=${report.status} confidence=${report.confidence} duration_ms=${report.duration_ms}`,
    );
    sendJson(response, 200, { report, markdown });
  } finally {
    await runtime.close();
  }
}

async function handleStream(
  request: http.IncomingMessage,
  response: http.ServerResponse,
  config: AgentConfig,
  log: (message: string) => void,
): Promise<void> {
  const body = (await readBody(request)) as InvestigateRequest;
  const input = toInput(body);

  response.writeHead(200, {
    "Content-Type": "text/event-stream; charset=utf-8",
    "Cache-Control": "no-cache, no-transform",
    Connection: "keep-alive",
    "X-Accel-Buffering": "no",
  });
  response.flushHeaders();

  const heartbeat = setInterval(() => {
    if (!response.writableEnded) response.write(": heartbeat\n\n");
  }, HEARTBEAT_MS);

  let sink: EventSink = () => undefined;
  let runtime: AgentRuntime | undefined;
  try {
    runtime = await createRuntime(config, (event) => sink(event));
    sink = (event) => {
      if (!response.writableEnded) writeSse(response, event);
    };
    await runInvestigation(runtime, input, sink);
  } catch (error) {
    const status = errorStatus(error);
    log(`failed event code=${status.code} message=${status.message}`);
    writeSse(response, { type: "failed", code: status.code, message: status.message });
    writeSse(response, { type: "done", duration_ms: 0 });
  } finally {
    clearInterval(heartbeat);
    await runtime?.close();
    if (!response.writableEnded) response.end();
  }
}

main();
