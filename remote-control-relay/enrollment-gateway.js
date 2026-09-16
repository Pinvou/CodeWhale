import http from "node:http";
import { URL } from "node:url";
import { WebSocket, WebSocketServer } from "ws";

const MAX_FIRST_FRAME_BYTES = 16 * 1024;
const DEFAULT_AUTH_TIMEOUT_MS = 10_000;

export class EnrollmentGatewayError extends Error {
  constructor(code) {
    super(code);
    this.name = "EnrollmentGatewayError";
    this.code = code;
  }
}

function fail(code) {
  throw new EnrollmentGatewayError(code);
}

function safeUpstream(value) {
  let url;
  try { url = new URL(value); } catch { fail("gateway_upstream_invalid"); }
  if (!(["ws:", "wss:"].includes(url.protocol)) || url.username || url.password || url.hash) {
    fail("gateway_upstream_invalid");
  }
  const host = url.hostname.toLowerCase();
  if (!(host === "127.0.0.1" || host === "::1" || host === "localhost")) {
    fail("gateway_upstream_not_loopback");
  }
  return url.href;
}

function genericEnrollmentError(requestId) {
  return {
    v: 2,
    type: "error",
    ...(typeof requestId === "string" ? { request_id: requestId } : {}),
    code: "endpoint_not_found",
    message: "endpoint not found",
  };
}

function firstFrame(message) {
  if (typeof message !== "string" || Buffer.byteLength(message, "utf8") > MAX_FIRST_FRAME_BYTES) fail("gateway_frame_invalid");
  let parsed;
  try { parsed = JSON.parse(message); } catch { fail("gateway_frame_invalid"); }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed) || parsed.v !== 2) fail("gateway_frame_invalid");
  if (!(["desktop_endpoint_register", "web_client_join"].includes(parsed.type))
    || typeof parsed.endpoint_id !== "string" || typeof parsed.access_token !== "string") {
    fail("gateway_frame_invalid");
  }
  return parsed;
}

function closePolicy(socket, requestId) {
  if (socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(genericEnrollmentError(requestId)));
  socket.close(1008, "endpoint unavailable");
}

export function createEnrollmentGateway({
  registry,
  upstreamUrl,
  basePath = "/pinvou3/remote",
  authTimeoutMs = DEFAULT_AUTH_TIMEOUT_MS,
  logger = null,
} = {}) {
  if (!registry || typeof registry.authorizeRegistration !== "function") fail("gateway_registry_invalid");
  upstreamUrl = safeUpstream(upstreamUrl);
  if (!/^\/[A-Za-z0-9/_-]*[A-Za-z0-9_-]$/u.test(basePath)) fail("gateway_base_path_invalid");
  if (!Number.isSafeInteger(authTimeoutMs) || authTimeoutMs < 100 || authTimeoutMs > 60_000) {
    fail("gateway_auth_timeout_invalid");
  }
  const server = http.createServer((request, response) => {
    if (request.url === `${basePath}/healthz` || request.url === "/healthz") {
      response.writeHead(200, {
        "content-type": "application/json; charset=utf-8",
        "cache-control": "no-store",
        "x-pinvou-enrollment-gateway": "v1",
      });
      response.end(JSON.stringify({ ok: true, enrollment_gateway: "v1", legacy_v2_guarded: true }));
      return;
    }
    response.writeHead(404, { "content-type": "text/plain; charset=utf-8" });
    response.end("not found");
  });
  const wss = new WebSocketServer({ noServer: true, maxPayload: MAX_FIRST_FRAME_BYTES });
  wss.on("headers", (headers) => headers.push("X-Pinvou-Enrollment-Gateway: v1"));

  server.on("upgrade", (request, socket, head) => {
    const path = new URL(request.url, "http://gateway.invalid").pathname;
    if (path !== `${basePath}/ws`) {
      socket.write("HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n");
      socket.destroy();
      return;
    }
    wss.handleUpgrade(request, socket, head, (client) => wss.emit("connection", client, request));
  });

  wss.on("connection", (client) => {
    let authenticated = false;
    let upstream = null;
    const timer = setTimeout(() => closePolicy(client), authTimeoutMs);
    client.once("message", async (data, isBinary) => {
      if (isBinary) {
        clearTimeout(timer);
        closePolicy(client);
        return;
      }
      let raw;
      let parsed;
      try {
        raw = data.toString("utf8");
        parsed = firstFrame(raw);
        await registry.authorizeRegistration({
          endpointId: parsed.endpoint_id,
          credential: parsed.access_token,
          purpose: "web_v2_reregister",
        });
      } catch (error) {
        clearTimeout(timer);
        logger?.warn?.("enrollment gateway rejected connection", { code: error?.code || "authorization_failed" });
        closePolicy(client, parsed?.request_id);
        return;
      }
      upstream = new WebSocket(upstreamUrl, {
        headers: { "x-pinvou-enrollment-gateway": "v1" },
        maxPayload: 16 * 1024 * 1024,
      });
      upstream.once("open", () => {
        clearTimeout(timer);
        authenticated = true;
        upstream.send(raw);
        client.on("message", (message, binary) => {
          if (upstream.readyState === WebSocket.OPEN) upstream.send(message, { binary });
        });
        upstream.on("message", (message, binary) => {
          if (client.readyState === WebSocket.OPEN) client.send(message, { binary });
        });
      });
      upstream.once("error", () => {
        clearTimeout(timer);
        if (!authenticated) closePolicy(client, parsed.request_id);
      });
      upstream.once("close", (code, reason) => {
        if (client.readyState === WebSocket.OPEN) client.close(code || 1011, reason.toString().slice(0, 123));
      });
    });
    client.once("close", () => {
      clearTimeout(timer);
      if (upstream && upstream.readyState < WebSocket.CLOSING) upstream.close(1000, "client closed");
    });
  });

  return Object.freeze({
    server,
    listen(options) {
      return new Promise((resolve, reject) => {
        server.once("error", reject);
        server.listen(options, () => {
          server.off("error", reject);
          resolve(server.address());
        });
      });
    },
    close() {
      for (const client of wss.clients) client.terminate();
      return new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
    },
  });
}

