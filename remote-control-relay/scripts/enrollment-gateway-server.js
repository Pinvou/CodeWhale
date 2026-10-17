#!/usr/bin/env node
import { EnrollmentRegistry, loadEnrollmentRegistryConfig } from "../enrollment-registry.js";
import { createEnrollmentGateway } from "../enrollment-gateway.js";
import { RelayStateStore } from "../relay-state-store.js";
import { createEnrollmentCoverageControl, loadEnrollmentCoverageControlConfig } from "../enrollment-coverage-control.js";

function port(value) {
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < 1024 || parsed > 65535) throw new Error("gateway_port_invalid");
  return parsed;
}

function csv(value) {
  return String(value || "").split(",").map((item) => item.trim()).filter(Boolean);
}

function optionalInteger(value) {
  if (value === undefined || value === "") return undefined;
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed)) throw new Error("gateway_limit_invalid");
  return parsed;
}

let store;
let gateway;
let coverageControl;
try {
  const host = process.env.PINVOU_REMOTE_GATEWAY_HOST;
  if (!(host === "127.0.0.1" || host === "::1")) throw new Error("gateway_host_not_loopback");
  const config = loadEnrollmentRegistryConfig(process.env);
  store = await RelayStateStore.open({
    statePath: config.controller_state_path,
    legacyStatePath: config.legacy_web_state_path,
  });
  const registry = EnrollmentRegistry.open({ stateStore: store, config });
  gateway = createEnrollmentGateway({
    registry,
    upstreamUrl: process.env.PINVOU_REMOTE_LEGACY_RELAY_URL,
    basePath: process.env.PINVOU_REMOTE_PUBLIC_BASE_PATH || "/pinvou3/remote",
    allowedWebOrigins: csv(process.env.PINVOU_REMOTE_ALLOWED_WEB_ORIGINS),
    trustedProxyIps: csv(process.env.PINVOU_REMOTE_GATEWAY_TRUSTED_PROXY_IPS),
    maxPayloadBytes: optionalInteger(process.env.MAX_PAYLOAD_BYTES),
    maxConnections: optionalInteger(process.env.MAX_WS_CONNECTIONS),
    maxSourceBuckets: optionalInteger(process.env.PINVOU_ENROLLMENT_GATEWAY_MAX_SOURCE_BUCKETS),
    connectionLimit: optionalInteger(process.env.WS_CONNECT_LIMIT),
    connectionWindowMs: optionalInteger(process.env.WS_CONNECT_WINDOW_MS),
    ingressMessageLimit: optionalInteger(process.env.WS_INGRESS_MESSAGE_LIMIT),
    ingressByteLimit: optionalInteger(process.env.WS_INGRESS_BYTE_LIMIT),
  });
  const address = await gateway.listen({ host, port: port(process.env.PINVOU_REMOTE_GATEWAY_PORT) });
  if (loadEnrollmentCoverageControlConfig(process.env)) {
    coverageControl = createEnrollmentCoverageControl({ registry, stateStore: store, gatewayInstanceId: gateway.instanceId });
    await coverageControl.listen();
  }
  process.stdout.write(`${JSON.stringify({ ok: true, enrollment_gateway: "v1", host: address.address, port: address.port })}\n`);
} catch (error) {
  process.stderr.write(`${JSON.stringify({ ok: false, code: error?.code || error?.message || "gateway_start_failed" })}\n`);
  await coverageControl?.close().catch(() => {});
  await gateway?.close().catch(() => {});
  await store?.close().catch(() => {});
  process.exit(1);
}

async function shutdown() {
  await coverageControl?.close().catch(() => {});
  await gateway?.close().catch(() => {});
  await store?.close().catch(() => {});
  process.exit(0);
}

process.once("SIGINT", shutdown);
process.once("SIGTERM", shutdown);
