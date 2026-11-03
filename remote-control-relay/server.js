import http from "node:http";
import crypto from "node:crypto";
import { isExampleHmacKey } from "./hmac-key-config.js";
import { isIP } from "node:net";
import { chmod, mkdir, open, readdir, readFile, rename, rm, stat } from "node:fs/promises";
import { basename, dirname, extname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { WebSocketServer } from "ws";
import { createTelemetryService } from "./telemetry-service.js";
import { ControllerTelemetry } from "./controller-telemetry.js";
import { ControllerLeaseManager } from "./controller-lease.js";
import { ControllerResumeHooks } from "./controller-resume.js";
import { ControllerReservationManager, EndpointInteractionSlots } from "./controller-reservation.js";
import { ControllerRouter } from "./controller-router.js";
import { EnrollmentRegistry, loadEnrollmentRegistryConfig } from "./enrollment-registry.js";
import { PairingRegistrationHandler } from "./pairing-registration-handler.js";
import { PairingStore } from "./pairing-store.js";
import {
  CONTROLLER_MESSAGE_TYPES, CONTROLLER_SUBPROTOCOL,
  parseControllerWireJson, parseLegacyWebWireJson,
} from "./protocol-controller.js";
import { RelayStateStore } from "./relay-state-store.js";
import { isLegacyWebEndpointIdentifier, isRelaySigningKeyIdentifier } from "./identifier-contract.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const MIB = 1024 * 1024;
const RELAY_STATE_VERSION = 1;
// Caps header- and config-derived IP text before parsing: the cap keeps
// garbage from reaching the parser at all. OS-reported peer addresses (short
// interface names included) stay far below it. Declared before the top-level
// parseIpSet calls, which run normalizeIp at module init.
const MAX_IP_TEXT_CHARS = 128;
// Config parsing runs at module init: an escaping throw would die with a
// stack that prints repo paths. Every configuration failure reports one
// fixed [relay-error] line and exits 1, like every other startup failure.
function configOrExit(parse) {
  try {
    return parse();
  } catch (error) {
    console.error(`[relay-error] ${error.message}`);
    process.exit(1);
  }
}

// listen() validates the port synchronously, so an invalid value would die
// with a RangeError whose stack prints the module path instead of the fixed
// startup report.
function parsePort(value) {
  const port = Number(value);
  if (!Number.isInteger(port) || port < 1 || port > 65_535) {
    throw new Error("invalid PORT: use an integer from 1 to 65535");
  }
  return port;
}

const PORT = configOrExit(() => parsePort(process.env.PORT || 8787));
const HOST = process.env.PINVOU_REMOTE_HOST || "0.0.0.0";
const PUBLIC_BASE_PATH = normalizeBasePath(
  process.env.PINVOU_REMOTE_PUBLIC_BASE_PATH || "/pinvou3/remote",
);
const WEB_ROOT = resolve(process.env.PINVOU_REMOTE_WEB_DIR || resolve(__dirname, "web", "dist"));
// safeWebPath maps the site root to exactly this path.
const WEB_INDEX_PATH = resolve(WEB_ROOT, "index.html");
// A non-numeric value used to reach setInterval as NaN, which Node clamps to
// a 1 ms tick; bound the input like every other numeric env instead. The
// heartbeat also drives reservation/lease/router expiry, so only the parsing
// is bounded here — the scheduling semantics are unchanged.
const HEARTBEAT_INTERVAL_MS = boundedInteger(
  process.env.HEARTBEAT_INTERVAL_MS,
  15_000,
  5_000,
  24 * 60 * 60_000,
);
// The v2 RPC contract permits up to 1 MiB of JSON args. Base64-heavy requests
// and normal envelope overhead need a Relay ceiling comfortably above that.
const MAX_PAYLOAD_BYTES = boundedInteger(
  process.env.MAX_PAYLOAD_BYTES,
  4 * MIB,
  4 * MIB,
  16 * MIB,
);
const MAX_ENDPOINTS = boundedInteger(
  process.env.MAX_ENDPOINTS ?? process.env.MAX_ROOMS,
  2000,
  1,
  100_000,
);
const ENDPOINT_CREATE_LIMIT = boundedInteger(
  process.env.ENDPOINT_CREATE_LIMIT ?? process.env.ROOM_CREATE_LIMIT,
  20,
  1,
  10_000,
);
const ENDPOINT_CREATE_WINDOW_MS = boundedInteger(
  process.env.ENDPOINT_CREATE_WINDOW_MS ?? process.env.ROOM_CREATE_WINDOW_MS,
  60_000,
  1000,
  60 * 60_000,
);
const MAX_WS_CONNECTIONS = boundedInteger(
  process.env.MAX_WS_CONNECTIONS,
  MAX_ENDPOINTS * 2 + 1000,
  2,
  250_000,
);
const WS_CONNECT_LIMIT = boundedInteger(process.env.WS_CONNECT_LIMIT, 120, 1, 100_000);
const WS_CONNECT_WINDOW_MS = boundedInteger(
  process.env.WS_CONNECT_WINDOW_MS,
  60_000,
  1000,
  60 * 60_000,
);
const WS_AUTH_TIMEOUT_MS = boundedInteger(
  process.env.WS_AUTH_TIMEOUT_MS,
  10_000,
  1000,
  60_000,
);
const ENDPOINT_OFFLINE_TTL_MS = boundedInteger(
  process.env.ENDPOINT_OFFLINE_TTL_MS,
  24 * 60 * 60_000,
  1000,
  365 * 24 * 60 * 60_000,
);
const WS_INGRESS_WINDOW_MS = boundedInteger(
  process.env.WS_INGRESS_WINDOW_MS,
  60_000,
  1000,
  60 * 60_000,
);
const WS_INGRESS_MESSAGE_LIMIT = boundedInteger(
  process.env.WS_INGRESS_MESSAGE_LIMIT,
  12_000,
  1,
  1_000_000,
);
const WS_INGRESS_BYTE_LIMIT = boundedInteger(
  process.env.WS_INGRESS_BYTE_LIMIT,
  512 * MIB,
  1024,
  16 * 1024 * MIB,
);
// Never configure the outbound high-water mark below one legal inbound frame:
// doing so would accept a request/response and then terminate its destination
// solely because of contradictory limits.
const WS_MAX_BUFFERED_BYTES = Math.max(
  MAX_PAYLOAD_BYTES,
  boundedInteger(
    process.env.WS_MAX_BUFFERED_BYTES,
    8 * MIB,
    512,
    256 * MIB,
  ),
);
const MAX_RELAY_STATE_BYTES = boundedInteger(
  process.env.MAX_RELAY_STATE_BYTES,
  16 * MIB,
  1024,
  256 * MIB,
);
// A tombstone serializes to well under 256 bytes even with the longest legal
// endpoint id, so clamping the configured capacity to what the state limit
// can hold keeps revocation persistence possible however the two env values
// combine; relayStateContents stays the exact backstop. The state limit's own
// floor (1024) makes the derivation's floor 4, so no extra guard below.
const MAX_REVOKED_ENDPOINTS = Math.min(
  boundedInteger(process.env.MAX_REVOKED_ENDPOINTS, 100_000, 1, 1_000_000),
  Math.floor(MAX_RELAY_STATE_BYTES / 256),
);
const RELAY_STATE_PATH = resolve(
  process.env.PINVOU_REMOTE_STATE_PATH || resolve(__dirname, "data", "relay-state.json"),
);
const ALLOWED_WEB_ORIGINS = configOrExit(
  () => parseOriginSet(process.env.PINVOU_REMOTE_ALLOWED_WEB_ORIGINS),
);
const ALLOWED_PROXY_IPS = configOrExit(() => parseIpSet(
  "PINVOU_REMOTE_ALLOWED_PROXY_IPS",
  process.env.PINVOU_REMOTE_ALLOWED_PROXY_IPS,
));
const TRUSTED_PROXY_IPS = configOrExit(() => parseIpSet(
  "PINVOU_REMOTE_TRUSTED_PROXY_IPS",
  process.env.PINVOU_REMOTE_TRUSTED_PROXY_IPS
    || process.env.PINVOU_REMOTE_ALLOWED_PROXY_IPS,
));
const DESKTOP_CONTROLLER_ENABLED = process.env.PINVOU_DESKTOP_CONTROLLER_ENABLED === "1";
const CONTROLLER_TYPES = new Set(CONTROLLER_MESSAGE_TYPES);

const WEB_TO_DESKTOP_TYPES = new Set([
  "rpc_request",
  "event_subscribe",
  "event_unsubscribe",
  "client_ready",
]);
const DESKTOP_TO_WEB_TYPES = new Set([
  "rpc_response",
  "event",
  "stream_reset",
  "desktop_snapshot",
]);
const MIME_TYPES = new Map([
  [".css", "text/css; charset=utf-8"],
  [".gif", "image/gif"],
  [".html", "text/html; charset=utf-8"],
  [".ico", "image/x-icon"],
  [".jpeg", "image/jpeg"],
  [".jpg", "image/jpeg"],
  [".js", "text/javascript; charset=utf-8"],
  [".json", "application/json; charset=utf-8"],
  [".map", "application/json; charset=utf-8"],
  [".mjs", "text/javascript; charset=utf-8"],
  [".png", "image/png"],
  [".svg", "image/svg+xml"],
  [".txt", "text/plain; charset=utf-8"],
  [".wasm", "application/wasm"],
  [".webp", "image/webp"],
  [".woff", "font/woff"],
  [".woff2", "font/woff2"],
]);

const endpoints = new Map();
// Unique endpoint reservations cover the async authorization/persistence gap.
// Multiple sockets for the same identity share one capacity slot.
const pendingEndpointCreations = new Map();
const revokedEndpoints = new Map();
const endpointCreationBuckets = new Map();
const wsConnectionBuckets = new Map();
const controllerTelemetry = new ControllerTelemetry({
  auditSink(record) {
    // `record` has already crossed the fixed controller telemetry allowlist.
    // Never merge caller patches or protocol values into this diagnostic.
    // High-volume encrypted chunks remain aggregate-only; lifecycle changes
    // and all failures retain a bounded structured diagnostic.
    if (record.outcome !== "ok"
      || ["register", "claim", "handshake", "lease", "resume"].includes(record.stage)) {
      console.info(`[controller-audit] ${JSON.stringify(record)}`);
    }
  },
});
const telemetry = createTelemetryService({
  controllerMetrics: () => controllerTelemetry.snapshot(),
});
let relayStateMutation = Promise.resolve();
let controllerStateStore = null;

await loadRelayState();

// A signal during a revocation write can strand a relay-state.json.<pid>.<hex>.tmp
// sibling; the rename design means none of them is ever the live state, so
// last boot's leftovers are swept before this process writes anything. The
// sweep matches only names the writer itself produces (basename, pid, hex
// nonce, .tmp) so a sibling state path that merely extends this basename —
// state.json versus state.json.backup — keeps its own writer's temp files.
// Failures are ignored on purpose: persistence renames over the live path
// regardless, and a sweep that cannot run must not block startup.
try {
  const stateDirectory = dirname(RELAY_STATE_PATH);
  const prefix = `${basename(RELAY_STATE_PATH)}.`;
  const writerTemp = new RegExp(`^\\d+\\.[0-9a-f]+\\.tmp$`);
  for (const entry of await readdir(stateDirectory)) {
    if (entry.startsWith(prefix) && writerTemp.test(entry.slice(prefix.length))) {
      await rm(join(stateDirectory, entry), { force: true }).catch(() => {});
    }
  }
} catch {}

const controllerRuntime = DESKTOP_CONTROLLER_ENABLED ? await createControllerRuntime() : null;

async function createControllerRuntime() {
  const config = loadEnrollmentRegistryConfig(process.env);
  // Official proof issuance and the live claim dispatcher are not installed in
  // this wave. Never advertise an enabled production controller without them.
  if (config.deployment === "official") throw new Error("official_controller_claim_dispatcher_unavailable");
  controllerStateStore = await RelayStateStore.open({
    statePath: config.controller_state_path,
    legacyStatePath: config.legacy_web_state_path,
  });
  try {
    const registry = EnrollmentRegistry.open({ stateStore: controllerStateStore, config });
    const digestKeyId = process.env.PINVOU_REMOTE_PAIRING_HMAC_KEY_ID || "";
    const digestKey = process.env.PINVOU_REMOTE_PAIRING_HMAC_KEY || "";
    if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/u.test(digestKeyId)
      || Buffer.byteLength(digestKey, "utf8") < 32 || Buffer.byteLength(digestKey, "utf8") > 4096
      || isExampleHmacKey(digestKey)) {
      throw new Error("pairing_digest_key_invalid");
    }
    const signingKeyId = process.env.PINVOU_REMOTE_RELAY_SIGNING_KEY_ID || "";
    if (!isRelaySigningKeyIdentifier(signingKeyId)) {
      throw new Error("relay_signing_key_id_invalid");
    }
    const signingKeyB64 = process.env.PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64 || "";
    const signingKeyDer = Buffer.from(signingKeyB64, "base64");
    if (signingKeyDer.length === 0 || signingKeyDer.toString("base64") !== signingKeyB64) {
      throw new Error("relay_signing_key_invalid");
    }
    const signingKey = crypto.createPrivateKey({ key: signingKeyDer, format: "der", type: "pkcs8" });
    if (signingKey.asymmetricKeyType !== "ed25519") throw new Error("relay_signing_key_invalid");
    const pairingStore = await PairingStore.open({
      stateStore: controllerStateStore,
      digestKeyring: {
        activeKeyId: digestKeyId,
        keys: [{ keyId: digestKeyId, key: digestKey }],
      },
    });
    await pairingStore.invalidateForRelayRestart();
    const relayEpoch = `relay-${crypto.randomBytes(18).toString("base64url")}`;
    const notifyReplacementCode = async ({ endpoint_id }) => {
      const endpoint = endpoints.get(endpoint_id);
      send(endpoint?.desktop, {
        v: 1,
        type: "error",
        request_id: `request-${crypto.randomBytes(12).toString("base64url")}`,
        code: "pairing_code_invalid",
        message: "连接码无效或已失效",
      });
    };
    const slots = new EndpointInteractionSlots();
    const controllerAudit = (event, latencyMs) => controllerTelemetry.audit(event, latencyMs);
    const reservations = new ControllerReservationManager({
      pairingStore,
      interactionSlots: slots,
      onPairingInvalidated: notifyReplacementCode,
      audit: controllerAudit,
    });
    const registration = new PairingRegistrationHandler({
      pairingStore,
      relayEpoch,
      authorizeEndpoint: async ({ endpointId, context }) => {
        if (context?.endpointId !== endpointId || typeof context?.credential !== "string") {
          throw new Error("pairing_endpoint_unauthorized");
        }
        await registry.authorizePairingRegistration({
          endpointId,
          desktopSecret: context.credential,
        });
      },
    });
    let router;
    const leases = new ControllerLeaseManager({
      pairingStore,
      reservationManager: reservations,
      relayEpoch,
      signingKeyId,
      signTranscript: (preimage) => crypto.sign(null, preimage, signingKey),
      resolveControlledEndpoint: (endpointId) => router?.resolveControlledEndpoint(endpointId),
      onLeaseTerminated: notifyReplacementCode,
      audit: controllerAudit,
    });
    const resumeHooks = new ControllerResumeHooks({ leaseManager: leases });
    router = new ControllerRouter({
      pairingRegistrationHandler: registration,
      leaseManager: leases,
      send,
      resumeHooks,
      audit: controllerAudit,
    });
    return Object.freeze({ pairingStore, registry, relayEpoch, reservations, leases, router,
      selfRegistration: config.self_registration });
  } catch (error) {
    await controllerStateStore.close().catch(() => {});
    controllerStateStore = null;
    throw error;
  }
}

