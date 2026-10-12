import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

const runner = new URL('../run-windows-relay-dacl.mjs', import.meta.url).href;
function invoke(platform, result) {
  const program = `import child from 'node:child_process';
    import { syncBuiltinESMExports } from 'node:module';
    Object.defineProperty(process,'platform',{value:${JSON.stringify(platform)}});
    child.spawnSync=()=>(${JSON.stringify(result)});
    syncBuiltinESMExports(); await import(${JSON.stringify(runner)});`;
  return spawnSync(process.execPath, ['--input-type=module', '-e', program], { encoding: 'utf8', windowsHide: true });
}

// These are runner decision tests; mocked TAP cannot qualify real permissions.
test('native qualification refuses non-Windows, no selected case, skip, and subprocess failure', () => {
  const valid = '# tests 1\n# pass 1\n# fail 0\n# skipped 0\n';
  assert.equal(invoke('linux', { status: 0, stdout: valid }).status, 1);
  for (const result of [
    { status: 0, stdout: '# tests 0\n# pass 0\n# fail 0\n# skipped 0\n' },
    { status: 0, stdout: '# tests 1\n# pass 0\n# fail 0\n# skipped 1\n' },
    { status: 1, stdout: valid },
    { status: null, stdout: valid },
  ]) assert.equal(invoke('win32', result).status, 1);
  assert.equal(invoke('win32', { status: 0, stdout: valid }).status, 0);
});
