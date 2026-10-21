// Opt-in companion to megacube_switch_follow_real.py. No fake model or settings store.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import { renameSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { createCmsdSwitchFollow } from '../src/features/megacube/switch-follow.js';
import { buildSavedModel } from '../src/features/model-store/pinvou-model-link.js';

const address = new URL(process.argv[2]);
assert.equal(address.protocol, 'http:');
assert.equal(address.hostname, '127.0.0.1');
const evidence = path.resolve(process.argv[3]);
const token = process.env.MEGACUBE_HANDOFF_QA_TOKEN;
assert.ok(token);
const report = { actualSwitchTasks: [], savedCalls: 0, selections: [], generations: [], notices: [],
  scope: 'Production frontend controller + real MCP/cmsd + production Rust registration/session/EnginePool. UI event transport supplied by acceptance driver; no native click claim.' };
const rpc = async (route, body = {}) => {
  const response = await fetch(new URL(route, address), { method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  const value = await response.json();
  if (!response.ok || value.error) throw new Error(value.error || `http ${response.status}`);
  return value.result;
};
const host = (command, args = {}) => rpc('/host', { command, ...args });
const get = endpoint => rpc('/api', { method: 'GET', path: endpoint });
const config = await rpc('/config');
const source = buildSavedModel({ modelName: config.models.a, displayName: 'Retained source registration name',
  baseUrl: config.gateway, contextWindow: 2048, existingId: 'qa-existing-a', storeCardId: config.cards.a });
await host('save', { model: source });
await host('default', { modelId: source.id });
const sessionId = (await host('create', { modelId: source.id })).sessionId;
const otherSessionId = (await host('create', { modelId: source.id })).sessionId;
const state = { activeSessionId: sessionId, currentSessionModelId: source.id,
  activeModelId: source.id, sessionBusy: {} };
const pendingFile = path.join(evidence, 'pending-handoffs.json');
let pending = [];
const savePending = value => {
  pending = structuredClone(value);
  writeFileSync(pendingFile + '.next', JSON.stringify(pending), { mode: 0o600 });
  renameSync(pendingFile + '.next', pendingFile);
};
const makeFollower = () => createCmsdSwitchFollow({
  getState: () => state,
  queryModels: async () => ({ available: true, data: await get('/v1/models') }),
  queryStatus: async () => ({ available: true, data: await get('/v1/status') }),
  listModels: () => host('models'), sessionModelId: id => host('binding', { sessionId: id }),
  saveModel: async model => { await host('save', { model }); report.savedCalls++; },
  switchModel: async (id, modelId, options) => {
    await host('switch', { sessionId: id, modelId, ...options });
    report.selections.push({ sessionId: id, modelId });
  },
  listCards: async () => [],
  notify: (kind, name) => { report.notices.push({ kind, name }); },
  loadPending: () => structuredClone(pending), savePending,
});
let follower = makeFollower();
const sleep = milliseconds => new Promise(resolve => { setTimeout(resolve, milliseconds); });
const infer = async label => {
  const started = Date.now();
  const value = await host('infer', { sessionId, prompt: 'Say hello in one short sentence.' });
  assert.ok(value.text?.trim(), JSON.stringify(value));
  assert.equal(value.status.toLowerCase(), 'completed', JSON.stringify(value));
  assert.ok(value.text.replace(/\uFFFD/g, '').trim(), 'generated only replacement characters');
  report.generations.push({ label, seconds: (Date.now() - started) / 1000, text: value.text });
};
await infer('source-before-switch');
const ongoing = await host('begin', { sessionId, prompt: 'Write five short sentences about the sky.' });
await assert.rejects(host('switch', { sessionId, modelId: source.id,
  expectedModelId: source.id, expectedDefaultModelId: source.id }), /session_turn_in_progress/);
const completed = await host('finish', { sessionId, turnId: ongoing.turnId });
assert.ok(completed.text?.trim());
assert.equal(await host('binding', { sessionId }), source.id);
report.atomicBusyGuardPreservedActualGeneratingTurn = true;
await assert.rejects(host('switch', { sessionId, modelId: source.id,
  expectedModelId: 'a-stale-binding', expectedDefaultModelId: source.id }), /session_model_changed/);
assert.equal(await host('binding', { sessionId }), source.id);
report.atomicManualChoiceGuardVerified = true;

async function beginSwitch(card, callId) {
  follower.toolStarted({ id: callId, session_id: sessionId,
    name: 'mcp_megacube_admin_cube_switch_model', args: { card } });
  const result = await rpc('/mcp', { name: 'cube_switch_model', arguments: { card } });
  assert.ok(!result.isError);
  const receipt = JSON.parse(result.content[0].text);
  assert.ok(receipt.data?.task_id, JSON.stringify(receipt));
  await follower.toolEnded({ id: callId, session_id: sessionId, success: true, output: result });
  report.actualSwitchTasks.push(receipt.data.task_id);
  return receipt.data.task_id;
}
async function settled(id) {
  const deadline = Date.now() + 90000;
  while (Date.now() < deadline) {
    const task = await get(`/v1/tasks/${id}`);
    if (task.status !== 'running') {
      assert.equal(task.status, 'succeeded', JSON.stringify(task));
      return task;
    }
    await follower.observe(task);
    await sleep(200);
  }
  throw new Error('actual switch deadline');
}
async function follow(task, expectedCard) {
  const deadline = Date.now() + 65000;
  while (Date.now() < deadline) {
    await follower.observe(task);
    const binding = await host('binding', { sessionId });
    const registry = await host('models');
    const row = registry.models.find(model => model.id === binding);
    if (row?.store_card_id === expectedCard) return row;
    await sleep(250);
  }
  throw new Error('handoff not applied');
}

// The accepting tool process and UI disappear while A is stopped and B starts.
const firstId = await beginSwitch(config.cards.b, 'actual-call-b');
assert.equal(pending.length, 1);
await fs.writeFile(pendingFile, JSON.stringify(pending), { mode: 0o600 });
follower.dispose();
await rpc('/close-mcp');
state.activeSessionId = otherSessionId;
const firstTask = await settled(firstId);
pending = JSON.parse(await fs.readFile(pendingFile, 'utf8'));
follower = makeFollower();
const linked = await follow(firstTask, config.cards.b);
assert.equal(report.savedCalls, 1);
assert.equal((await host('models')).active_model_id, source.id);
assert.equal(await host('binding', { sessionId: otherSessionId }), source.id);
await follower.observe(firstTask); await follower.settled(firstTask);
assert.equal(report.savedCalls, 1);
await infer('unregistered-target-after-UI-reopen');
report.newRegistration = { id: linked.id, card: linked.store_card_id, model: linked.model };
report.uiReopenAndInitiatingMcpExitRecovered = true;
report.otherConversationAndGlobalDefaultPreserved = true;

// The source row is already registered: preserve ID, display name and settings.
state.activeSessionId = sessionId; state.currentSessionModelId = linked.id;
const secondId = await beginSwitch(config.cards.a, 'actual-call-a');
const secondTask = await settled(secondId);
const reused = await follow(secondTask, config.cards.a);
assert.equal(reused.id, source.id);
assert.equal(reused.name, source.name);
assert.equal(report.savedCalls, 1);
await infer('existing-registration-reused');
report.reusedRegistration = { id: reused.id, name: reused.name };

// Repeated idempotent MCP requests still use one registered row and one task per request.
for (let index = 0; index < 5; index++) {
  state.currentSessionModelId = source.id;
  const id = await beginSwitch(config.cards.a, `idempotent-${index}`);
  const task = await settled(id);
  await follow(task, config.cards.a);
}
assert.equal(report.savedCalls, 1);
const finalModels = await host('models');
assert.equal(finalModels.models.filter(model => model.store_card_id === config.cards.b).length, 1);
report.idempotentRepeats = 5;
report.actualPinvouEngineGenerations = report.generations.length;
report.passed = true;
await fs.writeFile(path.join(evidence, 'report.json'), JSON.stringify(report, null, 2) + '\n', { mode: 0o600 });
follower.dispose();
console.log(JSON.stringify({ passed: true, switchTasks: report.actualSwitchTasks.length,
  generated: report.generations.length, savedCalls: report.savedCalls }));