function send(ws, value) {
  if (!ws || ws.readyState !== ws.OPEN || ws.policyClosing) return false;
  const encoded = JSON.stringify(value);
  const bytes = Buffer.byteLength(encoded);
  if (bytes > WS_MAX_BUFFERED_BYTES
    || ws.bufferedAmount + bytes > WS_MAX_BUFFERED_BYTES) {
    ws.policyClosing = true;
    clearTimeout(ws.authTimer);
    ws.authTimer = null;
    try { ws.terminate(); } catch {}
    return false;
  }
  try {
    ws.send(encoded, (error) => {
      if (!error) return;
      ws.policyClosing = true;
      try { ws.terminate(); } catch {}
    });
    return true;
  } catch {
    ws.policyClosing = true;
    try { ws.terminate(); } catch {}
    return false;
  }
}

// A policy close waits for the peer's close handshake so queued frames (an
// error message, a revocation or replacement notice) are delivered. A peer
// that withholds the handshake would otherwise keep the socket draining —
// receiving frames outside ingress accounting — until ws's own 30 s close
// timeout, so enforce a shorter bound.
const POLICY_CLOSE_DRAIN_MS = 5000;

function closeSocket(ws, code = 1000, reason = "") {
  if (!ws) return;
  ws.policyClosing = true;
  try { ws.close(code, String(reason).slice(0, 123)); } catch {}
  const drain = setTimeout(() => {
    try { ws.terminate(); } catch {}
  }, POLICY_CLOSE_DRAIN_MS);
  drain.unref();
  ws.once("close", () => clearTimeout(drain));
}

