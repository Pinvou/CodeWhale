import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';
import { isModelRouteUnchanged, modelRouteSnapshot } from '../src/features/settings/model-route-intent.js';

const source = fs.readFileSync(new URL('../src/features/settings/SettingsView.jsx', import.meta.url), 'utf8');
const start = source.indexOf('      async function doSave() {');
const end = source.indexOf('\n      // 视觉模型选择:', start);
assert.ok(start >= 0 && end > start, 'the real model-form save handler must exist');
const handler = source.slice(start, end);
const route = {
  preset: 'openai_compatible', provider_kind: 'custom', vendor: '', endpoint_mode: '',
  model: 'custom-wire', base_url: 'http://127.0.0.1:8000/v1',
};

async function submit(original, changes = {}, { existing = true } = {}) {
  const current = { ...original, ...changes };
  let payload;
  await vm.runInNewContext(`${handler}\ndoSave();`, {
    canSave: true, savingModel: false,
    initial: { id: 'route-model', has_secret: true, __new: !existing },
    saveName: 'Edited name', preset: current.preset, alias: 'Edited alias', showAliasField: true,
    contextWindow: '32768', contextWindowTouched: false, maxOutput: '4096',
    reasoningEffortTiers: ['low', 'high'], reasoningEffort: 'high',
    model: current.model, baseUrl: current.base_url,
    providerKind: current.provider_kind, vendor: current.vendor, endpointMode: current.endpoint_mode,
    storeRouteAtOpen: { existing, route: modelRouteSnapshot(original) }, isModelRouteUnchanged,
    isLocalPreset: false, localKeyEnabled: false, apiKey: '', hasSavedKey: true,
    imageCapability: 'pinvou', visionModelId: '',
    setSavingModel() {}, setSaveError(error) { assert.equal(error, ''); }, onCancel() {},
    async onSave(value) { payload = value; },
  }, { filename: 'SettingsView.jsx:doSave' });
  return payload;
}

test('the real form preserves the whole untouched route for a name or alias save', async () => {
  const payload = await submit(route);
  assert.equal(payload.preserve_model_store_route, true);
  assert.equal(payload.preserve_context_window_tokens, true);
  assert.equal(payload.credential_action, 'keep_existing');
  assert.equal(payload.alias, 'Edited alias');
});

for (const [field, value] of Object.entries({
  preset: 'openai_responses', provider_kind: 'official_api', vendor: 'anthropic',
  endpoint_mode: 'full_chat_completions', model: 'new-wire', base_url: 'http://127.0.0.1:8001/v1',
})) {
  test(`the real form treats an explicit ${field} edit as a route change`, async () => {
    const payload = await submit(route, { [field]: value });
    assert.equal(payload.preserve_model_store_route, false);
    assert.equal(payload[field], value);
    assert.equal(payload.preserve_context_window_tokens, true, 'route intent must not change context intent');
    assert.equal(payload.credential_action, 'keep_existing');
  });
}

test('switching Responses back to Chat at the same URL keeps the explicit protocol edit', async () => {
  const original = { ...route, preset: 'openai_responses' };
  const payload = await submit(original, { preset: 'openai_compatible' });
  assert.equal(payload.preserve_model_store_route, false);
  assert.equal(payload.preset, 'openai_compatible');
  assert.equal(payload.base_url, original.base_url);
});

test('route snapshots use initialized form values and trim the same fields as submission', async () => {
  const initialized = { ...route, vendor: ' openai ', endpoint_mode: null };
  assert.equal(isModelRouteUnchanged(modelRouteSnapshot(initialized), {
    ...initialized, vendor: 'openai', endpoint_mode: '',
  }), true);
  assert.equal((await submit(initialized, {}, { existing: false })).preserve_model_store_route, false);
});
