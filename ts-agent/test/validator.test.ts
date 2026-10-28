import assert from "node:assert/strict";
import test from "node:test";
import { validateConclusion } from "../src/validator/validator.js";

test("accepts an evidence-backed conclusion", () => {
  assert.equal(
    validateConclusion(
      "TaskManager heap exhausted, confirmed by OutOfMemoryError in the log",
      ["increase heap"],
      2,
    ).ok,
    true,
  );
});

test("accepts a Chinese conclusion that references evidence", () => {
  assert.equal(
    validateConclusion("日志中连续出现 OutOfMemoryError，TaskManager 堆内存不足", ["增大堆内存"], 2).ok,
    true,
  );
});

test("rejects a short root cause", () => {
  assert.equal(validateConclusion("OOM", ["x"], 1).ok, false);
});

test("rejects missing suggestions", () => {
  assert.equal(validateConclusion("TaskManager OOM from log evidence", [], 1).ok, false);
});

test("rejects an evidence-free conclusion", () => {
  assert.equal(validateConclusion("TaskManager OOM from log evidence", ["x"], 0).ok, false);
});

test("rejects a conclusion that does not reference evidence", () => {
  const result = validateConclusion("The job was unlucky and should be retried", ["retry"], 1);
  assert.equal(result.ok, false);
  assert.match(result.feedback, /evidence/);
});
