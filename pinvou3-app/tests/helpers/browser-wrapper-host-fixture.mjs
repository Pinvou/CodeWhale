import { unlinkSync, mkdirSync, watch } from 'node:fs';
import { readFile, readdir, writeFile, rename } from 'node:fs/promises';
import { join } from 'node:path';
import { Worker, MessageChannel, isMainThread, workerData } from 'node:worker_threads';
import { createServer } from './http-fixture.mjs';
import { fixtureScope } from './fixture-scope.mjs';

const progressPhases = ['starting', 'idle', 'scan', 'read', 'write', 'control', 'cdp', 'failed', 'exists', 'directory', 'audit', 'native-readdir'];
const inputReplySlot = 3;
let workerProgress;
async function waitForBarrier(shared) {
  if (!shared) return;
  const barrier = new Int32Array(shared);
  while (Atomics.load(barrier, 0) === 1) await Atomics.waitAsync(barrier, 0, 1).value;
}
function progress(phase) {
  if (!workerProgress) return;
  Atomics.store(workerProgress, 0, progressPhases.indexOf(phase));
  Atomics.store(workerProgress, 1, Math.trunc(performance.now()));
  Atomics.add(workerProgress, 2, 1);
}

async function atomicWriteJson(path, value) {
  progress('write');
  const temporary = `${path}.${process.pid}.tmp`;
  await writeFile(temporary, JSON.stringify(value));
  await rename(temporary, path);
}

