import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

const root = new URL('../src-tauri/src/', import.meta.url);
const lib = readFileSync(new URL('lib.rs', root), 'utf8');
const commandRoot = new URL('app/commands/', root);
const registered = [...lib.matchAll(/commands::([a-z_]+)::(desktop_remote_[a-z_]+),/gu)]
  .map((match) => ({ module: match[1], command: match[2] }));

test('every registered desktop-remote Tauri command crosses the unified release switch', () => {
  assert.ok(registered.length >= 30, 'expected the complete A/B command surface');
  for (const { module, command } of registered) {
    const source = readFileSync(new URL(`${module}.rs`, commandRoot), 'utf8');
    const start = source.indexOf(`fn ${command}(`);
    assert.notEqual(start, -1, `missing command implementation: ${command}`);
    const next = source.indexOf('#[tauri::command]', start + 1);
    const body = source.slice(start, next === -1 ? source.length : next);
    assert.match(
      body,
      /ensure_desktop_remote_control_enabled\(\)|ensure_supported\(/u,
      `${command} must fail closed through the shared release switch`,
    );
  }
});

test('the shared release helper returns the stable unsupported error', () => {
  const source = readFileSync(new URL('platform/capabilities.rs', root), 'utf8');
  assert.match(source, /fn ensure_desktop_remote_control_enabled\(\)[\s\S]*desktop_remote_control_unsupported/u);
});
