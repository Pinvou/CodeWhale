import { existsSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { basename, isAbsolute } from 'node:path';

import { isMainModule } from './account-platform-cli-entry.mjs';

const required = [
  'current Keycloak theme selected asset hashes and projected FTL controls agree',
  'native browser executes current Keycloak theme password protections',
  'manual typing uses one explicit mirror and blur clears accessible text',
  'initial saved or script-replaced values remain protected',
  'native Unicode editing and IME composition preserve manual provenance',
  'form reset retires old reveal state before new manual input',
  'maxlength nested microtask edit cannot reuse manual authorization',
  'Microsoft Edge displays only the current custom password reveal icon',
];

export function verifyBrowserEvidence(tap) {
  const lines = tap.split(/\r?\n/).map(line => line.trim());
  if (!lines.includes('# tests 8') || !lines.includes('# pass 8') || !lines.includes('# skipped 0')
    || required.some(name => lines.filter(line => /^ok \d+ - /.test(line)
      && line.replace(/^ok \d+ - /, '') === name).length !== 1)) {
    throw new Error('LOGIN_THEME_BROWSER_EVIDENCE_INCOMPLETE');
  }
}

function runWindowsGate() {
  if (process.platform !== 'win32') { console.error('LOGIN_THEME_WINDOWS_REQUIRED'); process.exit(1); }
  const edge = process.env.PINVOU_PASSWORD_BROWSER_EXECUTABLE || [
    'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
    'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
  ].find(existsSync);
  if (!edge || !isAbsolute(edge) || !existsSync(edge) || basename(edge).toLowerCase() !== 'msedge.exe') {
    console.error('LOGIN_THEME_EDGE_REQUIRED'); process.exit(1);
  }
  const result = spawnSync(process.execPath, ['--test', '--test-reporter=tap',
    fileURLToPath(new URL('../account-platform/test/browser/login-theme.test.mjs', import.meta.url))], {
    cwd: fileURLToPath(new URL('../account-platform/', import.meta.url)), windowsHide: true, timeout: 120000,
    env: { ...process.env, PINVOU_PASSWORD_BROWSER_EXECUTABLE: edge,
      PINVOU_REQUIRE_PASSWORD_BROWSER_TEST: '1', PINVOU_REQUIRE_PASSWORD_EDGE_VISUAL_TEST: '1' },
    encoding: 'utf8', maxBuffer: 4 * 1024 * 1024,
  });
  if (result.stdout) process.stdout.write(result.stdout);
  if (result.stderr) process.stderr.write(result.stderr);
  if (result.error || result.signal || result.status !== 0) process.exit(1);
  try { verifyBrowserEvidence(result.stdout || ''); } catch {
    console.error('LOGIN_THEME_BROWSER_EVIDENCE_INCOMPLETE'); process.exit(1);
  }
}
if (isMainModule(import.meta.url)) runWindowsGate();
