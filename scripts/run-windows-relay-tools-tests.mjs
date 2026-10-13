import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { assertTestSummary } from './windows-relay-tools-test-summary.mjs';

const root = dirname(dirname(fileURLToPath(import.meta.url)));
const files = [
  'local-selftest-ownership.test.js',
  'local-selftest-identity.test.js',
  'endpoint-launcher.test.js',
];

function run() {
  if (process.platform !== 'win32') throw new Error('Native Windows execution required');
  const version = spawnSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command',
    '$PSVersionTable.PSVersion.ToString()'], { encoding: 'utf8', timeout: 10_000 });
  if (version.error || version.status !== 0 || !/^5\.1\./.test(version.stdout.trim())) {
    throw new Error('Windows PowerShell 5.1 required');
  }
  for (const file of files) {
    const result = spawnSync(process.execPath, ['--test', '--test-reporter=tap',
      resolve(root, 'remote-control-relay/test', file)],
    { cwd: root, encoding: 'utf8', timeout: 90_000, maxBuffer: 4 * 1024 * 1024 });
    process.stdout.write(result.stdout ?? '');
    process.stderr.write(result.stderr ?? '');
    if (result.error || result.status !== 0) throw new Error(`Windows relay regression failed: ${file}`);
    assertTestSummary(result.stdout);
  }
}

// Dedicated CLI: a junction/symlink must never turn this gate into a no-op.
run();
