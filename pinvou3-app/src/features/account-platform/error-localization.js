const ERROR_CODE = /^[A-Za-z][A-Za-z0-9_]*$/;
const PREFIXED_ERROR_CODE = /([A-Za-z][A-Za-z0-9_]*)(?=\s*:)/g;
const SERVER_ERROR_CODE = /\b[A-Z][A-Z0-9_]{2,}\b/g;

/**
 * @param {unknown} reason
 * @returns {string[]}
 */
function errorTexts(reason) {
  /** @type {string[]} */
  const texts = [];
  /** @param {unknown} value */
  const append = (value) => {
    if (typeof value !== 'string') return;
    const text = value.trim();
    if (text && !texts.includes(text)) texts.push(text);
  };

  append(typeof reason === 'string' ? reason : '');
  if (reason && typeof reason === 'object') {
    const details = /** @type {Record<string, unknown>} */ (reason);
    // Preserve the existing contract: a concrete structured code wins over
    // conflicting wrapper text. Generic codes such as REQUEST_FAILED are not
    // in the UI catalog, so lookup naturally continues into message/cause.
    append(details.code);
    // Some Tauri/WebView versions wrap a Rust command rejection in a generic
    // REQUEST_FAILED code while preserving the domain code in message/cause.
    // Inspect all safe textual fields, but never surface their raw contents.
    append(details.message);
    append(details.error);
    if (typeof details.cause === 'string') append(details.cause);
    else if (details.cause && typeof details.cause === 'object') {
      const cause = /** @type {Record<string, unknown>} */ (details.cause);
      append(cause.message);
      append(cause.code);
    }
  }
  return texts;
}

/**
 * @param {unknown} reason
 * @returns {string[]}
 */
function accountPlatformErrorCodes(reason) {
  /** @type {string[]} */
  const codes = [];
  /** @param {unknown} code */
  const append = (code) => {
    if (typeof code === 'string' && ERROR_CODE.test(code) && !codes.includes(code)) codes.push(code);
  };

  for (const text of errorTexts(reason)) {
    append(text);
    for (const match of text.matchAll(PREFIXED_ERROR_CODE)) append(match[1]);
    for (const match of text.matchAll(SERVER_ERROR_CODE)) append(match[0]);
  }
  return codes;
}

/**
 * @param {{ errors?: Record<string, string>, requestFailed?: string }} copy
 * @param {unknown} reason
 * @param {Record<string, string>} [overrides]
 * @returns {string}
 */
function localizedAccountPlatformError(copy, reason, overrides = {}) {
  const catalog = copy?.errors || {};
  for (const code of accountPlatformErrorCodes(reason)) {
    if (Object.hasOwn(overrides, code)) return overrides[code];
    if (Object.hasOwn(catalog, code)) return catalog[code];
  }
  return copy?.requestFailed || '';
}

export { accountPlatformErrorCodes, localizedAccountPlatformError };