function boundedInteger(raw, fallback, min, max) {
  const value = Number(raw);
  if (!Number.isFinite(value)) return fallback;
  return Math.min(max, Math.max(min, Math.floor(value)));
}

// Expands an IPv6 address to eight lowercase zero-padded groups, reducing an
// IPv4-mapped address to its embedded dotted quad. Returns null when invalid.
function expandIpv6(address) {
  let left = address;
  let right = "";
  let compressed = false;
  if (address.includes("::")) {
    const halves = address.split("::");
    if (halves.length > 2) return null;
    [left, right] = halves;
    compressed = true;
  }
  // A stray colon outside the "::" marker (":1:2", "1::2::3" already handled
  // above) is invalid, as is an empty compression side like "1:::2".
  if ((left !== "" && (left.startsWith(":") || left.endsWith(":")))
    || (right !== "" && (right.startsWith(":") || right.endsWith(":")))) {
    return null;
  }
  const leftGroups = left === "" ? [] : left.split(":");
  const groups = leftGroups.concat(right === "" ? [] : right.split(":"));
  // An embedded IPv4 tail stands for the final two groups and is only valid
  // as the literal tail of the address: "1.2.3.4::" would otherwise place
  // the converted groups ahead of the compression fill and canonicalize
  // text that isIP rejects into an unrelated routable address.
  if (groups.length && groups[groups.length - 1].includes(".")) {
    const tail = groups.pop();
    if (isIP(tail) !== 4 || !address.endsWith(tail)) return null;
    const [o1, o2, o3, o4] = tail.split(".").map(Number);
    groups.push(
      (o1 * 256 + o2).toString(16).padStart(4, "0"),
      (o3 * 256 + o4).toString(16).padStart(4, "0"),
    );
  }
  if (groups.some((group) => !/^[0-9A-Fa-f]{1,4}$/.test(group))) return null;
  if (compressed) {
    if (groups.length > 7) return null;
    groups.splice(leftGroups.length, 0, ...Array(8 - groups.length).fill("0"));
  } else if (groups.length !== 8) {
    return null;
  }
  const full = groups.map((group) => group.toLowerCase().padStart(4, "0"));
  // IPv4-mapped spellings name an IPv4 peer, so they reduce to that address
  // regardless of how much of ::ffff:0:0/96 was written out.
  if (full.slice(0, 5).every((group) => group === "0000") && full[5] === "ffff") {
    const high = parseInt(full[6], 16);
    const low = parseInt(full[7], 16);
    return `${high >> 8}.${high & 255}.${low >> 8}.${low & 255}`;
  }
  return full.join(":");
}

// Configuration entries and attributed addresses funnel through this one
// canonical form — lowercase, IPv6 expanded to eight zero-padded groups,
// IPv4-mapped spellings reduced to the embedded dotted quad — so any valid
// spelling of the same address matches. Zone-id suffixes are not supported:
// any "%" is rejected outright instead of being matched verbatim.
function normalizeIp(value) {
  if (typeof value !== "string" || value.length > MAX_IP_TEXT_CHARS) return "";
  const text = value.trim();
  if (text === "" || text.includes("%")) return "";
  const address = text.toLowerCase();
  if (address.includes(":")) {
    const expanded = expandIpv6(address);
    return expanded === null ? "" : expanded;
  }
  return isIP(address) === 4 ? address : "";
}

function parseIpSet(name, value) {
  const raw = String(value || "");
  const result = new Set();
  for (const item of raw.split(",")) {
    if (!item.trim()) continue;
    const ip = normalizeIp(item);
    // Fail closed: a typo must shrink the trust set to nothing at startup,
    // never silently open the proxy allowlist to every client.
    if (!ip) throw new Error(`proxy_ip_configuration_invalid: ${name}`);
    result.add(ip);
  }
  // A value with visible content that yields no entries (e.g. ",,,") is a
  // broken edit, not an intentional empty allowlist; only a blank value means
  // "empty set".
  if (result.size === 0 && raw.trim()) {
    throw new Error(`proxy_ip_configuration_invalid: ${name}`);
  }
  return result;
}

function parseOriginSet(value) {
  const origins = new Set();
  for (const item of String(value || "").split(",").map((part) => part.trim()).filter(Boolean)) {
    let parsed;
    try { parsed = new URL(item); } catch {
      throw new Error("invalid PINVOU_REMOTE_ALLOWED_WEB_ORIGINS entry");
    }
    if (!/^https?:$/.test(parsed.protocol) || parsed.origin === "null") {
      throw new Error("invalid browser WebSocket origin");
    }
    origins.add(parsed.origin.toLowerCase());
  }
  return origins;
}

function browserOriginAllowed(req) {
  const raw = req.headers.origin;
  // Native desktop clients do not send Origin and remain eligible. This is a
  // browser CSWSH defense, not a replacement for the deployment enrollment gate.
  if (raw === undefined) return true;
  if (typeof raw !== "string" || raw.includes(",")) return false;
  let parsed;
  try { parsed = new URL(raw); } catch { return false; }
  if (!/^https?:$/.test(parsed.protocol) || parsed.origin === "null") return false;
  const origin = parsed.origin.toLowerCase();
  if (ALLOWED_WEB_ORIGINS.has(origin)) return true;
  const requestHost = String(req.headers.host || "").trim().toLowerCase();
  return Boolean(requestHost) && parsed.host.toLowerCase() === requestHost;
}

// Canonical loopback spellings. peerIp is normalizeIp's canonical form — an
// IPv6 peer arrives expanded to eight zero-padded groups — so comparing raw
// literals here would make the "::1" branch unreachable and silently drop
// the documented always-admitted guarantee for ::1 peers the moment a
// non-empty allowlist makes that guarantee observable.
const LOOPBACK_IPS = new Set([normalizeIp("127.0.0.1"), normalizeIp("::1")]);

function isLoopback(ip) {
  return LOOPBACK_IPS.has(ip);
}

function peerIp(req) {
  return normalizeIp(req.socket?.remoteAddress);
}

function sourceAllowed(req) {
  if (ALLOWED_PROXY_IPS.size === 0) return true;
  const peer = peerIp(req);
  return isLoopback(peer) || ALLOWED_PROXY_IPS.has(peer);
}

function clientIp(req) {
  const peer = peerIp(req);
  if (!TRUSTED_PROXY_IPS.has(peer)) return peer || "unknown";
  const header = req.headers["x-forwarded-for"];
  // Only the rightmost proxy-appended address is authoritative. Never skip a
  // malformed final item and fall back to an attacker-controlled earlier item.
  if (typeof header === "string" && header.length <= 4096) {
    return normalizeIp(header.split(",").at(-1)) || peer || "unknown";
  }
  return (header === undefined ? normalizeIp(req.headers["x-real-ip"]) : "") || peer || "unknown";
}

function consumeRateLimit(buckets, ip, limit, windowMs, now = Date.now()) {
  const bucket = buckets.get(ip);
  if (!bucket || now - bucket.started_at >= windowMs) {
    buckets.set(ip, { started_at: now, count: 1 });
    return true;
  }
  if (bucket.count >= limit) return false;
  bucket.count += 1;
  return true;
}

