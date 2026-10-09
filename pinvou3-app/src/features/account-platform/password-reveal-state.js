const manualInputTypes = new Set([
  'insertText', 'insertCompositionText', 'insertFromComposition',
  'insertFromPaste', 'insertFromDrop', 'deleteContentBackward',
  'deleteContentForward', 'deleteByCut', 'deleteByDrag',
]);

/**
 * @typedef {'empty' | 'protected' | 'manual'} PasswordSource
 * @typedef {'keyboard' | 'paste' | 'drop' | 'cut' | 'composition'} PasswordEditIntent
 * @typedef {{ value: string, selection: number }} PasswordMutationCandidate
 * @typedef {{ inputType: string, data: string | null, candidates: PasswordMutationCandidate[] }} PendingPasswordEdit
 * @typedef {{ source: PasswordSource, manualValue: string, revealed: boolean, pending: PendingPasswordEdit | null }} PasswordRevealState
 * @typedef {{
 *   trusted: boolean,
 *   inputType: string,
 *   intent?: PasswordEditIntent,
 *   data?: string | null,
 *   transferData?: string | null,
 *   value: string,
 *   selectionStart: number | null,
 *   selectionEnd: number | null,
 * }} PasswordEdit
 * @typedef {{
 *   trusted: boolean,
 *   inputType: string,
 *   data?: string | null,
 *   value: string,
 *   selectionStart: number | null,
 *   selectionEnd: number | null,
 * }} CompletedPasswordEdit
 */

/** @param {string} inputType @param {PasswordEditIntent | undefined} intent */
function intentAllows(inputType, intent) {
  if (inputType === 'insertFromPaste') return intent === 'paste';
  if (inputType === 'insertFromDrop' || inputType === 'deleteByDrag') return intent === 'drop';
  if (inputType === 'deleteByCut') return intent === 'cut';
  if (inputType === 'insertCompositionText' || inputType === 'insertFromComposition') return intent === 'composition';
  return intent === 'keyboard';
}

/**
 * Only keys that directly request a text edit may authorize an insert/delete
 * mutation. Navigation and confirmation keys are also used to choose saved
 * credentials in browser/password-manager UI and must never grant provenance.
 * @param {string} key
 * @param {boolean} [ctrlKey]
 * @param {boolean} [metaKey]
 * @param {boolean} [altKey]
 * @param {boolean} [altGraph]
 */
export function isPasswordEditKey(key, ctrlKey = false, metaKey = false, altKey = false, altGraph = false) {
  if (key === 'Backspace' || key === 'Delete') return true;
  return Array.from(key).length === 1 && !metaKey && ((!ctrlKey && !altKey) || altGraph);
}

/** @param {string} [value] @returns {PasswordRevealState} */
export function protectedPasswordState(value = '') {
  return { source: value ? 'protected' : 'empty', manualValue: '', revealed: false, pending: null };
}

/** @param {string} value @param {PasswordEdit} edit @returns {PasswordMutationCandidate[]} */
function expectedMutations(value, edit) {
  const { inputType, selectionStart: start, selectionEnd: end } = edit;
  if (typeof start !== 'number' || typeof end !== 'number'
      || !Number.isInteger(start) || !Number.isInteger(end) || start < 0 || end < start) return [];
  let inserted = '';
  if (inputType.startsWith('insert')) {
    const insertedValue = typeof edit.data === 'string' ? edit.data : edit.transferData;
    if (typeof insertedValue !== 'string') return [];
    inserted = insertedValue;
  } else if ((inputType === 'deleteByCut' || inputType === 'deleteByDrag') && start === end) return [];

  const characters = Array.from(value);
  const unitBoundaries = [0];
  for (const character of characters) unitBoundaries.push(unitBoundaries[unitBoundaries.length - 1] + character.length);
  /** @type {PasswordMutationCandidate[]} */
  const candidates = [];
  /** @param {boolean} offsetsAreUnits */
  const add = (offsetsAreUnits) => {
    if (offsetsAreUnits && (!unitBoundaries.includes(start) || !unitBoundaries.includes(end))) return;
    if (!offsetsAreUnits && (start > characters.length || end > characters.length)) return;
    let replaceStart = start;
    let replaceEnd = end;
    if (inputType === 'deleteContentBackward' && start === end) {
      if (start === 0) return;
      replaceStart = offsetsAreUnits ? unitBoundaries[unitBoundaries.indexOf(start) - 1] : start - 1;
    } else if (inputType === 'deleteContentForward' && start === end) {
      const limit = offsetsAreUnits ? value.length : characters.length;
      if (end === limit) return;
      replaceEnd = offsetsAreUnits ? unitBoundaries[unitBoundaries.indexOf(end) + 1] : end + 1;
    }
    const unitStart = offsetsAreUnits ? replaceStart : unitBoundaries[replaceStart];
    const unitEnd = offsetsAreUnits ? replaceEnd : unitBoundaries[replaceEnd];
    const nextValue = `${value.slice(0, unitStart)}${inserted}${value.slice(unitEnd)}`;
    const carets = [
      replaceStart + (offsetsAreUnits ? inserted.length : Array.from(inserted).length),
      unitStart + inserted.length,
    ];
    for (const selection of carets) {
      if (!candidates.some(candidate => candidate.value === nextValue && candidate.selection === selection)) {
        candidates.push({ value: nextValue, selection });
      }
    }
  };
  add(true);
  add(false);
  return candidates;
}

