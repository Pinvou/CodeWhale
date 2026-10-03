// Pure helpers for the Windows desktop remote-control UI (DEV-22/DEV-23).
// Kept in an .mjs module so the node contract tests can import them without a
// JSX transform; the JSX components import the same functions.

export const CODE_ALPHABET = '23456789ABCDEFGHJKMNPQRSTUVWXYZ';
export const CODE_LENGTH = 7;

export const CONTROLLED_UI_STATES = [
  'off',
  'registering',
  'waiting_for_pairing',
  'expiring',
  'expired',
  'pairing_reserved',
  'handshake_pending',
  'connected',
  'reconnecting',
  'resume_provisional',
  'resume_activated_pending_liveness',
  'retiring_generation',
  'stopping',
  'stopped',
  'error',
];

export const CONTROLLER_UI_STATES = [
  'empty',
  'typing',
  'ready',
  'format_invalid',
  'invalid',
  'connecting',
  'safety_handshake',
  'connected',
  'reconnecting',
  'disconnected',
  'busy',
  'offline',
  'version',
  'protocol',
  'unsupported',
  'local_busy',
  'failed',
];

const LOCAL_CONTROLLER_ATTEMPT_STATES = [
  'typing',
  'ready',
  'format_invalid',
  'connecting',
  'invalid',
  'busy',
  'offline',
  'version',
  'protocol',
  'unsupported',
  'local_busy',
  'failed',
];

/** Keeps only characters from the frozen pairing alphabet. */
export function pairingCodeCharacters(raw) {
  return String(raw || '')
    .toUpperCase()
    .replace(/[\s-]/g, '')
    .split('')
    .filter((char) => CODE_ALPHABET.includes(char))
    .join('');
}

/** Groups partial input without pretending it is already a valid code. */
export function groupPairingCodeInput(raw) {
  const cleaned = pairingCodeCharacters(raw);
  return cleaned.length > 4 ? `${cleaned.slice(0, 4)} ${cleaned.slice(4)}` : cleaned;
}

export function pairingCodeInputState(raw) {
  const compact = String(raw || '').toUpperCase().replace(/[\s-]/g, '');
  if (compact.length === 0) return 'empty';
  if ([...compact].some((char) => !CODE_ALPHABET.includes(char))) return 'format_invalid';
  if (compact.length < CODE_LENGTH) return 'typing';
  if (compact.length > CODE_LENGTH) return 'format_invalid';
  return 'ready';
}

/** Normalizes pasted/typed codes; returns null when the local shape is wrong. */
export function normalizePairingCode(raw) {
  if (pairingCodeInputState(raw) !== 'ready') return null;
  return String(raw).toUpperCase().replace(/[\s-]/g, '');
}

export function groupPairingCode(code) {
  const normalized = normalizePairingCode(code);
  if (!normalized) return String(code || '');
  return normalized.slice(0, 4) + ' ' + normalized.slice(4);
}

/** Maps only authoritative B states; unknown phases fail closed in the UI. */
export function controlledDeviceUiState(device, secondsRemaining = 0) {
  const status = String(device?.status || 'idle');
  if (status === 'waiting_for_pairing') {
    if (!device?.pairingCode || Number(secondsRemaining) <= 0) return 'expired';
    if (Number(secondsRemaining) <= 60) return 'expiring';
    return 'waiting_for_pairing';
  }
  if (CONTROLLED_UI_STATES.includes(status)) return status;
  return status === 'idle' || status === 'disabled' ? 'off' : 'error';
}

export function controlledAuthorityKey(device) {
  return `${String(device?.generation || '')}:${String(device?.status || 'idle')}`;
}

export function remoteControlTabTone(state) {
  if (state === 'connected') return 'success';
  if (['registering', 'waiting_for_pairing', 'expiring', 'pairing_reserved',
    'handshake_pending', 'connecting', 'safety_handshake', 'reconnecting',
    'resume_provisional', 'resume_activated_pending_liveness',
    'retiring_generation', 'stopping'].includes(state)) return 'waiting';
  if (['error', 'invalid', 'busy', 'offline', 'version', 'protocol', 'unsupported', 'local_busy', 'failed'].includes(state)) return 'error';
  return 'off';
}

/**
 * Invalidates late async completions after cancellation or component teardown.
 * The caller owns the actual operation; this helper only decides whether a
 * completion still belongs to the latest live attempt.
 */
export function createAsyncOperationGuard() {
  let generation = 0;
  let active = true;
  return {
    start() {
      generation += 1;
      return generation;
    },
    isCurrent(token) {
      return active && token === generation;
    },
    invalidate() {
      generation += 1;
    },
    dispose() {
      active = false;
      generation += 1;
    },
  };
}

/** Invalidates an outstanding clipboard read before IME emits its first change. */
export function createPairingInputGuard() {
  let revision = 0;
  let composing = false;
  return {
    revision: () => revision,
    changed: () => { revision += 1; },
    compositionStart: () => { composing = true; revision += 1; },
    compositionEnd: () => { composing = false; revision += 1; },
    isComposing: () => composing,
    canApplyPaste: (startedAt) => !composing && revision === startedAt,
  };
}

export function connectionErrorState(code) {
  switch (code) {
    case 'controller_busy':
      return 'busy';
    case 'controller_already_connecting':
      return 'local_busy';
    case 'pairing_code_invalid':
      return 'invalid';
    case 'version_incompatible':
      return 'version';
    case 'endpoint_offline':
      return 'offline';
    case 'invalid_message':
      return 'protocol';
    case 'desktop_remote_control_unsupported':
      return 'unsupported';
    default:
      return 'failed';
  }
}

/** Maps the A-side transport plus local input into the seventeen UI states. */
export function controllerConnectionUiState(connection, inputState = 'empty', localErrorCode = null) {
  if (localErrorCode) return connectionErrorState(localErrorCode);
  const status = String(connection?.status || 'idle');
  if (status === 'connecting') return 'connecting';
  if (status === 'safety_handshake') return 'safety_handshake';
  if (status === 'connected') return 'connected';
  if (status === 'reconnecting') return 'reconnecting';
  // A deliberate new input/submit attempt (including its bounded failure)
  // supersedes a stale terminal projection. Empty has no attempt semantics,
  // so an idle form still shows the authoritative disconnected/error state.
  if (LOCAL_CONTROLLER_ATTEMPT_STATES.includes(inputState)) return inputState;
  if (status === 'disconnected') return 'disconnected';
  if (status === 'error' || connection?.lastError) {
    return connectionErrorState(connection?.lastError);
  }
  return CONTROLLER_UI_STATES.includes(inputState) ? inputState : 'empty';
}

/** Maps a bounded server error code to one shared copy key. */
export function connectionErrorKey(code) {
  switch (code) {
    case 'controller_busy':
      return 'errorBusy';
    case 'controller_already_connecting':
      return 'errorLocalBusy';
    case 'pairing_code_invalid':
      return 'errorInvalidCode';
    case 'version_incompatible':
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
  return ['connected', 'reconnecting', 'resume_provisional',
    'resume_activated_pending_liveness'].includes(status);
}

/** Device ownership never changes the meaning of another local conversation. */
export function isControlledSessionChatActive(controlledDevice, controlledSession, activeSessionId) {
  return isControlledDeviceChatActive(controlledDevice)
    && typeof activeSessionId === 'string'
    && activeSessionId.length > 0
    && activeSessionId === controlledSession?.sessionId;
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
