import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import crypto from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { parse } from "yaml";
import { runReleaseProcess } from "../scripts/release-process.mjs";
import { buildReleaseSubprocessEnvironment } from "../scripts/controller-release-gate.mjs";

// Real ownership supervision is supported on Windows and Linux. The
// controlled clock and pre-start validation probes below remain portable.
const nativeSupported = ["win32", "linux"].includes(process.platform);
const registeredNativeCases = new Set();
const completedNativeCases = new Set();
function nativeReleaseTest(name, action) {
  registeredNativeCases.add(name);
  return (nativeSupported ? test : test.skip)(name, async (context) => {
    await action(context);
    completedNativeCases.add(name);
  });
}
test.after(() => {
  if (process.env.CI !== "true") return;
  if (process.platform !== "win32" && process.platform !== "linux") return;
  assert.ok(registeredNativeCases.size >= 10, "CI must retain the ten native ownership scenarios");
  assert.deepEqual([...completedNativeCases].sort(), [...registeredNativeCases].sort(),
    "every registered native ownership scenario must actually complete on supported CI platforms");
});

const guard = fileURLToPath(new URL("../../scripts/windows-e2e-process-guard.ps1", import.meta.url));
const orchestrator = fileURLToPath(new URL("../../scripts/run-windows-remote-control-e2e.ps1", import.meta.url));
const env = buildReleaseSubprocessEnvironment(process.env, "test");
const nodeHash = crypto.createHash("sha256").update(readFileSync(process.execPath)).digest("hex");
const quote = (text) => `'${text.replaceAll("'", "''")}'`;

const windowsNullInputProbe = `
import ctypes, os, sys
from ctypes import wintypes as w
kernel = ctypes.WinDLL('kernel32', use_last_error=True)
kernel.GetStdHandle.argtypes = [w.DWORD]
kernel.GetStdHandle.restype = w.HANDLE
kernel.GetFileType.argtypes = [w.HANDLE]
kernel.GetFileType.restype = w.DWORD
if kernel.GetFileType(kernel.GetStdHandle(-10)) != 2:
    sys.exit(41)
if os.read(0, 1) != b'':
    sys.exit(42)
`;
nativeReleaseTest("a command cannot consume its supervisor's private control input", async () => {
  if (process.platform === "win32") {
    const python = process.env.PINVOU_CI_PYTHON || "python";
    await runReleaseProcess(python, ["-B", "-c", windowsNullInputProbe], { env, python, timeoutMs: 5000 });
    return;
  }
  await runReleaseProcess(process.execPath, ["-e", "if(require('node:fs').readFileSync(0).length!==0)process.exitCode=41"],
    { env, timeoutMs: 5000 });
});

test("the Windows stdin probe rejects an ordinary pipe even when it has immediate EOF",
  { skip: process.platform !== "win32" }, () => {
    // The production-supervised case above supplies NUL. This independent
    // pipe also has EOF, so only the Win32 device assertion can reject it.
    const result = spawnSync(process.env.PINVOU_CI_PYTHON || "python", ["-B", "-c", windowsNullInputProbe],
      { env, input: "", encoding: "utf8", timeout: 5000, windowsHide: true });
    assert.equal(result.error, undefined);
    assert.equal(result.status, 41, result.stderr);
  });

test("CI preserves an interpreter usable by the filtered release environment", () => {
  const workflow = parse(readFileSync(new URL("../../.github/workflows/pr-check.yml", import.meta.url), "utf8"));
  for (const step of workflow.jobs["relay-test"].steps) {
    if (step.uses?.startsWith("actions/setup-python@")) {
      assert.equal(step.with?.["update-environment"], false,
        "Linux release contracts must retain the runner interpreter instead of a runtime needing filtered loader variables");
    }
  }
  const windows = workflow.jobs["release-contract-test"].steps.filter((step) => step.uses?.startsWith("actions/setup-python@"));
  assert.equal(windows.length, 1);
  assert.equal(windows[0].if, "${{ runner.os == 'Windows' && runner.environment == 'github-hosted' }}");
  const release = parse(readFileSync(new URL("../../.github/workflows/release-packages.yml", import.meta.url), "utf8"));
  const releaseSetup = release.jobs["remote-control-release-gate"].steps.filter((step) => step.uses?.startsWith("actions/setup-python@"));
  assert.equal(releaseSetup.length, 1);
  assert.equal(releaseSetup[0].if, "${{ runner.environment == 'github-hosted' }}");
});

