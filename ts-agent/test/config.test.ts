import assert from "node:assert/strict";
import test from "node:test";
import { llmConfigHint, type AgentConfig } from "../src/config.js";

/**
 * These exercise the hint directly. Driving it through loadConfig() would mean
 * mutating process.env, but env assignment does not reliably stick under the
 * tsx loader (a key set in the process keeps winning across writes), so an env
 * test would pass or fail for reasons unrelated to the logic under test.
 * Key precedence in loadConfig() is covered by the deployment wiring and by
 * the startup log line that prints the source variable.
 */
function config(llm: Partial<AgentConfig["llm"]>): AgentConfig {
  return {
    llm: { provider: "deepseek", modelId: "deepseek-flash", thinkingLevel: "medium", ...llm },
  } as AgentConfig;
}

test("a DeepSeek key behind a gateway provider is reported, not silently sent", () => {
  // Reproduces the deployed misconfiguration: modelverse provider, but the
  // gateway token was wired into DEEPSEEK_API_KEY, which the upstream rejected 401.
  const hint = llmConfigHint(
    config({ provider: "modelverse", apiKey: "gateway-key", apiKeySource: "DEEPSEEK_API_KEY" }),
  );
  assert.match(hint ?? "", /DEEPSEEK_API_KEY/);
  assert.match(hint ?? "", /TS_AGENT_LLM_API_KEY/);
});

test("a key from the matching variable is not reported", () => {
  assert.equal(
    llmConfigHint(config({ provider: "modelverse", apiKey: "k", apiKeySource: "MODELVERSE_API_KEY" })),
    undefined,
  );
  assert.equal(
    llmConfigHint(config({ provider: "deepseek", apiKey: "k", apiKeySource: "DEEPSEEK_API_KEY" })),
    undefined,
  );
});

test("TS_AGENT_LLM_API_KEY is accepted for any provider", () => {
  assert.equal(
    llmConfigHint(config({ provider: "modelverse", apiKey: "k", apiKeySource: "TS_AGENT_LLM_API_KEY" })),
    undefined,
  );
});

test("a missing TS_AGENT_MODELS_JSON file is reported", () => {
  const hint = llmConfigHint(config({ modelsPath: "/nonexistent/models.json" }));
  assert.match(hint ?? "", /TS_AGENT_MODELS_JSON/);
});

test("the shipped models.json declares the modelverse model the deployment uses", async () => {
  const { ModelRuntime } = await import("@earendil-works/pi-coding-agent");
  const modelsPath = new URL("../models.json", import.meta.url);
  const runtime = await ModelRuntime.create({ modelsPath: modelsPath.pathname });
  const model = runtime.getModel("modelverse", "deepseek-v4.1-flash-1017");
  assert.ok(model, "models.json must declare deepseek-v4.1-flash-1017");
  assert.equal(model.api, "openai-completions");
  assert.equal(model.baseUrl, "https://api.modelverse.cn/v1");
});
