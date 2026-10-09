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
  const passwordRevealOutput = form.querySelector('[data-password-reveal-output]');
  if (password && eye && passwordRevealOutput) {
    const manualInputTypes = new Set([
      'insertText', 'insertCompositionText', 'insertFromComposition',
      'insertFromPaste', 'insertFromDrop', 'deleteContentBackward',
      'deleteContentForward', 'deleteByCut', 'deleteByDrag',
    ]);
    let source = password.value ? 'protected' : 'empty';
    let manualInputPending = null;
    let lastManualValue = '';
    let revealed = false;
    let revealCheckPending = false;
    let manualTransferIntent = null;

    const expectedManualMutation = event => {
      const start = password.selectionStart;
      const end = password.selectionEnd;
      if (!Number.isInteger(start) || !Number.isInteger(end) || start < 0 || end < start) return null;
      const beforeValue = password.value;
      let inserted = '';
      if (event.inputType.startsWith('insert')) {
        inserted = typeof event.data === 'string'
          ? event.data
          : event.dataTransfer?.getData?.('text/plain')
            ?? (manualTransferIntent?.inputType === event.inputType
              && manualTransferIntent.start === start
              && manualTransferIntent.end === end
              ? manualTransferIntent.data
              : null);
        if (typeof inserted !== 'string') return null;
      } else if ((event.inputType === 'deleteByCut' || event.inputType === 'deleteByDrag') && start === end) {
        return null;
      }
      const characters = Array.from(beforeValue);
      const unitBoundaries = [0];
      for (const character of characters) unitBoundaries.push(unitBoundaries.at(-1) + character.length);
      const candidates = [];
      const addCandidate = mode => {
        const offsetsAreUnits = mode === 'units';
        if (offsetsAreUnits && (!unitBoundaries.includes(start) || !unitBoundaries.includes(end))) return;
        if (!offsetsAreUnits && (start > characters.length || end > characters.length)) return;
        let replaceStart = start;
        let replaceEnd = end;
        if (event.inputType === 'deleteContentBackward' && start === end) {
          if (start === 0) return;
          replaceStart = offsetsAreUnits ? unitBoundaries[unitBoundaries.indexOf(start) - 1] : start - 1;
        } else if (event.inputType === 'deleteContentForward' && start === end) {
          const limit = offsetsAreUnits ? beforeValue.length : characters.length;
          if (end === limit) return;
          replaceEnd = offsetsAreUnits ? unitBoundaries[unitBoundaries.indexOf(end) + 1] : end + 1;
        }
        const unitStart = offsetsAreUnits ? replaceStart : unitBoundaries[replaceStart];
        const unitEnd = offsetsAreUnits ? replaceEnd : unitBoundaries[replaceEnd];
        const afterValue = `${beforeValue.slice(0, unitStart)}${inserted}${beforeValue.slice(unitEnd)}`;
        const carets = [
          replaceStart + (offsetsAreUnits ? inserted.length : Array.from(inserted).length),
          unitStart + inserted.length,
        ];
        for (const selection of carets) {
          const candidate = { afterValue, selection };
          if (!candidates.some(item => item.afterValue === candidate.afterValue && item.selection === candidate.selection)) {
            candidates.push(candidate);
          }
        }
      };
      addCandidate('units');
      addCandidate('points');
      if (candidates.length === 0) return null;
      return {
        inputType: event.inputType,
        inputData: typeof event.data === 'string' ? event.data : null,
        beforeValue,
        candidates,
      };
    };

    const renderPasswordReveal = () => {
      const revealable = source === 'manual' && password.value !== '' && password.value === lastManualValue;
      if (!revealable) revealed = false;
      const showing = revealable && revealed;
      if (showing) {
        passwordRevealOutput.hidden = false;
        passwordRevealOutput.setAttribute('aria-hidden', 'false');
        passwordRevealOutput.textContent = lastManualValue;
      } else {
        passwordRevealOutput.textContent = '';
        passwordRevealOutput.setAttribute('aria-hidden', 'true');
        passwordRevealOutput.hidden = true;
      }
      eye.hidden = !revealable;
      eye.disabled = !revealable;
      eye.setAttribute('aria-pressed', String(showing));
    };
    const protectPassword = () => {
      source = password.value ? 'protected' : 'empty';
      manualInputPending = null;
      manualTransferIntent = null;
      lastManualValue = '';
      revealed = false;
      renderPasswordReveal();
    };
    const checkRevealedValue = () => {
      revealCheckPending = false;
      if (!revealed) return;
      if (source !== 'manual' || password.value !== lastManualValue || !password.value) {
        protectPassword();
        return;
      }
      revealCheckPending = true;
      requestAnimationFrame(checkRevealedValue);
    };
    const scheduleRevealedValueCheck = () => {
      if (!revealed || revealCheckPending) return;
      revealCheckPending = true;
      requestAnimationFrame(checkRevealedValue);
    };

    password.addEventListener('beforeinput', event => {
      if (manualInputPending) {
        // A second/nested beforeinput before the first mutation is resolved is
        // not the browser's normal edit sequence. Revoke both authorizations.
        protectPassword();
        return;
      }
      // A non-empty value that appeared without a preceding manual edit is an
      // autofill/password-manager value and stays protected until cleared.
      if ((source === 'empty' && password.value)
          || (source === 'manual' && password.value !== lastManualValue)) protectPassword();
      if (event.inputType.startsWith('delete')
          && (source !== 'manual' || password.value !== lastManualValue)) {
        protectPassword();
        return;
      }
      const manualEdit = event.isTrusted === true
        && manualInputTypes.has(event.inputType)
        && (source === 'empty' || source === 'manual');
      if (!manualEdit) {
        protectPassword();
        return;
      }
      const pending = expectedManualMutation(event);
      manualTransferIntent = null;
      const compositionCommit = event.inputType === 'insertCompositionText'
        || event.inputType === 'insertFromComposition';
      if (!pending || pending.candidates.every(candidate => candidate.afterValue === pending.beforeValue) && !compositionCommit) {
        protectPassword();
        return;
      }
      manualInputPending = pending;
      // beforeinput can be cancelled by the browser (for example at
      // maxlength) without a matching input event. Never let that intent
      // authorize a later mutation, even when its inputType happens to match.
      setTimeout(() => {
        if (manualInputPending === pending) protectPassword();
      }, 0);
    });
    for (const [eventName, inputType] of [['paste', 'insertFromPaste'], ['drop', 'insertFromDrop']]) {
      password.addEventListener(eventName, event => {
        const data = event.isTrusted === true ? event.clipboardData?.getData?.('text/plain')
          ?? event.dataTransfer?.getData?.('text/plain') : null;
        manualTransferIntent = typeof data === 'string' ? {
          inputType, data, start: password.selectionStart, end: password.selectionEnd,
        } : null;
        const intent = manualTransferIntent;
        setTimeout(() => {
          if (manualTransferIntent === intent) manualTransferIntent = null;
        }, 0);
      });
    }
    for (const eventName of ['keydown', 'keyup', 'paste', 'cut', 'drop', 'compositionstart', 'compositionend']) {
      password.addEventListener(eventName, () => {
        // A new physical edit boundary cannot consume an unmatched
        // beforeinput authorization left by an earlier browser action.
        if (manualInputPending) protectPassword();
      });
    }
    password.addEventListener('input', event => {
      const pending = manualInputPending;
      manualInputPending = null;
      if (!password.value) {
        source = 'empty';
        lastManualValue = '';
      } else if (pending
          && event.isTrusted === true
          && event.inputType === pending.inputType
          && (pending.inputData === null || event.data === pending.inputData)
          && pending.candidates.some(candidate => password.value === candidate.afterValue
            && password.selectionStart === candidate.selection
            && password.selectionEnd === candidate.selection)) {
        source = 'manual';
        lastManualValue = password.value;
      } else {
        source = 'protected';
        lastManualValue = '';
      }
      renderPasswordReveal();
    });
    password.addEventListener('animationstart', event => {
      if (event.animationName === 'pv-password-autofill-start') protectPassword();
    });
    password.addEventListener('change', () => {
      if (password.value !== lastManualValue) protectPassword();
    });
    password.addEventListener('blur', event => {
      if (event.relatedTarget !== eye) protectPassword();
    });
    eye.addEventListener('mousedown', event => event.preventDefault());
    eye.addEventListener('click', () => {
      // Recheck the value in case a password manager replaced it without an
      // observable input event while the manual reveal control was visible.
      if (source !== 'manual' || password.value !== lastManualValue || !password.value) {
        protectPassword();
        return;
      }
      revealed = !revealed;
      renderPasswordReveal();
      scheduleRevealedValueCheck();
    });
    eye.addEventListener('blur', event => {
      if (event.relatedTarget !== password) protectPassword();
    });
    form.addEventListener('submit', protectPassword);
    form.addEventListener('reset', () => {
      // reset dispatches before the browser applies the default value. Clear
      // plaintext immediately, then classify the post-reset value after that
      // default action and before accepting another edit authorization.
      protectPassword();
      setTimeout(protectPassword, 0);
    });
    renderPasswordReveal();
  }

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