test("supervisor receipt failures retain known aborts and expose only closed diagnostics", async () => {
  const key = Symbol.for("pinvou.release.runner.receipt.probe");
  for (const abort of [null, "cancelled", "timeout"]) {
   for (const scenario of ["missing", "invalid", "unconfirmed", "reported-timeout", "primary", "start-error",
     "unknown-reason", "false-reason", "object-reason", "string-cleanup", "missing-cleanup", "wrong-nonce", "invalid-supervisor-code",
     "false-execution-error", "object-execution-error", "missing-execution-error"]) {
    const child = Object.assign(new EventEmitter(), { pid: scenario === "start-error" ? undefined : 1234, stdin: new PassThrough(),
      stdout: new PassThrough(), stderr: new PassThrough(), unref: () => {}, kill: () => true });
    let next = 0;
    const timers = new Map();
    globalThis[key] = { spawn: () => child,
      setTimeout: (fn, delay) => { const id = ++next; timers.set(id, { fn, delay }); return id; },
      clearTimeout: (id) => timers.delete(id) };
    try {
      let source = readFileSync(new URL("../scripts/release-process.mjs", import.meta.url), "utf8");
      assert.ok(source.includes('import { spawn } from "node:child_process";'));
      source = source.replace('import { spawn } from "node:child_process";',
        'const {spawn,setTimeout,clearTimeout}=globalThis[Symbol.for("pinvou.release.runner.receipt.probe")];');
      assert.match(source, /^const supervisor = .+;$/mu);
      source = source.replace(/^const supervisor = .+;$/mu, 'const supervisor="controlled-supervisor";');
      const module = await import(`data:text/javascript;base64,${Buffer.from(source + `\n//${crypto.randomUUID()}`).toString("base64")}`);
      const controller = new AbortController();
      const running = module.runReleaseProcess(process.execPath, [], { env, timeoutMs: 1000, signal: controller.signal })
        .then(() => null, (error) => error);
      const request = JSON.parse(child.stdin.read().toString());
      if (abort === "cancelled") controller.abort();
      if (abort === "timeout") {
        const businessTimers = [...timers.values()].filter((timer) => timer.delay === 1000);
        assert.equal(businessTimers.length, 1);
        businessTimers[0].fn();
      }
      if (scenario === "start-error") child.emit("error", new Error("PRIVATE_DIAGNOSTIC_SENTINEL"));
      else {
        const receipt = { nonce: request.nonce, execution_error: null, exit_code: 86, forced: true,
          root_exit_code: scenario === "primary" ? 23 : 143,
          primary_exit_code: scenario === "primary" ? 23 : null, cleanup_confirmed: false,
          reason: scenario === "reported-timeout" ? "timeout" : abort };
        if (["unknown-reason", "false-reason", "object-reason", "wrong-nonce", "invalid-supervisor-code",
          "invalid", "false-execution-error", "object-execution-error", "missing-execution-error"].includes(scenario)) {
          Object.assign(receipt, { exit_code: 0, root_exit_code: 0, forced: false, cleanup_confirmed: true, reason: null });
        }
        if (scenario === "unknown-reason") receipt.reason = "PRIVATE_DIAGNOSTIC_SENTINEL";
        if (scenario === "false-reason") receipt.reason = false;
        if (scenario === "object-reason") receipt.reason = { private: "PRIVATE_DIAGNOSTIC_SENTINEL" };
        if (scenario === "invalid") receipt.execution_error = "PRIVATE_DIAGNOSTIC_SENTINEL";
        if (scenario === "false-execution-error") receipt.execution_error = false;
        if (scenario === "object-execution-error") receipt.execution_error = { private: "PRIVATE_DIAGNOSTIC_SENTINEL" };
        if (scenario === "missing-execution-error") delete receipt.execution_error;
        if (scenario === "string-cleanup") receipt.cleanup_confirmed = "true";
        if (scenario === "missing-cleanup") delete receipt.cleanup_confirmed;
        if (scenario === "wrong-nonce") receipt.nonce = `other-${request.nonce}`;
        child.stderr.write(scenario === "missing" ? "PRIVATE_DIAGNOSTIC_SENTINEL" :
          "PINVOU_RELEASE_PROCESS " + JSON.stringify(receipt) + "\n");
        child.emit("close", scenario === "missing" ? 127 : scenario === "invalid-supervisor-code" ? 0x100000000 : 0);
      }
      const error = await running;
      const expectedAbort = { cancelled: "release_subgate_cancelled", timeout: "release_subgate_timeout" }[abort];
      const expected = scenario === "primary" ? "release_subgate_failed" : expectedAbort
        || (scenario === "reported-timeout" ? "release_subgate_timeout"
          : scenario === "start-error" ? "release_subgate_start_failed" : "release_subgate_cleanup_unconfirmed");
      assert.equal(error.code, expected, `${abort}/${scenario}`);
      assert.equal(error.cleanupConfirmed, scenario === "start-error");
      if (scenario !== "start-error") assert.equal(error.cleanupError, "release_subgate_cleanup_unconfirmed");
      if (["missing", "invalid", "unknown-reason", "false-reason", "object-reason", "string-cleanup", "missing-cleanup", "wrong-nonce", "invalid-supervisor-code",
        "false-execution-error", "object-execution-error", "missing-execution-error"].includes(scenario)) {
        assert.equal(error.receiptStatus, ["missing", "wrong-nonce"].includes(scenario) ? "missing" : "invalid");
        assert.equal(error.supervisorExitCode, scenario === "missing" ? 127 : scenario === "invalid-supervisor-code" ? null : 0);
      }
      if (scenario === "primary") assert.equal(error.exitCode, 23, "a valid earlier primary must win over a later abort");
      assert.doesNotMatch(JSON.stringify(error), /PRIVATE_DIAGNOSTIC_SENTINEL/u);
      assert.equal(error.cause, undefined);
    } finally {
      child.stdin.destroy(); child.stdout.destroy(); child.stderr.destroy();
      delete globalThis[key];
    }
   }
  }
});

test("legal execution-error receipts retain classification, primary exits and independent cleanup evidence", async () => {
  const key = Symbol.for("pinvou.release.runner.execution.receipt.probe");
  for (const executionError of ["start_failed", "execution_failed"]) {
    for (const cleanupConfirmed of [true, false]) {
      for (const abort of [null, "cancelled", "timeout"]) {
        for (const primary of [null, 23]) {
          const child = Object.assign(new EventEmitter(), { pid: 1234, stdin: new PassThrough(),
            stdout: new PassThrough(), stderr: new PassThrough(), unref: () => {}, kill: () => true });
          let next = 0;
          const timers = new Map();
          globalThis[key] = { spawn: () => child,
            setTimeout: (fn, delay) => { const id = ++next; timers.set(id, { fn, delay }); return id; },
            clearTimeout: (id) => timers.delete(id) };
          try {
            let source = readFileSync(new URL("../scripts/release-process.mjs", import.meta.url), "utf8");
            assert.ok(source.includes('import { spawn } from "node:child_process";'));
            source = source.replace('import { spawn } from "node:child_process";',
              'const {spawn,setTimeout,clearTimeout}=globalThis[Symbol.for("pinvou.release.runner.execution.receipt.probe")];');
            assert.match(source, /^const supervisor = .+;$/mu);
            source = source.replace(/^const supervisor = .+;$/mu, 'const supervisor="controlled-supervisor";');
            const module = await import(`data:text/javascript;base64,${Buffer.from(source + `\n//${crypto.randomUUID()}`).toString("base64")}`);
            const controller = new AbortController();
            const running = module.runReleaseProcess(process.execPath, [], { env, timeoutMs: 1000, signal: controller.signal })
              .then(() => null, (error) => error);
            const request = JSON.parse(child.stdin.read().toString());
            if (abort === "cancelled") controller.abort();
            if (abort === "timeout") [...timers.values()].find(timer => timer.delay === 1000).fn();
            // Exercise the reader's valid grammar in both cleanup states.
            // The current adapter normally emits start_failed/true and
            // execution_failed/false; neither supplies an invented exit code.
            const receipt = { nonce: request.nonce, execution_error: executionError,
              exit_code: null, forced: false, root_exit_code: primary,
              primary_exit_code: primary, cleanup_confirmed: cleanupConfirmed, reason: abort };
            child.stderr.write("PINVOU_RELEASE_PROCESS " + JSON.stringify(receipt) + "\n");
            child.emit("close", 0);
            const error = await running;
            const expected = primary !== null ? "release_subgate_failed"
              : abort ? `release_subgate_${abort}` : `release_subgate_${executionError}`;
            assert.equal(error?.code, expected, `${executionError}/${cleanupConfirmed}/${abort}/${primary}`);
            assert.equal(error.cleanupConfirmed, cleanupConfirmed);
            assert.equal(error.exitCode, primary !== null ? primary : !cleanupConfirmed || abort ? null : undefined);
            assert.equal(error.cleanupError, cleanupConfirmed ? undefined : "release_subgate_cleanup_unconfirmed");
            assert.equal(error.cause, undefined);
            assert.equal(timers.size, 0);
          } finally {
            child.stdin.destroy(); child.stdout.destroy(); child.stderr.destroy(); delete globalThis[key];
          }
        }
      }
    }
  }
});

test("output read errors settle through bounded cleanup and retain earlier primary errors", async () => {
  // No OS child is created here: only spawn, streams and the clock are
  // controlled. The real runner decides cancellation, receipts and results.
  const key = Symbol.for("pinvou.release.runner.output.probe");
  for (const stream of ["stdout", "stderr"]) {
   for (const priorStop of [null, "cancelled", "timeout"]) {
    for (const ending of ["receipt", "primary", "unconfirmed", "missing-close"]) {
      let now = 0, next = 0;
      const timers = new Map(), kills = [];
      const child = Object.assign(new EventEmitter(), { pid: 1234, stdin: new PassThrough(),
        stdout: new PassThrough(), stderr: new PassThrough(), unref: () => {},
        kill: (signal) => { kills.push(signal); return true; } });
      const clock = {
        spawn: () => child,
        setTimeout: (fn, ms) => { const id = ++next; timers.set(id, { fn, at: now + ms }); return id; },
        clearTimeout: (id) => timers.delete(id),
        advance: (ms) => {
          const until = now + ms;
          while (true) {
            const due = [...timers].filter(([, value]) => value.at <= until).sort((a, b) => a[1].at - b[1].at)[0];
            if (!due) break;
            timers.delete(due[0]); now = due[1].at; due[1].fn();
          }
          now = until;
        },
      };
      globalThis[key] = clock;
      try {
        let source = readFileSync(new URL("../scripts/release-process.mjs", import.meta.url), "utf8");
        assert.ok(source.includes('import { spawn } from "node:child_process";'));
        source = source.replace('import { spawn } from "node:child_process";',
          'const {spawn,setTimeout,clearTimeout}=globalThis[Symbol.for("pinvou.release.runner.output.probe")];');
        assert.match(source, /^const supervisor = .+;$/mu);
        source = source.replace(/^const supervisor = .+;$/mu, 'const supervisor="controlled-supervisor";');
        const module = await import(`data:text/javascript;base64,${Buffer.from(source + `\n//${crypto.randomUUID()}`).toString("base64")}`);
        const controller = new AbortController();
        const running = module.runReleaseProcess(process.execPath, [], { env, timeoutMs: 1000, signal: controller.signal })
          .then(() => ({ success: true }), (error) => ({ error }));
        const request = JSON.parse(child.stdin.read().toString());
        if (priorStop === "cancelled") controller.abort();
        if (priorStop === "timeout") clock.advance(1000);
        const stopped = priorStop !== null || stream === "stdout";
        // A confirmed stop returns supervision code 143 independently of
        // the command's exit; an unconfirmed retirement returns 86 instead.
        const receipt = { nonce: request.nonce, execution_error: null,
          exit_code: ending === "unconfirmed" ? 86 : stopped ? 143 : ending === "primary" ? 23 : 0,
          forced: ending === "unconfirmed" || stopped,
          root_exit_code: ending === "primary" ? 23 : stopped ? 86 : 0,
          primary_exit_code: ending === "primary" ? 23 : null, cleanup_confirmed: ending !== "unconfirmed",
          reason: priorStop || (stream === "stdout" ? "cancelled" : null) };
        // A stderr failure can follow an already buffered final receipt;
        // stdout failure leaves stderr able to deliver the stop acknowledgement.
        if (ending !== "missing-close" && stream === "stderr") child.stderr.write("PINVOU_RELEASE_PROCESS " + JSON.stringify(receipt) + "\n");
        child[stream].emit("error", new Error("PRIVATE_DIAGNOSTIC_SENTINEL"));
        if (ending === "missing-close") {
          clock.advance(10000);
          child[stream === "stdout" ? "stderr" : "stdout"].emit("error", new Error("PRIVATE_DIAGNOSTIC_SENTINEL"));
          clock.advance(10000);
          assert.deepEqual(kills, ["SIGKILL"], "repeated read faults must not extend the first cleanup deadline");
        } else {
          if (stream === "stdout") child.stderr.write("PINVOU_RELEASE_PROCESS " + JSON.stringify(receipt) + "\n");
          child.emit("close", 0);
        }
        const actual = await Promise.race([running, new Promise((resolve) => setImmediate(() => resolve({ unsettled: true })))]);
        assert.equal(actual.unsettled, undefined);
        const expected = ending === "primary" ? "release_subgate_failed" : priorStop
          ? `release_subgate_${priorStop}` : "release_subgate_output_failed";
        assert.equal(actual.error?.code, expected, `${stream}/${priorStop}/${ending}`);
        assert.equal(actual.error.cleanupConfirmed, ending !== "missing-close" && ending !== "unconfirmed");
        if (ending === "missing-close" || ending === "unconfirmed") assert.equal(actual.error.cleanupError, "release_subgate_cleanup_unconfirmed");
        if (ending === "primary") assert.equal(actual.error.exitCode, 23);
        assert.doesNotMatch(JSON.stringify(actual.error), /PRIVATE_DIAGNOSTIC_SENTINEL/u);
        assert.equal(actual.error.cause, undefined);
        assert.equal(timers.size, 0);
        child.stdout.emit("error", new Error("PRIVATE_DIAGNOSTIC_SENTINEL"));
        child.stderr.emit("error", new Error("PRIVATE_DIAGNOSTIC_SENTINEL"));
        child.emit("close", 0);
        assert.equal(await running, actual, "late events cannot replace the first result");
      } finally {
        child.stdin.destroy(); child.stdout.destroy(); child.stderr.destroy();
        delete globalThis[key];
      }
    }
   }
  }
});

nativeReleaseTest("real output faults retain the primary error through the supervisor's stop receipt", async () => {
  // Only the read-error event is injected. Python, its cancellation protocol,
  // the command and exact process-tree retirement all execute normally.
  const key = Symbol.for("pinvou.release.runner.native.output.probe");
  for (const stream of ["stdout", "stderr"]) {
    let injected = false, receipt = null;
    globalThis[key] = {
      spawn: (...args) => {
        const child = spawn(...args);
        let output = "", diagnostics = "";
        child.stdout.on("data", (chunk) => {
          output = (output + chunk.toString("utf8")).slice(-1024);
          if (!injected && output.includes("owned-output-ready\n")) {
            injected = true;
            queueMicrotask(() => child[stream].emit("error", new Error("PRIVATE_DIAGNOSTIC_SENTINEL")));
          }
        });
        child.stderr.on("data", (chunk) => {
          diagnostics = (diagnostics + chunk.toString("utf8")).slice(-32768);
          for (const line of diagnostics.split(/\r?\n/u)) {
            if (!line.startsWith("PINVOU_RELEASE_PROCESS ")) continue;
            try { receipt = JSON.parse(line.slice("PINVOU_RELEASE_PROCESS ".length)); }
            catch { /* wait for the complete actual receipt */ }
          }
        });
        return child;
      },
    };
    try {
      let source = readFileSync(new URL("../scripts/release-process.mjs", import.meta.url), "utf8");
      assert.ok(source.includes('import { spawn } from "node:child_process";'));
      source = source.replace('import { spawn } from "node:child_process";',
        'const {spawn}=globalThis[Symbol.for("pinvou.release.runner.native.output.probe")];');
      assert.match(source, /^const supervisor = .+;$/mu);
      source = source.replace(/^const supervisor = .+;$/mu,
        `const supervisor=${JSON.stringify(fileURLToPath(new URL("../../scripts/release-owned-process.py", import.meta.url)))};`);
      const module = await import(`data:text/javascript;base64,${Buffer.from(source + `\n//${crypto.randomUUID()}`).toString("base64")}`);
      const error = await module.runReleaseProcess(process.execPath, ["-e",
        "process.stdout.write('owned-output-ready\\n');setInterval(()=>{},1000)"], { env, timeoutMs: 15000 })
        .then(() => null, (failure) => failure);
      assert.equal(injected, true, "the command must be running before the read fault");
      assert.equal(receipt?.exit_code, 143, "use the real supervision result instead of inventing a zero exit");
      assert.equal(receipt.primary_exit_code, null);
      assert.equal(receipt.reason, "cancelled");
      assert.equal(receipt.cleanup_confirmed, true);
      assert.equal(error?.code, "release_subgate_output_failed", stream);
      assert.equal(error.cleanupConfirmed, true);
      assert.doesNotMatch(JSON.stringify(error), /PRIVATE_DIAGNOSTIC_SENTINEL/u);
      // A completed read failure cannot poison a later independent operation.
      await module.runReleaseProcess(process.execPath, ["-e", "process.stdout.write('next-operation-ok')"], { env, timeoutMs: 5000 });
    } finally { delete globalThis[key]; }
  }
});

test("validated primary receipts survive a missing close and bounded diagnostic eviction", async () => {
  // No OS child exists in this fault probe. Only spawn and the clock are
  // controlled; execute the actual runner body, including its unchanged 20s
  // budget, and never emit close. Even a claimed success receipt cannot
  // independently establish retirement.
  const key = Symbol.for("pinvou.release.runner.clock.probe");
  for (const mode of ["timeout", "cancelled", "stdout", "stderr"]) {
   for (const scenario of ["missing", "primary", "wrong-nonce", "invalid-primary", "incomplete", "success"]) {
    let now = 0, next = 0;
    const timers = new Map();
    const kills = [];
    const child = Object.assign(new EventEmitter(), { pid: 1234, stdin: new PassThrough(),
      stdout: new PassThrough(), stderr: new PassThrough(),
      kill: (signal) => { kills.push(signal); return false; }, unref: () => {} });
    const clock = {
      spawn: () => child,
      setTimeout: (fn, ms) => { const id = ++next; timers.set(id, { fn, at: now + ms }); return id; },
      clearTimeout: (id) => timers.delete(id),
      advance: (ms) => {
        const end = now + ms;
        while (true) {
          const due = [...timers].filter(([, value]) => value.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
          if (!due) break;
          timers.delete(due[0]); now = due[1].at; due[1].fn();
        }
        now = end;
      },
    };
    globalThis[key] = clock;
    try {
      let source = readFileSync(new URL("../scripts/release-process.mjs", import.meta.url), "utf8");
      assert.ok(source.includes('import { spawn } from "node:child_process";'));
      source = source.replace('import { spawn } from "node:child_process";',
        'const {spawn,setTimeout,clearTimeout}=globalThis[Symbol.for("pinvou.release.runner.clock.probe")];');
      assert.match(source, /^const supervisor = .+;$/mu);
      source = source.replace(/^const supervisor = .+;$/mu, 'const supervisor="controlled-supervisor";');
      const module = await import(`data:text/javascript;base64,${Buffer.from(source + `\n//${crypto.randomUUID()}`).toString("base64")}`);
      const abort = new AbortController();
      const result = module.runReleaseProcess(process.execPath, [], { env, timeoutMs: 1000, signal: abort.signal })
        .then(() => ({ success: true }), (error) => ({ error }));
      const request = JSON.parse(child.stdin.read().toString());
      if (scenario !== "missing") {
        const receipt = { nonce: scenario === "wrong-nonce" ? `other-${request.nonce}` : request.nonce,
          execution_error: null, exit_code: scenario === "success" ? 0 : 86, forced: scenario !== "success",
          root_exit_code: scenario === "success" ? 0 : scenario === "invalid-primary" ? 24 : 23,
          primary_exit_code: scenario === "success" ? null : 23,
          cleanup_confirmed: scenario === "success", reason: null };
        const line = "PINVOU_RELEASE_PROCESS " + JSON.stringify(receipt);
        // A partial JSON document must not become evidence. A complete one
        // is delivered in two chunks, then evicted from the diagnostic ring.
        child.stderr.write(line.slice(0, -1));
        assert.equal(child.stdin.writableEnded, false, "a partial receipt must not be acknowledged");
        if (scenario !== "incomplete") child.stderr.write(line.slice(-1) + "\n");
        assert.equal(child.stdin.writableEnded, scenario === "primary" || scenario === "success",
          "only a complete, fully validated correlated receipt may be acknowledged");
        if (scenario === "primary") {
          child.stderr.write("PINVOU_RELEASE_PROCESS " + JSON.stringify({ ...receipt,
            root_exit_code: 24, primary_exit_code: 24 }) + "\n");
        }
      }
      child.stdout.write(Buffer.alloc(65536, 120));
      child.stderr.write(Buffer.alloc(65536, 120));
      child.stderr.write("\n");
      if (mode === "cancelled") abort.abort();
      if (mode === "stdout" || mode === "stderr") child[mode].emit("error", new Error("PRIVATE_DIAGNOSTIC_SENTINEL"));
      clock.advance(mode === "timeout" ? 21000 : 20000);
      const actual = await Promise.race([result, new Promise((resolve) => setImmediate(() => resolve({ unsettled: true })))]);
      assert.equal(actual.unsettled, undefined, "cleanup cannot depend on a missing close event or a later timeout resetting cancellation");
      const primary = scenario === "primary";
      assert.equal(actual.error.code, primary ? "release_subgate_failed"
        : mode === "stdout" || mode === "stderr" ? "release_subgate_output_failed" : `release_subgate_${mode}`,
      `${mode}/${scenario}`);
      assert.equal(actual.error.exitCode, primary ? 23 : undefined);
      assert.equal(actual.error.cleanupConfirmed, false);
      assert.equal(actual.error.cleanupError, "release_subgate_cleanup_unconfirmed");
      assert.deepEqual(kills, ["SIGKILL"]);
      assert.equal(timers.size, 0);
      assert.doesNotMatch(JSON.stringify(actual.error), /PRIVATE_DIAGNOSTIC_SENTINEL/u);
      child.emit("close", 0);
      assert.equal(await result, actual, "a late close must not replace the first failure or confirm cleanup");
      assert.ok(child.stdout.destroyed && child.stderr.destroyed && child.stdin.destroyed);
    } finally { delete globalThis[key]; }
   }
  }
});

nativeReleaseTest("real release runner drains both pipes beyond capacity without exposing diagnostics", async () => {
  await runReleaseProcess(process.execPath, ["-e", `
    const data=Buffer.alloc(2*1024*1024,120);
    process.stderr.write(data);process.stdout.write(data);
    process.stderr.write('PRIVATE_DIAGNOSTIC_SENTINEL');
  `], { env, timeoutMs: 15000 });
});

nativeReleaseTest("real release runner preserves nonzero exit while redacting stderr", async () => {
  await assert.rejects(runReleaseProcess(process.execPath, ["-e", `
    process.stderr.write('PRIVATE_DIAGNOSTIC_SENTINEL');process.exitCode=23;
  `], { env, timeoutMs: 15000 }), (error) => {
    assert.equal(error.code, "release_subgate_failed");
    assert.equal(error.exitCode, 23);
    assert.equal(error.cleanupConfirmed, true);
    assert.doesNotMatch(JSON.stringify(error), /PRIVATE_DIAGNOSTIC_SENTINEL/u);
    assert.equal(error.cause, undefined);
    return true;
  });
});

nativeReleaseTest("real release timeout retires a detached descendant within a finite budget", async () => {
  const began = Date.now();
  await assert.rejects(runReleaseProcess(process.execPath, ["-e", `
    require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],
      {detached:true,stdio:'ignore'}).unref();
    setInterval(()=>{},1000);
  `], { env, timeoutMs: 1000 }), (error) => {
    assert.equal(error.code, "release_subgate_timeout");
    assert.equal(error.cleanupConfirmed, true);
    return true;
  });
  assert.ok(Date.now() - began < 18000);
});

nativeReleaseTest("real release cancellation is bounded and never becomes success", async () => {
  const controller = new AbortController();
  const running = runReleaseProcess(process.execPath, ["-e", "setInterval(()=>{},1000)"],
    { env, timeoutMs: 15000, signal: controller.signal });
  controller.abort();
  await assert.rejects(running, (error) => {
    assert.equal(error.code, "release_subgate_cancelled");
    assert.equal(error.cleanupConfirmed, true);
    return true;
  });
});

nativeReleaseTest("a failed root exit remains the primary error during later descendant retirement", async () => {
  await assert.rejects(runReleaseProcess(process.execPath, ["-e", `
    require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],
      {detached:true,stdio:'ignore'}).unref();
    process.exitCode=23;
  `], { env, timeoutMs: 1500 }), (error) => {
    assert.equal(error.code, "release_subgate_failed");
    assert.equal(error.exitCode, 23);
    assert.equal(error.cleanupConfirmed, true);
    return true;
  });
});

nativeReleaseTest("a successful root cannot hide an unexpected surviving descendant", async () => {
  await assert.rejects(runReleaseProcess(process.execPath, ["-e", `
    require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],
      {detached:true,stdio:'ignore'}).unref();
  `], { env, timeoutMs: 15000 }), (error) => {
    assert.equal(error.code, "release_subgate_leftover_process");
    assert.equal(error.exitCode, 0);
    assert.equal(error.cleanupConfirmed, true);
    return true;
  });
});

test("release runner pre-start validation rejects cancellation, budget and environment", async () => {
  await assert.rejects(runReleaseProcess("does-not-exist", [],
    { env, signal: AbortSignal.abort() }), { code: "release_subgate_cancelled" });
  await assert.rejects(runReleaseProcess("does-not-exist", [], { env, timeoutMs: 0 }),
    { code: "release_subgate_timeout_invalid" });
  await assert.rejects(runReleaseProcess("does-not-exist", []),
    { code: "release_subgate_environment_missing" });
  for (const invalid of [{ key: 1 }, { key: "bad\0value" }, { ["bad\0key"]: "value" }]) {
    await assert.rejects(runReleaseProcess("does-not-exist", [], { env: invalid }),
      { code: "release_subgate_environment_invalid" });
  }
});

nativeReleaseTest("a missing command reports a bounded start failure without a leaked child", async () => {
  await assert.rejects(runReleaseProcess("pinvou-missing-owned-command-1449", [], { env, timeoutMs: 5000 }),
    { code: "release_subgate_start_failed", cleanupConfirmed: true });
  await assert.rejects(runReleaseProcess(process.execPath, ["bad\0argument"], { env }),
    { code: "release_subgate_command_invalid" });
});

nativeReleaseTest("owned command retirement does not terminate an unrelated live sentinel", async () => {
  const sentinel = spawn(process.execPath, ["-e", "setInterval(()=>{},1000)"], { stdio: "ignore", windowsHide: true });
  try {
    await once(sentinel, "spawn");
    await assert.rejects(runReleaseProcess(process.execPath, ["-e", "setInterval(()=>{},1000)"],
      { env, timeoutMs: 500 }), { code: "release_subgate_timeout", cleanupConfirmed: true });
    assert.equal(sentinel.exitCode, null);
    assert.equal(sentinel.signalCode, null);
  } finally {
    if (sentinel.exitCode === null && sentinel.signalCode === null) {
      const closed = once(sentinel, "close"); sentinel.kill(); await closed;
    }
  }
});

for (const shell of ["powershell.exe", "pwsh"]) {
  test(`production Windows process entry drains both pipes and keeps argv/env [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        $env:PINVOU_SECRET_SENTINEL='PRIVATE_ENV_SENTINEL';
        $code=${quote(`process.stderr.write('e'.repeat(2*1024*1024));process.stdout.write('o'.repeat(2*1024*1024));if(process.env.PINVOU_SECRET_SENTINEL)process.exitCode=9;`)};
        $r=Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e',$code) -TimeoutMilliseconds 15000;
        if($r.ExitCode -ne 0 -or $r.StdOut.Length -ne 2097152 -or $r.StdErr.Length -ne 2097152){throw 'pipe-drain-failed'};
        $argsCode=${quote("process.stdout.write(JSON.stringify(process.argv.slice(1)))")};
        $a=Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e',$argsCode,'','a b','a"b',('x'+[char]92)) -TimeoutMilliseconds 15000;
        [Console]::Out.Write($a.StdOut)`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.deepEqual(JSON.parse(result.stdout), ["", "a b", 'a"b', "x" + String.fromCharCode(92)]);
      assert.doesNotMatch(result.stderr, /PRIVATE_ENV_SENTINEL/u);
    });

  test(`production local entry delegates to the same bounded process scope [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        $tokens=$null;$errors=$null;$ast=[Management.Automation.Language.Parser]::ParseFile(${quote(orchestrator)},[ref]$tokens,[ref]$errors);
        if($errors.Count){throw 'production-parse-failed'};
        $f=$ast.Find({param($n)$n -is [Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Invoke-PinvouLocalWhitelistedProcess'},$true);Invoke-Expression $f.Extent.Text;
        $r=Invoke-PinvouLocalWhitelistedProcess -FilePath ${quote(process.execPath)} -Arguments @('-e',${quote("process.stderr.write('e'.repeat(1048576));process.stdout.write('o'.repeat(1048576))")}) -TimeoutMilliseconds 15000;
        if($r.ExitCode -ne 0 -or $r.StdOut.Length -ne 1048576 -or $r.StdErr.Length -ne 1048576){throw 'local-drain-failed'};[Console]::Out.Write('ok')`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, "ok");
    });

  test(`production Windows timeout kills its exact detached descendants [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        try { Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e',${quote("require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{detached:true,stdio:'ignore'}).unref();setInterval(()=>{},1000)")}) -TimeoutMilliseconds 500 | Out-Null; throw 'timeout-not-detected' }
        catch { $e=$_.Exception;while($e.InnerException){$e=$e.InnerException};if($e.Message -ne 'e2e_process_timeout' -or $e.CleanupFailed){throw};[Console]::Out.Write('retired') }`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, "retired");
    });

  test(`a zero root cannot hand off a service still holding command pipes [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const code = "require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{detached:true,stdio:'inherit'}).unref();process.exit(0)";
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        try { Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e',${quote(code)}) -TimeoutMilliseconds 1000 -RetainSuccessfulScope | Out-Null;throw 'incomplete-pipes-accepted' }
        catch { $e=$_.Exception;while($e.InnerException){$e=$e.InnerException};if($e.Message -ne 'e2e_process_timeout' -or $e.ChildExitCode -ne 0 -or $e.CleanupFailed){throw} };
        $next=Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e','process.stdout.write("next")') -TimeoutMilliseconds 5000;
        if($next.ExitCode -ne 0 -or $next.StdOut -ne 'next'){throw 'next-command-failed'};[Console]::Out.Write('retired-and-recovered')`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, "retired-and-recovered");
    });

  test(`Windows capture rejects output beyond its actual character limit [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        try { Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e',${quote("process.stdout.write('x'.repeat(17*1024*1024));setInterval(()=>{},1000)")}) -TimeoutMilliseconds 15000 | Out-Null;throw 'oversize-output-accepted' }
        catch { $e=$_.Exception;while($e.InnerException){$e=$e.InnerException};if($e.Message -ne 'e2e_process_output_limit' -or $e.CleanupFailed){throw};[Console]::Out.Write('oversize-retired') }`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, "oversize-retired");
    });

  test(`successful intentional Windows service remains owned until explicit cleanup [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        $r=Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e',${quote("require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{detached:true,stdio:'ignore'}).unref()")}) -TimeoutMilliseconds 15000 -RetainSuccessfulScope;
        if(-not $r.OwnedScope -or $r.OwnedScope.ActiveProcesses -ne 1){throw 'service-not-retained'};
        $r.OwnedScope.Dispose();if(-not $r.OwnedScope.IsClosed){throw 'scope-not-closed'};[Console]::Out.Write('retained-and-retired')`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, "retained-and-retired");
    });

  test(`production Windows cancellation follows an actual child-ready barrier [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        Add-Type -TypeDefinition 'using System;using System.IO;using System.Threading;using System.Threading.Tasks;public static class PinvouCancelProbe {public static Task Watch(string path,CancellationTokenSource source){return Task.Run(()=>{var end=DateTime.UtcNow.AddSeconds(10);while(!File.Exists(path)){if(DateTime.UtcNow>end)throw new Exception("child-ready-missing");Thread.Sleep(5);}source.Cancel();});}}';
        $warm=Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e','');
        $marker=Join-Path $env:PINVOU_CI_FIXTURE_ROOT ([Guid]::NewGuid().ToString('N')+'.ready');
        $cts=[Threading.CancellationTokenSource]::new();$watch=[PinvouCancelProbe]::Watch($marker,$cts);
        $code='require("node:fs").writeFileSync('+($marker|ConvertTo-Json -Compress)+',"ready");setInterval(()=>{},1000)';
        try { Invoke-PinvouWhitelistedProcess -FilePath ${quote(process.execPath)} -ExpectedSha256 '${nodeHash}' -Arguments @('-e',$code) -TimeoutMilliseconds 15000 -CancellationToken $cts.Token | Out-Null;throw 'cancellation-not-detected' }
        catch { $e=$_.Exception;while($e.InnerException){$e=$e.InnerException};if($e.Message -ne 'e2e_process_cancelled' -or $e.CleanupFailed){throw} }
        finally { $cts.Dispose() };
        $watch.GetAwaiter().GetResult();if(-not (Test-Path -LiteralPath $marker)){throw 'child-never-ready'};[Console]::Out.Write('cancelled-and-retired')`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, "cancelled-and-retired");
    });

  test(`cleanup failure preserves an existing error and remains fatal on its own [${shell}]`,
    { skip: process.platform !== "win32" }, () => {
      const script = `$ErrorActionPreference='Stop'; . ${quote(guard)};
        $primary=$null;try{throw 'primary_fixture_failure'}catch{$primary=$_};
        Invoke-PinvouCleanupPreservingFailure -PrimaryFailure $primary -FailureCode 'e2e_fixture_cleanup_failed' -Cleanup {throw 'secondary_fixture_failure'};
        if($primary.Exception.Message -ne 'primary_fixture_failure' -or $primary.Exception.Data['e2e_fixture_cleanup_failed'] -ne $true){throw 'primary-lost'};
        try{Invoke-PinvouCleanupPreservingFailure -FailureCode 'e2e_fixture_cleanup_failed' -Cleanup {throw 'cleanup_only_fixture_failure'};throw 'cleanup-error-hidden'}catch{if($_.Exception.Message -ne 'cleanup_only_fixture_failure'){throw}};
        [Console]::Out.Write('primary-preserved')`;
      const result = ps(shell, script);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(result.stdout, "primary-preserved");
      assert.equal(result.stderr.trim(), "e2e_fixture_cleanup_failed");
    });
}

function ps(shell, script) {
  const environment = { ...process.env };
  for (const key of Object.keys(environment)) if (key.toUpperCase() === "PSMODULEPATH") delete environment[key];
  return spawnSync(shell, ["-NoProfile", "-NonInteractive", "-EncodedCommand", Buffer.from(script, "utf16le").toString("base64")],
    { env: environment, encoding: "utf8", timeout: 30000, maxBuffer: 8 * 1024 * 1024, windowsHide: true });
}
