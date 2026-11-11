import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { fileURLToPath } from "node:url";

const supervisor = fileURLToPath(new URL("../../scripts/release-owned-process.py", import.meta.url));
const DEFAULT_TIMEOUT_MS = 10 * 60 * 1000;
const MAX_DIAGNOSTIC_BYTES = 32 * 1024;

function failure(code, details = {}) {
  return Object.assign(new Error(code), { code, ...details });
}

// All callers supply their existing purpose-filtered command environment.
// The supervisor inherits the same filtered environment; no secret env spread.
export function runReleaseProcess(command, args, {
  cwd, env, timeoutMs = DEFAULT_TIMEOUT_MS, signal,
  python = process.env.PINVOU_CI_PYTHON || (process.platform === "win32" ? "python" : "python3"),
} = {}) {
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs < 1 || timeoutMs > 3600000) {
    return Promise.reject(failure("release_subgate_timeout_invalid"));
  }
  if (!env || typeof env !== "object") return Promise.reject(failure("release_subgate_environment_missing"));
  if (signal?.aborted) return Promise.reject(failure("release_subgate_cancelled"));
  return new Promise((resolve, reject) => {
    const nonce = randomBytes(16).toString("hex");
    let child;
    try {
      child = spawn(python, ["-X", "utf8", supervisor], {
        cwd, env, shell: false, windowsHide: true, stdio: ["pipe", "pipe", "pipe"],
      });
    } catch { reject(failure("release_subgate_start_failed")); return; }
    let stderr = Buffer.alloc(0);
    let requested = null;
    let hardKilled = false;
    let finished = false;
    let hard;
    const requestStop = (reason) => {
      requested ||= reason;
      if (!child.stdin.destroyed) child.stdin.end("cancel\n");
      clearTimeout(hard);
      hard = setTimeout(() => { hardKilled = true; child.kill(); }, 20000);
    };
    const abort = () => requestStop("cancelled");
    signal?.addEventListener("abort", abort, { once: true });
    child.stdout.resume();
    child.stderr.on("data", (chunk) => {
      stderr = Buffer.concat([stderr, chunk]).subarray(-MAX_DIAGNOSTIC_BYTES);
      // Acknowledge the supervisor's final correlated receipt before waiting
      // for its exit, so its private input-monitor thread can finish too.
      for (const line of stderr.toString("utf8").split(/\r?\n/u)) {
        if (!line.startsWith("PINVOU_RELEASE_PROCESS ")) continue;
        try {
          if (JSON.parse(line.slice("PINVOU_RELEASE_PROCESS ".length)).nonce === nonce
            && !child.stdin.writableEnded) child.stdin.end();
        } catch { /* not a complete control line yet */ }
      }
    });
    // Pipe errors remain non-success, without leaking the request/environment.
    child.stdin.on("error", () => {});
    const timeout = setTimeout(() => requestStop("timeout"), timeoutMs);
    // Missing cleanup acknowledgement fails closed; this is never a retry or
    // a claimed successful retirement. The supervisor owns its own child Job.
    hard = setTimeout(() => { hardKilled = true; child.kill(); }, timeoutMs + 20000);
    const settle = (error) => {
      if (finished) return;
      finished = true;
      clearTimeout(timeout); clearTimeout(hard);
      signal?.removeEventListener("abort", abort);
      child.stdin.destroy();
      if (error) reject(error); else resolve();
    };
    child.once("error", () => settle(failure("release_subgate_start_failed")));
    child.once("close", (code) => {
      if (hardKilled) { settle(failure("release_subgate_cleanup_unconfirmed")); return; }
      const lines = stderr.toString("utf8").split(/\r?\n/u);
      let receipt = null;
      for (const line of lines) {
        if (!line.startsWith("PINVOU_RELEASE_PROCESS ")) continue;
        try {
          const candidate = JSON.parse(line.slice("PINVOU_RELEASE_PROCESS ".length));
          if (candidate.nonce === nonce) receipt = candidate;
        } catch { /* malformed child text is never a control receipt */ }
      }
      if (code !== 0 || !receipt || receipt.cleanup_confirmed !== true
        || !Number.isInteger(receipt.exit_code) || typeof receipt.forced !== "boolean") {
        settle(failure("release_subgate_cleanup_unconfirmed")); return;
      }
      const reason = requested || receipt.reason;
      if (reason) {
        settle(failure(reason === "timeout" ? "release_subgate_timeout" : "release_subgate_cancelled",
          { exitCode: receipt.exit_code, cleanupConfirmed: true, forced: receipt.forced }));
      } else if (receipt.exit_code !== 0) {
        settle(failure("release_subgate_failed", { exitCode: receipt.exit_code, cleanupConfirmed: true, forced: receipt.forced }));
      } else if (receipt.forced) {
        settle(failure("release_subgate_leftover_process", { exitCode: 0, cleanupConfirmed: true }));
      } else settle();
    });
    child.stdin.write(JSON.stringify({ nonce, timeout_ms: timeoutMs, command: [command, ...args], environment: env }) + "\n");
  });
}
