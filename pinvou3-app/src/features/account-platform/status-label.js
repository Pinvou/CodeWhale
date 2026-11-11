import { accountStatusCopy } from '../../shared/i18n.js';

const own = (object, key) => object != null && Object.prototype.hasOwnProperty.call(object, key);

// Display only: callers keep the original enum for requests, filters and guards.
// Contextual labels override the common vocabulary. Domain wrappers use a
// closed dictionary. Unknowns remain diagnostic text, never HTML or success.
export function statusLabel(value, language, labels, closed = false) {
  const copy = own(accountStatusCopy, language) ? accountStatusCopy[language] : accountStatusCopy.en;
  if (typeof value !== 'string' || value.length === 0) return copy.missing;
  const dictionary = labels ?? copy.labels;
  if (own(dictionary, value) && typeof dictionary[value] === 'string' && dictionary[value]) return dictionary[value];
  if (!closed && labels && own(copy.labels, value) && typeof copy.labels[value] === 'string' && copy.labels[value]) return copy.labels[value];
  // Bound untrusted display length and make controls/bidi marks visible. Do not
  // coerce objects or normalize a malformed code into a recognized status.
  let preview = '', length = 0, truncated = false;
  for (const point of value) {
    if (length++ === 120) { truncated = true; break; }
    preview += point;
  }
  const code = preview.replace(/[\u0000-\u001f\u007f-\u009f\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]/g,
    character => `\\u${character.charCodeAt(0).toString(16).padStart(4, '0')}`);
  return `${copy.unknown} (${code}${truncated ? '…' : ''})`;
}

export function auditExportStatusLabel(value, language) {
  const copy = own(accountStatusCopy, language) ? accountStatusCopy[language] : accountStatusCopy.en;
  return statusLabel(value, language, copy.auditExports, true);
}

export function accountUserStatusLabel(value, language) {
  const copy = own(accountStatusCopy, language) ? accountStatusCopy[language] : accountStatusCopy.en;
  return statusLabel(value, language, copy.accountUsers, true);
}

export function paymentOrderStatusLabel(value, language) {
  const copy = own(accountStatusCopy, language) ? accountStatusCopy[language] : accountStatusCopy.en;
  return statusLabel(value, language, copy.paymentOrders, true);
}
