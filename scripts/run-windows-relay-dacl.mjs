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

let stage = 'base_resolve';
let verifierPhase = 'unknown';
function reportFailure(error) {
  const diagnostic = { stage, code: 'unknown' };
  if (stage.endsWith('_reparse')) diagnostic.verifier_phase = verifierPhase;
  // Never stringify an exception: it may carry command, path or environment data.
  const read = (field) => {
    try { return error?.[field]; }
    catch { return undefined; }
  };
  const code = read('code');
  const errno = read('errno');
  const killed = read('killed');
  const signal = read('signal');
  if (['ETIMEDOUT', 'EACCES', 'EPERM', 'ENOENT', 'ENOTDIR', 'EINVAL',
    'ERR_CHILD_PROCESS_STDIO_MAXBUFFER', 'relay_state_windows_runtime_unavailable',
    'relay_state_path_component_unsafe'].includes(code)) diagnostic.code = code;
  if (Number.isSafeInteger(errno) && errno >= -2147483648
    && errno <= 2147483647) diagnostic.errno = errno;
  if (typeof killed === 'boolean') diagnostic.killed = killed;
  if (['SIGTERM', 'SIGKILL', 'SIGABRT', 'SIGINT'].includes(signal)) diagnostic.signal = signal;
  console.error(`windows_relay_dacl_stage_failure ${JSON.stringify(diagnostic)}`);
}

async function verifiedCanonical(original, scope) {
  // Check the original spelling before resolving a short-name alias: blindly
  // realpath-ing a junction would hide the unsafe ancestor from this preflight.
  original = resolve(original);
  const root = parse(original).root;
  let current = root;
  const ancestors = [];
  for (const part of original.slice(root.length).split(/[\\/]+/u).filter(Boolean)) {
    current = join(current, part);
    stage = `${scope}_lstat`;
    const entry = await lstat(current);
    stage = `${scope}_type`;
    if (!entry.isDirectory() || entry.isSymbolicLink()) throw new Error();
    ancestors.push(current);
  }
  stage = `${scope}_reparse`;
  verifierPhase = 'unknown';
  await verifyWindowsAncestors(ancestors, { onDiagnostic: (phase) => {
    verifierPhase = typeof phase === 'string'
      && ['script_started', 'paths_decoded', 'checks_completed'].includes(phase) ? phase : 'unknown';
  } });
  stage = `${scope}_realpath`;
  return realpath(original);
}

let temporary, base, identity;
try {
  base = await verifiedCanonical(tmpdir(), 'base');
  stage = 'owned_create';
  temporary = await mkdtemp(join(base, 'pinvou-relay-dacl-'));
  stage = 'owned_identity';
  identity = await lstat(temporary);
  const canonical = await verifiedCanonical(temporary, 'owned');
  stage = 'owned_canonical';
  if (canonical !== temporary) throw new Error();
  stage = 'child_spawn';
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
} catch (error) {
  reportFailure(error);
  console.error('windows_relay_dacl_temporary_unsafe');
  process.exitCode = 1;
} finally {
  if (temporary) {
    try {
      stage = 'cleanup_identity';
      const current = await lstat(temporary);
      if (!identity || !current.isDirectory() || current.isSymbolicLink()
        || current.dev !== identity.dev || current.ino !== identity.ino
        || dirname(temporary) !== base
        || !/^pinvou-relay-dacl-[A-Za-z0-9]+$/u.test(basename(temporary))
      ) throw new Error();
      const canonical = await verifiedCanonical(temporary, 'cleanup');
      stage = 'cleanup_canonical';
      if (canonical !== temporary) throw new Error();
      stage = 'cleanup_remove';
      await rm(temporary, { recursive: true });
    } catch (error) {
      reportFailure(error);
      console.error('windows_relay_dacl_cleanup_unsafe');
      process.exitCode = 1;
    }
  }
}
