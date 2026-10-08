#!/usr/bin/env node
import { isDirectCliEntry } from "../cli-entrypoint.js";
import { validateLegacyRollbackTopology } from "../enrollment-gateway.js";

export async function main(env = process.env, io = console) {
  try {
    const publicEntrypoints = JSON.parse(env.PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON || "[]");
    const result = await validateLegacyRollbackTopology({
      publicEntrypoints,
      legacyRelayUrl: env.PINVOU_REMOTE_LEGACY_RELAY_URL,
      legacyRelayBindHost: env.PINVOU_REMOTE_LEGACY_RELAY_BIND_HOST,
      expectedGatewayInstance: env.PINVOU_REMOTE_EXPECTED_GATEWAY_INSTANCE,
    });
    io.log(JSON.stringify({ ok: true, ...result }));
    return 0;
  } catch (error) {
    io.error(JSON.stringify({ ok: false, code: error?.code || "gateway_preflight_failed" }));
    return 1;
  }
}

if (isDirectCliEntry(import.meta.url)) {
  process.exitCode = await main();
}
