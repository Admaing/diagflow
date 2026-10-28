#!/usr/bin/env node
import fs from "node:fs";
import http from "node:http";
import { fileURLToPath } from "node:url";
import { hasLlmCredential, ensureRuntimeDirs, llmConfigHint, loadConfig, validateConfig, type AgentConfig } from "./config.js";
import {
  ConfigError,
  LlmUnavailableError,
  NoConclusionError,
  StageTimeoutError,
} from "./errors.js";
import type { AgentEvent, EventSink } from "./events.js";
import { HistoryStore } from "./history/store.js";
import { McpToolExecutor } from "./mcp/tool-client.js";
import { probeTcp } from "./net.js";
import type { InvestigationInput, DiagnosisReport } from "./report/types.js";
import { createPiRuntime } from "./runtime/pi.js";
import { errorMessage, truncate } from "./util.js";
import { runDiagnosis } from "./workflow/diagnose.js";
import { runInvestigation } from "./workflow/investigate.js";

type WorkflowMode = "investigate" | "diagnose";

const MAX_BODY_BYTES = 1 << 20;
const HEARTBEAT_MS = 15_000;

/**
 * The console is a single self-contained HTML file next to the compiled
 * server.js. `npm run build` copies it from src/ to dist/ so the container
 * does not need a separate asset directory.
 */
const WEBUI_HTML = (() => {
  try {
    return fs.readFileSync(fileURLToPath(new URL("./webui.html", import.meta.url)), "utf8");
  } catch {
    return "<!doctype html><meta charset=\"utf-8\"><title>DiagFlow</title>" +
      "<p>webui.html is missing from the image.</p>";
  }
})();

