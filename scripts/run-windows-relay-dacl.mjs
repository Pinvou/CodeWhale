// Native Windows permission qualification; never substitute an injected adapter.
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

if (process.platform !== 'win32') {
  console.error('windows_relay_dacl_platform_required');
  process.exit(1);
}

const result = spawnSync(process.execPath, [
  '--test', '--test-reporter=tap', '--test-concurrency=1',
  '--test-name-pattern=^Windows state directory and every durable/lock file have protected DACLs$',
  'test/enrollment-registry.test.js',
], { cwd: fileURLToPath(new URL('../remote-control-relay/', import.meta.url)),
  encoding: 'utf8', windowsHide: true, maxBuffer: 1024 * 1024 });
process.stdout.write(result.stdout || '');
process.stderr.write(result.stderr || '');
if (result.status !== 0 || !/^# tests 1\r?$/m.test(result.stdout || '')
  || !/^# pass 1\r?$/m.test(result.stdout || '') || !/^# fail 0\r?$/m.test(result.stdout || '')
  || !/^# skipped 0\r?$/m.test(result.stdout || '')) {
  console.error('windows_relay_dacl_qualification_failed');
  process.exit(1);
}
