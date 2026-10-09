#!/usr/bin/env node
import { spawn } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { requestEnrollmentCoverage } from "../remote-control-relay/enrollment-coverage-control.js";
import { isDirectCliEntry } from "../remote-control-relay/cli-entrypoint.js";
import { readEnrollmentInventory } from "../remote-control-relay/scripts/enrollment-migrate.js";

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const relay = join(root, "remote-control-relay");

function run(script, args = [], envOverrides = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [join(relay, "scripts", script), ...args], {
      cwd: root,
      env: { ...process.env, ...envOverrides },
      shell: false,
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
    });
    let stdout = "";
    let stderr = "";
    const timer = setTimeout(() => {
      child.kill();
      reject(Object.assign(new Error("rollback_check_timeout"), { code: "rollback_check_timeout" }));
    }, 60_000);
    const capture = (target, chunk) => {
      if (Buffer.byteLength(stdout) + Buffer.byteLength(stderr) + chunk.length > 256 * 1024) {
        clearTimeout(timer);
        child.kill();
        reject(Object.assign(new Error("rollback_check_output_oversized"), { code: "rollback_check_output_oversized" }));
        return;
      }
      if (target === "stdout") stdout += String(chunk);
      else stderr += String(chunk);
    };
    child.stdout.on("data", (chunk) => capture("stdout", chunk));
    child.stderr.on("data", (chunk) => capture("stderr", chunk));
    child.once("error", (error) => { clearTimeout(timer); reject(error); });
    child.once("exit", (code) => {
      clearTimeout(timer);
      const line = (code === 0 ? stdout : stderr).trim().split(/\r?\n/u).at(-1);
      let result;
      try { result = JSON.parse(line); } catch { return reject(new Error("rollback_check_output_invalid")); }
      if (code !== 0 || result.ok !== true) {
        const error = new Error(result.code || "rollback_check_failed");
        error.code = result.code || "rollback_check_failed";
        reject(error);
        return;
      }
      resolve(result);
    });
  });
}

export function requireCompleteCoverage(coverage) {
  if (!coverage || !Number.isSafeInteger(coverage.total) || coverage.total <= 0
    || coverage.migrated !== coverage.total || coverage.not_migrated !== 0
    || coverage.coverage_percent !== 100) {
    throw Object.assign(new Error("legacy_endpoint_coverage_incomplete"), { code: "legacy_endpoint_coverage_incomplete" });
  }
}

export async function checkLegacyRollback(env = process.env, preflight = (instance) => run("enrollment-gateway-preflight.js", [], {
  ...env, PINVOU_REMOTE_EXPECTED_GATEWAY_INSTANCE: instance,
})) {
  const inventory = env.PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY;
  if (!inventory) throw Object.assign(new Error("legacy_endpoint_inventory_required"), { code: "legacy_endpoint_inventory_required" });
  const document = await readEnrollmentInventory(inventory);
  const snapshot = await requestEnrollmentCoverage({ endpointIds: document.endpoint_ids, env });
  requireCompleteCoverage(snapshot.coverage);
  const gateway = await preflight(snapshot.gateway_instance_id);
  // Public probes await network/OS work. Never approve their earlier snapshot:
  // authenticate a fresh read from the same live owner after those probes.
  const finalSnapshot = await requestEnrollmentCoverage({ endpointIds: document.endpoint_ids, env });
  if (finalSnapshot.gateway_instance_id !== snapshot.gateway_instance_id
    || finalSnapshot.store_binding !== snapshot.store_binding) {
    throw Object.assign(new Error("coverage_owner_changed"), { code: "coverage_owner_changed" });
  }
  const coverage = finalSnapshot.coverage;
  requireCompleteCoverage(coverage);
  if (finalSnapshot.namespace_revision !== snapshot.namespace_revision) {
    throw Object.assign(new Error("rollback_registry_changed"), { code: "rollback_registry_changed" });
  }
  return {
    ok: true,
    safe_to_run_legacy_v2: true,
    store_revision: finalSnapshot.store_revision,
    namespace_revision: finalSnapshot.namespace_revision,
    migrated: coverage.migrated,
    not_migrated: coverage.not_migrated,
    coverage_percent: coverage.coverage_percent,
    checked_public_entrypoints: gateway.checked_public_entrypoints,
  };
}

if (isDirectCliEntry(import.meta.url)) {
  checkLegacyRollback()
    .then((result) => process.stdout.write(`${JSON.stringify(result)}\n`))
    .catch((error) => {
      process.stderr.write(`${JSON.stringify({ ok: false, safe_to_run_legacy_v2: false, code: error?.code || "rollback_check_failed" })}\n`);
      process.exitCode = 1;
    });
}