function rawMessageBytes(raw) {
  if (typeof raw === "string") return Buffer.byteLength(raw);
  if (raw && Number.isSafeInteger(raw.byteLength)) return raw.byteLength;
  if (raw && Number.isSafeInteger(raw.length)) return raw.length;
  return Buffer.byteLength(String(raw || ""));
}

function consumeIngress(ws, bytes, now = Date.now()) {
  if (!ws.ingressBucket || now - ws.ingressBucket.started_at >= WS_INGRESS_WINDOW_MS) {
    ws.ingressBucket = { started_at: now, messages: 0, bytes: 0 };
  }
  const nextMessages = ws.ingressBucket.messages + 1;
  const nextBytes = ws.ingressBucket.bytes + bytes;
  if (nextMessages > WS_INGRESS_MESSAGE_LIMIT || nextBytes > WS_INGRESS_BYTE_LIMIT) {
    return false;
  }
  ws.ingressBucket.messages = nextMessages;
  ws.ingressBucket.bytes = nextBytes;
  return true;
}

function rejectSocket(ws, code, message) {
  send(ws, { v: 2, type: "error", code, message });
  closeSocket(ws, 1008, message);
}

function rejectUpgrade(socket, status, message) {
  const body = `${message}\n`;
  socket.write(
    `HTTP/1.1 ${status}\r\n`
    + "Connection: close\r\n"
    + "Content-Type: text/plain; charset=utf-8\r\n"
    + `Content-Length: ${Buffer.byteLength(body)}\r\n`
    + "\r\n"
    + body,
  );
  socket.destroy();
}

function armAuthTimer(ws) {
  clearTimeout(ws.authTimer);
  ws.authTimer = setTimeout(() => {
    if (ws.role === "unknown" && !ws.controllerAuthenticated) {
      if (ws.protocol === CONTROLLER_SUBPROTOCOL) {
        controllerTelemetry.audit("controller_socket_auth_timeout");
      }
      rejectSocket(ws, "authentication_timeout", "websocket authentication timeout");
    }
  }, WS_AUTH_TIMEOUT_MS);
  ws.authTimer.unref?.();
}

function authenticateSocket(ws, role, endpointId, leaseId = null) {
  clearTimeout(ws.authTimer);
  ws.authTimer = null;
  ws.role = role;
  ws.endpointId = endpointId;
  ws.leaseId = leaseId;
}

