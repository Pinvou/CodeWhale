import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { fileURLToPath } from "node:url";

const supervisor = fileURLToPath(new URL("../../scripts/release-owned-process.py", import.meta.url));
const DEFAULT_TIMEOUT_MS = 10 * 60 * 1000;
const MAX_DIAGNOSTIC_BYTES = 32 * 1024;

function failure(code, details = {}) {
  return Object.assign(new Error(code), { code, ...details });
}

function interruptionFailureCode(reason) {
  return reason === "timeout" ? "release_subgate_timeout"
    : reason === "cancelled" ? "release_subgate_cancelled" : null;
}

// All callers supply their existing purpose-filtered command environment.
// The supervisor inherits the same filtered environment; no secret env spread.
export function runReleaseProcess(command, args, {
  cwd, env, timeoutMs = DEFAULT_TIMEOUT_MS, signal,
  python = process.env.PINVOU_CI_PYTHON || (process.platform === "win32" ? "python" : "python3"),
} = {}) {
  if (typeof command !== "string" || command.includes("\0") || !Array.isArray(args)
    || args.some((arg) => typeof arg !== "string" || arg.includes("\0"))) {
    return Promise.reject(failure("release_subgate_command_invalid"));
  }
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs < 1 || timeoutMs > 3600000) {
    return Promise.reject(failure("release_subgate_timeout_invalid"));
  }
  if (!env || typeof env !== "object") return Promise.reject(failure("release_subgate_environment_missing"));
  if (Object.entries(env).some(([key, value]) => typeof value !== "string"
    || key.includes("\0") || value.includes("\0"))) {
    return Promise.reject(failure("release_subgate_environment_invalid"));
  }
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
    const abandonUnconfirmed = () => {
      child.stdout.destroy(); child.stderr.destroy(); child.stdin.destroy();
      child.unref();
      const primaryCode = interruptionFailureCode(requested) || "release_subgate_cleanup_unconfirmed";
      settle(failure(primaryCode, { cleanupConfirmed: false, cleanupError: "release_subgate_cleanup_unconfirmed" }));
    };
    const forceUnconfirmed = () => {
      if (finished || hardKilled) return;
      hardKilled = true;
      // SIGKILL is not a cooperative request on POSIX. Still do not claim
      // retirement without the supervisor's acknowledgement, or wait forever
      // for EOF inherited by an unconfirmed descendant.
      try { child.kill("SIGKILL"); } catch { /* retirement stays unconfirmed */ }
      finally { abandonUnconfirmed(); }
    };
    const requestStop = (reason) => {
      if (requested !== null || finished) return;
      requested = reason;
      clearTimeout(timeout);
      if (!child.stdin.destroyed) child.stdin.end("cancel\n");
      clearTimeout(hard);
      hard = setTimeout(forceUnconfirmed, 20000);
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
    hard = setTimeout(forceUnconfirmed, timeoutMs + 20000);
    const settle = (error) => {
      if (finished) return;
      finished = true;
      clearTimeout(timeout); clearTimeout(hard);
      signal?.removeEventListener("abort", abort);
      child.stdin.destroy();
      if (error) reject(error); else resolve();
    };
    child.once("error", () => {
      if (child.pid == null) settle(failure(interruptionFailureCode(requested) || "release_subgate_start_failed", { cleanupConfirmed: true }));
      else if (!hardKilled) forceUnconfirmed();
      else abandonUnconfirmed();
    });
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
      if (code !== 0 || !receipt
        || (receipt.execution_error !== null && receipt.execution_error !== "start_failed" && receipt.execution_error !== "execution_failed")
        || (!Number.isInteger(receipt.exit_code) && !(receipt.exit_code === null && receipt.execution_error !== null))
        || typeof receipt.forced !== "boolean"
        || (receipt.root_exit_code !== null && !Number.isInteger(receipt.root_exit_code))
        || (receipt.primary_exit_code !== null && (!Number.isInteger(receipt.primary_exit_code)
          || receipt.primary_exit_code === 0 || receipt.primary_exit_code !== receipt.root_exit_code))) {
        settle(failure(interruptionFailureCode(requested) || "release_subgate_cleanup_unconfirmed", {
          cleanupConfirmed: false, cleanupError: "release_subgate_cleanup_unconfirmed",
          supervisorExitCode: Number.isInteger(code) && code >= 0 && code <= 0xffffffff ? code : null,
          receiptStatus: receipt === null ? "missing" : "invalid",
        })); return;
      }
      const reason = requested || receipt.reason;
      if (receipt.cleanup_confirmed !== true) {
        const primaryCode = receipt.primary_exit_code !== null ? "release_subgate_failed"
          : interruptionFailureCode(reason)
            || (receipt.execution_error ? `release_subgate_${receipt.execution_error}` : "release_subgate_cleanup_unconfirmed");
        settle(failure(primaryCode,
          { exitCode: receipt.root_exit_code ?? receipt.exit_code, cleanupConfirmed: false,
            cleanupError: "release_subgate_cleanup_unconfirmed" }));
        return;
      }
      const rootExit = receipt.root_exit_code ?? receipt.exit_code;
      if (receipt.primary_exit_code !== null) {
        settle(failure("release_subgate_failed", { exitCode: receipt.primary_exit_code,
          supervisionExitCode: receipt.exit_code, cleanupConfirmed: true, forced: receipt.forced }));
      } else if (reason) {
        settle(failure(reason === "timeout" ? "release_subgate_timeout" : "release_subgate_cancelled",
          { exitCode: rootExit, supervisionExitCode: receipt.exit_code, cleanupConfirmed: true, forced: receipt.forced }));
      } else if (receipt.execution_error) {
        settle(failure(`release_subgate_${receipt.execution_error}`, { cleanupConfirmed: true }));
      } else if (receipt.exit_code !== 0) {
        settle(failure("release_subgate_failed", { exitCode: rootExit, supervisionExitCode: receipt.exit_code, cleanupConfirmed: true, forced: receipt.forced }));
      } else if (receipt.forced) {
        settle(failure("release_subgate_leftover_process", { exitCode: 0, cleanupConfirmed: true }));
      } else settle();
    });
    child.stdin.write(JSON.stringify({ nonce, timeout_ms: timeoutMs, command: [command, ...args], environment: env }) + "\n");
  });
}
