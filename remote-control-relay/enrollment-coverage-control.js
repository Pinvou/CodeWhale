import crypto from "node:crypto";
import http from "node:http";
import { EnrollmentMigrationHandler } from "./enrollment-migration-handler.js";
import { ENROLLMENT_NAMESPACE, loadEnrollmentRegistryConfig } from "./enrollment-registry.js";
import { isEnrollmentEndpointIdentifier } from "./identifier-contract.js";

const REQUEST_SCHEMA = "pinvou-enrollment-coverage-request";
const RESPONSE_SCHEMA = "pinvou-enrollment-coverage-response";
const PATH = "/coverage";
const MAX_REQUEST_BYTES = 4 * 1024 * 1024;
const MAX_RESPONSE_BYTES = 16 * 1024;
const CLOCK_WINDOW_MS = 15_000;
const MAX_NONCES = 4096;
const SIGNATURE_HEADER = "x-pinvou-coverage-signature";
const HEX_64 = /^[a-f0-9]{64}$/u;

function fail(code) { throw Object.assign(new Error(code), { code }); }

function exact(value, keys) {
  return value && typeof value === "object" && !Array.isArray(value)
    && Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}

function mac(key, domain, bytes) {
  return crypto.createHmac("sha256", key).update(`pinvou-enrollment-coverage-${domain}-v1\0`, "ascii").update(bytes).digest("hex");
}

function verified(key, domain, bytes, signature) {
  return typeof signature === "string" && HEX_64.test(signature)
    && crypto.timingSafeEqual(Buffer.from(signature, "hex"), Buffer.from(mac(key, domain, bytes), "hex"));
}

export function loadEnrollmentCoverageControlConfig(env = process.env, { required = false } = {}) {
  const host = env.PINVOU_REMOTE_GATEWAY_CONTROL_HOST;
  const portText = env.PINVOU_REMOTE_GATEWAY_CONTROL_PORT;
  const keyText = env.PINVOU_REMOTE_GATEWAY_CONTROL_KEY;
  if (!host && !portText && !keyText) {
    if (required) fail("coverage_control_required");
    return null;
  }
  if (!["127.0.0.1", "::1"].includes(host)) fail("coverage_control_host_not_loopback");
  if (typeof portText !== "string" || !/^[1-9][0-9]{3,4}$/u.test(portText)) fail("coverage_control_port_invalid");
  const port = Number(portText);
  if (port < 1024 || port > 65535) fail("coverage_control_port_invalid");
  const key = Buffer.from(keyText || "", "utf8");
  if (key.length < 32 || key.length > 4096
    || keyText === env.PINVOU_REMOTE_ENROLLMENT_HMAC_KEY
    || keyText === env.PINVOU_REMOTE_PAIRING_HMAC_KEY) fail("coverage_control_key_invalid");
  return { host, port, key };
}

function storeBinding(env, key) {
  const config = loadEnrollmentRegistryConfig(env);
  // Bind the configured store, deployment policy and credential key material
  // without putting paths, keys or endpoint identities on the control wire.
  return mac(key, "store", Buffer.from(JSON.stringify({ config,
    key_id: env.PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID,
    key_fingerprint: mac(key, "credential-key", Buffer.from(env.PINVOU_REMOTE_ENROLLMENT_HMAC_KEY, "utf8")),
  }), "utf8"));
}

async function readBounded(stream, limit) {
  const chunks = [];
  let size = 0;
  for await (const chunk of stream) {
    size += chunk.length;
    if (size > limit) fail("coverage_message_oversized");
    chunks.push(Buffer.from(chunk));
  }
  return Buffer.concat(chunks);
}

