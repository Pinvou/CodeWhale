(() => {
  'use strict';

  const VERSION = 1;
  const STAGE_TIMEOUT_MS = 3000;
  const NONCE = /^[A-Za-z0-9_-]{43}$/;
  const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
  let stage = 'BOOTING';
  let nonce = '';
  let port = null;
  let timer = 0;

  const exact = (value, keys) => {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
    const actual = Object.keys(value).sort();
    const expected = [...keys].sort();
    return actual.length === expected.length && actual.every((key, index) => key === expected[index]);
  };

  const stop = () => {
    if (timer) window.clearTimeout(timer);
    timer = 0;
    if (port) port.close();
    port = null;
    nonce = '';
    window.removeEventListener('message', bind);
    try { window.opener = null; } catch { /* best-effort browser isolation */ }
    window.close();
  };

  const arm = () => {
    if (timer) window.clearTimeout(timer);
    timer = window.setTimeout(stop, STAGE_TIMEOUT_MS);
  };

  const safeCredentialOrigin = () => {
    const value = document.querySelector('meta[name="pinvou-credential-origin"]')?.content || '';
    try {
      const parsed = new URL(value);
      const production = parsed.protocol === 'https:' && !parsed.port && ['credentials.account.pinvou.com', 'credentials-uat.account.pinvou.com'].includes(parsed.hostname);
      const development = parsed.protocol === 'http:' && ['127.0.0.1', 'localhost'].includes(parsed.hostname);
      return (production || development) && parsed.origin === value ? parsed.origin : '';
    } catch {
      return '';
    }
  };

  const submit = (event) => {
    if (stage !== 'BOUND' || event.currentTarget !== port || !exact(event.data, ['version', 'type', 'nonce', 'launch_id']) || event.data.version !== VERSION || event.data.type !== 'HANDOFF_SUBMIT' || event.data.nonce !== nonce || !UUID.test(event.data.launch_id)) {
      stop();
      return;
    }
    const credentialOrigin = safeCredentialOrigin();
    if (!credentialOrigin || navigator.serviceWorker?.controller !== null) {
      stop();
      return;
    }
    stage = 'SUBMITTED';
    if (timer) window.clearTimeout(timer);
    timer = 0;
    const form = document.createElement('form');
    form.method = 'post';
    form.action = `${credentialOrigin}/launch`;
    form.target = '_self';
    form.enctype = 'application/x-www-form-urlencoded';
    form.hidden = true;
    const input = document.createElement('input');
    input.type = 'hidden';
    input.name = 'launch_id';
    let launchID = event.data.launch_id;
    input.value = launchID;
    form.append(input);
    event.data.launch_id = '';
    launchID = '';
    port.postMessage({ version: VERSION, type: 'HANDOFF_ACCEPTED', nonce });
    port.close();
    port = null;
    nonce = '';
    window.removeEventListener('message', bind);
    window.opener = null;
    document.body.append(form);
    form.submit();
  };

  const bind = (event) => {
    if (event.source !== window.opener) {
      return;
    }
    if (stage !== 'READY' || event.origin !== location.origin || event.ports.length !== 1 || !exact(event.data, ['version', 'type', 'nonce']) || event.data.version !== VERSION || event.data.type !== 'HANDOFF_BIND' || !NONCE.test(event.data.nonce) || navigator.serviceWorker?.controller !== null) {
      stop();
      return;
    }
    stage = 'BOUND';
    nonce = event.data.nonce;
    port = event.ports[0];
    port.onmessage = submit;
    port.onmessageerror = stop;
    port.start();
    port.postMessage({ version: VERSION, type: 'HANDOFF_BOUND', nonce });
    arm();
  };

  const scopeCoversHandoff = async () => {
    if (!navigator.serviceWorker) return false;
    if (navigator.serviceWorker.controller !== null) return true;
    const registrations = await navigator.serviceWorker.getRegistrations();
    return registrations.some((registration) => location.href.startsWith(registration.scope));
  };

  const boot = async () => {
    if (location.pathname !== '/credential-handoff' || location.search || location.hash || !window.opener || window.opener === window || await scopeCoversHandoff()) {
      stop();
      return;
    }
    stage = 'READY';
    window.addEventListener('message', bind);
    window.opener.postMessage({ version: VERSION, type: 'HANDOFF_READY' }, location.origin);
    arm();
  };

  void boot().catch(stop);
})();
