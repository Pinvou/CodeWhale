(() => {
  const form = document.getElementById('pv-auth-form');
  if (!form) return;
  const intent = document.getElementById('pv-intent');
  const targetMode = document.getElementById('pv-target-mode');
  const device = document.getElementById('pv-device-id');
  const deviceKey = 'pinvou.identity.device.v1';
  let deviceID = '';
  try {
    deviceID = localStorage.getItem(deviceKey) || `web-${crypto.randomUUID()}`;
    localStorage.setItem(deviceKey, deviceID);
  } catch { deviceID = `web-${Date.now()}-${Math.random().toString(16).slice(2)}`; }
  if (device) device.value = deviceID;

  for (const button of form.querySelectorAll('[data-mode]')) {
    button.addEventListener('click', () => {
      intent.value = 'switch_mode';
      targetMode.value = button.dataset.mode;
    });
  }
  for (const button of form.querySelectorAll('[data-request-sms]')) {
    button.addEventListener('click', () => { intent.value = 'request_sms'; });
    let remaining = Number(button.dataset.cooldown || 0);
    if (remaining > 0) {
      const original = button.textContent;
      button.disabled = true;
      const tick = () => {
        button.textContent = `${remaining}s`;
        remaining -= 1;
        if (remaining < 0) { button.disabled = false; button.textContent = original; return; }
        setTimeout(tick, 1000);
      };
      tick();
    }
  }
  for (const button of form.querySelectorAll('[data-send-sms]')) button.addEventListener('click', () => { intent.value = 'send_sms'; });
  const submit = form.querySelector('[data-submit-auth]');
  if (submit) submit.addEventListener('click', () => { intent.value = 'submit'; });

  const password = document.getElementById('pv-password');
  const eye = form.querySelector('[data-toggle-password]');
  if (password && eye) eye.addEventListener('click', () => {
    password.type = password.type === 'password' ? 'text' : 'password';
    eye.setAttribute('aria-pressed', String(password.type === 'text'));
  });

  const account = document.getElementById('pv-account');
  const remember = document.getElementById('pv-remember');
  const accountKey = 'pinvou.identity.remembered-account.v1';
  if (account && remember) {
    try {
      const saved = localStorage.getItem(accountKey) || '';
      account.value = saved;
      remember.checked = Boolean(saved);
    } catch {}
    form.addEventListener('submit', () => {
      if (intent.value !== 'submit') return;
      try {
        if (remember.checked) localStorage.setItem(accountKey, account.value.trim());
        else localStorage.removeItem(accountKey);
      } catch {}
    });
  }

  const offset = document.getElementById('pv-captcha-offset');
  const piece = form.querySelector('.pv-captcha-piece');
  if (offset && piece) {
    const update = () => piece.style.setProperty('--piece-x', `${offset.value}px`);
    offset.addEventListener('input', update);
    update();
  }
})();