/** @param {PasswordRevealState} state @param {PasswordEdit} edit @returns {PasswordRevealState} */
export function beginPasswordEdit(state, edit) {
  if (state.pending) return protectedPasswordState(edit.value);
  if (state.source === 'manual' && edit.value !== state.manualValue) return protectedPasswordState(edit.value);
  if (state.source === 'empty' && edit.value) return protectedPasswordState(edit.value);
  if (edit.inputType?.startsWith('delete') && (state.source !== 'manual' || edit.value !== state.manualValue)) {
    return protectedPasswordState(edit.value);
  }
  if (!edit.trusted || !manualInputTypes.has(edit.inputType) || !intentAllows(edit.inputType, edit.intent)
      || !['empty', 'manual'].includes(state.source)) {
    return protectedPasswordState(edit.value);
  }
  const candidates = expectedMutations(edit.value, edit);
  const compositionCommit = edit.inputType === 'insertCompositionText' || edit.inputType === 'insertFromComposition';
  if (candidates.length === 0 || candidates.every(candidate => candidate.value === edit.value) && !compositionCommit) {
    return protectedPasswordState(edit.value);
  }
  return {
    ...state,
    pending: { inputType: edit.inputType, data: typeof edit.data === 'string' ? edit.data : null, candidates },
  };
}

/** @param {PasswordRevealState} state @param {CompletedPasswordEdit} input @returns {PasswordRevealState} */
export function completePasswordEdit(state, input) {
  const pending = state.pending;
  if (!input.value) return protectedPasswordState('');
  const matched = pending && input.trusted && input.inputType === pending.inputType
    && (pending.data === null || input.data === pending.data)
    && pending.candidates.some(candidate => candidate.value === input.value
      && candidate.selection === input.selectionStart && candidate.selection === input.selectionEnd);
  return matched
    ? { source: 'manual', manualValue: input.value, revealed: state.revealed, pending: null }
    : protectedPasswordState(input.value);
}

/**
 * @param {PasswordRevealState} state
 * @param {PendingPasswordEdit | null} pending
 * @param {string} value
 * @returns {PasswordRevealState}
 */
export function expirePasswordEdit(state, pending, value) {
  return state.pending === pending ? protectedPasswordState(value) : state;
}

/** @param {PasswordRevealState} state @param {string} currentValue @returns {PasswordRevealState} */
export function togglePasswordReveal(state, currentValue) {
  if (state.source !== 'manual' || !state.manualValue || currentValue !== state.manualValue) {
    return protectedPasswordState(currentValue);
  }
  return { ...state, revealed: !state.revealed, pending: null };
}

/** @param {PasswordRevealState} state @param {string} currentValue @param {boolean} [revoke] @returns {PasswordRevealState} */
export function concealPassword(state, currentValue, revoke = false) {
  if (revoke || state.source !== 'manual' || currentValue !== state.manualValue) return protectedPasswordState(currentValue);
  return { ...state, revealed: false, pending: null };
}

/** @param {PasswordRevealState} state @param {string} currentValue */
export function passwordCanReveal(state, currentValue) {
  return state.source === 'manual' && Boolean(state.manualValue) && currentValue === state.manualValue;
}
