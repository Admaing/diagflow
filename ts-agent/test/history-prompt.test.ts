import assert from "node:assert/strict";
import test from "node:test";
import { buildIntentPrompt, buildSynthesisPrompt } from "../src/workflow/prompts.js";
import type { InvestigationInput } from "../src/report/types.js";

const base: InvestigationInput = { component: "hdfs", problem: "health_check" };

test("history turns are replayed as conclusions with a no-stale-facts rule", () => {
  const prompt = buildIntentPrompt(
    {
      ...base,
      history: [
        { question: "查下 NameNode 状态", conclusion: "NameNode 主备均存活，未见 safemode" },
        { question: "core 节点呢", conclusion: "core1/core2 DataNode 块数为 0" },
      ],
    },
    {},
    "zh-CN",
  );
  assert.match(prompt, /本会话之前的排查/);
  assert.match(prompt, /第 1 轮 问「查下 NameNode 状态」→ 结论：NameNode 主备均存活/);
  assert.match(prompt, /第 2 轮 问「core 节点呢」→ 结论：core1\/core2 DataNode 块数为 0/);
  assert.match(prompt, /绝不能把历史结论当作当前状态/);
});

test("a first turn carries no history block", () => {
  const prompt = buildIntentPrompt(base, {}, "zh-CN");
  assert.doesNotMatch(prompt, /本会话之前的排查/);
});

test("long history lines are truncated so the block cannot dominate the prompt", () => {
  const prompt = buildIntentPrompt(
    {
      ...base,
      history: [{ question: "问".repeat(500), conclusion: "答".repeat(500) }],
    },
    {},
    "zh-CN",
  );
  const line = prompt.split("\n").find((l) => l.includes("第 1 轮"));
  assert.ok(line, "history line missing");
  assert.ok(line!.length < 400, `history line was not truncated: ${line!.length} chars`);
});

test("synthesis sees the same history as intent planning", () => {
  const prompt = buildSynthesisPrompt(
    { ...base, history: [{ question: "上一次问题", conclusion: "上一次结论" }] },
    [{ intent: "x", judgment: "y", suspect_component_bug: false, actions: [], snippets: [] }],
    {},
    "zh-CN",
  );
  assert.match(prompt, /上一次结论/);
  assert.match(prompt, /绝不能把历史结论当作当前状态/);
});
