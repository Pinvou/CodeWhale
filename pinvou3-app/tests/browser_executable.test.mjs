import assert from 'node:assert/strict';
import test from 'node:test';
import { findBrowserExecutable } from './helpers/browser-executable.mjs';

test('explicit browser overrides preserve precedence without silent fallback', () => {
  const exists = () => { throw new Error('explicit selection must not search'); };
  assert.equal(findBrowserExecutable({ env: { CHROME: '/chosen/chrome', CHROMIUM: '/other' }, exists }), '/chosen/chrome');
  assert.equal(findBrowserExecutable({ env: { CHROMIUM: '/chosen/chromium' }, exists }), '/chosen/chromium');
});

test('standard macOS Chrome and existing Windows/Linux installations are discoverable', () => {
  for (const installed of ['/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
    'C:/Program Files/Google/Chrome/Application/chrome.exe', '/usr/bin/chromium']) {
    assert.equal(findBrowserExecutable({ env: {}, exists: value => value === installed }), installed);
  }
});

test('no available browser remains unavailable to mandatory smoke callers', () => {
  assert.equal(findBrowserExecutable({ env: {}, exists: () => false }), undefined);
});