function createFakeHostModel(fixture, { sessionId: SESSION_ID, sessionToken: SESSION_TOKEN }, watchRequests, requestReadBarrier) {
  const requestDirectory = join(fixture.root, 'host-requests');
  mkdirSync(requestDirectory, { recursive: true });
  const handled = new Set();
  const state = {
    prepareCalls: 0,
    generation: 0,
    preparePublishesWorkspace: true,
    operationCalls: new Map(),
    operationLog: [],
    operationTimeline: [],
    operationErrors: new Map(),
    malformedOperationResponses: new Set(),
    deferredOperations: new Set(),
    // Barrier-visibility freeze: frozen operations are still handled and
    // answered normally (the wrapper lease stays healthy), but their
    // observations are withheld from operationCalls/Log/Timeline until the
    // freeze lifts, then flushed in arrival order. This models delayed
    // observation only; real IPC delay is tested separately by withholding
    // responses. Neither mode changes the wrapper's authorization deadlines.
    deferredObservations: new Set(),
    pendingObservations: [],
    failure: null,
  };

  const publishWorkspace = async () => {
    state.generation += 1;
    await atomicWriteJson(fixture.workspacePath, {
      version: 2,
      mapping_authority: 'host',
      revision: state.generation,
      session_token: SESSION_TOKEN,
      active_tab: SESSION_TOKEN,
      tabs: [{ token: SESSION_TOKEN, target_id: `target-a-${state.generation}` }],
    });
  };

  const respond = async (request, response) => {
    await atomicWriteJson(
      join(requestDirectory, `${SESSION_TOKEN}-${request.request_id}.response`),
      {
        protocol_version: request.protocol_version,
        request_id: request.request_id,
        idempotency_key: request.idempotency_key,
        ...response,
      },
    );
    // Count accepted replies only after the real IPC response is published.
    // Idle progress, rejected requests and withheld responses cannot satisfy
    // the observer's synchronous barrier.
    if (request.operation === 'refresh_agent_input' && response.ok === true
      && response.result?.accepted === true) {
      Atomics.add(workerProgress, inputReplySlot, 1);
      Atomics.notify(workerProgress, inputReplySlot);
    }
  };

  const recordOperation = async (operation) => {
    const event = { operation, at: Date.now() };
    if (operation === 'end_agent_operation') {
      // Capture an actual happens-before witness, rather than ordering wall
      // clocks from independent processes. The fake MCP appends completion
      // before writing its response; a premature host end cannot see it.
      event.completedToolCounts = {};
      progress('audit');
      const audit = await readFile(fixture.auditPath, 'utf8');
      progress('read');
      for (const line of audit.split('\n').slice(0, -1)) {
        if (!line) continue;
        const entry = JSON.parse(line);
        if (entry.kind === 'tool-complete') {
          event.completedToolCounts[entry.name] = (event.completedToolCounts[entry.name] || 0) + 1;
        }
      }
    }
    // Snapshot RPC may run while the audit read is pending. Publish the count,
    // log and completed witness together so it cannot observe a partial event.
    state.operationCalls.set(
      operation,
      (state.operationCalls.get(operation) || 0) + 1,
    );
    state.operationLog.push(operation);
    state.operationTimeline.push(event);
  };

  let stopped = false;
  let scheduled;
  let pendingPoll;
  const poll = async () => {
    if (stopped) return;
    progress('scan');
    if (state.failure) { progress('failed'); return; }
    try {
      if (
        state.deferredObservations.size === 0 &&
        state.pendingObservations.length
      ) {
        for (const operation of state.pendingObservations.splice(0)) {
          await recordOperation(operation);
        }
      }
      progress('directory');
      // Real host control and CDP do not synchronously wait for a filesystem
      // metadata call. The optional test barrier models a pending directory
      // read without consuming a worker thread or changing lease deadlines.
      await waitForBarrier(requestReadBarrier);
      // Separate the intentional barrier from actual filesystem completion.
      // The observer remains able to read this while that I/O is pending.
      progress('native-readdir');
      const names = await readdir(requestDirectory);
      progress('scan');
      for (const name of names) {
        if (!name.endsWith('.json') || handled.has(name)) continue;
        progress('read');
        let raw;
        try { raw = await readFile(join(requestDirectory, name), 'utf8'); }
        catch (error) {
          if (['ENOENT', 'EPERM', 'EBUSY'].includes(error?.code)) continue;
          throw error;
        }
        const request = JSON.parse(raw);
        if (state.deferredOperations.has(request.operation)) continue;
        handled.add(name);
        if (state.deferredObservations.has(request.operation)) {
          state.pendingObservations.push(request.operation);
        } else {
          await recordOperation(request.operation);
        }

        if (request.operation === 'prepare') {
          state.prepareCalls += 1;
          if (state.preparePublishesWorkspace) await publishWorkspace();
          await respond(request, { ok: true, result: { ready: true } });
          continue;
        }

        const configuredError = state.operationErrors.get(request.operation);
        if (configuredError) {
          await respond(request, { ok: false, error: configuredError });
          continue;
        }

        if (request.operation === 'activate_tab') {
          const workspace = JSON.parse(await readFile(fixture.workspacePath, 'utf8'));
          const tab = workspace.tabs.find((candidate) => candidate.token === request.tab_token);
          if (!tab) {
            await respond(request, { ok: false, error: 'browser/page-not-found' });
            continue;
          }
          await respond(request, {
            ok: true,
            result: {
              sessionId: SESSION_ID,
              tabToken: tab.token,
              targetId: tab.target_id,
              revision: workspace.revision,
              owner: 'agent',
              lease: 'a'.repeat(32),
            },
          });
          continue;
        }

        if (request.operation === 'close_tab') {
          const workspace = JSON.parse(await readFile(fixture.workspacePath, 'utf8'));
          const tabs = workspace.tabs.filter((tab) => tab.token !== request.tab_token);
          if (tabs.length === workspace.tabs.length || tabs.length === 0) {
            await respond(request, { ok: false, error: 'browser/page-not-found' });
            continue;
          }
          await atomicWriteJson(fixture.workspacePath, {
            ...workspace,
            revision: workspace.revision + 1,
            active_tab: tabs[0].token,
            tabs,
          });
          if (state.malformedOperationResponses.has(request.operation)) {
            await atomicWriteJson(
              join(requestDirectory, `${SESSION_TOKEN}-${request.request_id}.response`),
              { malformed: true },
            );
          } else {
            await respond(request, { ok: true, result: {} });
          }
          continue;
        }

        if ([
          'assert_host_lease',
          'begin_agent_operation',
          'refresh_agent_input',
          'refresh_agent_operation',
          'end_agent_operation',
        ].includes(request.operation)) {
          await respond(request, { ok: true, result: { accepted: true } });
          continue;
        }

        await respond(request, { ok: false, error: `unexpected operation: ${request.operation}` });
      }
    } catch (error) {
      state.failure = error;
      progress('failed');
    }
    if (!state.failure) progress('idle');
  };
  // Keep one owned read/write sequence in flight; notifications coalesce.
  // Notifications are hints only: keep a bounded fallback for lost/coalesced
  // Windows events, still well below the unchanged 400ms host-refresh deadline.
  const schedule = () => {
    if (stopped || scheduled || pendingPoll) return;
    scheduled = setImmediate(() => {
      scheduled = undefined;
      pendingPoll = poll().finally(() => { pendingPoll = undefined; });
    });
  };
  const watcher = watchRequests ? watch(requestDirectory, schedule) : null;
  const watcherExit = watcher ? new Promise(resolve => watcher.once('close', resolve)) : Promise.resolve();
  watcher?.on('error', error => { state.failure ||= error; progress('failed'); });
  const timer = setInterval(schedule, 100);
  let closing;

  return {
    state,
    stopSessionA() {
      try { unlinkSync(fixture.workspacePath); } catch { /* already stopped */ }
    },
    close() {
      return closing ||= (async () => {
        stopped = true;
        clearInterval(timer);
        if (scheduled) clearImmediate(scheduled);
        if (watcher) { watcher.close(); await watcherExit; }
        await pendingPoll;
        if (state.failure) throw state.failure;
      })();
    },
  };
}

