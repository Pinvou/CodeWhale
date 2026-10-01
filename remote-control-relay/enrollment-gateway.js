import http from "node:http";
import crypto from "node:crypto";
import { execFile } from "node:child_process";
import { isIP } from "node:net";
import { promisify } from "node:util";
import { URL } from "node:url";
import { WebSocket, WebSocketServer } from "ws";

const MAX_FIRST_FRAME_BYTES = 16 * 1024;
const DEFAULT_MAX_PAYLOAD_BYTES = 4 * 1024 * 1024;
const DEFAULT_AUTH_TIMEOUT_MS = 10_000;
const DEFAULT_CONNECTION_LIMIT = 120;
const DEFAULT_CONNECTION_WINDOW_MS = 60_000;
const DEFAULT_MAX_CONNECTIONS = 5000;
const DEFAULT_MAX_SOURCE_BUCKETS = 100_000;
const DEFAULT_INGRESS_MESSAGE_LIMIT = 12_000;
const DEFAULT_INGRESS_BYTE_LIMIT = 512 * 1024 * 1024;
const execFileAsync = promisify(execFile);

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
  if (!(["ws:", "wss:"].includes(url.protocol)) || url.username || url.password || url.hash || url.search) {
    fail("gateway_upstream_invalid");
  }
  const host = url.hostname.toLowerCase();
  if (!(host === "127.0.0.1" || host === "[::1]" || host === "::1")) {
    fail("gateway_upstream_not_loopback");
  }
  return url.href;
}

function normalizeIp(value) {
  const ip = String(value || "").trim();
  return ip.startsWith("::ffff:") ? ip.slice(7) : ip;
}

function stringSet(value, label, normalize = (item) => item) {
  if (value === undefined) return new Set();
  if (!Array.isArray(value) || value.some((item) => typeof item !== "string" || item.includes(","))) fail(label);
  return new Set(value.map((item) => normalize(item.trim())).filter(Boolean));
}

function boundedInteger(value, fallback, min, max, label) {
  if (value === undefined) return fallback;
  if (!Number.isSafeInteger(value) || value < min || value > max) fail(label);
  return value;
}

function browserOriginAllowed(request, allowedOrigins) {
  const raw = request.headers.origin;
  if (raw === undefined) return true;
  if (typeof raw !== "string" || raw.includes(",")) return false;
  let parsed;
  try { parsed = new URL(raw); } catch { return false; }
  if (!/^https?:$/u.test(parsed.protocol) || parsed.origin === "null") return false;
  const origin = parsed.origin.toLowerCase();
  if (allowedOrigins.size > 0) return allowedOrigins.has(origin);
  const requestHost = String(request.headers.host || "").trim().toLowerCase();
  return Boolean(requestHost) && parsed.host.toLowerCase() === requestHost;
}

function requestSource(request, trustedProxyIps) {
  const peer = normalizeIp(request.socket?.remoteAddress) || "unknown";
  const forwardedHeader = request.headers["x-forwarded-for"];
  if (forwardedHeader !== undefined && !trustedProxyIps.has(peer)) fail("gateway_untrusted_proxy_header");
  if (!trustedProxyIps.has(peer)) return peer;
  if (typeof forwardedHeader !== "string" || forwardedHeader.includes(",")) {
    fail("gateway_proxy_header_invalid");
  }
  const source = normalizeIp(forwardedHeader);
  if (!source || isIP(source) === 0) fail("gateway_proxy_header_invalid");
  return source;
}

