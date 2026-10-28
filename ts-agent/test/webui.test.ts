import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { fileURLToPath } from "node:url";

const html = fs.readFileSync(
  fileURLToPath(new URL("../src/webui.html", import.meta.url)),
  "utf8",
);

test("web console is self-contained and calls the same-origin API", () => {
  assert.match(html, /id="diagnose-form"/);
  assert.match(html, /\/api\/v1\//);
  assert.match(html, /Authorization/);
  assert.ok(!/https?:\/\//.test(html), "the console must not load external origins");
});

test("web console has both workflow modes", () => {
  assert.match(html, /value="diagnose"/);
  assert.match(html, /value="investigate"/);
});

test("web console has anonymous session management", () => {
  assert.match(html, /id="session-list"/);
  assert.match(html, /id="new-session"/);
  assert.match(html, /\/api\/v1\/sessions/);
  assert.match(html, /X-Diagflow-User/);
  assert.match(html, /session_id/);
  assert.match(html, /diagflow\.ts\.user/);
});
