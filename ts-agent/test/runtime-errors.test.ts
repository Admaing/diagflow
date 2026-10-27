import assert from "node:assert/strict";
import test from "node:test";
import { llmErrorFromMessages } from "../src/runtime/pi.js";

test("llmErrorFromMessages surfaces a provider authentication failure", () => {
  const messages = [
    { role: "user", content: "diagnose" },
    {
      role: "assistant",
      content: [],
      stopReason: "error",
      errorMessage: '401: {"message":"Authentication Fails"}',
    },
  ];
  assert.equal(llmErrorFromMessages(messages), '401: {"message":"Authentication Fails"}');
});

test("llmErrorFromMessages ignores a successful last assistant message", () => {
  const messages = [
    {
      role: "assistant",
      content: [],
      stopReason: "error",
      errorMessage: "earlier failure",
    },
    { role: "assistant", content: [{ type: "text", text: "done" }], stopReason: "stop" },
  ];
  assert.equal(llmErrorFromMessages(messages), undefined);
});

test("llmErrorFromMessages returns undefined without an assistant message", () => {
  assert.equal(llmErrorFromMessages([{ role: "user", content: "hi" }]), undefined);
  assert.equal(llmErrorFromMessages([]), undefined);
});
