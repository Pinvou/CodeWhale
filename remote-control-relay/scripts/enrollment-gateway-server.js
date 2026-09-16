#!/usr/bin/env node
import { EnrollmentRegistry, loadEnrollmentRegistryConfig } from "../enrollment-registry.js";
import { createEnrollmentGateway } from "../enrollment-gateway.js";
import { RelayStateStore } from "../relay-state-store.js";

function port(value) {
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < 1024 || parsed > 65535) throw new Error("gateway_port_invalid");
  return parsed;
}

let store;
let gateway;
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
  });
  const address = await gateway.listen({ host, port: port(process.env.PINVOU_REMOTE_GATEWAY_PORT) });
  process.stdout.write(`${JSON.stringify({ ok: true, enrollment_gateway: "v1", host: address.address, port: address.port })}\n`);
} catch (error) {
  process.stderr.write(`${JSON.stringify({ ok: false, code: error?.code || error?.message || "gateway_start_failed" })}\n`);
  await store?.close().catch(() => {});
  process.exit(1);
}

async function shutdown() {
  await gateway?.close().catch(() => {});
  await store?.close().catch(() => {});
  process.exit(0);
}

process.once("SIGINT", shutdown);
process.once("SIGTERM", shutdown);
