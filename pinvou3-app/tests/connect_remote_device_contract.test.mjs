// DEV-22 contract: code normalization, unified service errors, no code reuse
// and accessible copy for the A-side connection shell.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { dict } from '../src/shared/i18n-all.js';
import {
  connectionErrorKey,
  groupPairingCode,
  normalizePairingCode,
} from '../src/features/remote-control/remote-control-logic.mjs';

const source = (relative) => readFileSync(new URL(`../src/${relative}`, import.meta.url), 'utf8');
const dialog = source('features/remote-control/ConnectRemoteDeviceDialog.jsx');
const shell = source('features/remote-control/RemoteConnectionShell.jsx');
const main = source('app/main.jsx');

test('pasted, mixed-case and separated codes normalize to one wire value', () => {
  assert.equal(normalizePairingCode('2345 678'), '2345678');
  assert.equal(normalizePairingCode('2345-678'), '2345678');
  assert.equal(normalizePairingCode('  2345\t678  '), '2345678');
  assert.equal(normalizePairingCode('2345678'), '2345678');
  // Ambiguous characters from the frozen alphabet are dropped, not guessed.
  assert.equal(normalizePairingCode('234567O'), null);
  assert.equal(normalizePairingCode('234567'), null);
  assert.equal(normalizePairingCode('23456789'), null);
  assert.equal(normalizePairingCode(''), null);
  assert.equal(groupPairingCode('2345678'), '2345 678');
});

test('every service failure maps to one shared copy key', () => {
  const keys = new Set();
  for (const code of [
    'controller_busy', 'desktop_controller_active', 'desktop_controller_pending',
    'endpoint_not_found', 'controller_offline', 'pairing_code_invalid', 'pairing_expired',
    'protocol_version_unsupported', 'unsupported_capability', 'anything_else',
  ]) {
    const key = connectionErrorKey(code);
    keys.add(key);
    assert.ok(dict.zh.controllerConnection[key], `copy key ${key} must exist`);
    assert.ok(dict.en.controllerConnection[key], `copy key ${key} must exist`);
    assert.ok(dict.ja.controllerConnection[key], `copy key ${key} must exist`);
  }
  // Distinct server reasons collapse onto a small bounded set of copy keys.
  assert.ok(keys.size <= 4);
  assert.equal(connectionErrorKey('endpoint_not_found'), connectionErrorKey('pairing_expired'));
});

test('the dialog never persists or replays a code', () => {
  assert.ok(!/localStorage|sessionStorage|indexedDB/.test(dialog));
  assert.ok(!/recent|lastCode|codeHistory/i.test(dialog));
  assert.ok(dialog.includes('autoComplete="off"'));
});

test('the shell separates hiding the window from disconnecting', () => {
  assert.ok(shell.includes('backgroundHint'));
  assert.ok(shell.includes('disconnect'));
  // Closing the shell calls onClose; only disconnect requests the transport.
  assert.ok(shell.includes('onRequestDisconnect'));
  assert.ok(!/onClose[\s\S]{0,80}controllerDisconnect/.test(shell));
});

test('the entry is gated by the Windows capability and stays independent of Web share', () => {
  assert.ok(main.includes("can('desktopRemoteControl')"));
  assert.ok(main.includes('LazyConnectRemoteDeviceDialog'));
  assert.ok(main.includes('LazyWebAccessModal'));
  assert.ok(dialog.includes('role="dialog"'));
  assert.ok(dialog.includes('aria-modal="true"'));
  assert.ok(dialog.includes('aria-invalid'));
  assert.ok(shell.includes('aria-live'));
});
