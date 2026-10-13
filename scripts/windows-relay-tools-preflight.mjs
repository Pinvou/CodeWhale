import { spawnSync } from 'node:child_process';

// Cold Windows runners may need more than ten seconds to initialize PowerShell.
// Keep this bounded, and never use timeout/error as permission to skip the suite.
export const POWERSHELL_PREFLIGHT_TIMEOUT_MS = 60_000;

export function requireWindowsPowerShell51(spawn = spawnSync) {
  const result = spawn('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command',
    '$PSVersionTable.PSVersion.ToString()'], {
    encoding: 'utf8', timeout: POWERSHELL_PREFLIGHT_TIMEOUT_MS, windowsHide: true,
  });
  if (result.error) {
    const reason = result.error.code === 'ETIMEDOUT' ? 'preflight timed out' : 'could not start';
    throw new Error(`Windows PowerShell 5.1 required: ${reason}`, { cause: result.error });
  }
  if (result.status !== 0) throw new Error('Windows PowerShell 5.1 required: preflight exited unsuccessfully');
  if (!/^5\.1\.\d+(?:\.\d+)?$/.test((result.stdout ?? '').trim())) {
    throw new Error('Windows PowerShell 5.1 required: unexpected version');
  }
}
