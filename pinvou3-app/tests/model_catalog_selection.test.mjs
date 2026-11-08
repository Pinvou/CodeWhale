import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { latestModelLifecycleRejection } from '../src/features/account-platform/model-lifecycle.js';
import { dict, ensureLanguage, modelUsageCopy, tokenManagementCopy } from '../src/shared/i18n.js';

import {
  ACCOUNT_PLATFORM_SOURCE,
  LOCAL_CONFIG_SOURCE,
  composerPlatformModels,
  platformModelSelectableForNewSession,
  platformModelSelectableAsDefault,
  sameModelSelectionRef,
} from '../src/features/account-platform/model-catalog-selection.js';

test('platform model selection follows context and new-session lifecycle cutoffs', () => {
  const now = Date.parse('2026-08-25T08:00:00Z');
  assert.equal(platformModelSelectableForNewSession({ context_eligible: true, lifecycle_status: 'PUBLISHED' }, now), true);
  assert.equal(platformModelSelectableForNewSession({ context_eligible: false, lifecycle_status: 'PUBLISHED' }, now), false);
  assert.equal(platformModelSelectableForNewSession({ context_eligible: true, lifecycle_status: 'REMOVED' }, now), false);
  assert.equal(platformModelSelectableForNewSession({
    context_eligible: true,
    lifecycle_status: 'DEPRECATED',
    new_session_cutoff_at: '2026-08-25T08:00:01Z',
  }, now), true);
  assert.equal(platformModelSelectableForNewSession({
    context_eligible: true,
    lifecycle_status: 'DEPRECATED',
    new_session_cutoff_at: '2026-08-25T08:00:00Z',
  }, now), false);
  assert.equal(platformModelSelectableForNewSession({ context_eligible: true, lifecycle_status: 'DEPRECATED' }, now), false);
});

test('composer lists context-visible models while selection references remain source-aware', () => {
  const visible = composerPlatformModels([
    { model_id: 'published', context_eligible: true, lifecycle_status: 'PUBLISHED' },
    { model_id: 'deprecated', context_eligible: true, lifecycle_status: 'DEPRECATED' },
    { model_id: 'removed', context_eligible: true, lifecycle_status: 'REMOVED' },
    { model_id: 'denied', context_eligible: false, lifecycle_status: 'PUBLISHED' },
  ]);
  assert.deepEqual(visible.map(item => item.model_id), ['published', 'deprecated']);
  assert.equal(sameModelSelectionRef(
    { source: ACCOUNT_PLATFORM_SOURCE, model_id: 'active' },
    { source: ACCOUNT_PLATFORM_SOURCE, model_id: 'active' },
  ), true);
  assert.equal(sameModelSelectionRef(
    { source: ACCOUNT_PLATFORM_SOURCE, model_id: 'active' },
    { source: LOCAL_CONFIG_SOURCE, saved_model_id: 'active' },
  ), false);
  assert.equal(sameModelSelectionRef(
    { source: LOCAL_CONFIG_SOURCE, saved_model_id: 'local-a' },
    { source: LOCAL_CONFIG_SOURCE, saved_model_id: 'local-b' },
  ), false);
});