export function createEnrollmentCoverageControl({ registry, stateStore, gatewayInstanceId, env = process.env, clock = Date.now } = {}) {
  if (typeof gatewayInstanceId !== "string" || !HEX_64.test(gatewayInstanceId)) fail("coverage_instance_invalid");
  const control = loadEnrollmentCoverageControlConfig(env, { required: true });
  const binding = storeBinding(env, control.key);
  const policy = stateStore.getPathPolicy();
  const config = loadEnrollmentRegistryConfig(env);
  if (policy.controller_state_path !== config.controller_state_path
    || policy.legacy_web_state_path !== config.legacy_web_state_path) fail("coverage_store_mismatch");
  const handler = new EnrollmentMigrationHandler({ registry });
  const nonces = new Map();
  const server = http.createServer(async (request, response) => {
    request.setTimeout(5000, () => request.destroy());
    const reply = (status, value) => {
      const bytes = Buffer.from(JSON.stringify(value), "utf8");
      response.writeHead(status, { "content-type": "application/json; charset=utf-8", "cache-control": "no-store",
        [SIGNATURE_HEADER]: mac(control.key, "response", bytes) });
      response.end(bytes);
    };
    try {
      if (request.method !== "POST" || request.url !== PATH
        || !["127.0.0.1", "::1", "::ffff:127.0.0.1"].includes(request.socket.remoteAddress)) {
        request.resume();
        reply(404, { ok: false, code: "coverage_request_invalid" });
        return;
      }
      const bytes = await readBounded(request, MAX_REQUEST_BYTES);
      if (!verified(control.key, "request", bytes, request.headers[SIGNATURE_HEADER])) {
        reply(403, { ok: false, code: "coverage_authorization_failed" });
        return;
      }
      let input;
      try { input = JSON.parse(bytes.toString("utf8")); } catch { fail("coverage_request_invalid"); }
      const now = clock();
      if (!exact(input, ["schema", "version", "nonce", "issued_at_ms", "store_binding", "endpoint_ids"])
        || input.schema !== REQUEST_SCHEMA || input.version !== 1 || typeof input.nonce !== "string" || !HEX_64.test(input.nonce)
        || !Number.isSafeInteger(now) || !Number.isSafeInteger(input.issued_at_ms)
        || Math.abs(now - input.issued_at_ms) > CLOCK_WINDOW_MS) fail("coverage_request_invalid");
      if (input.store_binding !== binding) fail("coverage_store_mismatch");
      if (!Array.isArray(input.endpoint_ids) || input.endpoint_ids.length > 100_000
        || input.endpoint_ids.some((id) => !isEnrollmentEndpointIdentifier(id))) fail("coverage_inventory_invalid");
      for (const [nonce, expires] of nonces) if (expires < now) nonces.delete(nonce);
      if (nonces.has(input.nonce)) fail("coverage_request_replayed");
      if (nonces.size >= MAX_NONCES) fail("coverage_capacity_reached");
      nonces.set(input.nonce, now + CLOCK_WINDOW_MS * 2);
      // No await between the metadata and the real registry read: both belong
      // to this live writer's same synchronous namespace snapshot.
      const revision = stateStore.readNamespaceRevision(ENROLLMENT_NAMESPACE);
      const coverage = handler.coverage(input.endpoint_ids);
      reply(200, { ok: true, schema: RESPONSE_SCHEMA, version: 1, request_nonce: input.nonce,
        store_binding: binding, gateway_instance_id: gatewayInstanceId, ...revision, checked_at_ms: now,
        coverage: { total: coverage.total, migrated: coverage.migrated, web_migrated: coverage.web_migrated,
          not_migrated: coverage.not_migrated, coverage_percent: coverage.coverage_percent } });
    } catch (error) {
      const allowed = new Set(["coverage_message_oversized", "coverage_request_invalid", "coverage_store_mismatch",
        "coverage_inventory_invalid", "coverage_request_replayed", "coverage_capacity_reached"]);
      if (!response.headersSent && !response.destroyed) reply(400,
        { ok: false, code: allowed.has(error?.code) ? error.code : "coverage_state_unavailable" });
    }
  });
  server.requestTimeout = 5000;
  server.headersTimeout = 5000;
  server.maxConnections = 16;
  return Object.freeze({
    listen() {
      return new Promise((resolve, reject) => {
        server.once("error", reject);
        server.listen(control.port, control.host, () => { server.off("error", reject); resolve(server.address()); });
      });
    },
    close() {
      server.closeAllConnections();
      return new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
    },
  });
}

