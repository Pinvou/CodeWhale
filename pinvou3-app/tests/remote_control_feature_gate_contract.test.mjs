import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
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

test('compiled production gate is immutable under runtime environment overrides', async (t) => {
  const directory = await mkdtemp(join(tmpdir(), 'pinvou-build-gate-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const capabilities = fileURLToPath(new URL('platform/capabilities.rs', root)).replaceAll('\\', '/');
  const helper = fileURLToPath(new URL('../src-tauri/build_support/desktop_controller.rs', import.meta.url));
  const build = readFileSync(new URL('../src-tauri/build.rs', import.meta.url), 'utf8');
  assert.match(build, /cargo:rerun-if-env-changed=PINVOU_DESKTOP_CONTROLLER_ENABLED/u);
  assert.match(build, /desktop_controller::build_flag/u);
  assert.match(build, /cargo:rustc-env=PINVOU_DESKTOP_CONTROLLER_BUILD_ENABLED=\{flag\}/u);
  const executable = (name) => join(directory, `${name}${process.platform === 'win32' ? '.exe' : ''}`);
  const rustc = (args, env = process.env) => {
    const toolchain = process.env.PINVOU_REMOTE_GATE_TEST_TOOLCHAIN;
    const result = spawnSync(toolchain ? 'rustup' : 'rustc',
      toolchain ? ['run', toolchain, 'rustc', ...args] : args,
      { env, encoding: 'utf8', windowsHide: true, timeout: 60_000 });
    assert.equal(result.status, 0, `${result.error || ''}\n${result.stderr}`);
  };
  const run = (path, env) => {
    const result = spawnSync(path, [], { env, encoding: 'utf8', windowsHide: true, timeout: 30_000 });
    assert.equal(result.status, 0, `${result.error || ''}\n${result.stderr}`);
    return result.stdout;
  };
  const helperTests = executable('build-helper-tests');
  rustc(['--test', helper, '-o', helperTests]);
  assert.match(run(helperTests, process.env), /2 passed; 0 failed/u);
  const host = join(directory, 'host.rs');
  await writeFile(host, `#[path=${JSON.stringify(capabilities)}] mod capabilities;\nfn main(){ println!("{} {}", capabilities::desktop_remote_control_enabled(), capabilities::ensure_desktop_remote_control_enabled().is_ok()); }\n`);
  for (const buildFlag of ['0', '1']) {
    const env = { ...process.env, PINVOU_DESKTOP_CONTROLLER_BUILD_ENABLED: buildFlag };
    const unit = executable(`capabilities-${buildFlag}-tests`);
    rustc(['--test', capabilities, '-o', unit], env);
    assert.match(run(unit, env), /6 passed; 0 failed/u);
    const binary = executable(`artifact-${buildFlag}`);
    rustc([host, '-o', binary], env);
    for (const runtimeFlag of [undefined, '0', '1', 'true']) {
      // Even spoofing the build-env key at runtime must not reopen an off binary.
      const runtime = { ...process.env, PINVOU_DESKTOP_CONTROLLER_BUILD_ENABLED: '1' };
      delete runtime.PINVOU_DESKTOP_CONTROLLER_ENABLED;
      if (runtimeFlag !== undefined) runtime.PINVOU_DESKTOP_CONTROLLER_ENABLED = runtimeFlag;
      const enabled = process.platform === 'win32' && buildFlag === '1' && (runtimeFlag === undefined || runtimeFlag === '1');
      assert.equal(run(binary, runtime).trim(), `${enabled} ${enabled}`);
    }
  }
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