function consumeBucket(buckets, key, limit, windowMs, maxEntries, now = Date.now()) {
  const bucket = buckets.get(key);
  if (!bucket || now - bucket.started_at >= windowMs) {
    if (!bucket && buckets.size >= maxEntries) {
      for (const [candidate, record] of buckets) {
        if (now - record.started_at >= windowMs) buckets.delete(candidate);
      }
      if (buckets.size >= maxEntries) return false;
    }
    buckets.set(key, { started_at: now, count: 1 });
    return true;
  }
  if (bucket.count >= limit) return false;
  bucket.count += 1;
  return true;
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
  if (parsed.type === "desktop_endpoint_register" && typeof parsed.desktop_secret !== "string") fail("gateway_frame_invalid");
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
  maxPayloadBytes = DEFAULT_MAX_PAYLOAD_BYTES,
  maxConnections = DEFAULT_MAX_CONNECTIONS,
  maxSourceBuckets = DEFAULT_MAX_SOURCE_BUCKETS,
  connectionLimit = DEFAULT_CONNECTION_LIMIT,
  connectionWindowMs = DEFAULT_CONNECTION_WINDOW_MS,
  ingressMessageLimit = DEFAULT_INGRESS_MESSAGE_LIMIT,
  ingressByteLimit = DEFAULT_INGRESS_BYTE_LIMIT,
  allowedWebOrigins = [],
  trustedProxyIps = [],
  logger = null,
} = {}) {
  if (!registry || typeof registry.authorizeWebRegistration !== "function"
    || typeof registry.authorizeDesktopRegistration !== "function") fail("gateway_registry_invalid");
  upstreamUrl = safeUpstream(upstreamUrl);
  if (!/^\/[A-Za-z0-9/_-]*[A-Za-z0-9_-]$/u.test(basePath)) fail("gateway_base_path_invalid");
  if (!Number.isSafeInteger(authTimeoutMs) || authTimeoutMs < 100 || authTimeoutMs > 60_000) {
    fail("gateway_auth_timeout_invalid");
  }
  maxPayloadBytes = boundedInteger(maxPayloadBytes, DEFAULT_MAX_PAYLOAD_BYTES,
    4 * 1024 * 1024, 16 * 1024 * 1024, "gateway_max_payload_invalid");
  maxConnections = boundedInteger(maxConnections, DEFAULT_MAX_CONNECTIONS, 1, 250_000,
    "gateway_max_connections_invalid");
  maxSourceBuckets = boundedInteger(maxSourceBuckets, DEFAULT_MAX_SOURCE_BUCKETS, 1, 250_000,
    "gateway_source_buckets_invalid");
  connectionLimit = boundedInteger(connectionLimit, DEFAULT_CONNECTION_LIMIT, 1, 100_000,
    "gateway_connection_limit_invalid");
  connectionWindowMs = boundedInteger(connectionWindowMs, DEFAULT_CONNECTION_WINDOW_MS, 1000, 60 * 60_000,
    "gateway_connection_window_invalid");
  ingressMessageLimit = boundedInteger(ingressMessageLimit, DEFAULT_INGRESS_MESSAGE_LIMIT, 1, 1_000_000,
    "gateway_ingress_limit_invalid");
  ingressByteLimit = boundedInteger(ingressByteLimit, DEFAULT_INGRESS_BYTE_LIMIT, maxPayloadBytes,
    16 * 1024 * 1024 * 1024, "gateway_ingress_limit_invalid");
  const origins = stringSet(allowedWebOrigins, "gateway_origins_invalid", (origin) => {
    let parsed;
    try { parsed = new URL(origin); } catch { fail("gateway_origins_invalid"); }
    if (!/^https?:$/u.test(parsed.protocol) || parsed.origin === "null") fail("gateway_origins_invalid");
    return parsed.origin.toLowerCase();
  });
  const proxies = stringSet(trustedProxyIps, "gateway_trusted_proxies_invalid", (value) => {
    const ip = normalizeIp(value);
    if (isIP(ip) === 0) fail("gateway_trusted_proxies_invalid");
    return ip;
  });
  const connectionBuckets = new Map();
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
  const wss = new WebSocketServer({ noServer: true, maxPayload: maxPayloadBytes });
  wss.on("headers", (headers) => headers.push("X-Pinvou-Enrollment-Gateway: v1"));

  server.on("upgrade", (request, socket, head) => {
    let path;
    try { path = new URL(request.url, "http://gateway.invalid").pathname; } catch {
      socket.write("HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n");
      socket.destroy();
      return;
    }
    if (path !== `${basePath}/ws`) {
      socket.write("HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n");
      socket.destroy();
      return;
    }
    let source;
    try { source = requestSource(request, proxies); } catch {
      socket.write("HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n");
      socket.destroy();
      return;
    }
    if (!browserOriginAllowed(request, origins)) {
      socket.write("HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n");
      socket.destroy();
      return;
    }
    if (wss.clients.size >= maxConnections
      || !consumeBucket(connectionBuckets, source, connectionLimit, connectionWindowMs, maxSourceBuckets)) {
      socket.write("HTTP/1.1 429 Too Many Requests\r\nConnection: close\r\n\r\n");
      socket.destroy();
      return;
    }
    request.enrollmentGatewaySource = source;
    wss.handleUpgrade(request, socket, head, (client) => wss.emit("connection", client, request));
  });

  wss.on("connection", (client, request) => {
    let authenticated = false;
    let upstream = null;
    const ingress = { started_at: Date.now(), messages: 0, bytes: 0 };
    const consumeIngress = (bytes) => {
      const now = Date.now();
      if (now - ingress.started_at >= connectionWindowMs) {
        ingress.started_at = now;
        ingress.messages = 0;
        ingress.bytes = 0;
      }
      ingress.messages += 1;
      ingress.bytes += bytes;
      return ingress.messages <= ingressMessageLimit && ingress.bytes <= ingressByteLimit;
    };
    const timer = setTimeout(() => closePolicy(client), authTimeoutMs);
    timer.unref?.();
    client.on("error", (error) => {
      clearTimeout(timer);
      logger?.warn?.("enrollment gateway client socket failed", { code: error?.code || "socket_error" });
      if (client.readyState === WebSocket.OPEN) client.close(1008, "invalid websocket frame");
      else if (client.readyState === WebSocket.CLOSING) {
        const forceClose = setTimeout(() => {
          try { client.terminate(); } catch {}
        }, 1000);
        forceClose.unref?.();
        client.once("close", () => clearTimeout(forceClose));
      }
    });
    client.once("message", async (data, isBinary) => {
      if (isBinary) {
        clearTimeout(timer);
        closePolicy(client);
        return;
      }
      let raw;
      let parsed;
      const rejectPipelinedFrame = () => closePolicy(client);
      client.on("message", rejectPipelinedFrame);
      try {
        raw = data.toString("utf8");
        if (!consumeIngress(Buffer.byteLength(raw, "utf8"))) fail("gateway_ingress_rate_limited");
        parsed = firstFrame(raw);
        if (parsed.type === "desktop_endpoint_register") {
          await registry.authorizeDesktopRegistration({ endpointId: parsed.endpoint_id,
            accessToken: parsed.access_token, desktopSecret: parsed.desktop_secret });
        } else {
          await registry.authorizeWebRegistration({ endpointId: parsed.endpoint_id, accessToken: parsed.access_token });
        }
      } catch (error) {
        clearTimeout(timer);
        logger?.warn?.("enrollment gateway rejected connection", { code: error?.code || "authorization_failed" });
        closePolicy(client, parsed?.request_id);
        return;
      }
      if (client.readyState !== WebSocket.OPEN) return;
      upstream = new WebSocket(upstreamUrl, {
        headers: {
          "x-pinvou-enrollment-gateway": "v1",
          "x-forwarded-for": request.enrollmentGatewaySource,
          ...(typeof request.headers.origin === "string" ? { origin: request.headers.origin } : {}),
        },
        maxPayload: maxPayloadBytes,
      });
      upstream.once("open", () => {
        clearTimeout(timer);
        authenticated = true;
        client.off("message", rejectPipelinedFrame);
        upstream.send(raw);
        client.on("message", (message, binary) => {
          const bytes = Buffer.isBuffer(message) ? message.length : Buffer.byteLength(String(message), "utf8");
          if (!consumeIngress(bytes)) {
            closePolicy(client);
            return;
          }
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
  listenerInspector = inspectLegacyListeners,
} = {}) {
  if (!Array.isArray(publicEntrypoints) || publicEntrypoints.length === 0) fail("gateway_public_entrypoints_invalid");
  let direct;
  try { direct = new URL(legacyRelayUrl); } catch { fail("gateway_upstream_invalid"); }
  if (!["ws:", "wss:"].includes(direct.protocol) || direct.username || direct.password
    || direct.hash || direct.search) fail("gateway_upstream_invalid");
  const host = direct.hostname.toLowerCase();
  if (!(host === "127.0.0.1" || host === "[::1]" || host === "::1")) fail("gateway_direct_bypass_exposed");
  if (!(legacyRelayBindHost === "127.0.0.1" || legacyRelayBindHost === "::1")) {
    fail("gateway_legacy_bind_not_loopback");
  }
  let listeners;
  try { listeners = await listenerInspector(direct); } catch { fail("gateway_listener_inspection_failed"); }
  if (!Array.isArray(listeners) || listeners.length === 0
    || listeners.some((address) => !["127.0.0.1", "::1"].includes(normalizeIp(address)))) {
    fail("gateway_legacy_listener_exposed");
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
    let body;
    try { body = await response.json(); } catch { fail("gateway_preflight_unverified"); }
    if (body?.enrollment_gateway !== "v1" || body?.legacy_v2_guarded !== true) fail("gateway_preflight_unverified");
    const wsUrl = new URL(`${url.pathname.replace(/\/$/u, "")}/ws`, url);
    wsUrl.protocol = wsUrl.protocol === "https:" ? "wss:" : "ws:";
    if (!(await webSocketProbe(wsUrl))) fail("gateway_websocket_route_unverified");
    checked.push(url.origin + url.pathname);
  }
  return Object.freeze({ safe_to_run_legacy_v2: true, checked_public_entrypoints: checked.sort() });
}

function splitListener(value) {
  const token = String(value || "").trim();
  const bracketed = /^\[([^\]]+)\]:(\d+)$/u.exec(token);
  if (bracketed) return { host: bracketed[1].split("%")[0], port: Number(bracketed[2]) };
  const separator = token.lastIndexOf(":");
  if (separator <= 0) return null;
  return { host: token.slice(0, separator).split("%")[0], port: Number(token.slice(separator + 1)) };
}

export async function inspectLegacyListeners(url, { platform = process.platform, exec = execFileAsync } = {}) {
  const port = Number(url.port || (url.protocol === "wss:" ? 443 : 80));
  let stdout;
  if (platform === "win32") {
    ({ stdout } = await exec("netstat", ["-ano", "-p", "tcp"], {
      windowsHide: true, timeout: 5000, maxBuffer: 1024 * 1024,
    }));
    return [...new Set(String(stdout).split(/\r?\n/u).map((line) => line.trim().split(/\s+/u))
      .filter((parts) => parts.length >= 4 && parts.at(-2)?.toUpperCase() === "LISTENING")
      .map((parts) => splitListener(parts[1]))
      .filter((entry) => entry?.port === port)
      .map((entry) => normalizeIp(entry.host)))];
  }
  ({ stdout } = await exec("ss", ["-H", "-ltn"], { timeout: 5000, maxBuffer: 1024 * 1024 }));
  return [...new Set(String(stdout).split(/\r?\n/u).map((line) => line.trim().split(/\s+/u))
    .map((parts) => splitListener(parts.at(-2)))
    .filter((entry) => entry?.port === port)
    .map((entry) => normalizeIp(entry.host)))];
}

function probeEnrollmentGateway(url) {
  return new Promise((resolve) => {
    const socket = new WebSocket(url, { handshakeTimeout: 5000 });
    let marked = false;
    let denied = false;
    let policyClosed = false;
    let settled = false;
    const finish = (value) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      try { socket.terminate(); } catch {}
      resolve(value);
    };
    const timer = setTimeout(() => {
      finish(false);
    }, 5500);
    socket.once("upgrade", (response) => {
      marked = response.headers["x-pinvou-enrollment-gateway"] === "v1";
    });
    socket.once("open", () => {
      const suffix = crypto.randomBytes(12).toString("hex");
      socket.send(JSON.stringify({
        v: 2,
        type: "desktop_endpoint_register",
        endpoint_id: `preflight-${suffix}`,
        access_token: `preflight-access-${suffix}-00000000`,
        desktop_secret: `preflight-secret-${suffix}-00000000`,
      }));
    });
    socket.on("message", (data) => {
      try {
        const message = JSON.parse(String(data));
        denied = message?.type === "error" && message?.code === "endpoint_not_found";
      } catch { finish(false); }
    });
    socket.once("close", (code) => {
      policyClosed = code === 1008;
      finish(marked && denied && policyClosed);
    });
    socket.once("unexpected-response", (_request, response) => {
      response.resume();
      finish(false);
    });
    socket.once("error", () => {
      finish(false);
    });
  });
}
