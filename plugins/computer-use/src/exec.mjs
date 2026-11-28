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
    const stdoutChunks = [];
    const stderrChunks = [];
    let stdoutBytes = 0;
    let stderrBytes = 0;
    let stdoutTruncated = false;
    let stderrTruncated = false;
    let timedOut = false;
    let settled = false;
    // Diagnostics from the 'error' event are kept outside the capture cap:
    // appended into the chunks after an over-cap stderr they would sit past
    // the cap cut and be lost, leaving the truncation marker as the only
    // trace of a failed spawn/kill.
    let spawnError = null;
    const timer = setTimeout(() => {
      timedOut = true;
      try { child.kill("SIGTERM"); } catch {}
      // Hard kill after grace period
      setTimeout(() => { try { child.kill("SIGKILL"); } catch {} }, 1500);
    }, timeoutMs);
    child.stdout.on("data", (d) => {
      if (stdoutBytes < maxBuffer) stdoutChunks.push(d);
      stdoutBytes += Buffer.byteLength(d);
      if (stdoutBytes > maxBuffer) stdoutTruncated = true;
    });
    child.stderr.on("data", (d) => {
      if (stderrBytes < maxBuffer) stderrChunks.push(d);
      stderrBytes += Buffer.byteLength(d);
      if (stderrBytes > maxBuffer) stderrTruncated = true;
    });
    // Cap the kept prefix to maxBuffer bytes, dropping a trailing partial
    // UTF-8 sequence the cut may have split, so the truncation marker's byte
    // claim stays truthful and the kept prefix never ends in a replacement
    // character that was never in the output.
    const capToBytes = (buf) => {
      let end = Math.min(buf.length, maxBuffer);
      let start = end - 1;
      while (start >= 0 && end - start < 4 && (buf[start] & 0xc0) === 0x80) start -= 1;
      if (start >= 0 && (buf[start] & 0xc0) !== 0x80) {
        const lead = buf[start];
        const expected = lead >= 0xf0 ? 4 : lead >= 0xe0 ? 3 : lead >= 0xc0 ? 2 : 1;
        if (end - start < expected) end = start;
      }
      return buf.subarray(0, end);
    };
    const finish = (code, signal) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      // Decode the raw chunks once, at the end: per-chunk string decodes
      // would turn a multi-byte sequence split across pipe-read chunks into
      // a fabricated replacement character at each internal join.
      let out = Buffer.concat(stdoutChunks);
      let err = Buffer.concat(stderrChunks);
      // Collection stops at maxBuffer; mark the cut so consumers know data
      // was dropped instead of mistaking the prefix for the whole output.
      if (stdoutTruncated) out = capToBytes(out);
      if (stderrTruncated) err = capToBytes(err);
      let stdout = out.toString("utf8");
      let stderr = err.toString("utf8");
      if (stdoutTruncated) stdout += `\n…[stdout truncated at ${maxBuffer} byte capture limit]`;
      if (stderrTruncated) stderr += `\n…[stderr truncated at ${maxBuffer} byte capture limit]`;
      if (spawnError) stderr += `${stderr ? "\n" : ""}spawn error: ${spawnError}`;
      resolve({ code, stdout, stderr, timedOut, signal });
    };
    child.on("error", (err) => {
      spawnError = String(err?.message ?? err);
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
