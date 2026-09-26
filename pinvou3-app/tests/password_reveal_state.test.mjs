import assert from 'node:assert/strict';
import test from 'node:test';
import {
  beginPasswordEdit,
  completePasswordEdit,
  concealPassword,
  expirePasswordEdit,
  isPasswordEditKey,
  passwordCanReveal,
  protectedPasswordState,
  togglePasswordReveal,
} from '../src/features/account-platform/password-reveal-state.js';

function manualInsert(state, before, inserted, selection = before.length) {
  const pending = beginPasswordEdit(state, {
    trusted: true,
    inputType: 'insertText',
    intent: 'keyboard',
    data: inserted,
    value: before,
    selectionStart: selection,
    selectionEnd: selection,
  });
  const value = `${before.slice(0, selection)}${inserted}${before.slice(selection)}`;
  return completePasswordEdit(pending, {
    trusted: true,
    inputType: 'insertText',
    data: inserted,
    value,
    selectionStart: selection + inserted.length,
    selectionEnd: selection + inserted.length,
  });
}

test('only a paired trusted manual edit can become revealable', () => {
  const manual = manualInsert(protectedPasswordState(''), '', 'typed-secret', 0);
  assert.equal(passwordCanReveal(manual, 'typed-secret'), true);
  const shown = togglePasswordReveal(manual, 'typed-secret');
  assert.equal(shown.revealed, true);
  assert.equal(shown.manualValue, 'typed-secret');

  const autofill = completePasswordEdit(protectedPasswordState(''), {
    trusted: true, inputType: 'insertReplacementText', data: null,
    value: 'saved-secret', selectionStart: 12, selectionEnd: 12,
  });
  assert.equal(passwordCanReveal(autofill, 'saved-secret'), false);
  assert.equal(togglePasswordReveal(autofill, 'saved-secret').revealed, false);

  const trustedAutofillBeforeInput = beginPasswordEdit(protectedPasswordState(''), {
    trusted: true, inputType: 'insertText', data: 'saved-secret',
    value: '', selectionStart: 0, selectionEnd: 0,
  });
  assert.equal(trustedAutofillBeforeInput.pending, null, 'trusted input without a human edit intent stays protected');

  const scripted = completePasswordEdit(protectedPasswordState(''), {
    trusted: false, inputType: 'insertText', data: 'scripted-secret',
    value: 'scripted-secret', selectionStart: 15, selectionEnd: 15,
  });
  assert.equal(passwordCanReveal(scripted, 'scripted-secret'), false);

  assert.equal(isPasswordEditKey('a'), true);
  assert.equal(isPasswordEditKey('Backspace'), true);
  assert.equal(isPasswordEditKey('Enter'), false, 'confirming a saved-password picker cannot authorize its fill');
  assert.equal(isPasswordEditKey('ArrowDown'), false, 'navigating a saved-password picker cannot authorize its fill');
  assert.equal(isPasswordEditKey('v', true), false, 'paste provenance comes from the trusted paste event');
});

test('saved values and silent replacements remain protected', () => {
  const saved = protectedPasswordState('saved-secret');
  assert.equal(passwordCanReveal(saved, 'saved-secret'), false);
  const editedSaved = manualInsert(saved, 'saved-secret', '!', 12);
  assert.equal(passwordCanReveal(editedSaved, 'saved-secret!'), false);

  const manual = manualInsert(protectedPasswordState(''), '', 'manual-secret', 0);
  const shown = togglePasswordReveal(manual, 'manual-secret');
  const replaced = togglePasswordReveal(shown, 'password-manager-replacement');
  assert.equal(replaced.revealed, false);
  assert.equal(replaced.manualValue, '');
  assert.equal(passwordCanReveal(replaced, 'password-manager-replacement'), false);
});

test('an active manual reveal follows only a precisely matched human edit', () => {
  const manual = manualInsert(protectedPasswordState(''), '', 'manual', 0);
  const shown = togglePasswordReveal(manual, 'manual');
  const updated = manualInsert(shown, 'manual', '!', 6);
  assert.equal(updated.revealed, true);
  assert.equal(updated.manualValue, 'manual!');

  const pending = beginPasswordEdit(updated, {
    trusted: true, inputType: 'insertText', intent: 'keyboard', data: '?',
    value: 'manual!', selectionStart: 7, selectionEnd: 7,
  });
  const replaced = completePasswordEdit(pending, {
    trusted: true, inputType: 'insertText', data: 'saved',
    value: 'saved', selectionStart: 5, selectionEnd: 5,
  });
  assert.equal(replaced.revealed, false);
  assert.equal(replaced.manualValue, '');
});

