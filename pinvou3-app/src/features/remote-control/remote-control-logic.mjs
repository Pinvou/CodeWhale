// Pure helpers for the Windows desktop remote-control UI (DEV-22/DEV-23).
// Kept in an .mjs module so the node contract tests can import them without a
// JSX transform; the JSX components import the same functions.

export const CODE_ALPHABET = '23456789ABCDEFGHJKMNPQRSTUVWXYZ';
export const CODE_LENGTH = 7;

/** Normalizes pasted/typed codes; returns null when the local shape is wrong. */
export function normalizePairingCode(raw) {
  const cleaned = String(raw || '')
    .toUpperCase()
    .replace(/[^0-9A-Z]/g, '')
    .split('')
    .filter((char) => CODE_ALPHABET.includes(char))
    .join('');
  if (cleaned.length !== CODE_LENGTH) return null;
  return cleaned;
}

export function groupPairingCode(code) {
  const normalized = normalizePairingCode(code);
  if (!normalized) return String(code || '');
  return normalized.slice(0, 4) + ' ' + normalized.slice(4);
}

/** Maps a bounded server error code to one shared copy key. */
export function connectionErrorKey(code) {
  switch (code) {
    case 'controller_busy':
    case 'desktop_controller_active':
    case 'desktop_controller_pending':
    case 'pairing_code_generation_failed':
      return 'errorBusy';
    case 'controller_already_connecting':
      return 'errorLocalBusy';
    case 'endpoint_not_found':
    case 'controller_offline':
    case 'pairing_code_invalid':
    case 'pairing_expired':
    case 'permission_unknown':
      return 'errorInvalidCode';
    case 'protocol_version_unsupported':
    case 'unsupported_capability':
      return 'errorVersion';
    // E2E-07：A 端连接链路的两个有界码必须有自己的文案，不再回落到通用文案。
    case 'endpoint_offline':
      return 'errorOffline';
    case 'invalid_message':
      return 'errorProtocol';
    case 'desktop_remote_control_unsupported':
      return 'errorUnsupported';
    default:
      return 'errorGeneric';
  }
}

export const TASK_TERMINAL_STATUSES = [
  'succeeded',
  'failed',
  'cancelled',
  'interrupted',
  'result_unknown',
];

export function taskStatusKey(status) {
  switch (status) {
    case 'admitted': return 'statusAdmitted';
    case 'running': return 'statusRunning';
    case 'waiting_permission': return 'statusWaitingPermission';
    case 'succeeded': return 'statusSucceeded';
    case 'failed': return 'statusFailed';
    case 'cancelled': return 'statusCancelled';
    case 'interrupted': return 'statusInterrupted';
    case 'result_unknown': return 'statusResultUnknown';
    default: return 'statusAdmitted';
  }
}

export function isTaskTerminal(status) {
  return TASK_TERMINAL_STATUSES.includes(status);
}

/**
 * The controlled-side badge remains visible while the transport is recovering.
 * @param {{ status?: string } | null | undefined} controlledDevice
 */
export function isControlledDeviceChatActive(controlledDevice) {
  const status = controlledDevice && controlledDevice.status;
  return status === 'connected' || status === 'reconnecting';
}

/**
 * Retries a B-side chat navigation only while the originating connection and
 * view intent are still current. A failed attempt is never treated as opened.
 * @param {{
 *   navigate: () => boolean | Promise<boolean>,
 *   isCurrent?: () => boolean,
 *   wait?: (delayMs: number) => Promise<void>,
 *   maxAttempts?: number,
 *   retryDelayMs?: number,
 *   onError?: (error: unknown) => void,
 * }} options
 * @returns {Promise<boolean>}
 */
export async function openControlledDeviceChatWithRetry({
  navigate,
  isCurrent = () => true,
  wait = (delayMs) => new Promise((resolve) => setTimeout(resolve, delayMs)),
  maxAttempts = 3,
  retryDelayMs = 200,
  onError = () => {},
}) {
  if (typeof navigate !== 'function') throw new TypeError('navigate must be a function');
  const attempts = Math.max(1, Math.min(3, Number(maxAttempts) || 1));
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    if (!isCurrent()) return false;
    try {
      if (await navigate()) return true;
    } catch (error) {
      onError(error);
    }
    if (attempt + 1 >= attempts || !isCurrent()) return false;
    await wait(retryDelayMs);
  }
  return false;
}
