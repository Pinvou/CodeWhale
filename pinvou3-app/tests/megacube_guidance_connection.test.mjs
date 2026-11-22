import assert from 'node:assert/strict';
import { test } from 'node:test';
import { guidanceSelectedModel, guidanceSelectionKey, guidanceConnectionRequest, probeGuidanceConnection, guidanceDiscoveryRequests, modelGuidancePresentation, selectedLocalModelConnection } from '../src/features/megacube/model-guidance-connection.js';

const selected = { id: 'chat-local', name: 'Chat local', model: 'ornith', base_url: 'http://127.0.0.1:8000/v1' };
const direct = { id: 'direct', model: 'ornith', base_url: 'http://127.0.0.1:18080/v1' };
const service = model => ({ base_url: model.base_url, status: 'ready', models: [{ id: model.model, loaded: true }] });
const checked = (model, result, candidates = [service(direct)]) => ({ phase: 'available', available: candidates,
  localCandidates: candidates, selectedConnection: { key: guidanceSelectionKey(model), result } });

test('session selection wins over the global model and stale effective session fields', () => {
  const model = guidanceSelectedModel({ currentSessionModelId: selected.id, activeModelId: direct.id, savedModels: [selected, direct],
    effectiveModelConfig: { base_url: direct.base_url, model: direct.model, api_key: 'never-copy-this' } });
  assert.equal(model.base_url, selected.base_url); assert.equal(model.id, selected.id);
  assert.equal('api_key' in model, false);
  assert.equal(guidanceSelectedModel({ currentSessionModelId: 'removed', activeModelId: selected.id, savedModels: [direct, selected] }).id, selected.id);
  assert.equal(guidanceSelectedModel({ activeModelId: 'removed', savedModels: [selected] }).id, selected.id);
  assert.equal(guidanceSelectedModel({ savedModels: null }), null);
});

test('explicit environment URL/model overrides apply but environment credential overrides stay unconfirmed', () => {
  const model = guidanceSelectedModel({ activeModelId: selected.id, savedModels: [selected], effectiveModelConfig: {
    env_overrides: ['base_url', 'model', 'api_key'], base_url: direct.base_url, model: 'overridden', credential_state: 'env_override',
  } });
  assert.equal(model.base_url, direct.base_url); assert.equal(model.model, 'overridden');
  assert.equal(guidanceConnectionRequest(model), null);
});

test('explicit model references override legacy ids and never borrow the global local model for cloud chat', () => {
  const cloud = { ...selected, id: 'cloud', base_url: 'https://example.test/v1' };
  const bridge = { currentSessionModelId: selected.id, activeModelId: selected.id, savedModels: [selected, cloud] };
  assert.equal(guidanceSelectedModel({ ...bridge, modelRef: { source: 'LOCAL_CONFIG', saved_model_id: cloud.id } }).id, cloud.id);
  assert.equal(guidanceSelectedModel({ ...bridge, modelRef: { source: 'ACCOUNT_PLATFORM', model_id: 'cloud-model' } }), null);
  assert.equal(guidanceSelectedModel({ ...bridge, modelRef: { source: 'LOCAL_CONFIG', saved_model_id: 'removed' } }), null);
});

test('confirmed local connection failures remain visible independently of device setup and recovery phases', () => {
  for (const phase of ['checking', 'hidden', 'installCms', 'starting', 'recover', 'unknown', 'available']) {
    const snapshot = { ...checked(selected, { ok: false, code: 'server_unavailable', http_status: 503 }), phase };
    const result = selectedLocalModelConnection(snapshot, selected);
    assert.equal(result.phase, 'connectionUnavailable', phase);
    assert.equal(result.canTry, false, 'a chat failure must not offer to start an unrelated device default');
    assert.equal(selectedLocalModelConnection(snapshot, { ...selected, base_url: 'https://example.test/v1' }).phase, 'hidden');
    assert.equal(selectedLocalModelConnection(snapshot, direct).phase, 'connectionUnknown', 'an old endpoint result cannot describe a new selection');
  }
});

test('uncertain and unsupported model-list responses never assert an unavailable local model', () => {
  for (const code of ['endpoint_not_found', 'method_not_allowed', 'redirect', 'request_invalid', 'unsupported_query', 'client_error', 'credential_unavailable']) {
    assert.equal(selectedLocalModelConnection(checked(selected, { ok: false, code }), selected).phase, 'connectionUnknown', code);
  }
  for (const models of [[{ id: selected.model, loaded: null }], [{ id: selected.model }], [null], [{}]]) {
    const snapshot = checked(selected, { ok: true, code: 'ok' }, []);
    snapshot.localCandidates = [{ base_url: selected.base_url, models }];
    assert.equal(selectedLocalModelConnection(snapshot, selected).phase, 'connectionUnknown');
  }
});

