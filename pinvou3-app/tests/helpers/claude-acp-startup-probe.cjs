"use strict";

// Only the native ACP smoke preloads this observer. Never log settings, RPC
// bodies, paths, environment values or child arguments. Do not consume stdin or
// replace SDK/child_process functions: managed policy must execute unchanged.
const { registerHooks } = require("node:module");
const { channel } = require("node:diagnostics_channel");
const started = process.hrtime.bigint();
let emitted = 0;

function record(phase, extra = {}) {
  if (emitted++ >= 48) return;
  try {
    const cpu = process.cpuUsage();
    process.stderr.write(`[acp-startup] ${JSON.stringify({
      phase,
      elapsed_ms: Number(process.hrtime.bigint() - started) / 1e6,
      cpu_ms: (cpu.user + cpu.system) / 1000,
      rss_mib: process.memoryUsage().rss / 1048576,
      ...extra,
    })}\n`);
  } catch {
    // Observability must not replace an application error or block startup.
  }
}

record("preload");
registerHooks({
  load(url, context, nextLoad) {
    const label = url.includes("/@anthropic-ai/claude-agent-sdk/") ? "sdk"
      : url.endsWith("/managed-policy.js") ? "managed-policy"
        : url.endsWith("/claude-agent-acp/dist/index.js") ? "entry" : null;
    if (label) record("load-start", { label });
    const result = nextLoad(url, context);
    if (label) record("load-end", { label });
    return result;
  },
});

channel("child_process").subscribe(({ process: child }) => {
  // Node publishes this channel before spawnfile is assigned. Observe the real
  // spawn event without replacing execFile (including its promisify contract).
  child.once("spawn", () => {
    if (!/(?:^|[\\/])reg(?:\.exe)?$/iu.test(String(child.spawnfile))) return;
    const before = process.hrtime.bigint();
    record("registry-start");
    child.once("close", (code) => record("registry-close", {
      code,
      duration_ms: Number(process.hrtime.bigint() - before) / 1e6,
    }));
  });
});

process.stdin.on("newListener", (name) => {
  if (name === "data") record("stdin-data-listener");
});
let ticks = 0;
const timer = setInterval(() => {
  record("heartbeat");
  if (++ticks >= 16) clearInterval(timer);
}, 1000);
timer.unref();
