const VERSION = 1;
const NONCE_BYTES = 32;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

function exact(value, keys) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  return actual.length === expected.length && actual.every((key, index) => key === expected[index]);
}

function failure(code, outcomeUnknown = false) {
  return Object.assign(new Error(code), { code, outcomeUnknown });
}

function nonce(cryptoObject) {
  const bytes = cryptoObject.getRandomValues(new Uint8Array(NONCE_BYTES));
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
}

export function openCredentialHandoff({ signal, timeoutMs = 3000, windowObject = globalThis.window, cryptoObject = globalThis.crypto, MessageChannelClass = globalThis.MessageChannel } = {}) {
  const portalOrigin = windowObject.location.origin;
  const popup = windowObject.open('/credential-handoff', '_blank');
  if (!popup) return Promise.reject(failure('CREDENTIAL_WINDOW_BLOCKED'));
  let port = null;
  let timer = 0;
  let closedPoll = 0;
  let finished = false;
  let boundNonce = '';
  let readyListener = null;

  const clearTimers = () => {
    if (timer) windowObject.clearTimeout(timer);
    if (closedPoll) windowObject.clearInterval(closedPoll);
    timer = 0;
    closedPoll = 0;
  };
  const close = () => {
    clearTimers();
    if (readyListener) windowObject.removeEventListener('message', readyListener);
    port?.close();
    port = null;
    boundNonce = '';
    try { if (!popup.closed) popup.close(); } catch { /* cross-origin close remains best effort */ }
  };
  const abort = () => close();
  signal?.addEventListener('abort', abort, { once: true });

  const ready = new Promise((resolve, reject) => {
    const fail = (code) => {
      if (finished) return;
      finished = true;
      close();
      reject(failure(code));
    };
    const accept = (value) => {
      if (finished) return;
      finished = true;
      clearTimers();
      if (readyListener) windowObject.removeEventListener('message', readyListener);
      resolve(value);
    };
    const onPortMessage = (event) => {
      if (!exact(event.data, ['version', 'type', 'nonce']) || event.data.version !== VERSION || event.data.type !== 'HANDOFF_BOUND' || event.data.nonce !== boundNonce) {
        fail('CREDENTIAL_HANDOFF_INVALID');
        return;
      }
      const submit = (launchID) => new Promise((submitResolve, submitReject) => {
        if (!UUID.test(launchID) || !port) {
          submitReject(failure('CREDENTIAL_HANDOFF_INVALID'));
          return;
        }
        let settled = false;
        let submitted = false;
        const submitFail = (code) => {
          if (settled) return;
          settled = true;
          close();
          submitReject(failure(submitted ? 'CREDENTIAL_HANDOFF_OUTCOME_UNKNOWN' : code, submitted));
        };
        timer = windowObject.setTimeout(() => submitFail('CREDENTIAL_WINDOW_BLOCKED'), timeoutMs);
        port.onmessage = (accepted) => {
          if (!exact(accepted.data, ['version', 'type', 'nonce']) || accepted.data.version !== VERSION || accepted.data.type !== 'HANDOFF_ACCEPTED' || accepted.data.nonce !== boundNonce) {
            submitFail('CREDENTIAL_HANDOFF_INVALID');
            return;
          }
          settled = true;
          clearTimers();
          port.close();
          port = null;
          boundNonce = '';
          submitResolve();
        };
        port.onmessageerror = () => submitFail('CREDENTIAL_HANDOFF_INVALID');
        try {
          submitted = true;
          port.postMessage({ version: VERSION, type: 'HANDOFF_SUBMIT', nonce: boundNonce, launch_id: launchID });
        } catch {
          submitted = false;
          submitFail('CREDENTIAL_HANDOFF_INVALID');
        }
      });
      accept({ popup, submit, close });
    };
    function onReady(event) {
      if (event.source !== popup) return;
      if (event.origin !== portalOrigin || !exact(event.data, ['version', 'type']) || event.data.version !== VERSION || event.data.type !== 'HANDOFF_READY') {
        fail('CREDENTIAL_HANDOFF_INVALID');
        return;
      }
      if (!cryptoObject?.getRandomValues || !MessageChannelClass) {
        fail('CREDENTIAL_HANDOFF_UNAVAILABLE');
        return;
      }
      const channel = new MessageChannelClass();
      port = channel.port1;
      boundNonce = nonce(cryptoObject);
      port.onmessage = onPortMessage;
      port.onmessageerror = () => fail('CREDENTIAL_HANDOFF_INVALID');
      port.start?.();
      try {
        popup.postMessage({ version: VERSION, type: 'HANDOFF_BIND', nonce: boundNonce }, portalOrigin, [channel.port2]);
      } catch {
        fail('CREDENTIAL_HANDOFF_INVALID');
        return;
      }
      if (timer) windowObject.clearTimeout(timer);
      timer = windowObject.setTimeout(() => fail('CREDENTIAL_WINDOW_BLOCKED'), timeoutMs);
    }
    readyListener = onReady;
    windowObject.addEventListener('message', onReady);
    timer = windowObject.setTimeout(() => fail('CREDENTIAL_WINDOW_BLOCKED'), timeoutMs);
    closedPoll = windowObject.setInterval(() => { if (popup.closed) fail('CREDENTIAL_WINDOW_BLOCKED'); }, 100);
    if (signal?.aborted) fail('CREDENTIAL_LAUNCH_CANCELLED');
  });
  return ready.finally(() => signal?.removeEventListener('abort', abort));
}
