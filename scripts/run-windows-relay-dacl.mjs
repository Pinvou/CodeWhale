// Native Windows permission qualification; never substitute an injected adapter.
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { lstat, realpath, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, parse, resolve, dirname, basename } from 'node:path';
import { verifyWindowsAncestors } from '../remote-control-relay/relay-state-store.js';

if (process.platform !== 'win32') {
  console.error('windows_relay_dacl_platform_required');
  process.exit(1);
}

async function verifiedCanonical(original) {
  // Check the original spelling before resolving a short-name alias: blindly
  // realpath-ing a junction would hide the unsafe ancestor from this preflight.
  original = resolve(original);
  const root = parse(original).root;
  let current = root;
  const ancestors = [];
  for (const part of original.slice(root.length).split(/[\\/]+/u).filter(Boolean)) {
    current = join(current, part);
    const entry = await lstat(current);
    if (!entry.isDirectory() || entry.isSymbolicLink()) throw new Error();
    ancestors.push(current);
  }
  await verifyWindowsAncestors(ancestors);
  return realpath(original);
}

let temporary, base, identity;
try {
  base = await verifiedCanonical(tmpdir());
  temporary = await mkdtemp(join(base, 'pinvou-relay-dacl-'));
  identity = await lstat(temporary);
  if (await verifiedCanonical(temporary) !== temporary) throw new Error();
  const result = spawnSync(process.execPath, [
    '--test', '--test-reporter=tap', '--test-concurrency=1',
    '--test-name-pattern=^Windows state directory and every durable/lock file have protected DACLs$',
    'test/enrollment-registry.test.js',
  ], { cwd: fileURLToPath(new URL('../remote-control-relay/', import.meta.url)),
    env: { ...process.env, TEMP: temporary, TMP: temporary },
    encoding: 'utf8', windowsHide: true, maxBuffer: 1024 * 1024 });
  process.stdout.write(result.stdout || '');
  process.stderr.write(result.stderr || '');
  if (result.status !== 0 || !/^# tests 1\r?$/m.test(result.stdout || '')
    || !/^# pass 1\r?$/m.test(result.stdout || '') || !/^# fail 0\r?$/m.test(result.stdout || '')
    || !/^# skipped 0\r?$/m.test(result.stdout || '')) {
    console.error('windows_relay_dacl_qualification_failed');
    process.exitCode = 1;
  }
} catch {
  console.error('windows_relay_dacl_temporary_unsafe');
  process.exitCode = 1;
} finally {
  if (temporary) {
    try {
      const current = await lstat(temporary);
      if (!identity || !current.isDirectory() || current.isSymbolicLink()
        || current.dev !== identity.dev || current.ino !== identity.ino
        || dirname(temporary) !== base
        || !/^pinvou-relay-dacl-[A-Za-z0-9]+$/u.test(basename(temporary))
        || await verifiedCanonical(temporary) !== temporary) throw new Error();
      await rm(temporary, { recursive: true });
    } catch {
      console.error('windows_relay_dacl_cleanup_unsafe');
      process.exitCode = 1;
    }
  }
}
