#!/usr/bin/env node
import { spawn } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const relay = join(root, "remote-control-relay");
const inventory = process.env.PINVOU_REMOTE_LEGACY_ENDPOINT_INVENTORY;

function run(script, args = []) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [join(relay, "scripts", script), ...args], {
      cwd: root,
      env: process.env,
      shell: false,
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk) => { stdout += String(chunk); });
    child.stderr.on("data", (chunk) => { stderr += String(chunk); });
    child.once("error", reject);
    child.once("exit", (code) => {
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

try {
  if (!inventory) throw Object.assign(new Error("legacy_endpoint_inventory_required"), { code: "legacy_endpoint_inventory_required" });
  const coverage = await run("enrollment-migrate.js", ["--coverage", "--input", inventory]);
  const gateway = await run("enrollment-gateway-preflight.js");
  process.stdout.write(`${JSON.stringify({
    ok: true,
    safe_to_run_legacy_v2: true,
    migrated: coverage.migrated,
    not_migrated: coverage.not_migrated,
    coverage_percent: coverage.coverage_percent,
    checked_public_entrypoints: gateway.checked_public_entrypoints,
  })}\n`);
} catch (error) {
  process.stderr.write(`${JSON.stringify({ ok: false, safe_to_run_legacy_v2: false, code: error?.code || "rollback_check_failed" })}\n`);
  process.exitCode = 1;
}
