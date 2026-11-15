import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { parseSelection, runSelected, selectShard } from "../run-frontend-smokes.mjs";
import { FULL_FRONTEND_SMOKES, selectFrontendSmokes } from "../select-frontend-smokes.mjs";

const item = (kind, target) => ({ kind, target });
const label = ({ kind, target }) => `${kind}:${target}`;

test('four shards conserve every selected identity exactly once, including future entries', () => {
  const selections = [FULL_FRONTEND_SMOKES,
    selectFrontendSmokes(['pinvou3-app/src/features/remote-control/RemoteProxyPanel.jsx']),
    ...[1, 3, 4, 5, 59].map(count => Array.from({ length: count }, (_, i) => item('node', `future-${i}`)))];
  for (const selected of selections) {
    const shards = Array.from({ length: 4 }, (_, i) => selectShard(selected, `${i + 1}/4`));
    assert.deepEqual(shards.flat().map(label).sort(), selected.map(label).sort());
    assert.equal(new Set(shards.flat().map(label)).size, selected.length);
    assert.ok(Math.max(...shards.map(part => part.length)) - Math.min(...shards.map(part => part.length)) <= 1);
    assert.deepEqual(shards, Array.from({ length: 4 }, (_, i) => selectShard(selected, `${i + 1}/4`)));
  }
});

test('invalid coordinates, empty global input and duplicates cannot become empty shards', () => {
  for (const coordinate of ['0/4', '5/4', '1/0', '-1/4', '1.5/4', '1/4x', '1/04', '1/9007199254740992']) {
    assert.throws(() => selectShard([item('node', 'a')], coordinate), /invalid smoke shard/);
  }
  assert.throws(() => selectShard([], '4/4'), /empty global/);
  assert.throws(() => selectShard([item('node', 'a'), item('node', 'a')], '4/4'), /duplicate/);
});

test('CLI distinguishes a valid empty shard from global-empty and malformed input', () => {
  const valid = runRunnerCli(['--selected', '--shard', '4/4'], 'node\tunused.mjs\n');
  assert.equal(valid.status, 0, valid.stderr);
  assert.match(valid.stdout, /Explicit empty shard/);
  for (const input of ['', 'shell\tbad\n', 'node\t\n']) {
    assert.notEqual(runRunnerCli(['--selected', '--shard', '4/4'], input).status, 0);
  }
  for (const args of [['--selected', '--shard'], ['--selected', '--shard', '5/4'],
    ['--selected', '--unknown', '1/4']]) {
    assert.notEqual(runRunnerCli(args, 'node\tunused.mjs\n').status, 0);
  }
});

test('unconfirmed resource retirement stops without retrying or starting the next item', async () => {
  const calls = [];
  await assert.rejects(runSelected([item('node', 'first'), item('node', 'second')], {
    attempts: 2,
    settleMs: 0,
    run: async current => { calls.push(current.target); throw Object.assign(new Error('owned tree still live'), { cleanupFailed: true }); },
  }), /owned tree still live/);
  assert.deepEqual(calls, ['first']);
});

const runnerCli = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
  "run-frontend-smokes.mjs",
);

const runRunnerCli = (args, input) =>
  spawnSync(process.execPath, [runnerCli, ...args], {
    input,
    encoding: "utf8",
    timeout: 15000,
  });

test("parseSelection reads the selector's kind<TAB>target lines", () => {
  assert.deepEqual(
    parseSelection("npm\ttest:ui-smoke\nnode\ttests/kb_smoke.js\n"),
    [item("npm", "test:ui-smoke"), item("node", "tests/kb_smoke.js")],
  );
});

test("parseSelection tolerates blank lines and CRLF", () => {
  assert.deepEqual(
    parseSelection("\rnpm\ttest:ui-smoke\r\n\r\nnode\ttests/kb_smoke.js\n"),
    [item("npm", "test:ui-smoke"), item("node", "tests/kb_smoke.js")],
  );
});

test("parseSelection rejects malformed lines fail-closed", () => {
  assert.throws(() => parseSelection("npm test:ui-smoke\n"), /malformed smoke selection line/);
  assert.throws(() => parseSelection("shell\trm -rf /\n"), /unsupported frontend smoke command kind: shell/);
  assert.throws(() => parseSelection("npm\t\n"), /empty target/);
  assert.throws(() => parseSelection("npm\t   \n"), /empty target/);
});

test("runSelected runs every passing smoke exactly once in order", async () => {
  const runOrder = [];
  await runSelected(
    [item("npm", "a"), item("node", "b")],
    { run: async (smoke) => { runOrder.push(label(smoke)); } },
  );
  assert.deepEqual(runOrder, ["npm:a", "node:b"]);
});

test("runSelected runs nothing for an empty selection", async () => {
  let ran = false;
  await runSelected([], { run: async () => { ran = true; } });
  assert.equal(ran, false);
});

test("runSelected retries a failed smoke once and then continues", async () => {
  const attempts = [];
  const warnings = [];
  let firstAttempt = true;
  await runSelected(
    [item("npm", "flaky"), item("node", "next")],
    {
      settleMs: 0,
      warn: (message) => warnings.push(message),
      run: async (smoke) => {
        attempts.push(label(smoke));
        if (firstAttempt) {
          firstAttempt = false;
          throw new Error("launch timed out");
        }
      },
    },
  );
  assert.deepEqual(attempts, ["npm:flaky", "npm:flaky", "node:next"]);
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /npm:flaky failed on attempt 1\/2 \(launch timed out\)/);
});

