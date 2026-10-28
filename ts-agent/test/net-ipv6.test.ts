import assert from "node:assert/strict";
import net from "node:net";
import test from "node:test";
import { probeTcp } from "/Users/user/Prj/diagflow/ts-agent/src/net.js";

test("probeTcp handles a bracketed IPv6 literal", async () => {
  const server = net.createServer();
  await new Promise<void>((resolve, reject) =>
    server.listen(0, "::1", (error?: Error) => (error ? reject(error) : resolve())),
  ).catch(() => undefined);
  const address = server.address();
  if (!address || typeof address === "string") {
    await new Promise<void>((r) => server.close(() => r()));
    return;
  }
  try {
    assert.equal(await probeTcp(`http://[::1]:${address.port}`, 1_000), true);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});
