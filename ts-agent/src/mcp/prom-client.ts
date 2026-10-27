export interface PrometheusConfig {
  prometheusUrl?: string;
  alertmanagerUrl?: string;
  basicAuth?: string;
  bearerToken?: string;
  timeoutMs?: number;
}

function authHeaders(config: PrometheusConfig): Record<string, string> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (config.bearerToken) {
    headers.Authorization = `Bearer ${config.bearerToken}`;
  } else if (config.basicAuth) {
    headers.Authorization = `Basic ${Buffer.from(config.basicAuth).toString("base64")}`;
  }
  return headers;
}

function endpoint(base: string, path: string, params?: Record<string, string | undefined>): URL {
  const root = base.replace(/\/+$/, "");
  const url = new URL(`${root}${path}`);
  for (const [key, value] of Object.entries(params ?? {})) {
    if (value !== undefined && value !== "") url.searchParams.set(key, value);
  }
  return url;
}

async function getJson(
  base: string | undefined,
  path: string,
  params: Record<string, string | undefined>,
  config: PrometheusConfig,
): Promise<unknown> {
  if (!base) {
    throw new Error(`no endpoint configured for ${path}`);
  }
  const url = endpoint(base, path, params);
  const response = await fetch(url, {
    method: "GET",
    headers: authHeaders(config),
    signal: AbortSignal.timeout(config.timeoutMs ?? 15_000),
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`${url.pathname} returned HTTP ${response.status}: ${text.slice(0, 500)}`);
  }
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return { raw: text.slice(0, 20_000) };
  }
}

export async function promQuery(
  config: PrometheusConfig,
  query: string,
  time?: string,
): Promise<unknown> {
  return getJson(config.prometheusUrl, "/api/v1/query", { query, time }, config);
}

export async function promQueryRange(
  config: PrometheusConfig,
  query: string,
  start: string,
  end: string,
  step: string,
): Promise<unknown> {
  return getJson(config.prometheusUrl, "/api/v1/query_range", { query, start, end, step }, config);
}

export async function promAlerts(config: PrometheusConfig): Promise<unknown> {
  return getJson(
    config.alertmanagerUrl,
    "/api/v2/alerts",
    { active: "true", silenced: "false", inhibited: "false" },
    config,
  );
}

export async function promRules(config: PrometheusConfig): Promise<unknown> {
  return getJson(config.prometheusUrl, "/api/v1/rules", { type: "alert" }, config);
}

export async function promTargets(config: PrometheusConfig): Promise<unknown> {
  return getJson(config.prometheusUrl, "/api/v1/targets", { state: "active" }, config);
}

export function prometheusConfigFromEnv(): PrometheusConfig {
  return {
    prometheusUrl: process.env.PROMETHEUS_URL?.trim() || undefined,
    alertmanagerUrl: process.env.ALERTMANAGER_URL?.trim() || undefined,
    basicAuth: process.env.PROMETHEUS_BASIC_AUTH?.trim() || undefined,
    bearerToken: process.env.PROMETHEUS_BEARER_TOKEN?.trim() || undefined,
    timeoutMs: 15_000,
  };
}
