import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

import { localClassicScriptPaths } from '../scripts/vite-runtime-assets.mjs';
import {
  classicStartupBundlePaths,
  desktopPlatformMarkerScript,
  transformIndexHtmlForClassicBundle,
  transformIndexHtmlForPlatform,
} from '../vite.config.mjs';

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const sourceIndex = fs.readFileSync(path.join(appRoot, 'src', 'index.html'), 'utf8');

test('classic startup manifests keep source order and isolate each platform', () => {
  const desktop = classicStartupBundlePaths(false, sourceIndex);
  const web = classicStartupBundlePaths(true, sourceIndex);

  assert.ok(desktop.length > web.length);
  assert.ok(desktop.every(relative => !relative.startsWith('platform/web/')));
  assert.ok(web.every(relative => !relative.startsWith('platform/tauri/')));
  assert.ok(desktop.indexOf('shared/bridge-messages.js') < desktop.indexOf('platform/tauri/bridge.js'));
  for (const excluded of [
    'shared/legacy-polyfills.js',
    'platform/web/bootstrap.js',
    'features/personas/personas-i18n.js',
    'features/updater/update-notice-logic.js',
  ]) {
    assert.ok(!desktop.includes(excluded));
    assert.ok(!web.includes(excluded));
  }
});

test('desktop classic transform replaces source tags with ordered bundles and preserves lifecycle marks', () => {
  const platformHtml = transformIndexHtmlForPlatform(false, sourceIndex);
  const bundlePaths = classicStartupBundlePaths(false, sourceIndex);
  const bundleFiles = [
    'startup/pinvou-desktop-classic-1-12345678.js',
    'startup/pinvou-desktop-classic-2-abcdef01.js',
  ];
  const transformed = transformIndexHtmlForClassicBundle(false, platformHtml, bundlePaths, bundleFiles);
  const scripts = localClassicScriptPaths(transformed);

  assert.deepEqual(
    scripts.filter(relative => relative.startsWith('startup/')),
    bundleFiles,
  );
  assert.ok(bundlePaths.every(relative => !scripts.includes(relative)));
  assert.ok(transformed.includes(desktopPlatformMarkerScript));
  assert.equal((transformed.match(/app:tauri_bridge_loaded/gu) || []).length, 1);
  assert.ok(
    transformed.indexOf(bundleFiles[0]) < transformed.indexOf(bundleFiles[1]),
    'bundle execution order must match the generated order',
  );
});

test('web classic transform preserves a rewritten deployment base', () => {
  const based = sourceIndex.replaceAll('%BASE_URL%', '/pinvou3/remote/');
  const platformHtml = transformIndexHtmlForPlatform(true, based);
  const bundlePaths = classicStartupBundlePaths(true, sourceIndex);
  const bundleFiles = ['startup/pinvou-web-classic-1-12345678.js'];
  const transformed = transformIndexHtmlForClassicBundle(true, platformHtml, bundlePaths, bundleFiles);

  assert.ok(transformed.includes('/pinvou3/remote/startup/pinvou-web-classic-1-12345678.js'));
  assert.ok(transformed.includes('/pinvou3/remote/platform/web/bootstrap.js'));
  assert.ok(bundlePaths.every(relative => !transformed.includes(`/${relative}`)));
  assert.equal(transformed.includes('app:tauri_bridge_loaded'), false);
});

test('classic transform fails closed when a declared source tag is absent', () => {
  assert.throws(
    () => transformIndexHtmlForClassicBundle(
      false,
      '<script src="/shared/authority-sync-diagnostics.js"></script>',
      ['shared/authority-sync-diagnostics.js', 'shared/bridge-messages.js'],
      ['startup/example.js'],
    ),
    /shared\/bridge-messages\.js/u,
  );
});
