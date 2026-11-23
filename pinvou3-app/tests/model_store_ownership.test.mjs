import assert from 'node:assert/strict';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from './helpers/vite-fixture.mjs';
import react from '@vitejs/plugin-react';
import { mergeModels, filterModels } from '../src/features/model-store/model-store-logic.js';

const server = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), configFile: false, logLevel: 'error',
  plugins: [react()], server: { middlewareMode: true, watch: null, hmr: false, ws: false }, optimizeDeps: { noDiscovery: true } });
after(() => server.close());
const { ModelRow } = await server.ssrLoadModule('/src/features/model-store/ModelRow.jsx');
const copies = [];
for (const [lang, name] of [['zh', 'dictZh'], ['en', 'dictEn'], ['ja', 'dictJa']]) {
  copies.push((await server.ssrLoadModule(`/src/shared/i18n/${lang}.js`))[name].uiModelStore);
}
const options = { onOpen() {}, onDownload() {}, onDeploy() {}, onEnable() {}, onStop() {}, onUninstall() {} };
const render = (model, copy) => renderToStaticMarkup(React.createElement(ModelRow, { ...options, model, copy }));

test('a fresh unmanaged observation never changes existing download, deployment or runtime state', () => {
  for (const [status, running, state, key] of [['pending', false, 'downloaded', 'statusPending'], ['deployed', false, 'deployed', 'statusDeployed'], ['deployed', true, 'running', 'statusRunning']]) {
    // weights_ready_at：主线把「无已核实权重时间戳」的已下载行派生为
    // incomplete 状态（resume-download）；这三行要测的是已下载/已部署/运行中，
    // 给上时间戳以走主线派生的这三个状态。
    const record = { card_id: 'existing', card_name: 'Existing model', status, running, weights_ready_at: '2026-10-01T00:00:00Z', cmsd: { managed: false, status: 'stopped', canAdopt: true } };
    const original = structuredClone(record);
    const models = mergeModels([], { models: [record] });
    assert.equal(models[0].state, state);
    assert.equal(filterModels(models, { filter: 'installed' }).length, 1);
    assert.equal(filterModels(models, { filter: 'running' }).length, running ? 1 : 0);
    for (const copy of copies) {
      const html = render(models[0], copy);
      assert.ok(html.includes(copy[key]));
      assert.ok(html.includes(copy.cmsdObserved));
      assert.ok(html.includes(copy.cmsdOwnership));
      assert.ok(html.indexOf('model-store-row-state') < html.indexOf('model-store-row-ownership'));
      assert.ok(!html.slice(0, html.indexOf('model-store-row-ownership')).includes('model-store-row-cmsd"'));
      assert.ok(!html.includes(copy.cmsdManagedHint));
    }
    assert.deepEqual(record, original);
  }
});

test('unmanaged running models remain protected, and enable follows explicit adoption eligibility', () => {
  for (const [running, canAdopt, button, disabled] of [[true, true, 'stop', true], [false, false, 'enable', true], [false, true, 'enable', false]]) {
    const [model] = mergeModels([], { models: [{ card_id: 'existing', status: 'deployed', running, weights_ready_at: '2026-10-01T00:00:00Z', cmsd: { managed: false, canAdopt } }] });
    const html = render(model, copies[0]);
    const tag = html.match(new RegExp(`<button[^>]*data-testid="model-store-${button}"[^>]*>`))?.[0];
    assert.ok(tag); assert.equal(/\sdisabled(?:=|[\s>])/.test(tag), disabled);
  }
});

test('ordinary store rows omit device ownership, managed rows keep their management badge', () => {
  for (const copy of copies) {
    const [ordinary] = mergeModels([], { models: [{ card_id: 'local', status: 'pending' }] });
    assert.ok(!render(ordinary, copy).includes('model-store-row-ownership'));
    const managed = { ...ordinary, record: { ...ordinary.record, cmsd: { managed: true, restartCount: 0 } } };
    const html = render(managed, copy);
    assert.ok(html.includes(copy.cmsdManaged));
    assert.ok(!html.includes(copy.cmsdObservedHint));
  }
});