function normalizeBasePath(value) {
  let raw = String(value || "").trim();
  if (!raw) return "";
  try {
    if (/^https?:\/\//i.test(raw)) raw = new URL(raw).pathname;
  } catch {}
  raw = raw.replace(/\/+$/, "");
  if (!raw || raw === "/") return "";
  return raw.startsWith("/") ? raw : `/${raw}`;
}

function isWithinPublicBasePath(pathname) {
  if (!PUBLIC_BASE_PATH) return true;
  return pathname === PUBLIC_BASE_PATH || pathname.startsWith(`${PUBLIC_BASE_PATH}/`);
}

function stripPublicBasePath(pathname) {
  if (!PUBLIC_BASE_PATH) return pathname;
  if (pathname === PUBLIC_BASE_PATH) return "/";
  if (pathname.startsWith(`${PUBLIC_BASE_PATH}/`)) {
    return pathname.slice(PUBLIC_BASE_PATH.length) || "/";
  }
  return pathname;
}

function tokenHash(token) {
  return crypto.createHash("sha256").update(String(token || "")).digest("hex");
}

function tokenHashMatches(token, expectedHash) {
  if (!expectedHash) return false;
  const actual = Buffer.from(tokenHash(token), "hex");
  const expected = Buffer.from(expectedHash, "hex");
  return actual.length === expected.length && crypto.timingSafeEqual(actual, expected);
}

function boundedCredential(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 1024
    ? value
    : null;
}

function hasLegacyTokenAlias(message) {
  return Object.hasOwn(message, "token") || Object.hasOwn(message, "pairing_token");
}

function validEndpointId(value) {
  return isLegacyWebEndpointIdentifier(value);
}

function relayStateContents(entries) {
  const revoked = [...entries]
    .map(([endpointId, revokedAt]) => ({
      endpoint_id: endpointId,
      revoked_at: revokedAt,
    }))
    .sort((left, right) => left.endpoint_id.localeCompare(right.endpoint_id));
  const contents = `${JSON.stringify({
    version: RELAY_STATE_VERSION,
    revoked_endpoints: revoked,
  }, null, 2)}\n`;
  if (Buffer.byteLength(contents) > MAX_RELAY_STATE_BYTES) {
    throw new Error("relay state exceeds MAX_RELAY_STATE_BYTES");
  }
  return contents;
}

async function loadRelayState() {
  let contents;
  try {
    const metadata = await stat(RELAY_STATE_PATH);
    if (!metadata.isFile()) throw new Error("relay state path is not a regular file");
    if (metadata.size > MAX_RELAY_STATE_BYTES) {
      throw new Error("relay state exceeds MAX_RELAY_STATE_BYTES");
    }
    contents = await readFile(RELAY_STATE_PATH, "utf8");
    if (Buffer.byteLength(contents) > MAX_RELAY_STATE_BYTES) {
      throw new Error("relay state exceeds MAX_RELAY_STATE_BYTES");
    }
  } catch (error) {
    if (error?.code === "ENOENT") return;
    throw new Error("relay_state_load_failed", { cause: error });
  }

  let state;
  try { state = JSON.parse(contents); } catch (error) {
    throw new Error("failed to load relay state: invalid JSON", { cause: error });
  }
  // The size limit above is the real bound on a persisted file. The
  // MAX_REVOKED_ENDPOINTS clamp only governs new revocations: an older relay
  // could legitimately persist more tombstones than today's derivation
  // allows, and refusing to load them would brick startup on a valid file.
  if (!state || typeof state !== "object" || Array.isArray(state)
    || state.version !== RELAY_STATE_VERSION
    || !Array.isArray(state.revoked_endpoints)) {
    throw new Error("failed to load relay state: unsupported or malformed state");
  }

  for (const entry of state.revoked_endpoints) {
    const revokedAt = typeof entry?.revoked_at === "string" ? entry.revoked_at : "";
    if (!entry || typeof entry !== "object" || Array.isArray(entry)
      || !validEndpointId(entry.endpoint_id)
      || !Number.isFinite(Date.parse(revokedAt))
      || revokedEndpoints.has(entry.endpoint_id)) {
      throw new Error("failed to load relay state: malformed revoked endpoint");
    }
    revokedEndpoints.set(entry.endpoint_id, revokedAt);
  }
}

async function syncDirectory(path) {
  let directory;
  try {
    directory = await open(path, "r");
    await directory.sync();
  } catch (error) {
    // Directory fsync is not supported on every platform. The file itself is
    // always fsynced before rename; supported hosts get the stronger guarantee.
    if (!["EACCES", "EINVAL", "EISDIR", "ENOTSUP", "EPERM"].includes(error?.code)) {
      throw error;
    }
  } finally {
    try { await directory?.close(); } catch {}
  }
}

async function atomicWriteRelayState(nextState) {
  const contents = relayStateContents(nextState);
  const stateDirectory = dirname(RELAY_STATE_PATH);
  await mkdir(stateDirectory, { recursive: true });
  const temporaryPath = `${RELAY_STATE_PATH}.${process.pid}.${crypto.randomBytes(8).toString("hex")}.tmp`;
  let temporaryFile;
  try {
    temporaryFile = await open(temporaryPath, "wx", 0o600);
    await temporaryFile.writeFile(contents, "utf8");
    await temporaryFile.sync();
    await temporaryFile.close();
    temporaryFile = null;
    await rename(temporaryPath, RELAY_STATE_PATH);
    try { await chmod(RELAY_STATE_PATH, 0o600); } catch {}
    await syncDirectory(stateDirectory);
  } catch (error) {
    try { await temporaryFile?.close(); } catch {}
    try { await rm(temporaryPath, { force: true }); } catch {}
    throw error;
  }
}

function persistRevocation(endpointId) {
  const operation = relayStateMutation.then(async () => {
    if (revokedEndpoints.has(endpointId)) return revokedEndpoints.get(endpointId);
    if (revokedEndpoints.size >= MAX_REVOKED_ENDPOINTS) {
      // Not a persistence failure: nothing was attempted. The code lets the
      // caller reply with the distinct capacity rejection instead of the
      // [relay-error] persistence log.
      const error = new Error("revoked endpoint capacity reached");
      error.code = "revoked_endpoint_capacity_reached";
      throw error;
    }
    const revokedAt = new Date().toISOString();
    const nextState = new Map(revokedEndpoints);
    nextState.set(endpointId, revokedAt);
    await atomicWriteRelayState(nextState);
    revokedEndpoints.set(endpointId, revokedAt);
    return revokedAt;
  });
  relayStateMutation = operation.catch(() => {});
  return operation;
}

function randomLeaseId() {
  return `lease_${crypto.randomBytes(18).toString("base64url")}`;
}

function withoutCredentials(message) {
  const {
    access_token: _accessToken,
    token: _token,
    pairing_token: _pairingToken,
    desktop_secret: _desktopSecret,
    ...safe
  } = message;
  return safe;
}

function socketOpen(ws) {
  return Boolean(ws && ws.readyState === ws.OPEN);
}

function desktopStatus(endpoint, status) {
  send(endpoint.web, {
    v: 2,
    type: "desktop_connection_state",
    endpoint_id: endpoint.endpoint_id,
    lease_id: endpoint.lease_id,
    status,
  });
}

function notifyDesktopOfWeb(endpoint) {
  if (!socketOpen(endpoint.desktop) || !socketOpen(endpoint.web)) return;
  send(endpoint.desktop, {
    v: 2,
    type: "web_client_connected",
    endpoint_id: endpoint.endpoint_id,
    lease_id: endpoint.lease_id,
    stream_epoch: endpoint.web.streamEpoch || null,
    after_seq: endpoint.web.afterSeq,
  });
}

function teardownEndpoint(endpoint, reason, closeReason, requester = null) {
  if (!endpoint || endpoints.get(endpoint.endpoint_id) !== endpoint) return;
  controllerRuntime?.reservations.releaseWeb({ endpointId: endpoint.endpoint_id, leaseId: endpoint.lease_id });
  endpoints.delete(endpoint.endpoint_id);
  if (requester) {
    send(requester, {
      v: 2,
      type: "desktop_endpoint_revoked",
      endpoint_id: endpoint.endpoint_id,
      reason,
    });
  }
  send(endpoint.web, {
    v: 2,
    type: "endpoint_revoked",
    endpoint_id: endpoint.endpoint_id,
    reason,
  });
  if (endpoint.desktop !== requester) {
    send(endpoint.desktop, {
      v: 2,
      type: "desktop_endpoint_revoked",
      endpoint_id: endpoint.endpoint_id,
      reason,
    });
  }
  closeSocket(endpoint.web, 4003, closeReason);
  if (endpoint.desktop !== requester) closeSocket(endpoint.desktop, 4003, closeReason);
  endpoint.web = null;
  endpoint.desktop = null;
  endpoint.lease_id = null;
}

function revokeEndpoint(endpoint, reason = "revoked", requester = null) {
  teardownEndpoint(endpoint, reason, "endpoint revoked", requester);
}

function expireEndpoint(endpoint) {
  if (!endpoint || endpoint.revoking) return;
  teardownEndpoint(endpoint, "desktop_offline_ttl_expired", "endpoint expired");
}

function pruneExpiredEndpoints(now = Date.now()) {
  for (const endpoint of endpoints.values()) {
    if (endpoint.revoking || socketOpen(endpoint.desktop)
      || !Number.isFinite(endpoint.desktop_offline_since)) continue;
    if (now - endpoint.desktop_offline_since >= ENDPOINT_OFFLINE_TTL_MS) {
      expireEndpoint(endpoint);
    }
  }
}

function healthSummary() {
  pruneExpiredEndpoints();
  const values = [...endpoints.values()];
  return {
    ok: true,
    desktop_controller_enabled: Boolean(controllerRuntime),
    endpoint_count: values.length,
    // Keep the aggregate-only key consumed by the current deployment probe.
    room_count: values.length,
    connected_endpoint_count: values.filter((endpoint) => socketOpen(endpoint.desktop)).length,
    desktop_open_count: values.filter((endpoint) => socketOpen(endpoint.desktop)).length,
    desktop_offline_count: values.filter((endpoint) => !socketOpen(endpoint.desktop)).length,
    web_client_open_count: values.filter((endpoint) => socketOpen(endpoint.web)).length,
    ws_connection_count: wss.clients.size,
    unauthenticated_connection_count: [...wss.clients]
      .filter((ws) => ws.role === "unknown").length,
  };
}

function webHeaders(contentType, cacheControl) {
  return {
    "content-type": contentType,
    "cache-control": cacheControl,
    "content-security-policy": "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' blob:; connect-src 'self' ws: wss:; object-src 'none'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'",
    "cross-origin-opener-policy": "same-origin",
    "referrer-policy": "no-referrer",
    "x-content-type-options": "nosniff",
    "x-frame-options": "DENY",
  };
}

function safeWebPath(routePath) {
  let decoded;
  try { decoded = decodeURIComponent(routePath); } catch { return null; }
  if (decoded.includes("\0")) return null;
  const requested = decoded === "/" ? "index.html" : decoded.replace(/^\/+/, "");
  const candidate = resolve(WEB_ROOT, requested);
  const relation = relative(WEB_ROOT, candidate);
  if (relation.startsWith("..") || relation.includes(":") || relation.startsWith("/")) return null;
  return candidate;
}

async function regularFile(path) {
  try { return (await stat(path)).isFile(); } catch { return false; }
}

async function serveWebUi(req, res, routePath) {
  if (req.method !== "GET" && req.method !== "HEAD") {
    res.writeHead(405, { allow: "GET, HEAD" });
    res.end();
    return;
  }

  let path = safeWebPath(routePath);
  if (!path) {
    res.writeHead(400, { "content-type": "text/plain; charset=utf-8" });
    res.end("bad path");
    return;
  }
  if (!(await regularFile(path))) {
    // A missing asset is an ordinary 404 (a stale reference to a replaced
    // build); a missing site entry means the build itself is unavailable.
    if (path !== WEB_INDEX_PATH && extname(path)) {
      res.writeHead(404, { "content-type": "text/plain; charset=utf-8" });
      res.end("not found");
      return;
    }
    path = WEB_INDEX_PATH;
  }
  if (!(await regularFile(path))) {
    res.writeHead(503, { "content-type": "text/plain; charset=utf-8", "cache-control": "no-store" });
    res.end("WebUI build is not available");
    return;
  }

  const extension = extname(path).toLowerCase();
  const webRelativePath = relative(WEB_ROOT, path).replaceAll("\\", "/");
  const contentHashedAsset = /^assets\/(?:.*\/)?[^/]+-[A-Za-z0-9_-]{8,}\.[^/]+$/.test(webRelativePath);
  const cacheControl = extension === ".html"
    ? "no-store"
    : contentHashedAsset
      ? "public, max-age=31536000, immutable"
      : "no-cache";
  let body;
  // The file can vanish or become unreadable between the stat above and this
  // read (a redeploy in progress); that must not take the relay down. HEAD
  // reads too, so its status can never disagree with the GET that follows it.
  try {
    body = await readFile(path);
  } catch {
    res.writeHead(503, { "content-type": "text/plain; charset=utf-8", "cache-control": "no-store" });
    res.end("WebUI build is not available");
    return;
  }
  res.writeHead(200, webHeaders(MIME_TYPES.get(extension) || "application/octet-stream", cacheControl));
  res.end(req.method === "HEAD" ? undefined : body);
}

const server = http.createServer(async (req, res) => {
  if (!sourceAllowed(req)) {
    res.writeHead(403, { "content-type": "text/plain; charset=utf-8" });
    res.end("forbidden");
    return;
  }
  let url;
  try {
    url = new URL(req.url || "/", `http://${req.headers.host || "127.0.0.1"}`);
  } catch {
    // A hostile Host header must not crash the relay; the fixed reply never
    // echoes the offending value.
    res.writeHead(400, { "content-type": "text/plain; charset=utf-8" });
    res.end("bad request");
    return;
  }
  const routePath = stripPublicBasePath(url.pathname);
  if (await telemetry.handleHttp(req, res, routePath, { clientIp: clientIp(req) })) return;
  if (isWithinPublicBasePath(url.pathname) && routePath === "/healthz") {
    res.writeHead(200, {
      "content-type": "application/json; charset=utf-8",
      "cache-control": "no-store",
    });
    res.end(JSON.stringify(healthSummary()));
    return;
  }
  if (isWithinPublicBasePath(url.pathname)) {
    await serveWebUi(req, res, routePath);
    return;
  }
  res.writeHead(404, { "content-type": "text/plain; charset=utf-8" });
  res.end("not found");
});

const wss = new WebSocketServer({
  noServer: true,
  maxPayload: MAX_PAYLOAD_BYTES,
  handleProtocols: (protocols) => protocols.has(CONTROLLER_SUBPROTOCOL)
    ? CONTROLLER_SUBPROTOCOL : false,
});

server.on("upgrade", (req, socket, head) => {
  const controllerUpgrade = String(req.headers["sec-websocket-protocol"] || "")
    .split(",")
    .some((value) => value.trim() === CONTROLLER_SUBPROTOCOL);
  if (!sourceAllowed(req)) {
    socket.write("HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n");
    socket.destroy();
    return;
  }
  let url;
  try {
    url = new URL(req.url || "/", `http://${req.headers.host || "127.0.0.1"}`);
  } catch {
    rejectUpgrade(socket, "400 Bad Request", "malformed host header");
    return;
  }
  if (!isWithinPublicBasePath(url.pathname) || stripPublicBasePath(url.pathname) !== "/ws") {
    socket.destroy();
    return;
  }
  if (!browserOriginAllowed(req)) {
    rejectUpgrade(socket, "403 Forbidden", "websocket origin forbidden");
    return;
  }
  if (wss.clients.size >= MAX_WS_CONNECTIONS) {
    if (controllerUpgrade) controllerTelemetry.audit("controller_socket_capacity_rejected");
    rejectUpgrade(socket, "503 Service Unavailable", "websocket capacity reached");
    return;
  }
  if (!consumeRateLimit(
    wsConnectionBuckets,
    clientIp(req),
    WS_CONNECT_LIMIT,
    WS_CONNECT_WINDOW_MS,
  )) {
    if (controllerUpgrade) controllerTelemetry.audit("controller_socket_rate_limited");
    rejectUpgrade(socket, "429 Too Many Requests", "websocket connection rate limited");
    return;
  }
  wss.handleUpgrade(req, socket, head, (ws) => {
    wss.emit("connection", ws, req);
  });
});

async function registerDesktop(ws, msg) {
  const endpointId = msg.endpoint_id;
  const accessToken = boundedCredential(msg.access_token);
  const desktopSecret = boundedCredential(msg.desktop_secret);
  if (ws.role !== "unknown" || msg.v !== 2 || !validEndpointId(endpointId)
    || !accessToken || !desktopSecret
    || (controllerRuntime?.selfRegistration && ws.registrationPending)) {
    rejectSocket(ws, "bad_desktop_endpoint_register", "bad desktop endpoint registration");
    return;
  }

  // Apply cheap admission before an anonymous first registration can persist.
  // Recheck capacity/revocation after the async registry operation as well.
  let creationAdmitted = false;
  let releaseCreation;
  if (controllerRuntime?.selfRegistration && !endpoints.has(endpointId)) {
    pruneExpiredEndpoints();
    if (revokedEndpoints.has(endpointId)) {
      rejectSocket(ws, "endpoint_revoked", "endpoint was permanently revoked");
      return;
    }
    const reserved = [...pendingEndpointCreations.keys()].filter(id => !endpoints.has(id)).length;
    if (!pendingEndpointCreations.has(endpointId) && endpoints.size + reserved >= MAX_ENDPOINTS) {
      rejectSocket(ws, "endpoint_capacity_reached", "endpoint capacity reached");
      return;
    }
    if (!consumeRateLimit(endpointCreationBuckets, ws.clientIp, ENDPOINT_CREATE_LIMIT, ENDPOINT_CREATE_WINDOW_MS)) {
      rejectSocket(ws, "endpoint_creation_rate_limited", "endpoint creation rate limited");
      return;
    }
    creationAdmitted = true;
    pendingEndpointCreations.set(endpointId, (pendingEndpointCreations.get(endpointId) || 0) + 1);
    releaseCreation = () => {
      const remaining = pendingEndpointCreations.get(endpointId) - 1;
      if (remaining === 0) pendingEndpointCreations.delete(endpointId);
      else pendingEndpointCreations.set(endpointId, remaining);
    };
  }
  try {
    await finishDesktopRegistration(ws, { endpointId, accessToken, desktopSecret, creationAdmitted });
  } finally {
    releaseCreation?.();
  }
}

async function finishDesktopRegistration(ws, { endpointId, accessToken, desktopSecret, creationAdmitted }) {
  if (controllerRuntime) {
    if (controllerRuntime.selfRegistration) ws.registrationPending = true;
    try {
      await controllerRuntime.registry.authorizeDesktopRegistration({
        endpointId,
        accessToken,
        desktopSecret,
        admitSelfRegistration: controllerRuntime.selfRegistration,
      });
    } catch (error) {
      rejectSocket(ws, error?.code || "enrollment_required", "desktop endpoint enrollment required");
      return;
    } finally {
      if (controllerRuntime.selfRegistration) ws.registrationPending = false;
    }
  }

  // Authentication may yield: another registration can claim this socket, or
  // its close handler can run, before the validated credentials return.
  if (!socketOpen(ws)) return;
  if (ws.role !== "unknown") {
    rejectSocket(ws, "bad_desktop_endpoint_register", "bad desktop endpoint registration");
    return;
  }

  pruneExpiredEndpoints();
  if (revokedEndpoints.has(endpointId)) {
    rejectSocket(ws, "endpoint_revoked", "endpoint was permanently revoked");
    return;
  }

  let endpoint = endpoints.get(endpointId);
  if (!endpoint) {
    if (endpoints.size >= MAX_ENDPOINTS) {
      rejectSocket(ws, "endpoint_capacity_reached", "endpoint capacity reached");
      return;
    }
    if (!creationAdmitted && !consumeRateLimit(
      endpointCreationBuckets,
      ws.clientIp,
      ENDPOINT_CREATE_LIMIT,
      ENDPOINT_CREATE_WINDOW_MS,
    )) {
      rejectSocket(ws, "endpoint_creation_rate_limited", "endpoint creation rate limited");
      return;
    }
    endpoint = {
      endpoint_id: endpointId,
      access_token_hash: tokenHash(accessToken),
      desktop_secret_hash: tokenHash(desktopSecret),
      desktop: null,
      web: null,
      lease_id: null,
      desktop_offline_since: null,
      revoking: false,
    };
    endpoints.set(endpointId, endpoint);
  } else {
    if (!tokenHashMatches(desktopSecret, endpoint.desktop_secret_hash)) {
      rejectSocket(ws, "invalid_desktop_secret", "invalid desktop secret");
      return;
    }
    if (!tokenHashMatches(accessToken, endpoint.access_token_hash)) {
      rejectSocket(ws, "invalid_access_token", "invalid access token");
      return;
    }
    if (socketOpen(endpoint.desktop) && endpoint.desktop !== ws) {
      send(endpoint.desktop, {
        v: 2,
        type: "desktop_endpoint_replaced",
        endpoint_id: endpointId,
      });
      closeSocket(endpoint.desktop, 4001, "desktop endpoint replaced");
    }
  }

  endpoint.desktop = ws;
  endpoint.desktop_offline_since = null;
  authenticateSocket(ws, "desktop", endpointId);
    if (controllerRuntime && ws.protocol === CONTROLLER_SUBPROTOCOL) {
    controllerRuntime.router.authenticateControlled({
      socketIdentity: ws.controllerSocketIdentity,
      endpointId,
      authenticatedContext: Object.freeze({ endpointId, credential: desktopSecret }),
    });
  }
  send(ws, {
    v: 2,
    type: "desktop_endpoint_registered",
    endpoint_id: endpointId,
    web_client_connected: socketOpen(endpoint.web),
    lease_id: socketOpen(endpoint.web) ? endpoint.lease_id : null,
  });
  if (socketOpen(endpoint.web)) {
    desktopStatus(endpoint, "connected");
    notifyDesktopOfWeb(endpoint);
  }
}

function joinWebClient(ws, msg) {
  const endpointId = msg.endpoint_id;
  const accessToken = boundedCredential(msg.access_token);
  if (ws.role !== "unknown" || msg.v !== 2 || !validEndpointId(endpointId) || !accessToken) {
    rejectSocket(ws, "bad_web_client_join", "bad web client join");
    return;
  }
  pruneExpiredEndpoints();
  const endpoint = endpoints.get(endpointId);
  if (!endpoint) {
    rejectSocket(ws, "endpoint_not_found", "endpoint not found");
    return;
  }
  if (!tokenHashMatches(accessToken, endpoint.access_token_hash)) {
    rejectSocket(ws, "invalid_token", "invalid token");
    return;
  }

  const leaseId = randomLeaseId();
  if (controllerRuntime) {
    try {
      controllerRuntime.reservations.acquireWeb({ endpointId, leaseId });
    } catch {
      rejectSocket(ws, "endpoint_not_found", "endpoint not found");
      return;
    }
  }
  const previousWeb = socketOpen(endpoint.web) ? endpoint.web : null;
  if (previousWeb) {
    send(previousWeb, {
      v: 2,
      type: "endpoint_replaced",
      endpoint_id: endpointId,
      lease_id: previousWeb.leaseId,
    });
  }

  ws.streamEpoch = typeof msg.stream_epoch === "string" ? msg.stream_epoch.slice(0, 256) : null;
  ws.afterSeq = Math.max(0, Number.isSafeInteger(msg.after_seq) ? msg.after_seq : 0);
  endpoint.web = ws;
  endpoint.lease_id = leaseId;
  authenticateSocket(ws, "web", endpointId, leaseId);
  send(ws, {
    v: 2,
    type: "web_client_joined",
    endpoint_id: endpointId,
    lease_id: leaseId,
    desktop_connected: socketOpen(endpoint.desktop),
  });
  notifyDesktopOfWeb(endpoint);

  if (previousWeb) closeSocket(previousWeb, 4001, "endpoint replaced");
}

async function revokeFromSocket(ws, msg) {
  const endpointId = msg.endpoint_id || ws.endpointId;
  const desktopSecret = boundedCredential(msg.desktop_secret);
  const endpoint = validEndpointId(endpointId) ? endpoints.get(endpointId) : null;
  if (!endpoint) {
    rejectSocket(ws, "endpoint_not_found", "endpoint not found");
    return;
  }
  if ((ws.role !== "desktop" || ws.endpointId !== endpointId)
    && !tokenHashMatches(desktopSecret, endpoint.desktop_secret_hash)) {
    rejectSocket(ws, "invalid_desktop_secret", "invalid desktop secret");
    return;
  }
  if (desktopSecret && !tokenHashMatches(desktopSecret, endpoint.desktop_secret_hash)) {
    rejectSocket(ws, "invalid_desktop_secret", "invalid desktop secret");
    return;
  }
  if (endpoint.revoking) {
    rejectSocket(ws, "revocation_in_progress", "endpoint revocation is already in progress");
    return;
  }
  endpoint.revoking = true;
  ws.revokePending = true;
  clearTimeout(ws.authTimer);
  ws.authTimer = null;
  try {
    await persistRevocation(endpointId);
  } catch (error) {
    endpoint.revoking = false;
    ws.revokePending = false;
    if (error?.code === "revoked_endpoint_capacity_reached") {
      // An expected operational condition, not a relay error: the tombstone
      // write was never attempted, and the endpoint stays registered.
      rejectSocket(ws, "revoked_endpoint_capacity_reached", "revoked endpoint capacity reached");
      return;
    }
    controllerTelemetry.audit("revocation_persist_failed");
    console.error("[relay-error] revocation_persist_failed");
    rejectSocket(ws, "revoke_persistence_failed", "failed to persist endpoint revocation");
    return;
  }
  revokeEndpoint(endpoint, msg.reason || "revoked", ws);
  // Only a socket of the revoked endpoint goes down with it. A requester
  // revoking a different endpoint with that endpoint's secret — an
  // authenticated desktop/web socket of another endpoint, or an
  // authenticated controller — keeps its own session; teardownEndpoint
  // already told it desktop_endpoint_revoked.
  if (ws.endpointId !== endpointId) {
    ws.revokePending = false;
    // The revoke cleared the authentication timer. A cross-endpoint revoker
    // that never authenticated keeps its socket, so the timer must be
    // re-armed to keep unauthenticated connections bounded like every other;
    // an authenticated socket legitimately has none.
    if (ws.role === "unknown" && !ws.controllerAuthenticated) armAuthTimer(ws);
    return;
  }
  closeSocket(ws, 1000, "endpoint revoked");
}

function forwardAuthenticatedMessage(ws, msg) {
  const endpoint = endpoints.get(ws.endpointId);
  if (!endpoint) {
    rejectSocket(ws, "endpoint_not_found", "endpoint not found");
    return;
  }

  if (ws.role === "web") {
    if (endpoint.web !== ws || endpoint.lease_id !== ws.leaseId) {
      rejectSocket(ws, "stale_lease", "stale web client lease");
      return;
    }
    if (!WEB_TO_DESKTOP_TYPES.has(msg.type)) {
      send(ws, {
        v: 2,
        type: "error",
        code: "unsupported_message_type",
        message: `unsupported web message type: ${String(msg.type || "")}`,
      });
      return;
    }
    if (!socketOpen(endpoint.desktop)) {
      send(ws, {
        v: 2,
        type: "error",
        code: "desktop_offline",
        message: "desktop is offline",
      });
      return;
    }
    send(endpoint.desktop, {
      ...withoutCredentials(msg),
      v: 2,
      endpoint_id: endpoint.endpoint_id,
      lease_id: endpoint.lease_id,
    });
    return;
  }

  if (ws.role === "desktop") {
    if (endpoint.desktop !== ws) {
      rejectSocket(ws, "desktop_endpoint_replaced", "desktop endpoint was replaced");
      return;
    }
    if (!DESKTOP_TO_WEB_TYPES.has(msg.type)) {
      send(ws, {
        v: 2,
        type: "error",
        code: "unsupported_message_type",
        message: `unsupported desktop message type: ${String(msg.type || "")}`,
      });
      return;
    }
    if (!socketOpen(endpoint.web)) return;
    if (msg.lease_id && msg.lease_id !== endpoint.lease_id) {
      return;
    }
    send(endpoint.web, {
      ...withoutCredentials(msg),
      v: 2,
      endpoint_id: endpoint.endpoint_id,
      lease_id: endpoint.lease_id,
    });
  }
}

wss.on("connection", (ws, req) => {
  ws.role = "unknown";
  ws.endpointId = null;
  ws.leaseId = null;
  ws.clientIp = clientIp(req);
  ws.isAlive = true;
  ws.policyClosing = false;
  ws.revokePending = false;
  ws.controllerAuthenticated = false;
  ws.controllerSocketIdentity = `socket-${crypto.randomBytes(18).toString("base64url")}`;
  // ws emits "error" on the socket for protocol violations (e.g. a frame over
  // maxPayload). Without a listener, one unauthenticated frame would crash the
  // whole relay process. Attached before any lane-specific setup so the
  // controller and legacy lanes — and the early-reject path below — are all
  // covered.
  ws.on("error", (error) => {
    // ws closes the offending socket itself; the fixed code keeps contained
    // protocol violations visible without echoing frame content or any
    // session or path detail.
    console.error(`[relay-error] websocket_protocol_violation code=${error?.code ?? "unknown"}`);
    try { ws.terminate(); } catch {}
  });
  if (ws.protocol === CONTROLLER_SUBPROTOCOL) {
    try {
      controllerTelemetry.audit("controller_socket_opened");
      controllerRuntime?.router.addSocket({
        socketIdentity: ws.controllerSocketIdentity,
        source: ws.clientIp,
        socket: ws,
      });
    } catch {
      rejectSocket(ws, "connection_initialization_failed", "websocket initialization failed");
      return;
    }
  }
  ws.ingressBucket = { started_at: Date.now(), messages: 0, bytes: 0 };
  armAuthTimer(ws);
  ws.on("pong", () => { ws.isAlive = true; });

  ws.on("message", (raw, isBinary) => {
    if (ws.policyClosing || ws.revokePending) return;
    if (!consumeIngress(ws, rawMessageBytes(raw))) {
      if (ws.protocol === CONTROLLER_SUBPROTOCOL) {
        controllerTelemetry.audit("controller_socket_rate_limited");
      }
      rejectSocket(ws, "ingress_rate_limited", "websocket ingress rate limit exceeded");
      return;
    }
    if (isBinary) {
      send(ws, { v: 2, type: "error", code: "binary_unsupported", message: "binary messages are unsupported" });
      return;
    }
    let msg;
    const rawText = String(raw);
    try {
      // The negotiated controller lane sees strict JSON at every depth. A
      // WebUI v2 socket preserves legacy duplicate-field handling except for
      // duplicate version keys, which could cross the protocol boundary.
      msg = ws.protocol === CONTROLLER_SUBPROTOCOL
        ? parseControllerWireJson(rawText)
        : parseLegacyWebWireJson(rawText);
    } catch {
      send(ws, { v: 2, type: "error", code: "bad_json", message: "bad json" });
      return;
    }
    if (!msg || typeof msg !== "object" || Array.isArray(msg)) {
      send(ws, { v: 2, type: "error", code: "bad_message", message: "bad message" });
      return;
    }
    if (msg.v === 1 && ws.protocol !== CONTROLLER_SUBPROTOCOL) {
      rejectSocket(ws, "unsupported_protocol_version", "protocol version 2 is required");
      return;
    }
    if (msg.v === 1 && CONTROLLER_TYPES.has(msg.type) && !controllerRuntime) {
      rejectSocket(
        ws,
        "desktop_controller_unavailable",
        "desktop controller is unavailable",
      );
      return;
    }
    if (msg.v === 1 && ws.protocol === CONTROLLER_SUBPROTOCOL
      && controllerRuntime && CONTROLLER_TYPES.has(msg.type)) {
      void controllerRuntime.router.handle({
        socketIdentity: ws.controllerSocketIdentity,
        message: msg,
      }).then(() => {
        if (msg.type === "controller_pairing_claim"
          || (typeof msg.type === "string" && msg.type.startsWith("controller_"))) {
          ws.controllerAuthenticated = true;
          clearTimeout(ws.authTimer);
          ws.authTimer = null;
        }
      }).catch(() => {});
      return;
    }
    if (msg.v !== 2) {
      rejectSocket(
        ws,
        "unsupported_protocol_version",
        "protocol version 2 is required",
      );
      return;
    }
    if (ws.protocol === CONTROLLER_SUBPROTOCOL
      && msg.type !== "desktop_endpoint_register"
      && msg.type !== "desktop_endpoint_revoke"
      && ws.role !== "desktop") {
      rejectSocket(ws, "authentication_required", "authenticate desktop endpoint first");
      return;
    }
    if (hasLegacyTokenAlias(msg)) {
      rejectSocket(
        ws,
        "legacy_token_alias_unsupported",
        "legacy token aliases are unsupported",
      );
      return;
    }

    if (msg.type === "desktop_endpoint_register") {
      void registerDesktop(ws, msg).catch(() => {
        rejectSocket(ws, "desktop_registration_failed", "desktop endpoint registration failed");
      });
      return;
    }
    if (msg.type === "web_client_join") {
      joinWebClient(ws, msg);
      return;
    }
    if (msg.type === "desktop_endpoint_revoke") {
      void revokeFromSocket(ws, msg).catch(() => {
        // Defensive: every reachable failure path inside revokeFromSocket
        // already resets these flags. Reset them here too so an unexpected
        // throw can never leave the socket ignoring messages or the endpoint
        // unable to expire.
        ws.revokePending = false;
        const failedEndpoint = endpoints.get(msg.endpoint_id || ws.endpointId);
        if (failedEndpoint) failedEndpoint.revoking = false;
        console.error("[relay-error] revocation_unexpected_failure");
        rejectSocket(ws, "revoke_failed", "endpoint revocation failed");
      });
      return;
    }
    if (ws.role === "unknown") {
      rejectSocket(ws, "authentication_required", "authenticate first");
      return;
    }
    forwardAuthenticatedMessage(ws, msg);
  });

  ws.on("close", () => {
    clearTimeout(ws.authTimer);
    ws.authTimer = null;
    if (controllerRuntime && ws.protocol === CONTROLLER_SUBPROTOCOL) {
      void controllerRuntime.router.removeSocket(ws.controllerSocketIdentity).catch(() => {});
    }
    const endpoint = endpoints.get(ws.endpointId);
    if (!endpoint) return;
    if (ws.role === "web" && endpoint.web === ws) {
      const leaseId = endpoint.lease_id;
      endpoint.web = null;
      endpoint.lease_id = null;
      controllerRuntime?.reservations.releaseWeb({ endpointId: endpoint.endpoint_id, leaseId });
      send(endpoint.desktop, {
        v: 2,
        type: "web_client_disconnected",
        endpoint_id: endpoint.endpoint_id,
        lease_id: leaseId,
      });
    }
    if (ws.role === "desktop" && endpoint.desktop === ws) {
      endpoint.desktop = null;
      endpoint.desktop_offline_since = Date.now();
      desktopStatus(endpoint, "offline");
    }
  });
});

const heartbeatTimer = setInterval(() => {
  pruneExpiredEndpoints();
  if (controllerRuntime) void controllerRuntime.reservations.expire().catch(() => {});
  if (controllerRuntime) void controllerRuntime.leases.expire().catch(() => {});
  if (controllerRuntime) void controllerRuntime.router.expire().catch(() => {});
  const endpointBucketExpiry = Date.now() - ENDPOINT_CREATE_WINDOW_MS;
  for (const [ip, bucket] of endpointCreationBuckets) {
    if (bucket.started_at <= endpointBucketExpiry) endpointCreationBuckets.delete(ip);
  }
  const wsBucketExpiry = Date.now() - WS_CONNECT_WINDOW_MS;
  for (const [ip, bucket] of wsConnectionBuckets) {
    if (bucket.started_at <= wsBucketExpiry) wsConnectionBuckets.delete(ip);
  }
  for (const ws of wss.clients) {
    if (ws.isAlive === false) {
      ws.terminate();
      continue;
    }
    ws.isAlive = false;
    try { ws.ping(); } catch { ws.terminate(); }
  }
}, HEARTBEAT_INTERVAL_MS);
heartbeatTimer.unref();

server.on("error", (error) => {
  // An unhandled "error" event here would crash with a stack that prints the
  // bind address and module paths.
  console.error(`[relay-error] listen_failed${error?.code ? ` code=${error.code}` : ""}`);
  process.exit(1);
});

server.listen(PORT, HOST, () => {
  console.log(
    "pinvou remote relay listening"
    + ` (protocol=${controllerRuntime ? "v2+controller-v1" : "v2"}, max_endpoints=${MAX_ENDPOINTS}`
    + `, max_ws_connections=${MAX_WS_CONNECTIONS}`
    + `, ws_connect_limit=${WS_CONNECT_LIMIT}/${WS_CONNECT_WINDOW_MS}ms`
    + `, ws_auth_timeout=${WS_AUTH_TIMEOUT_MS}ms`
    + `, ws_ingress_limit=${WS_INGRESS_MESSAGE_LIMIT}msg/${WS_INGRESS_BYTE_LIMIT}B/${WS_INGRESS_WINDOW_MS}ms`
    + `, ws_buffer_high_water=${WS_MAX_BUFFERED_BYTES}B`
    + `, endpoint_create_limit=${ENDPOINT_CREATE_LIMIT}/${ENDPOINT_CREATE_WINDOW_MS}ms`
    + `, endpoint_offline_ttl=${ENDPOINT_OFFLINE_TTL_MS}ms`
    + `, revoked_endpoints=${revokedEndpoints.size}`
    + `, max_payload=${MAX_PAYLOAD_BYTES})`,
  );
});

let shuttingDown = false;
async function shutdown() {
  if (shuttingDown) return;
  shuttingDown = true;
  clearInterval(heartbeatTimer);
  for (const ws of wss.clients) {
    try { ws.terminate(); } catch {}
  }
  try { server.close(); } catch {}
  await controllerStateStore?.close().catch(() => {});
  process.exit(0);
}

process.once("SIGINT", shutdown);
process.once("SIGTERM", shutdown);