// The real host is independent of the MCP test coordinator. Keep the model,
// request poller and CDP endpoint together in an owned worker as well: blocking
// an observer must not suppress the host's 400ms input-refresh replies.
export async function createBrowserWrapperHost(fixture, identity, {
  scope = fixtureScope, watchRequests = true, requestReadBarrier, configurationReplyBarrier,
} = {}) {
  if (typeof watchRequests !== 'boolean') throw new TypeError('watchRequests must be boolean');
  for (const barrier of [requestReadBarrier, configurationReplyBarrier]) {
    if (barrier !== undefined && (!(barrier instanceof SharedArrayBuffer)
      || barrier.byteLength !== Int32Array.BYTES_PER_ELEMENT)) throw new TypeError('invalid host fixture barrier');
  }
  const { port1, port2 } = new MessageChannel();
  const sharedProgress = new SharedArrayBuffer(Int32Array.BYTES_PER_ELEMENT * 4);
  const observedProgress = new Int32Array(sharedProgress);
  const diagnostic = () => ({
    phase: progressPhases[Atomics.load(observedProgress, 0)] || 'unknown',
    sequence: Atomics.load(observedProgress, 2),
    ageMs: Math.max(0, Math.trunc(performance.now()) - Atomics.load(observedProgress, 1)),
    inputRefreshReplies: Atomics.load(observedProgress, inputReplySlot),
    exited,
  });
  let worker;
  let exited = false;
  let serverClosed = false;
  let revision = 0;
  let next = 0;
  let stopped = false;
  let expectedShutdown = false;
  let terminalFailure = null;
  let reportedHostFailure = null;
  let unreportedHostFailure = null;
  let readyResolve;
  let readyReject;
  let exitResolve;
  const pending = new Map();
  const ready = new Promise((resolve, reject) => { readyResolve = resolve; readyReject = reject; });
  const exit = new Promise(resolve => { exitResolve = resolve; });

  const rejectPending = error => {
    readyReject(error);
    for (const waiter of pending.values()) {
      clearTimeout(waiter.timer);
      waiter.reject(error);
    }
    pending.clear();
  };
  const rpc = (command, values = {}) => {
    if (terminalFailure) return Promise.reject(terminalFailure);
    if (exited) return Promise.reject(new Error('browser host fixture worker has exited'));
    const id = ++next;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        pending.delete(id);
        reject(new Error(`browser host fixture ${command} acknowledgement timed out; progress=${JSON.stringify(diagnostic())}`));
      }, 5_000);
      pending.set(id, { resolve, reject, timer });
      try { port1.postMessage({ id, command, expectedRevision: revision, ...values }); }
      catch (error) { pending.delete(id); clearTimeout(timer); reject(error); }
    });
  };
  port1.on('message', message => {
    if (message?.ready === true) { readyResolve(message); return; }
    const waiter = pending.get(message?.id);
    if (!waiter) return;
    pending.delete(message.id);
    clearTimeout(waiter.timer);
    if (message.error) waiter.reject(new Error(message.error));
    else waiter.resolve(message.value);
  });

  let release;
  try { release = scope.register({
    kind: 'worker',
    close: async () => {
      try {
        if (worker && !exited) {
          try {
            const result = await rpc('shutdown');
            if (result?.serverClosed !== true || result?.pollerStopped !== true) {
              throw new Error('browser host fixture shutdown was not acknowledged');
            }
            if (result.modelFailure && result.modelFailure !== reportedHostFailure) {
              unreportedHostFailure = new Error(result.modelFailure);
            }
            serverClosed = true;
            expectedShutdown = true;
            port1.postMessage({ command: 'ack_shutdown' });
            let exitTimer;
            try {
              await Promise.race([exit, new Promise((resolve, reject) => {
                exitTimer = setTimeout(() => reject(new Error('browser host fixture worker exit was not confirmed')), 2_000);
              })]);
            } finally { clearTimeout(exitTimer); }
          } finally {
            // The exact worker object is the ownership capability. Even a
            // lost control response must not leave its timer/server alive.
            if (!exited) await worker.terminate();
          }
        }
      } finally {
        if (!worker) exited = true;
        port1.close();
        if (!worker) port2.close();
      }
      if (terminalFailure) throw terminalFailure;
      if (unreportedHostFailure) throw unreportedHostFailure;
    },
    verify: () => exited && terminalFailure === null,
  }); } catch (primary) {
    port1.close();
    port2.close();
    throw primary;
  }

  try {
    worker = new Worker(new URL(import.meta.url), {
      workerData: {
        kind: 'pinvou-browser-fixture-host-v1',
        fixture: { root: fixture.root, workspacePath: fixture.workspacePath, portPath: fixture.portPath, auditPath: fixture.auditPath },
        identity,
        control: port2,
        sharedProgress,
        watchRequests,
        requestReadBarrier,
        configurationReplyBarrier,
      },
      transferList: [port2],
    });
    worker.on('error', error => {
      terminalFailure ||= error;
      rejectPending(terminalFailure);
    });
    worker.once('exit', code => {
      exited = true;
      if (!expectedShutdown || code !== 0) {
        terminalFailure ||= new Error(`browser host fixture worker exited unexpectedly (${code})`);
      }
      port1.close();
      rejectPending(terminalFailure || new Error(`browser host fixture worker exited (${code})`));
      exitResolve(code);
    });
    let startupTimer;
    try {
      await Promise.race([ready, new Promise((resolve, reject) => {
        startupTimer = setTimeout(() => reject(new Error('browser host fixture startup timed out')), 10_000);
      })]);
    } finally { clearTimeout(startupTimer); }
  } catch (primary) {
    try { await release(); }
    catch (cleanup) { throw new AggregateError([primary, cleanup], 'browser host fixture startup and cleanup failed', { cause: cleanup }); }
    throw primary;
  }

  fixture.cdp = {
    get listening() { return !exited && !serverClosed; },
    close(callback) { release().then(() => callback(), callback); },
  };
  const configure = async change => {
    const result = await rpc('configure', { change });
    if (result?.revision !== revision + 1) throw new Error('browser host fixture configuration revision mismatch');
    revision = result.revision;
  };
  return {
    // Shared counters remain readable when the worker cannot acknowledge RPC.
    // No fixture paths, host/tool messages, request payloads or identities.
    diagnostic,
    waitForInputRefreshesWhileBlocked(count, { minBlockedMs = 0, timeoutMs = 10_000 } = {}) {
      if (!Number.isSafeInteger(count) || count < 1 || count > 100
        || !Number.isFinite(timeoutMs) || timeoutMs < 1 || timeoutMs > 10_000
        || !Number.isFinite(minBlockedMs) || minBlockedMs < 0 || minBlockedMs >= timeoutMs) {
        throw new RangeError('invalid blocked-observer barrier');
      }
      const started = performance.now();
      const before = Atomics.load(observedProgress, inputReplySlot);
      while (true) {
        const after = Atomics.load(observedProgress, inputReplySlot);
        const blockedMs = performance.now() - started;
        if (after - before >= count && blockedMs >= minBlockedMs) return { before, after, blockedMs };
        const remaining = timeoutMs - blockedMs;
        if (remaining <= 0) {
          throw new Error('blocked observer did not receive real input refresh replies: '
            + JSON.stringify({ expected: count, received: after - before, progress: diagnostic() }));
        }
        const untilMinimum = after - before >= count ? minBlockedMs - blockedMs : remaining;
        Atomics.wait(observedProgress, inputReplySlot, after, Math.max(1, Math.min(remaining, untilMinimum)));
      }
    },
    async snapshot() {
      const result = await rpc('snapshot');
      if (result?.revision !== revision) throw new Error('browser host fixture snapshot revision mismatch');
      return result.state;
    },
    setOperationError: (key, value) => configure({ field: 'operationErrors', action: 'set', key, value }),
    clearOperationError: key => configure({ field: 'operationErrors', action: 'delete', key }),
    deferOperation: (key, enabled) => configure({ field: 'deferredOperations', action: enabled ? 'add' : 'delete', key }),
    freezeObservations: (key, enabled) => configure({ field: 'deferredObservations', action: enabled ? 'add' : 'delete', key }),
    malformResponse: (key, enabled) => configure({ field: 'malformedOperationResponses', action: enabled ? 'add' : 'delete', key }),
    setPreparePublishesWorkspace: value => configure({ field: 'preparePublishesWorkspace', action: 'set', value }),
    stopSessionA() { try { unlinkSync(fixture.workspacePath); } catch { /* already stopped, matching the original fixture */ } },
    async close() {
      if (stopped) return;
      try { await rpc('stop_polling'); }
      catch (error) { reportedHostFailure = error.message; throw error; }
      stopped = true;
    },
  };
}

