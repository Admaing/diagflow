import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import test from "node:test";
import { promAlerts, promQuery, promQueryRange } from "../src/mcp/prom-client.js";

test("prometheus client sends read-only queries with auth and parses JSON", async () => {
  const seen: Array<{ url?: string; authorization?: string }> = [];
  const server = http.createServer((request, response) => {
    seen.push({
      url: request.url,
      authorization: request.headers.authorization,
    });
    response.setHeader("content-type", "application/json");
    response.end(JSON.stringify({ status: "success", data: { resultType: "vector", result: [] } }));
  });

  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  const config = {
    prometheusUrl: `http://127.0.0.1:${port}`,
    alertmanagerUrl: `http://127.0.0.1:${port}`,
    bearerToken: "test-token",
  };

  try {
    const instant = await promQuery(config, "up", "1700000000");
    assert.equal((instant as { status: string }).status, "success");

    await promQueryRange(config, "rate(http_requests_total[5m])", "1700000000", "1700000600", "30s");
    await promAlerts(config);

    assert.equal(seen.length, 3);
    assert.ok(seen[0]?.url?.startsWith("/api/v1/query?"));
    assert.ok(seen[0]?.url?.includes("query=up"));
    assert.ok(seen[1]?.url?.startsWith("/api/v1/query_range?"));
    assert.ok(seen[2]?.url?.startsWith("/api/v2/alerts?"));
    assert.equal(seen[0]?.authorization, "Bearer test-token");
  } finally {
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  }
});

test("prometheus client surfaces HTTP errors", async () => {
  const server = http.createServer((_request, response) => {
    response.statusCode = 400;
    response.end("bad query");
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  try {
    await assert.rejects(
      promQuery({ prometheusUrl: `http://127.0.0.1:${port}` }, "up"),
      /HTTP 400: bad query/,
    );
  } finally {
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  }
});
