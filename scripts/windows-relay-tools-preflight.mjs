import { spawnSync } from 'node:child_process';

// Cold Windows runners may need more than ten seconds to initialize PowerShell.
// Keep this bounded, and never use timeout/error as permission to skip the suite.
export const POWERSHELL_PREFLIGHT_TIMEOUT_MS = 60_000;

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
  try { console.error('windows_relay_tools_version_failure ' + JSON.stringify(diagnostic)); } catch { /* Preserve the bounded rejection if observation fails. */ }
}

export function requireWindowsPowerShell51(spawn = spawnSync) {
  let result;
  try {
    result = spawn('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command',
      '$PSVersionTable.PSVersion.ToString()'], {
      encoding: 'utf8', timeout: POWERSHELL_PREFLIGHT_TIMEOUT_MS, windowsHide: true,
    });
  } catch (error) {
    reportVersionFailure('spawn_error', undefined, error, undefined);
    throw new Error('Windows PowerShell 5.1 required: could not start');
  }
  let error;
  try { error = result?.error; } catch {
    // A failed authoritative read is not evidence of a successful probe.
    reportVersionFailure('spawn_error', result, undefined, undefined);
    throw new Error('Windows PowerShell 5.1 required: could not start');
  }
  const status = error ? undefined : safelyRead(result, 'status');
  if (error) {
    // No raw cause: CLI exception rendering must not reveal child material.
    const code = safelyRead(error, 'code');
    // Cache the code already read; other safe fields remain single-read.
    const cached = { code, errno: safelyRead(error, 'errno') };
    reportVersionFailure('spawn_error', result, cached, undefined);
    throw new Error(`Windows PowerShell 5.1 required: ${code === 'ETIMEDOUT' ? 'preflight timed out' : 'could not start'}`);
  }
  if (status !== 0) {
    reportVersionFailure('unsuccessful_exit', result, undefined, status);
    throw new Error('Windows PowerShell 5.1 required: preflight exited unsuccessfully');
  }
  const stdout = safelyRead(result, 'stdout');
  if (typeof stdout !== 'string' || !/^5\.1\.\d+(?:\.\d+)?$/.test(stdout.trim())) {
    reportVersionFailure('version_mismatch', result, undefined, status);
    throw new Error('Windows PowerShell 5.1 required: unexpected version');
  }
}