// Execute the exact installed ChatView callbacks, without substituting a renderer.
// This controlled callback check does not mount the whole Chat/Native host.
const chatSource = fs.readFileSync(new URL('../src/features/chat/ChatView.jsx', import.meta.url), 'utf8');
const effectStart = chatSource.indexOf('      useEffect(() => {', chatSource.indexOf('const [replacementError'));
const effectEnd = chatSource.indexOf('\n      }, [modelContinuationBlocked', effectStart);
assert.ok(effectStart >= 0 && effectEnd > effectStart);
const effectBody = chatSource.slice(effectStart + '      useEffect(() => {'.length, effectEnd);
const branchBody = chatSource.match(/const createReplacementBranch = useCallback\(async \(\) => \{([\s\S]*?)\n      \}, \[/)?.[1];
const alertBody = chatSource.match(/replacementError && <div role="alert"[^>]*>\{([^}]+)\}/)?.[1];
assert.ok(branchBody && alertBody);
const effectCallback = new Function('bridge', 'activeSessionId', 'modelContinuationBlocked', 'setReplacementModels', 'setReplacementModelId', 'setReplacementError', 't', 'platformModelSelectableForNewSession', effectBody);
const effect = (...args) => effectCallback(...args, platformModelSelectableForNewSession);
const branch = new Function('bridge', 'activeSessionId', 'replacementModelId', 'replacementBusy', 'chatItems', 'setReplacementBusy', 'setReplacementError', 't', 'remoteTranscriptActive', 'modelLifecycleRejection', `return (async () => {${branchBody}})();`);
const renderAlert = new Function('t', 'replacementError', `return ${alertBody};`);
for (const language of ['zh', 'en', 'ja']) {
  test(`ChatView catalog rejection renders shared safe error in ${language}`, async () => {
    assert.equal(await ensureLanguage(language), true); const t = dict[language]; let error = ''; let calls = 0;
    effect({ available: true, accountPlatform: { modelCatalog: async () => { calls++; throw new Error('CONTROLLED_RAW_PROVIDER_DETAIL'); } } }, 'source-a', true, () => assert.fail('rejected catalog must not install models'), () => assert.fail('rejected catalog must not select model'), value => { error = value; }, t);
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(calls, 1); assert.equal(renderAlert(t, error), t.uiSettingsDetail.modelSwitchFailed); assert.ok(!renderAlert(t, error).includes('CONTROLLED_RAW_PROVIDER_DETAIL'));
  });
  test(`ChatView replacement rejection retains tuple and busy release with safe copy in ${language}`, async () => {
    assert.equal(await ensureLanguage(language), true); const t = dict[language]; let error = ''; const busy = []; const calls = [];
    await branch({ accountPlatform: { branchReplacementSession: async input => { calls.push(input); throw new Error('CONTROLLED_RAW_FILESYSTEM_DETAIL'); } }, sessions: { switchToSession: () => assert.fail('failed branch must not switch sessions') } }, 'source-a', 'catalog-id-a', false, [{}, {}], value => busy.push(value), value => { error = value; }, t, false, { turnId: 'authoritative-rejected-turn', rejection: {} });
    assert.deepEqual(calls, [{sourceConversationId:'source-a',sourceTurnId:'authoritative-rejected-turn',sourceMessageCount:0,replacementModelId:'catalog-id-a',replacementTitleSuffix:t.uiModelReplacement.branchTitleSuffix,defaultChatTitle:t.newChat}]); assert.deepEqual(busy, [true, false]);
    assert.equal(renderAlert(t, error), t.uiSettingsDetail.modelSwitchFailed); assert.ok(!renderAlert(t, error).includes('CONTROLLED_RAW_FILESYSTEM_DETAIL'));
    assert.equal(renderAlert(dict.zh, error), dict.zh.uiSettingsDetail.modelSwitchFailed, 'current locale renders a stored failure without another catalog request');
  });
}
test('ChatView replacement callback preserves remote transcript ownership without local dispatch', async () => {
  await branch({ accountPlatform: { branchReplacementSession: () => assert.fail('remote transcript must not create a local replacement') },
    sessions: { switchToSession: () => assert.fail('remote transcript must not switch a local session') } },
  'remote-session', 'catalog-id-a', false, [], () => assert.fail('remote transcript must not enter local replacement busy state'),
  () => assert.fail('remote transcript must not replace its error state'), dict.zh, true);
});

test('ChatView replacement catalog uses the existing new-session lifecycle cutoff', async () => {
  const future = new Date(Date.now() + 3600000).toISOString();
  const past = new Date(Date.now() - 3600000).toISOString();
  const items = [
    { model_id: 'published', context_eligible: true, lifecycle_status: 'PUBLISHED' },
    { model_id: 'deprecated-before-cutoff', context_eligible: true, lifecycle_status: 'DEPRECATED', new_session_cutoff_at: future },
    { model_id: 'deprecated-after-cutoff', context_eligible: true, lifecycle_status: 'DEPRECATED', new_session_cutoff_at: past },
    { model_id: 'deprecated-no-cutoff', context_eligible: true, lifecycle_status: 'DEPRECATED' },
    { model_id: 'deprecated-invalid-cutoff', context_eligible: true, lifecycle_status: 'DEPRECATED', new_session_cutoff_at: 'invalid' },
    { model_id: 'context-denied', context_eligible: false, lifecycle_status: 'PUBLISHED' },
    { model_id: 'removed', context_eligible: true, lifecycle_status: 'REMOVED' },
  ];
  let models, selection;
  effect({ available: true, accountPlatform: { modelCatalog: async () => ({ items }) } }, 'source-a', true,
    value => { models = value; }, update => { selection = update('deprecated-before-cutoff'); },
    () => assert.fail('valid catalog must not set an error'), dict.en);
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(models.map(item => item.model_id), ['published', 'deprecated-before-cutoff']);
  assert.equal(selection, 'deprecated-before-cutoff', 'a still-selectable existing choice remains selected');
});

test('Personal known execution/settlement labels match current management meanings without altering states', () => {
  const execution = ['RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING','SUCCEEDED','FAILED','CANCELLED_BEFORE_DISPATCH','REVIEW'];
  const settlement = ['HOLD_PENDING','HOLD_ACTIVE','USAGE_PENDING','SETTLED','RELEASED','REVIEW'];
  for (const language of ['zh','en','ja']) for (const [key, states] of [['executionStatuses',execution],['settlementStatuses',settlement]]) for (const state of states) {
    assert.equal(modelUsageCopy[language][key]?.[state], tokenManagementCopy[language].modelToken.enumLabels[state], `${language}:${key}:${state}`);
    assert.notEqual(modelUsageCopy[language][key]?.[state], state);
  }
});

test('global defaults exclude every deprecated model while explicit new-session grace remains', () => {
  const now = Date.parse('2026-08-25T08:00:00Z');
  for (const cutoff of [undefined, 'invalid', '2026-08-25T07:59:59Z', '2026-08-25T08:00:00Z', '2026-08-25T08:00:01Z']) {
    const item = { model_id: 'catalog-id-a', context_eligible: true, lifecycle_status: 'DEPRECATED', new_session_cutoff_at: cutoff };
    assert.equal(platformModelSelectableAsDefault(item), false);
  }
  assert.equal(platformModelSelectableForNewSession({context_eligible:true,lifecycle_status:'DEPRECATED',new_session_cutoff_at:'2026-08-25T08:00:01Z'},now),true);
  for (const item of [null,{}, {context_eligible:false,lifecycle_status:'PUBLISHED'}, {context_eligible:true,lifecycle_status:'REMOVED'}, {context_eligible:true,lifecycle_status:'DRAFT'}, {context_eligible:true,lifecycle_status:'UNKNOWN'}]) assert.equal(platformModelSelectableAsDefault(item),false);
  assert.equal(platformModelSelectableAsDefault({context_eligible:true,lifecycle_status:'PUBLISHED'}),true);
});

// Tool/system presentation rows have no relationship to authoritative turn ids.
// Execute the installed callback through the actual account-platform IPC adapter.
test('ChatView replacement carries current authoritative turn through the actual IPC adapter', async () => {
  const marker = { schema_version: 1, code: 'MODEL_UNAVAILABLE', model_id: '01a08b7a-654d-7e8d-8a7c-87b4506f2c7c' };
  const timeline = [
    { event: 'user_start', turn_id: 'previous-authority' },
    { event: 'assistant_done', turn_id: 'previous-authority', status: 'Completed' },
    { event: 'user_start', turn_id: 'authoritative-rejected-turn' },
    { event: 'assistant_done', turn_id: 'authoritative-rejected-turn', status: 'send_error', model_rejection: marker },
  ];
  for (const chatItems of [[], [{ type: 'user' }, { type: 'tool' }, { type: 'system' }, { type: 'assistant' }], Array.from({ length: 17 }, (_, id) => ({ id, type: id % 2 ? 'system' : 'tool' }))]) {
    const sandbox = { window: {} }, calls = [], switched = [];
    vm.runInNewContext(fs.readFileSync(new URL('../src/platform/tauri/bridge/account-platform.js', import.meta.url), 'utf8'), sandbox);
    const accountPlatform = sandbox.window.__PINVOU_TAURI_BRIDGE_FEATURES__['account-platform']({ invoke(command, args) {
      calls.push({ command, args }); return Promise.resolve({ session: { id: 'replacement-a' } });
    } });
    await branch({ accountPlatform, sessions: { switchToSession: id => switched.push(id) } }, 'source-a', 'catalog-id-a', false,
      chatItems, () => {}, value => assert.equal(value, ''), dict.en, false, latestModelLifecycleRejection(timeline));
    assert.equal(calls.length, 1); assert.equal(calls[0].command, 'account_platform_branch_replacement_session');
    assert.equal(calls[0].args.input.sourceTurnId, 'authoritative-rejected-turn');
    assert.equal(calls[0].args.input.sourceConversationId, 'source-a');
    assert.equal(calls[0].args.input.sourceMessageCount, 0);
    assert.equal(calls[0].args.input.replacementModelId, 'catalog-id-a');
    assert.deepEqual(switched, ['replacement-a']);
  }
});

test('replacement callback requires a current typed rejected turn, excluding newer running/completed turns', async () => {
  const marker = { schema_version: 1, code: 'MODEL_UNAVAILABLE', model_id: '01a08b7a-654d-7e8d-8a7c-87b4506f2c7c' };
  const old = [{ event: 'user_start', turn_id: 'old' }, { event: 'assistant_done', turn_id: 'old', status: 'Failed', model_rejection: marker }];
  for (const timeline of [[], [...old, { event: 'user_start', turn_id: 'new' }], [...old, { event: 'user_start', turn_id: 'new' }, { event: 'assistant_done', turn_id: 'new', status: 'Completed' }]]) {
    await branch({ accountPlatform: { branchReplacementSession: () => assert.fail('no current authority must not dispatch') },
      sessions: { switchToSession: () => assert.fail('no current authority must not navigate') } }, 'source-a', 'catalog-id-a', false,
      [{ type: 'system' }], () => assert.fail('no authority must not enter busy state'), () => assert.fail('no authority must not replace error'), dict.en,
      false, latestModelLifecycleRejection(timeline));
  }
});