export async function requestEnrollmentCoverage({ endpointIds, env = process.env, fetchImpl = fetch } = {}) {
  if (!Array.isArray(endpointIds) || endpointIds.length > 100_000
    || endpointIds.some((id) => !isEnrollmentEndpointIdentifier(id))) fail("coverage_inventory_invalid");
  const control = loadEnrollmentCoverageControlConfig(env, { required: true });
  const binding = storeBinding(env, control.key);
  const nonce = crypto.randomBytes(32).toString("hex");
  const bytes = Buffer.from(JSON.stringify({ schema: REQUEST_SCHEMA, version: 1, nonce,
    issued_at_ms: Date.now(), store_binding: binding, endpoint_ids: endpointIds }), "utf8");
  if (bytes.length > MAX_REQUEST_BYTES) fail("coverage_message_oversized");
  let response;
  try {
    const host = control.host === "::1" ? "[::1]" : control.host;
    response = await fetchImpl(`http://${host}:${control.port}${PATH}`, { method: "POST", redirect: "error",
      headers: { "content-type": "application/json", [SIGNATURE_HEADER]: mac(control.key, "request", bytes) },
      body: bytes, signal: AbortSignal.timeout(5000) });
  } catch { fail("coverage_control_unavailable"); }
  let contents;
  try { contents = await readBounded(response.body, MAX_RESPONSE_BYTES); } catch { fail("coverage_response_invalid"); }
  if (!verified(control.key, "response", contents, response.headers.get(SIGNATURE_HEADER))) fail("coverage_response_unverified");
  let output;
  try { output = JSON.parse(contents.toString("utf8")); } catch { fail("coverage_response_invalid"); }
  if (!response.ok || output?.ok !== true) fail("coverage_control_rejected");
  if (!exact(output, ["ok", "schema", "version", "request_nonce", "store_binding", "gateway_instance_id", "store_revision",
    "namespace_revision", "checked_at_ms", "coverage"]) || output.schema !== RESPONSE_SCHEMA || output.version !== 1
    || output.request_nonce !== nonce || output.store_binding !== binding
    || typeof output.gateway_instance_id !== "string" || !HEX_64.test(output.gateway_instance_id)
    || !Number.isSafeInteger(output.checked_at_ms) || Math.abs(Date.now() - output.checked_at_ms) > CLOCK_WINDOW_MS
    || typeof output.store_revision !== "string" || !/^(?:0|[1-9][0-9]{0,19})$/u.test(output.store_revision)
    || typeof output.namespace_revision !== "string" || !/^(?:0|[1-9][0-9]{0,19})$/u.test(output.namespace_revision)
    || !exact(output.coverage, ["total", "migrated", "web_migrated", "not_migrated", "coverage_percent"])) {
    fail("coverage_response_invalid");
  }
  const coverage = output.coverage;
  if (BigInt(output.store_revision) > 18_446_744_073_709_551_615n
    || BigInt(output.namespace_revision) > 18_446_744_073_709_551_615n
    || coverage.total !== new Set(endpointIds).size || !Number.isSafeInteger(coverage.total)
    || !Number.isSafeInteger(coverage.migrated) || coverage.migrated < 0 || coverage.migrated > coverage.total
    || !Number.isSafeInteger(coverage.web_migrated) || coverage.web_migrated < coverage.migrated
    || coverage.web_migrated > coverage.total || coverage.not_migrated !== coverage.total - coverage.migrated
    || coverage.coverage_percent !== (coverage.total === 0 ? 100 : Number(((coverage.migrated / coverage.total) * 100).toFixed(2)))) {
    fail("coverage_response_invalid");
  }
  return output;
}
