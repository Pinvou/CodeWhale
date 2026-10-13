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

function safelyRead(value, key) {
  try { return value?.[key]; } catch { return undefined; }
}

function reportVersionFailure(reason, result, error, status) {
  const code = safelyRead(error, 'code');
  const errno = safelyRead(error, 'errno');
  const signal = safelyRead(result, 'signal');
  const diagnostic = {
    stage: 'version_probe', reason,
    code: typeof code === 'string' && ['ETIMEDOUT', 'ENOENT', 'EACCES', 'EINVAL', 'ENOBUFS', 'EAGAIN', 'EPERM'].includes(code) ? code : 'unknown',
    errno: Number.isInteger(errno) && errno >= -2147483648 && errno <= 2147483647 ? errno : 'unknown',
    status: Number.isInteger(status) && status >= 0 && status <= 255 ? status : 'unknown',
    signal: typeof signal === 'string' && ['SIGTERM', 'SIGKILL', 'SIGINT'].includes(signal) ? signal : 'unknown',
  };
  try { console.error('windows_relay_tools_version_failure ' + JSON.stringify(diagnostic)); } catch { /* Preserve the original fixed rejection. */ }
}

function run() {
  if (process.platform !== 'win32') throw new Error('Native Windows execution required');
  const version = spawnSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command',
    '$PSVersionTable.PSVersion.ToString()'], { encoding: 'utf8', timeout: 10_000 });
  const error = version.error;
  const status = error ? undefined : version.status;
  const reason = error ? 'spawn_error' : status !== 0 ? 'unsuccessful_exit'
    : !/^5\.1\./.test(version.stdout.trim()) ? 'version_mismatch' : undefined;
  if (reason) {
    reportVersionFailure(reason, version, error, status);
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
