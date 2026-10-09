const MAX_ERROR_TEXT_LENGTH = 4096;
const ERROR_CODE = /^[A-Za-z][A-Za-z0-9_]{0,127}$/;
const PREFIXED_ERROR_CODE = /\b([A-Za-z][A-Za-z0-9_]{0,127})[ \t]*:/g;
const SERVER_ERROR_CODE = /\b[A-Z][A-Z0-9_]{2,127}\b/g;
const GENERIC_WRAPPER_CODES = new Set(['REQUEST_FAILED']);
/**
 * @param {Record<string, string>} object
 * @param {string} key
 * @returns {boolean}
 */
const hasOwn = (object, key) => Object.prototype.hasOwnProperty.call(object, key);

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
    const text = value.trim().slice(0, MAX_ERROR_TEXT_LENGTH);
    if (text && !texts.includes(text)) texts.push(text);
  };

  append(typeof reason === 'string' ? reason : '');
  if (reason && typeof reason === 'object') {
    const details = /** @type {Record<string, unknown>} */ (reason);
    // Preserve the candidate order so the localizer can keep a concrete
    // structured code ahead of conflicting wrapper text.
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
 * @param {unknown} reason
 * @returns {string}
 */
function structuredAccountPlatformErrorCode(reason) {
  if (!reason || typeof reason !== 'object') return '';
  const code = /** @type {Record<string, unknown>} */ (reason).code;
  return typeof code === 'string' && ERROR_CODE.test(code) ? code : '';
}

/**
 * @param {{ errors?: Record<string, string> }} copy
 * @param {Record<string, string>} overrides
 * @param {string} code
 * @returns {string}
 */
function mappedAccountPlatformError(copy, overrides, code) {
  if (hasOwn(overrides, code)) return overrides[code];
  const catalog = copy.errors || {};
  return hasOwn(catalog, code) ? catalog[code] : '';
}

/**
 * @param {{ errors?: Record<string, string>, requestFailed?: string }} copy
 * @param {unknown} reason
 * @param {Record<string, string>} [overrides]
 * @returns {string}
 */
function localizedAccountPlatformError(copy, reason, overrides = {}) {
  const fallback = copy.requestFailed || '';
  const structuredCode = structuredAccountPlatformErrorCode(reason);
  if (structuredCode && !GENERIC_WRAPPER_CODES.has(structuredCode)) {
    return mappedAccountPlatformError(copy, overrides, structuredCode) || fallback;
  }
  for (const code of accountPlatformErrorCodes(reason)) {
    const message = mappedAccountPlatformError(copy, overrides, code);
    if (message) return message;
  }
  return fallback;
}

export { accountPlatformErrorCodes, localizedAccountPlatformError };
