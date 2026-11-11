// Process execution helper: spawn, timeout, text capture. Zero dependencies.
import { spawn } from "node:child_process";

/**
 * Run a command. Never uses a shell: cmd + args array only, so tool arguments
 * can never become command injection.
 * @returns {Promise<{code:number|null, stdout:string, stderr:string, timedOut:boolean, signal:string|null}>}
 */
export function run(cmd, args = [], opts = {}) {
  const timeoutMs = opts.timeoutMs ?? 20_000;
  const maxBuffer = opts.maxBuffer ?? 32 * 1024 * 1024;
  return new Promise((resolve) => {
    let child;
    try {
      child = spawn(cmd, args, {
        env: opts.env ? { ...process.env, ...opts.env } : process.env,
        cwd: opts.cwd,
        stdio: ["ignore", "pipe", "pipe"],
        // Windows: node handles .cmd/.exe resolution for known tools via shell:false + full name
        windowsHide: true,
      });
    } catch (err) {
      resolve({ code: -1, stdout: "", stderr: String(err?.message ?? err), timedOut: false, signal: null });
      return;
    }
    let stdout = "";
    let stderr = "";
    let stdoutBytes = 0;
    let stderrBytes = 0;
    let stdoutTruncated = false;
    let stderrTruncated = false;
    let timedOut = false;
    let settled = false;
    const timer = setTimeout(() => {
      timedOut = true;
      try { child.kill("SIGTERM"); } catch {}
      // Hard kill after grace period
      setTimeout(() => { try { child.kill("SIGKILL"); } catch {} }, 1500);
    }, timeoutMs);
    child.stdout.on("data", (d) => {
      if (stdoutBytes < maxBuffer) stdout += d.toString();
      stdoutBytes += Buffer.byteLength(d);
      if (stdoutBytes > maxBuffer) stdoutTruncated = true;
    });
    child.stderr.on("data", (d) => {
      if (stderrBytes < maxBuffer) stderr += d.toString();
      stderrBytes += Buffer.byteLength(d);
      if (stderrBytes > maxBuffer) stderrTruncated = true;
    });
    // Cap the kept prefix to maxBuffer bytes on a UTF-8 boundary so the
    // truncation marker's byte claim stays truthful for multi-byte output.
    const capToBytes = (s) => {
      let capped = Buffer.from(s, "utf8").subarray(0, maxBuffer).toString("utf8");
      while (Buffer.byteLength(capped, "utf8") > maxBuffer) capped = capped.slice(0, -1);
      return capped;
    };
    const finish = (code, signal) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      // Collection stops at maxBuffer; mark the cut so consumers know data
      // was dropped instead of mistaking the prefix for the whole output.
      if (stdoutTruncated) stdout = capToBytes(stdout) + `\n…[stdout truncated at ${maxBuffer} byte capture limit]`;
      if (stderrTruncated) stderr = capToBytes(stderr) + `\n…[stderr truncated at ${maxBuffer} byte capture limit]`;
      resolve({ code, stdout, stderr, timedOut, signal });
    };
    child.on("error", (err) => {
      stderr += String(err?.message ?? err);
      finish(-1, null);
    });
    child.on("close", (code, signal) => finish(code, signal));
  });
}

/** run() and throw a typed error on non-zero exit / timeout. */
export async function runOk(cmd, args = [], opts = {}) {
  const r = await run(cmd, args, opts);
  if (r.timedOut) throw new ExecError(`timeout after ${opts.timeoutMs ?? 20_000}ms: ${cmd}`, r);
  if (r.code !== 0) throw new ExecError(`${cmd} exited ${r.code}: ${trim(r.stderr || r.stdout)}`, r);
  return r;
}

export class ExecError extends Error {
  constructor(message, result) {
    super(message);
    this.name = "ExecError";
    this.result = result;
  }
}

/** True when the executable exists on PATH (or opts.fullPath exists). */
export async function have(cmd) {
  const probe = process.platform === "win32" ? "where" : "which";
  const r = await run(probe, [cmd], { timeoutMs: 5000 });
  return r.code === 0 && r.stdout.trim().length > 0;
}

export function trim(s, n = 400) {
  s = String(s ?? "").trim();
  return s.length > n ? s.slice(0, n) + "…" : s;
}

/** Parse JSON safely, returning fallback on failure. */
export function tryJson(s, fallback = null) {
  try { return JSON.parse(s); } catch { return fallback; }
}
