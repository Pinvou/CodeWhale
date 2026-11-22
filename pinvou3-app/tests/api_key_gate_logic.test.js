#!/usr/bin/env node
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
(async () => {
  const { baseUrlIsLoopback, isLocalModel, shouldShowModelAccessGate } = await import('../src/shared/model-access-state.js');
  const { modelAccessAppliesToView } = await import('../src/shared/model-access-state.js');
  const { isControlledSessionChatActive } = await import('../src/features/remote-control/remote-control-logic.mjs');
  const { remoteProxyForSession } = await import('../src/features/remote-control/remote-session-adapter.mjs');
  const guest = { accountStatus: 'guest', refreshStatus: 'ready' };
  const empty = { modelsLoaded: true, savedModels: [], effectiveModelConfig: {} };
  for (const url of ['http://localhost:8000/v1', 'http://localhost.:8000/v1', 'http://127.0.0.42:8000/v1', 'http://[::1]:8000/v1']) assert.equal(baseUrlIsLoopback(url), true);
  for (const url of ['https://localhost.example.com/v1', 'https://127.0.0.10.example.com/v1', 'not a url']) assert.equal(baseUrlIsLoopback(url), false);
  assert.equal(isLocalModel({ preset: 'local_vllm' }), true);
  assert.equal(shouldShowModelAccessGate(empty, 'chat', true, false, guest), true);
  assert.equal(shouldShowModelAccessGate(empty, 'chat', true), false, 'unverified identity cannot create first-use login');
  assert.equal(shouldShowModelAccessGate({ ...empty, activeSessionId: 'history' }, 'chat', true, false, guest), false, 'history stays reachable');
  for (const view of ['settings', 'tools', 'scheduled']) assert.equal(shouldShowModelAccessGate(empty, view, true, false, guest), false);
  assert.equal(shouldShowModelAccessGate({ ...empty, scheduledRunContext: { id: 'run' } }, 'scheduled', true, false, guest), true);
  assert.equal(shouldShowModelAccessGate(empty, 'chat', false, false, guest), false);
  assert.equal(shouldShowModelAccessGate(empty, 'chat', true, true, guest), false, 'remote ownership does not depend on local model access');
  for (const credential_state of ['missing', 'unavailable', 'configured']) {
    const configured = { ...empty, savedModels: [{ id: 'cloud', model: 'cloud' }], effectiveModelConfig: { model: 'cloud', credential_state } };
    assert.equal(shouldShowModelAccessGate(configured, 'chat', true, false, guest), false, 'credential repair is not first use');
  }
  const main = fs.readFileSync(path.join(__dirname, '../src/app/main.jsx'), 'utf8');
  for (const status of ['connected', 'reconnecting', 'resume_provisional', 'resume_activated_pending_liveness']) {
    const device = { status, generation: 'generation-1' }, session = { sessionId: 'remote-session' };
    const owned = isControlledSessionChatActive(device, session, 'remote-session');
    assert.equal(owned, true);
    assert.equal(modelAccessAppliesToView(empty, 'chat', true, owned), false);
    assert.equal(modelAccessAppliesToView(empty, 'chat', true, isControlledSessionChatActive(device, session, 'local-session')), true);
  }
  for (const [id, history, remote] of [
    ['remote', { localSessionId: 'remote', entries: [] }, true],
    ['local', { localSessionId: 'remote', entries: [] }, false],
    ['remote', null, false], ['remote', { localSessionId: 'remote' }, false],
  ]) assert.equal(modelAccessAppliesToView(empty, 'chat', true, Boolean(remoteProxyForSession(id, history))), !remote);
  assert.match(main, /modelAccessAppliesToView\(bs, currentView, bridge.available, isControlledChatActive \|\| isRemoteChatActive\)/);
  assert.match(main, /!bs\?\.activeSessionId && modelAccessViewActive && modelAccessState.kind === 'first_use'/);
  const bridgeSource = fs.readFileSync(path.join(__dirname, '../src/shared/bridge-shared-helpers.js'), 'utf8');
  assert.match(bridgeSource, /invoke\("get_effective_model_config", \{ sessionId: requestedSessionId \}\)/);
  assert.match(bridgeSource, /await loadSessionModel\(sessionId\)/);
  assert.match(main, /LazyModelAccessController/);
  assert.match(main, /modelAccessLayerOpen && browserOverlayPublicationReady/);
  assert.match(main, /onUseExternalModels=\{openModelAccessSettings\}/);
  console.log('Model access gate scope and first-use checks passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