interface InvestigateRequest {
  session_id?: string;
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

function toInput(body: InvestigateRequest, mode: WorkflowMode): InvestigationInput {
  const component = body.component?.trim();
  const problem = body.problem?.trim();
  if (!component || !problem) {
    throw Object.assign(new Error("component and problem are required"), { statusCode: 400 });
  }
  if (mode === "diagnose" && !body.cluster_id?.trim()) {
    throw Object.assign(new Error("cluster_id is required for diagnose mode"), {
      statusCode: 400,
      code: "cluster_id_required",
    });
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

/**
 * Anonymous console identity. The id is minted by the browser and kept in
 * localStorage, so it is a pseudonym, not an authenticated user: it only keeps
 * one browser's sessions together. Swapping in a gateway header or a verified
 * JWT later means changing this function and nothing else.
 */
function callerId(request: http.IncomingMessage): string | undefined {
  const raw = request.headers["x-diagflow-user"];
  const value = (Array.isArray(raw) ? raw[0] : raw)?.trim();
  if (!value) return undefined;
  // Constrain to something safe to store and log; browsers send a UUID.
  return /^[A-Za-z0-9._:-]{8,64}$/.test(value) ? value : undefined;
}

function writeSse(response: http.ServerResponse, event: AgentEvent): void {
  response.write(`event: ${event.type}\n`);
  response.write(`data: ${JSON.stringify(event)}\n\n`);
}

async function main(): Promise<void> {
  const config = loadConfig();
  const problems = validateConfig(config);
  if (problems.length > 0) {
    for (const problem of problems) console.error(`config: ${problem}`);
    process.exit(1);
  }
  ensureRuntimeDirs(config);

  const semaphore = new Semaphore(config.server.maxConcurrent);
  const log = (message: string) => console.error(`[diagflow-ts] ${message}`);
  const history = await HistoryStore.open(config.history.url, config.history, log);

  const server = http.createServer(async (request, response) => {
    try {
      const url = new URL(request.url ?? "/", "http://localhost");

      if (request.method === "GET" && (url.pathname === "/" || url.pathname === "/ui")) {
        response.writeHead(200, {
          "Content-Type": "text/html; charset=utf-8",
          "Cache-Control": "no-store",
          "X-Content-Type-Options": "nosniff",
          "X-Frame-Options": "DENY",
          "Content-Security-Policy":
            "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; " +
            "connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'",
        });
        response.end(WEBUI_HTML);
        return;
      }

      if (request.method === "GET" && url.pathname === "/favicon.ico") {
        response.writeHead(204);
        response.end();
        return;
      }

      if (request.method === "GET" && url.pathname === "/healthz") {
        sendJson(response, 200, { status: "ok" });
        return;
      }

      if (request.method === "GET" && url.pathname === "/readyz") {
        const [diagflowOk, promOk, alertOk] = await Promise.all([
          config.diagflow.mcpUrl ? probeTcp(config.diagflow.mcpUrl) : Promise.resolve(false),
          config.prometheus.url ? probeTcp(config.prometheus.url) : Promise.resolve(false),
          config.prometheus.alertmanagerUrl
            ? probeTcp(config.prometheus.alertmanagerUrl)
            : Promise.resolve(false),
        ]);
        const dataSource =
          Boolean(config.diagflow.mcpUrl) ||
          Boolean(config.prometheus.url) ||
          Boolean(config.prometheus.alertmanagerUrl);
        const reachable = diagflowOk || promOk || alertOk;
        const llm = hasLlmCredential(config);
        const body = {
          status: !dataSource
            ? "not ready"
            : !reachable
              ? "not ready"
              : llm
                ? "ready"
                : "degraded",
          checks: {
            llm: llm ? "ok" : "degraded",
            diagflow_mcp: config.diagflow.mcpUrl
              ? diagflowOk
                ? "reachable"
                : "unreachable"
              : "disabled",
            prometheus: config.prometheus.url
              ? promOk
                ? "reachable"
                : "unreachable"
              : "disabled",
            alertmanager: config.prometheus.alertmanagerUrl
              ? alertOk
                ? "reachable"
                : "unreachable"
              : "disabled",
            api_auth: config.server.authToken ? "ok" : "degraded",
          },
          model: `${config.llm.provider}/${config.llm.modelId}`,
        };
        sendJson(response, dataSource && reachable ? 200 : 503, body);
        return;
      }

      const stream = url.pathname.endsWith("/stream");
      const basePath = stream ? url.pathname.slice(0, -"/stream".length) : url.pathname;
      let mode: WorkflowMode | undefined;
      if (request.method === "POST" && basePath === "/api/v1/investigate") mode = "investigate";
      if (request.method === "POST" && basePath === "/api/v1/diagnose") mode = "diagnose";

      // Console sessions. Without a store these endpoints report 503 rather than
      // silently pretending the session was saved.
      if (mode === undefined && url.pathname.startsWith("/api/v1/sessions")) {
        await handleSessions(request, response, url, config, history);
        return;
      }
      if (!mode) {
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
        if (stream) {
          await handleStream(request, response, config, log, mode, history);
        } else {
          await handleJson(request, response, config, log, mode, history);
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
    log(`model key=${config.llm.apiKeySource ?? "none"}`);
    // Surface a mismatched provider/key pair at boot instead of as a 401 deep
    // in the first diagnosis.
    const hint = llmConfigHint(config);
    if (hint) log(`config warning: ${hint}`);
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

async function executeWorkflow(
  config: AgentConfig,
  input: InvestigationInput,
  mode: WorkflowMode,
  sink: EventSink,
): Promise<{ report: DiagnosisReport; markdown: string }> {
  const runtime = await createPiRuntime(config, sink);
  const executor = mode === "diagnose" ? await McpToolExecutor.create(config) : undefined;
  try {
    return mode === "diagnose" && executor
      ? await runDiagnosis(runtime, executor, input, sink)
      : await runInvestigation(runtime, input, sink);
  } finally {
    await runtime.close();
    await executor?.close();
  }
}

/**
 * Resolve the session a request belongs to: attach the previous conclusions
 * and return a writer for this turn. An unknown or foreign session id is a 404
 * rather than a silent fresh start, so a stale browser tab cannot quietly
 * diagnose without its history.
 */
async function resolveSession(
  request: http.IncomingMessage,
  body: InvestigateRequest,
  history: HistoryStore | undefined,
): Promise<{
  history: InvestigationInput["history"];
  record?: (report: DiagnosisReport, mode: WorkflowMode) => Promise<void>;
}> {
  const sessionId = body.session_id?.trim();
  const store = history;
  if (!sessionId || !store) return { history: undefined };
  const userId = callerId(request);
  if (!userId) {
    throw Object.assign(new Error("session_id requires an X-Diagflow-User header"), {
      statusCode: 400,
      code: "user_id_required",
    });
  }
  if (!(await store.ownsSession(userId, sessionId))) {
    throw Object.assign(new Error(`unknown session ${sessionId}`), {
      statusCode: 404,
      code: "session_not_found",
    });
  }
  const turns = await store.contextFor(userId, sessionId);
  const question = (body.detail?.trim() || body.problem || "").slice(0, 2_000);
  return {
    history: turns.map((turn) => ({ question: turn.question, conclusion: turn.conclusion })),
    record: async (report, mode) => {
      await store.appendTurn({
        sessionId,
        eventId: report.event_id,
        mode,
        component: report.component,
        problem: report.problem_type,
        clusterId: body.cluster_id?.trim() ?? "",
        // What the user typed, not the normalized input: this becomes the
        // turn's label in the session and the line replayed into later prompts.
        question,
        conclusion: truncate(report.root_cause, 4_000),
        confidence: report.confidence,
      });
    },
  };
}

async function handleJson(
  request: http.IncomingMessage,
  response: http.ServerResponse,
  config: AgentConfig,
  log: (message: string) => void,
  mode: WorkflowMode,
  history: HistoryStore | undefined,
): Promise<void> {
  const body = (await readBody(request)) as InvestigateRequest;
  const session = await resolveSession(request, body, history);
  const input: InvestigationInput = {
    ...toInput(body, mode),
    ...(session.history ? { history: session.history } : {}),
  };
  const { report, markdown } = await executeWorkflow(config, input, mode, () => undefined);
  await session.record?.(report, mode);
  log(
    `done mode=${mode} event=${report.event_id} component=${report.component} ` +
      `status=${report.status} confidence=${report.confidence} duration_ms=${report.duration_ms}`,
  );
  sendJson(response, 200, { report, markdown });
}

/**
 * Console session endpoints:
 *   GET    /api/v1/sessions            list this browser's sessions
 *   POST   /api/v1/sessions            create one
 *   GET    /api/v1/sessions/:id        turns of one session
 *   DELETE /api/v1/sessions/:id        remove one (cascades to its turns)
 */
async function handleSessions(
  request: http.IncomingMessage,
  response: http.ServerResponse,
  url: URL,
  config: AgentConfig,
  history: HistoryStore | undefined,
): Promise<void> {
  if (!authorized(request, config)) {
    sendJson(response, 401, {
      error: { code: "unauthorized", message: "missing or invalid bearer token" },
    });
    return;
  }
  if (!history) {
    sendJson(response, 503, {
      error: { code: "history_disabled", message: "session history needs TS_AGENT_MYSQL_URL" },
    });
    return;
  }
  const userId = callerId(request);
  if (!userId) {
    sendJson(response, 400, {
      error: { code: "user_id_required", message: "X-Diagflow-User header is required" },
    });
    return;
  }

  const rest = url.pathname.slice("/api/v1/sessions".length).replace(/^\//, "");
  const sessionId = rest ? decodeURIComponent(rest) : undefined;

  if (!sessionId) {
    if (request.method === "GET") {
      const limit = Number.parseInt(url.searchParams.get("limit") ?? "20", 10);
      const sessions = await history.listSessions(userId, Number.isFinite(limit) ? limit : 20);
      sendJson(response, 200, { sessions });
      return;
    }
    if (request.method === "POST") {
      const body = (await readBody(request)) as { title?: string };
      const created = await history.createSession(userId, body.title?.trim() ?? "");
      if (!created) {
        sendJson(response, 500, {
          error: { code: "session_create_failed", message: "database error" },
        });
        return;
      }
      sendJson(response, 201, { session: created });
      return;
    }
    sendJson(response, 405, { error: { code: "method_not_allowed", message: request.method ?? "" } });
    return;
  }

  if (request.method === "GET") {
    // getTurns already scopes by user, so a foreign id would look empty;
    // distinguish "not yours" from "has no turns" with an explicit check.
    if (!(await history.ownsSession(userId, sessionId))) {
      sendJson(response, 404, { error: { code: "session_not_found", message: sessionId } });
      return;
    }
    const turns = await history.getTurns(userId, sessionId);
    sendJson(response, 200, { session_id: sessionId, turns });
    return;
  }

  if (request.method === "DELETE") {
    const deleted = await history.deleteSession(userId, sessionId);
    if (!deleted) {
      sendJson(response, 404, { error: { code: "session_not_found", message: sessionId } });
      return;
    }
    sendJson(response, 200, { deleted: sessionId });
    return;
  }

  sendJson(response, 405, { error: { code: "method_not_allowed", message: request.method ?? "" } });
}

async function handleStream(
  request: http.IncomingMessage,
  response: http.ServerResponse,
  config: AgentConfig,
  log: (message: string) => void,
  mode: WorkflowMode,
  history: HistoryStore | undefined,
): Promise<void> {
  const body = (await readBody(request)) as InvestigateRequest;
  const session = await resolveSession(request, body, history);
  const input: InvestigationInput = {
    ...toInput(body, mode),
    ...(session.history ? { history: session.history } : {}),
  };

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

  const sink: EventSink = (event) => {
    if (!response.writableEnded) writeSse(response, event);
  };

  try {
    const { report } = await executeWorkflow(config, input, mode, sink);
    await session.record?.(report, mode);
  } catch (error) {
    const status = errorStatus(error);
    log(`failed mode=${mode} code=${status.code} message=${status.message}`);
    writeSse(response, { type: "failed", code: status.code, message: status.message });
    writeSse(response, { type: "done", duration_ms: 0 });
  } finally {
    clearInterval(heartbeat);
    if (!response.writableEnded) response.end();
  }
}

main().catch((error) => {
  console.error(`[diagflow-ts] fatal: ${errorMessage(error)}`);
  process.exit(1);
});
