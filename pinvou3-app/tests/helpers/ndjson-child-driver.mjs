import { spawn } from 'node:child_process';
import { monitorEventLoopDelay, performance } from 'node:perf_hooks';
import { setTimeout as sleep } from 'node:timers/promises';

export function spawnNdjsonChild({
  args,
  env = process.env,
  stderr = 'capture',
  timeoutMs = 10_000,
  collectResponses = false,
  unrefTimeout = false,
}) {
  const startedAt = performance.now();
  const child = spawn(process.execPath, args, {
    stdio: ['pipe', 'pipe', stderr === 'inherit' ? 'inherit' : 'pipe'],
    env,
  });
  let stdout = '';
  let stderrOutput = '';
  let nextId = 1;
  const pending = new Map();
  const responses = [];
  let spawnedAt = null;
  let exitedAt = null;
  let closedAt = null;
  let stdoutBytes = 0;
  let stderrBytes = 0;
  let lastStdoutAt = null;
  child.once('spawn', () => { spawnedAt = performance.now(); });
  child.once('exit', () => { exitedAt = performance.now(); });
  child.once('close', () => { closedAt = performance.now(); });
  // Counters and lifecycle only: never serialize protocol messages or env.
  const diagnostic = () => ({
    pid: child.pid ?? null,
    spawned: spawnedAt !== null,
    spawnElapsedMs: spawnedAt === null ? null : spawnedAt - startedAt,
    ageMs: performance.now() - startedAt,
    exited: exitedAt !== null,
    closed: closedAt !== null,
    exitCode: child.exitCode,
    signalCode: child.signalCode,
    stdoutBytes,
    stderrBytes: child.stderr ? stderrBytes : null,
    lastStdoutAgeMs: lastStdoutAt === null ? null : performance.now() - lastStdoutAt,
    pendingRequests: pending.size,
  });
  const observerDelay = monitorEventLoopDelay({ resolution: 20 });
  observerDelay.enable();
  child.once('close', () => observerDelay.disable());

  child.stderr?.on('data', (chunk) => { stderrBytes += chunk.length; stderrOutput += chunk; });
  child.stdout.on('data', (chunk) => {
    stdoutBytes += chunk.length;
    lastStdoutAt = performance.now();
    stdout += chunk;
    let newline;
    while ((newline = stdout.indexOf('\n')) >= 0) {
      const line = stdout.slice(0, newline);
      stdout = stdout.slice(newline + 1);
      if (!line.trim()) continue;
      const message = JSON.parse(line);
      if (collectResponses) responses.push(message);
      const waiter = pending.get(message.id);
      if (!waiter) continue;
      pending.delete(message.id);
      clearTimeout(waiter.timeout);
      waiter.resolve(message);
    }
  });

  const send = (message) => child.stdin.write(`${JSON.stringify(message)}\n`);
  const startRequest = (method, params = {}, options) => {
    // Preserve the numeric main caller and the labelled per-request options.
    const { timeoutMs: requestTimeoutMs = timeoutMs, label = method } =
      typeof options === 'number' ? { timeoutMs: options } : options ?? {};
    const id = nextId++;
    const requestedAt = performance.now();
    const cpuAtRequest = process.cpuUsage();
    const response = new Promise((resolve, reject) => {
      const timeout = setTimeout(() => {
        if (!pending.delete(id)) return;
        const cpu = process.cpuUsage(cpuAtRequest);
        const observedMs = performance.now() - requestedAt;
        const observation = { ...diagnostic(), deadlineMs: requestTimeoutMs, observedMs,
          timerDelayMs: Math.max(0, observedMs - requestTimeoutMs),
          observerProcessCpuMs: (cpu.user + cpu.system) / 1000 };
        const observer = JSON.stringify({
          observer_elapsed_ms: Math.round(observedMs),
          observer_loop_max_delay_ms: Math.round(observerDelay.max / 1e6),
          child_exit_code: child.exitCode,
        });
        const error = new Error(`${label} timed out after ${requestTimeoutMs}ms (request ${id}); observer: ${observer}; child stderr: ${stderrOutput}; observation: ${JSON.stringify(observation)}`);
        error.childDiagnostic = observation;
        reject(error);
      }, requestTimeoutMs);
      if (unrefTimeout) timeout.unref();
      pending.set(id, { resolve, timeout });
    });
    send({ jsonrpc: '2.0', id, method, params });
    return { id, response };
  };

  return {
    child,
    responses,
    stderrText: () => stderrOutput,
    diagnostic,
    send,
    notify(method, params = {}) {
      send({ jsonrpc: '2.0', method, params });
    },
    startRequest,
    request(method, params = {}, options) {
      return startRequest(method, params, options).response;
    },
    dropRequest(id, resolution) {
      const waiter = pending.get(id);
      if (!waiter) return;
      pending.delete(id);
      clearTimeout(waiter.timeout);
      if (arguments.length > 1) waiter.resolve(resolution);
    },
  };
}

export async function stopNdjsonChild(child, graceMs = 500) {
  if (child.exitCode == null) child.stdin.end();
  if (child.exitCode == null) {
    await Promise.race([
      new Promise((resolve) => { child.once('exit', resolve); }),
      sleep(graceMs),
    ]);
  }
  if (child.exitCode == null) child.kill('SIGKILL');
}
