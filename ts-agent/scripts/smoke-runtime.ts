import { loadConfig } from "../src/config.js";
import { createPiRuntime } from "../src/runtime/pi.js";

const config = loadConfig();
config.diagflow.mcpUrl = process.env.SMOKE_MCP_URL ?? "http://127.0.0.1:1/mcp";
config.server.stageTimeoutMs = 5_000;

const runtime = await createPiRuntime(config, (event) => {
  if (event.type === "step") console.error(`[${event.kind}] ${event.text}`);
});
console.log("runtime-created");
await runtime.close();
