import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import path from "node:path";
import test from "node:test";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";

test("prom MCP server exposes only read-only monitoring tools", async () => {
  const server = http.createServer((_request, response) => {
    response.setHeader("content-type", "application/json");
    response.end(JSON.stringify({ status: "success", data: { resultType: "vector", result: [] } }));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  const root = path.resolve(import.meta.dirname, "..");

  const transport = new StdioClientTransport({
    command: process.execPath,
    args: ["--import", "tsx", path.join(root, "src", "mcp", "prom-server.ts")],
    cwd: root,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: process.env.HOME ?? "",
      PROMETHEUS_URL: `http://127.0.0.1:${port}`,
      ALERTMANAGER_URL: `http://127.0.0.1:${port}`,
    },
  });
  const client = new Client({ name: "test", version: "0.0.0" }, { capabilities: {} });

  try {
    await client.connect(transport);
    const listed = await client.listTools();
    const names = listed.tools.map((tool) => tool.name).sort();
    assert.deepEqual(names, [
      "prom_alerts",
      "prom_query",
      "prom_query_range",
      "prom_rules",
      "prom_targets",
    ]);
    for (const tool of listed.tools) {
      assert.equal(tool.annotations?.readOnlyHint, true);
    }

    const result = await client.callTool({ name: "prom_query", arguments: { query: "up" } });
    assert.equal(result.isError, undefined);
    const content = Array.isArray(result.content) ? result.content : [];
    assert.ok(
      content.some(
        (item) =>
          item.type === "text" &&
          typeof item.text === "string" &&
          item.text.includes('"status": "success"'),
      ),
    );
  } finally {
    await client.close();
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  }
});
