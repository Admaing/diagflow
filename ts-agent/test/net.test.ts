import assert from "node:assert/strict";
import net from "node:net";
import test from "node:test";
import { probeTcp } from "../src/net.js";

async function listen(): Promise<{ server: net.Server; port: number }> {
  const server = net.createServer();
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (!address || typeof address === "string") throw new Error("no address");
  return { server, port: address.port };
}

test("probeTcp returns true for a listening endpoint", async () => {
  const { server, port } = await listen();
  try {
    assert.equal(await probeTcp(`http://127.0.0.1:${port}`, 1_000), true);
  } finally {
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve())),
    );
  }
});

test("probeTcp returns false for a closed port", async () => {
  const { server, port } = await listen();
  await new Promise<void>((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve())),
  );
  assert.equal(await probeTcp(`http://127.0.0.1:${port}`, 500), false);
});

test("probeTcp returns false for an invalid URL", async () => {
  assert.equal(await probeTcp("not a url", 100), false);
});
