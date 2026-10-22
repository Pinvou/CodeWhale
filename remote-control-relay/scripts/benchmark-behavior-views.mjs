// Reproducible synthetic aggregation benchmark. No production data or network.
// node --expose-gc scripts/benchmark-behavior-views.mjs OLD_ROOT NEW_ROOT MAIN_ROOT
// Run without concurrent builds. Timing is descriptive, never a CI threshold.
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { performance } from 'node:perf_hooks';
import { spawnSync } from 'node:child_process';
import { fileURLToPath, pathToFileURL } from 'node:url';

const now = Date.parse('2026-10-21T06:00:00Z');
Date.now = () => now;
const methods = [
  ['behaviorOverview', s => s.behaviorOverview('30')],
  ['overview', s => s.overview()], ['devices', s => s.deviceList()],
  ['tools', s => s.behaviorTools('30', 'call')],
  ['scenes', s => s.behaviorScenes('30')], ['models', s => s.behaviorModels('30')],
];
const median = values => [...values].sort((a, b) => a - b)[Math.floor(values.length / 2)];
const digest = value => createHash('sha256').update(JSON.stringify(value)).digest('hex');

function taskRows(index, remote, time) {
  const common = { occurred_at: time, received_at: time, platform: 'windows', arch: 'x86_64',
    app_version: '1.0.0', mode: 'assistant', initiator_type: 'user' };
  const names = remote
    ? ['task_submit', 'task_admit', 'first_output', 'model_used', 'task_terminal', 'result_received']
    : ['task_submitted', 'task_admitted', 'task_first_output', 'task_finished'];
  return names.map((name, offset) => {
    const controller = offset === 0 || offset === 5;
    const owner = remote ? index % 100 + (controller ? 0 : 100) : index % 200;
    const attempt = `attempt_${index}`;
    return { ...common, event_id: `${index}_${offset}`, device_id: `install_${owner}`,
      identity_id: `install_${owner}`, occurred_at: time + offset * 1000,
      event_name: remote ? 'remote_control_event' : name,
      ...(remote ? {
        remote_task_key: index.toString(16).padStart(64, '0'),
        remote_role: controller ? 'controller' : 'controlled', remote_stage: name,
        remote_outcome: 'ok', remote_reason: 'none', remote_mode: 'assistant',
        remote_task_status: offset >= 4 ? 'success' : undefined,
        remote_duration_ms: offset === 2 ? 500 : offset >= 4 ? 4000 : undefined,
        remote_model_source: 'controlled', remote_provider_key: 'openai', remote_model_key: 'gpt_5',
      } : {
        attempt_id: attempt, session_id: attempt, turn_id: attempt,
        status: offset === 3 ? 'success' : undefined,
        duration_ms: offset === 2 ? 500 : offset === 3 ? 3000 : undefined,
      }),
    };
  });
}

function fixture(remote) {
  const rows = [];
  for (let i = 0; i < 20000; i++) rows.push(...taskRows(i, remote && i < 12000,
    now - (i % 30) * 86400000 - 60000 - i));
  while (rows.length < 106000) {
    const i = rows.length;
    rows.push({ event_id: `filler_${i}`, event_name: i % 2 ? 'scene_impression' : 'tool_call_completed',
      device_id: `install_${i % 200}`, identity_id: `install_${i % 200}`,
      occurred_at: now - (i % 30) * 86400000 - 10000, received_at: now,
      platform: 'windows', arch: 'x86_64', app_version: '1.0.0', mode: 'assistant', initiator_type: 'user',
      scene_l2: 'general', tool_key: 'web_search', tool_type: 'builtin', operation_id: `operation_${i}`, success: true });
  }
  return rows;
}

async function makeStore(root, rows) {
  const { TelemetryStore } = await import(pathToFileURL(join(root, 'remote-control-relay/telemetry-service.js')));
  const directory = mkdtempSync(join(tmpdir(), 'relay-synthetic-benchmark-'));
  const store = new TelemetryStore(directory, 'benchmark-pepper-at-least-24-characters', { maxEvents: 200000 });
  for (let i = 0; i < 200; i++) store.devices.set(`install_${i}`, {
    device_id: `install_${i}`, hardware_source: 'installation_id', hardware_hash: `hardware_${i}`,
    device_token: `token_${i}`, first_seen_at: now - 40 * 86400000, last_seen_at: now,
    platform: 'windows', arch: 'x86_64', app_version: '1.0.0', behavior_collection: { status: 'active' },
  });
  store.behaviorEvents = rows.slice();
  store.lastEventCompactionAt = now;
  return { root, store, directory };
}