test('native discovery evidence can confirm the selected loaded model without a status field', () => {
  const snapshot = checked(selected, { ok: true, code: 'ok' }, []);
  snapshot.localCandidates = [{ base_url: selected.base_url, models: [{ id: selected.model, loaded: true }] }];
  assert.equal(selectedLocalModelConnection(snapshot, selected).phase, 'available');
});

test('a healthy alternative never makes the failing selected connection appear usable, even when registered', () => {
  const snapshot = checked(selected, { ok: false, code: 'server_unavailable', http_status: 503 });
  assert.equal(modelGuidancePresentation(snapshot, selected, [selected, direct]).phase, 'connectionUnavailable');
});

test('a responding endpoint must also advertise the loaded selected model', () => {
  const up = checked(selected, { ok: true, code: 'ok' }, [service(selected)]);
  assert.equal(modelGuidancePresentation(up, selected, [selected]).phase, 'available');
  const wrong = checked(selected, { ok: true, code: 'ok' }, [service({ ...selected, model: 'other-model' })]);
  const result = modelGuidancePresentation(wrong, selected, [selected]);
  assert.equal(result.phase, 'connectionUnavailable'); assert.equal(result.connectionMismatch, true);
  const unloaded = checked(selected, { ok: true, code: 'ok' }, []);
  unloaded.localCandidates = [{ base_url: selected.base_url, models: [{ id: selected.model, loaded: false }] }];
  assert.equal(modelGuidancePresentation(unloaded, selected, [selected]).phase, 'connectionUnavailable');
});

test('query failures, malformed results and an unprobed healthy custom target do not claim the selected model is ready or stopped', () => {
  for (const result of [null, { ok: false, code: 'client_error' }, { ok: false, code: 'credential_unavailable' }, { ok: true, code: 'ok' }]) {
    assert.equal(modelGuidancePresentation(checked(selected, result), selected, [selected, direct]).phase, 'connectionUnknown');
  }
});

test('old successful or failed probes cannot describe a newly selected conversation model', () => {
  for (const result of [{ ok: true, code: 'ok' }, { ok: false, code: 'connection_refused' }]) {
    assert.equal(modelGuidancePresentation(checked(selected, result), direct, [selected, direct]).phase, 'connectionUnknown');
  }
});

test('component installation and an ongoing MCP switch keep their existing guidance priority', () => {
  for (const phase of ['installCms', 'starting', 'hidden', 'checking']) {
    assert.equal(modelGuidancePresentation({ ...checked(selected, { ok: false, code: 'server_unavailable' }), phase }, selected, [selected]).phase, phase);
  }
});

test('cloud chat selection is never called unavailable because a local service is unlinked', () => {
  const cloud = { ...selected, id: 'cloud', base_url: 'https://example.test/v1' };
  assert.equal(modelGuidancePresentation(checked(selected, null), cloud, [cloud]).phase, 'unlinked');
  assert.equal(modelGuidancePresentation(checked(selected, null), cloud, [cloud, direct]).phase, 'available');
  assert.equal(guidanceConnectionRequest(cloud), null);
});

test('connection probing reuses saved credentials by id and stores no response details or secrets', async () => {
  const calls = [];
  const result = await probeGuidanceConnection(selected, { canInvoke: () => true, invoke: async (...args) => {
    calls.push(args); return { ok: false, code: 'server_unavailable', http_status: 503, detail: 'private details', message: 'backend-language' };
  } });
  assert.deepEqual(calls, [['test_model_connection', { baseUrl: selected.base_url, modelId: selected.id, apiKey: '' }]]);
  assert.deepEqual(result.result, { ok: false, code: 'server_unavailable', http_status: 503 });
  assert.equal((await probeGuidanceConnection(selected, { canInvoke: () => true, invoke: async () => { throw new Error('offline bridge'); } })).result, null);
  assert.equal((await probeGuidanceConnection(selected, { canInvoke: () => true, invoke: async () => ({ ok: false }) })).result, null);
});

test('remote targets, blocked environment credentials and missing commands cause no automatic probe', async () => {
  let calls = 0;
  for (const model of [{ ...selected, base_url: 'https://example.test/v1' }, { ...selected, probeBlocked: true }, { ...selected, base_url: 'file://localhost/tmp' }]) {
    assert.equal((await probeGuidanceConnection(model, { canInvoke: () => true, invoke: () => ++calls })).result, null);
  }
  await probeGuidanceConnection(selected, { canInvoke: () => false, invoke: () => ++calls });
  assert.equal(calls, 0);
});

test('discovery covers the current and device-default known ports without scanning a range', () => {
  assert.deepEqual(guidanceDiscoveryRequests(selected, 18080), [{ currentBaseUrl: selected.base_url, customPort: 18080 }]);
  assert.deepEqual(guidanceDiscoveryRequests(direct, 19080), [{ currentBaseUrl: direct.base_url, customPort: 19080 }, { customPort: 18080 }]);
  assert.deepEqual(guidanceDiscoveryRequests(direct, null), [{ currentBaseUrl: direct.base_url, customPort: 18080 }]);
});