async function serveHostWorker({ fixture, identity, control, sharedProgress, watchRequests, requestReadBarrier, configurationReplyBarrier }) {
  workerProgress = new Int32Array(sharedProgress);
  progress('starting');
  const cdp = createServer((request, response) => {
    progress('cdp');
    if (request.url === '/json/version') {
      response.writeHead(200, { 'content-type': 'application/json' });
      response.end('{"webSocketDebuggerUrl":"ws://127.0.0.1/devtools/browser/fake"}');
    } else { response.writeHead(404); response.end(); }
  });
  await new Promise((resolve, reject) => { cdp.once('error', reject); cdp.listen(0, '127.0.0.1', resolve); });
  await atomicWriteJson(fixture.portPath, { port: cdp.address().port, owner: 'app' });
  const model = createFakeHostModel(fixture, identity, watchRequests, requestReadBarrier);
  let revision = 0;
  let pollerStopped = false;
  let shuttingDown = false;
  let commands = Promise.resolve();
  let stopping;
  const stopPoller = () => stopping ||= (async () => {
    pollerStopped = true;
    await model.close();
  })();
  const configure = change => {
    const { field, action, key, value } = change ?? {};
    if (field === 'preparePublishesWorkspace' && action === 'set' && typeof value === 'boolean') {
      model.state.preparePublishesWorkspace = value;
    } else if (field === 'operationErrors' && typeof key === 'string') {
      if (action === 'set' && typeof value === 'string') model.state.operationErrors.set(key, value);
      else if (action === 'delete') model.state.operationErrors.delete(key);
      else throw new Error('invalid browser host fixture map configuration');
    } else if (['deferredOperations', 'deferredObservations', 'malformedOperationResponses'].includes(field) && typeof key === 'string') {
      if (action === 'add') model.state[field].add(key);
      else if (action === 'delete') model.state[field].delete(key);
      else throw new Error('invalid browser host fixture set configuration');
    } else throw new Error('invalid browser host fixture configuration');
    revision += 1;
    return { revision };
  };
  const execute = async request => {
    progress('control');
    if (shuttingDown) throw new Error('browser host fixture is shutting down');
    // The transferred control port owns this worker. Retirement must remain
    // possible after a timed-out configure was applied but its reply was lost.
    if (!['stop_polling', 'shutdown'].includes(request.command) && request.expectedRevision !== revision) {
      throw new Error('browser host fixture stale configuration revision');
    }
    switch (request.command) {
      case 'configure': return configure(request.change);
      case 'snapshot': return { revision, state: model.state };
      case 'stop_polling': await stopPoller(); return { pollerStopped };
      case 'shutdown': {
        shuttingDown = true;
        let modelFailure = null;
        try { await stopPoller(); } catch (error) { modelFailure = error; }
        modelFailure ||= model.state.failure;
        try {
          await new Promise((resolve, reject) => cdp.close(error => error ? reject(error) : resolve()));
        } catch (cleanup) {
          if (modelFailure) throw new Error(`${modelFailure.message}; browser host server cleanup also failed: ${cleanup.message}`, { cause: cleanup });
          throw cleanup;
        }
        return { pollerStopped, serverClosed: !cdp.listening, modelFailure: modelFailure?.message || null };
      }
      default: throw new Error('unknown browser host fixture command');
    }
  };
  control.on('message', request => {
    if (request?.command === 'ack_shutdown' && shuttingDown) { control.close(); return; }
    commands = commands.then(async () => {
      try {
        const value = await execute(request);
        if (request.command === 'configure') await waitForBarrier(configurationReplyBarrier);
        control.postMessage({ id: request.id, value });
      }
      catch (error) { control.postMessage({ id: request.id, error: error?.message || 'browser host fixture command failed' }); }
    });
  });
  control.postMessage({ ready: true });
}

if (!isMainThread && workerData?.kind === 'pinvou-browser-fixture-host-v1') {
  await serveHostWorker(workerData);
}