test('paste, expiry, blur, submit, reset and independent confirmation state fail closed', () => {
  const pastePending = beginPasswordEdit(protectedPasswordState(''), {
    trusted: true,
    inputType: 'insertFromPaste',
    intent: 'paste',
    data: null,
    transferData: 'pasted-secret',
    value: '', selectionStart: 0, selectionEnd: 0,
  });
  const pasted = completePasswordEdit(pastePending, {
    trusted: true,
    inputType: 'insertFromPaste',
    data: null,
    value: 'pasted-secret', selectionStart: 13, selectionEnd: 13,
  });
  assert.equal(passwordCanReveal(pasted, 'pasted-secret'), true);

  const expiredPending = beginPasswordEdit(protectedPasswordState(''), {
    trusted: true, inputType: 'insertText', intent: 'keyboard', data: 'x',
    value: '', selectionStart: 0, selectionEnd: 0,
  });
  assert.equal(expirePasswordEdit(expiredPending, expiredPending.pending, '').source, 'empty');

  for (const reason of ['blur', 'submit', 'reset', 'mode', 'close', 'new-guest']) {
    const shown = togglePasswordReveal(pasted, 'pasted-secret');
    const concealed = concealPassword(shown, 'pasted-secret', true);
    assert.equal(concealed.revealed, false, reason);
    assert.equal(concealed.manualValue, '', reason);
  }

  const password = manualInsert(protectedPasswordState(''), '', 'first-field', 0);
  const confirmation = manualInsert(protectedPasswordState(''), '', 'second-field', 0);
  assert.equal(togglePasswordReveal(password, 'first-field').manualValue, 'first-field');
  assert.equal(togglePasswordReveal(confirmation, 'second-field').manualValue, 'second-field');
});

test('unicode edits use exact DOM offsets and stale authorizations cannot unlock another mutation', () => {
  let state = manualInsert(protectedPasswordState(''), '', 'A', 0);
  state = manualInsert(state, 'A', '🔐', 1);
  state = manualInsert(state, 'A🔐', 'B', 3);
  assert.equal(passwordCanReveal(state, 'A🔐B'), true);

  const deleting = beginPasswordEdit(state, {
    trusted: true, inputType: 'deleteContentBackward', intent: 'keyboard', data: null,
    value: 'A🔐B', selectionStart: 3, selectionEnd: 3,
  });
  const afterDelete = completePasswordEdit(deleting, {
    trusted: true, inputType: 'deleteContentBackward', data: null,
    value: 'AB', selectionStart: 1, selectionEnd: 1,
  });
  assert.equal(passwordCanReveal(afterDelete, 'AB'), true);

  const pending = beginPasswordEdit(protectedPasswordState(''), {
    trusted: true, inputType: 'insertText', intent: 'keyboard', data: 'x',
    value: '', selectionStart: 0, selectionEnd: 0,
  });
  const mismatched = completePasswordEdit(pending, {
    trusted: true, inputType: 'insertText', data: 'saved-secret',
    value: 'saved-secret', selectionStart: 12, selectionEnd: 12,
  });
  assert.equal(passwordCanReveal(mismatched, 'saved-secret'), false);
});

test('login, register and forgot password modes use the intended autocomplete contract', async () => {
  const { readFile } = await import('node:fs/promises');
  const popover = await readFile(new URL('../src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8');
  const passwordInput = await readFile(new URL('../src/features/account-platform/PasswordInput.jsx', import.meta.url), 'utf8');
  assert.match(popover, /autoComplete=\{loginMode === 'password' \? 'current-password' : 'new-password'\}/);
  assert.match(popover, /autoComplete="new-password"/);
  assert.match(popover, /setPassword\(''\);\s*setPasswordConfirmation\(''\);\s*setCredentialEpoch/);
  assert.match(popover, /resetKey=\{`\$\{auth\}:\$\{loginMode\}:\$\{open \? 'open' : 'closed'\}:\$\{credentialEpoch\}:password`\}/);
  assert.match(popover, /resetKey=\{`\$\{auth\}:\$\{loginMode\}:\$\{open \? 'open' : 'closed'\}:\$\{credentialEpoch\}:confirmation`\}/);
  assert.match(passwordInput, /type="password"/);
  assert.doesNotMatch(passwordInput, /type=\{revealed \? 'text' : 'password'\}/);
  assert.match(passwordInput, /event\.currentTarget\.contains\(event\.relatedTarget\)/,
    'moving focus to the reveal control must not trigger group blur validation');
  assert.match(passwordInput, /onMouseDown=\{\(event\) => event\.preventDefault\(\)\}/,
    'mouse reveal keeps the credential input focused');
});
