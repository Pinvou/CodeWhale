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
      /ensure_desktop_remote_control_enabled\(\)|ensure_supported\(|desktop_remote_state::ensure_ready\(&app\)/u,
      `${command} must fail closed through the shared release switch`,
    );
  }
});

test('the shared release helper returns the stable unsupported error', () => {
  const source = readFileSync(new URL('platform/capabilities.rs', root), 'utf8');
  assert.match(source, /fn ensure_desktop_remote_control_enabled\(\)[\s\S]*desktop_remote_control_unsupported/u);
});

test('the readiness helper includes the release switch and never supplies default state', () => {
  const source = readFileSync(new URL('desktop_remote_state.rs', commandRoot), 'utf8');
  const start = source.indexOf('pub(crate) fn ensure_ready');
  const end = source.indexOf('pub(crate) struct DesktopRemoteState', start);
  assert.match(source.slice(start, end), /ensure_desktop_remote_control_enabled\(\)\?/u);
  assert.match(source.slice(start, end), /is_ready\(app\)/u);
  assert.match(source.slice(start, end), /UNAVAILABLE/u);
  assert.match(source, /try_get::<DesktopRemoteReady>\(\)/u);
  assert.match(source, /try_get::<T>\(\)/u);
  assert.doesNotMatch(source.slice(0, source.indexOf('#[cfg(test)]')), /unwrap_or_default|\.manage\(/u);
});