function dispose({ store, directory }) {
  if (store.saveTimer) clearTimeout(store.saveTimer);
  rmSync(directory, { recursive: true, force: true, maxRetries: 3, retryDelay: 50 });
}

async function heapSample(root, remote) {
  assert.equal(typeof global.gc, 'function', 'heap probe requires --expose-gc');
  const item = await makeStore(root, fixture(remote));
  try {
    global.gc();
    const before = process.memoryUsage().heapUsed;
    // The pre-PR main implementation has no persisted projection snapshot.
    item.store.preparedBehaviorEvents?.([...item.store.devices.values()]);
    global.gc();
    const projected = process.memoryUsage().heapUsed;
    // Discard outputs. Measure retained report indexes/projections, not JSON/UI
    // or another implementation's live objects. Each probe is a fresh process.
    for (const [, run] of methods) run(item.store);
    global.gc();
    const after = process.memoryUsage().heapUsed;
    return { before_bytes: before, projected_bytes: projected, after_bytes: after,
      projection_delta_bytes: projected - before, report_delta_bytes: after - projected,
      retained_delta_bytes: after - before };
  } finally { dispose(item); }
}

if (process.argv[2] === '--heap') {
  console.log(JSON.stringify(await heapSample(process.argv[3], process.argv[4] === 'remote')));
} else {
  const roots = process.argv.slice(2);
  assert.equal(roots.length, 3, 'provide OLD_ROOT NEW_ROOT MAIN_ROOT');
  console.log(JSON.stringify({ node: process.version, warmup_rounds: 2, measured_rounds: 5,
    initial_raw_events: 106000, clock: new Date(now).toISOString(), roots }));
  for (const remote of [true, false]) for (const mode of ['warm', 'cold', 'append']) {
    const base = fixture(remote);
    const items = [];
    try {
      for (const root of roots) items.push({ ...await makeStore(root, base),
        timings: Object.fromEntries([...methods.map(([name]) => [name, []]), ['all_sections', []]]) });
      for (let round = 0; round < 7; round++) {
        const appended = mode === 'append' ? Array.from({ length: 100 }, (_, i) =>
          taskRows(20000 + round * 100 + i, remote, now - 10000)).flat() : [];
        const ordered = [...items.slice(round % 3), ...items.slice(0, round % 3)];
        if (round % 2) ordered.reverse();
        const outputs = new Map();
        for (const item of ordered) {
          if (mode === 'cold') item.store.behaviorEvents = base.slice();
          if (mode === 'append') item.store.behaviorEvents.push(...appended);
          const output = {};
          let elapsedTotal = 0;
          for (const [name, run] of methods) {
            const started = performance.now();
            output[name] = run(item.store);
            const elapsed = performance.now() - started;
            elapsedTotal += elapsed;
            if (round >= 2) item.timings[name].push(elapsed);
          }
          if (round >= 2) item.timings.all_sections.push(elapsedTotal);
          outputs.set(item.root, output);
        }
        // Performance work must preserve the existing PR's full JSON outputs.
        assert.deepEqual(outputs.get(roots[1]), outputs.get(roots[0]));
        if (!remote) {
          const common = output => {
            const { remote_control, ...overview } = output.behaviorOverview;
            return { ...output, behaviorOverview: overview };
          };
          // Main lacks the new remote-control fields even for an empty cohort.
          assert.deepEqual(common(outputs.get(roots[1])), common(outputs.get(roots[2])));
        }
        if (round === 6) for (const item of items) item.outputHash = digest(outputs.get(item.root));
      }
      for (const item of items) console.log(JSON.stringify({ scenario: remote ? 'remote' : 'local', mode,
        root: item.root, raw_events: item.store.behaviorEvents.length, output_sha256: item.outputHash,
        median_ms: Object.fromEntries(Object.entries(item.timings).map(([key, values]) =>
          [key, Math.round(median(values) * 100) / 100])) }));
    } finally { for (const item of items) dispose(item); }
  }
  for (const remote of [true, false]) for (const root of roots) {
    const samples = [];
    for (let i = 0; i < 3; i++) {
      const child = spawnSync(process.execPath, ['--expose-gc', fileURLToPath(import.meta.url),
        '--heap', root, remote ? 'remote' : 'local'], { encoding: 'utf8' });
      assert.equal(child.status, 0, child.stderr || child.stdout);
      samples.push(JSON.parse(child.stdout));
    }
    console.log(JSON.stringify({ scenario: remote ? 'remote' : 'local', mode: 'isolated_heap', root,
      median_bytes: Object.fromEntries(Object.keys(samples[0]).map(key => [key, median(samples.map(s => s[key]))])) }));
  }
}