test("runSelected stops the gate when a smoke fails on both attempts", async () => {
  let attempts = 0;
  await assert.rejects(
    runSelected([item("npm", "broken")], {
      settleMs: 0,
      warn: () => {},
      run: async () => {
        attempts += 1;
        throw new Error(`still failing ${attempts}`);
      },
    }),
    /still failing 2/,
  );
  assert.equal(attempts, 2);
});

test("runSelected never retries the deterministic exit-2 skip", async () => {
  let attempts = 0;
  await assert.rejects(
    runSelected([item("node", "skipped")], {
      settleMs: 0,
      warn: () => {},
      run: async () => {
        attempts += 1;
        throw Object.assign(new Error("skipped"), { exitCode: 2 });
      },
    }),
    /skipped/,
  );
  assert.equal(attempts, 1);
});

test("runSelected retries real failures of any other exit code", async () => {
  let attempts = 0;
  await assert.rejects(
    runSelected([item("node", "broken")], {
      settleMs: 0,
      warn: () => {},
      run: async () => {
        attempts += 1;
        throw Object.assign(new Error("ui assertion failed"), { exitCode: 1 });
      },
    }),
    /ui assertion failed/,
  );
  assert.equal(attempts, 2);
});

test("the CLI fails closed on an empty --selected stdin", () => {
  const result = runRunnerCli(["--selected"], "");
  assert.equal(result.status, 1);
  assert.match(result.stderr, /failing closed/);
});

test("the CLI rejects unknown usage with exit code 2", () => {
  const result = runRunnerCli([], "");
  assert.equal(result.status, 2);
  assert.match(result.stderr, /usage:/);
});

test("runSelected waits out the settle window before retrying", async (t) => {
  const settleMs = 120;
  const realSetTimeout = global.setTimeout;
  let requestedMs = 0;
  t.mock.method(global, "setTimeout", (callback, ms, ...rest) => {
    requestedMs = ms;
    return realSetTimeout(callback, ms, ...rest);
  });
  let attempts = 0;
  const started = performance.now();
  await runSelected([item("npm", "flaky")], {
    settleMs,
    warn: () => {},
    run: async () => {
      attempts += 1;
      if (attempts === 1) throw new Error("transient");
    },
  });
  assert.equal(attempts, 2);
  assert.equal(requestedMs, settleMs);
  // libuv floors its loop clock to whole milliseconds, so the wall clock can
  // observe a fired timer up to a couple of milliseconds early; anything below
  // half the settle window means the sleep never ran at all.
  assert.ok(
    performance.now() - started >= settleMs / 2,
    "the retry must actually wait the settle window",
  );
});

const writeSelfCountingSmoke = async (dir, name, log, exitCode) => {
  const script = path.join(dir, name);
  await writeFile(
    script,
    [
      `import { appendFileSync } from "node:fs";`,
      `appendFileSync(${JSON.stringify(log)}, "run\\n");`,
      ...(exitCode === 2 ? [`console.log("SKIP: chrome unavailable");`] : []),
      `process.exit(${exitCode});`,
      "",
    ].join("\n"),
  );
  return script;
};

test('four concurrent CLI workers execute the shared global list exactly once', async (t) => {
  const dir = await mkdtemp(path.join(tmpdir(), 'smoke-runner-shards-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const logs = Array.from({ length: 5 }, (_, i) => path.join(dir, `attempts-${i}.log`));
  const scripts = await Promise.all(logs.map((log, i) => writeSelfCountingSmoke(dir, `case-${i}.mjs`, log, 0)));
  const input = scripts.map(script => `node\t${script}\n`).join('');
  const results = await Promise.all(Array.from({ length: 4 }, (_, index) => new Promise(resolve => {
    const child = spawn(process.execPath, [runnerCli, '--selected', '--shard', `${index + 1}/4`],
      { stdio: ['pipe', 'pipe', 'pipe'], windowsHide: true });
    let output = '';
    child.stdout.on('data', data => { output += data; });
    child.stderr.on('data', data => { output += data; });
    child.on('error', error => resolve({ code: null, output: String(error) }));
    child.on('close', code => resolve({ code, output }));
    child.stdin.end(input);
  })));
  for (const result of results) assert.equal(result.code, 0, result.output);
  for (const log of logs) assert.equal(await readFile(log, 'utf8'), 'run\n');
});

test("a real exit-2 skip runs once and never triggers a retry", async (t) => {
  const dir = await mkdtemp(path.join(tmpdir(), "smoke-runner-skip-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const log = path.join(dir, "attempts.log");
  const script = await writeSelfCountingSmoke(dir, "skip_smoke.mjs", log, 2);
  const warnings = [];
  await assert.rejects(
    runSelected([item("node", script)], {
      settleMs: 0,
      warn: (message) => warnings.push(message),
    }),
    (error) =>
      error.exitCode === 2 && /exited with status 2/.test(error.message),
  );
  assert.equal(await readFile(log, "utf8"), "run\n");
  assert.deepEqual(warnings, []);
});

test("a real failing smoke is retried once and then fails the gate", async (t) => {
  const dir = await mkdtemp(path.join(tmpdir(), "smoke-runner-fail-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const log = path.join(dir, "attempts.log");
  const script = await writeSelfCountingSmoke(dir, "fail_smoke.mjs", log, 1);
  const warnings = [];
  await assert.rejects(
    runSelected([item("node", script)], {
      settleMs: 0,
      warn: (message) => warnings.push(message),
    }),
    (error) =>
      error.exitCode === 1 && /exited with status 1/.test(error.message),
  );
  assert.equal(await readFile(log, "utf8"), "run\nrun\n");
  assert.equal(warnings.length, 1);
  assert.match(warnings[0], /failed on attempt 1\/2 .*exited with status 1\)/);
});
