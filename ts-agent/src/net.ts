import net from "node:net";

/**
 * TCP reachability probe for a configured dependency URL.
 *
 * Readiness only needs to know whether the endpoint accepts a connection; it
 * deliberately does not send an MCP handshake, so a busier/slower gateway does
 * not flap the pod.
 */
export function probeTcp(rawUrl: string, timeoutMs = 2_000): Promise<boolean> {
  let url: URL;
  try {
    url = new URL(rawUrl);
  } catch {
    return Promise.resolve(false);
  }
  const port = url.port
    ? Number.parseInt(url.port, 10)
    : url.protocol === "https:"
      ? 443
      : 80;

  return new Promise((resolve) => {
    // WHATWG URL keeps the brackets on an IPv6 literal hostname; net.connect
    // wants the bare address.
    const host = url.hostname.replace(/^\[|\]$/g, "");
    const socket = net.createConnection({ host, port });
    let settled = false;
    const finish = (ok: boolean) => {
      if (settled) return;
      settled = true;
      socket.destroy();
      resolve(ok);
    };
    socket.setTimeout(timeoutMs);
    socket.once("connect", () => finish(true));
    socket.once("timeout", () => finish(false));
    socket.once("error", () => finish(false));
  });
}
