#!/usr/bin/env node
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { validateLegacyRollbackTopology } from "../enrollment-gateway.js";

export async function main(env = process.env, io = console) {
  try {
    const publicEntrypoints = JSON.parse(env.PINVOU_REMOTE_PUBLIC_ENTRYPOINTS_JSON || "[]");
    const result = await validateLegacyRollbackTopology({
      publicEntrypoints,
      legacyRelayUrl: env.PINVOU_REMOTE_LEGACY_RELAY_URL,
      legacyRelayBindHost: env.PINVOU_REMOTE_LEGACY_RELAY_BIND_HOST,
    });
    io.log(JSON.stringify({ ok: true, ...result }));
    return 0;
  } catch (error) {
    io.error(JSON.stringify({ ok: false, code: error?.code || "gateway_preflight_failed" }));
    return 1;
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1])) {
  process.exitCode = await main();
}