export async function validateLegacyRollbackTopology({
  publicEntrypoints,
  legacyRelayUrl,
  legacyRelayBindHost,
  fetchImpl = fetch,
  webSocketProbe = probeEnrollmentGateway,
} = {}) {
  if (!Array.isArray(publicEntrypoints) || publicEntrypoints.length === 0) fail("gateway_public_entrypoints_invalid");
  let direct;
  try { direct = new URL(legacyRelayUrl); } catch { fail("gateway_upstream_invalid"); }
  const host = direct.hostname.toLowerCase();
  if (!(host === "127.0.0.1" || host === "::1" || host === "localhost")) fail("gateway_direct_bypass_exposed");
  if (!(legacyRelayBindHost === "127.0.0.1" || legacyRelayBindHost === "::1")) {
    fail("gateway_legacy_bind_not_loopback");
  }
  const checked = [];
  for (const entrypoint of [...new Set(publicEntrypoints)]) {
    let url;
    try { url = new URL(entrypoint); } catch { fail("gateway_public_entrypoint_invalid"); }
    const local = ["127.0.0.1", "::1", "localhost"].includes(url.hostname.toLowerCase());
    if ((url.protocol !== "https:" && !(local && url.protocol === "http:"))
      || url.username || url.password || url.hash || url.search) fail("gateway_public_entrypoint_invalid");
    const health = new URL(`${url.pathname.replace(/\/$/u, "")}/healthz`, url);
    let response;
    try { response = await fetchImpl(health, { redirect: "error", signal: AbortSignal.timeout(5000) }); } catch {
      fail("gateway_preflight_unavailable");
    }
    if (!response.ok || response.headers.get("x-pinvou-enrollment-gateway") !== "v1") {
      fail("gateway_preflight_unverified");
    }
    const body = await response.json();
    if (body?.enrollment_gateway !== "v1" || body?.legacy_v2_guarded !== true) fail("gateway_preflight_unverified");
    const wsUrl = new URL(`${url.pathname.replace(/\/$/u, "")}/ws`, url);
    wsUrl.protocol = wsUrl.protocol === "https:" ? "wss:" : "ws:";
    if (!(await webSocketProbe(wsUrl))) fail("gateway_websocket_route_unverified");
    checked.push(url.origin + url.pathname);
  }
  return Object.freeze({ safe_to_run_legacy_v2: true, checked_public_entrypoints: checked.sort() });
}

function probeEnrollmentGateway(url) {
  return new Promise((resolve) => {
    const socket = new WebSocket(url, { handshakeTimeout: 5000 });
    let marked = false;
    const timer = setTimeout(() => {
      socket.terminate();
      resolve(false);
    }, 5500);
    socket.once("upgrade", (response) => {
      marked = response.headers["x-pinvou-enrollment-gateway"] === "v1";
    });
    socket.once("open", () => {
      clearTimeout(timer);
      socket.close(1000, "preflight complete");
      resolve(marked);
    });
    socket.once("unexpected-response", (_request, response) => {
      clearTimeout(timer);
      response.resume();
      resolve(false);
    });
    socket.once("error", () => {
      clearTimeout(timer);
      resolve(false);
    });
  });
}
