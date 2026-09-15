// Independent test oracle for the approval protocol's restricted JCS domain.
// Production JSON numbers are only small, closed protocol discriminants;
// domain int64 values must already be canonical decimal strings.
import { createHash } from 'node:crypto';

export function canonicalApprovalJSON(value) {
  if (value === null) return 'null';
  if (typeof value === 'boolean') return value ? 'true' : 'false';
  if (typeof value === 'string') {
    if (!value.isWellFormed()) throw new Error('invalid Unicode scalar sequence');
    return JSON.stringify(value);
  }
  if (typeof value === 'number') {
    if (!Number.isSafeInteger(value) || Object.is(value, -0)) throw new Error('unsafe protocol number');
    return JSON.stringify(value);
  }
  if (typeof value !== 'object' || Array.isArray(value) || Object.getPrototypeOf(value) !== Object.prototype) {
    throw new Error('unsupported approval value');
  }
  // JS default sorting compares UTF-16 code units, as required by RFC 8785.
  return '{' + Object.keys(value).sort().map(key => canonicalApprovalJSON(key) + ':' + canonicalApprovalJSON(value[key])).join(',') + '}';
}

export function approvalSHA256(text) {
  return createHash('sha256').update(text, 'utf8').digest('hex');
}

export function canonicalInt64(text) {
  if (typeof text !== 'string' || !/^(?:0|-?[1-9][0-9]*)$/.test(text)) throw new Error('noncanonical int64');
  const value = BigInt(text);
  if (value < -(1n << 63n) || value > (1n << 63n) - 1n) throw new Error('int64 overflow');
  return value;
}

// Fixture legacy fragments contain domain int64 numbers, not protocol numbers.
// Quote their original lexemes BEFORE JSON.parse can round them to binary64.
export function parseLegacyDomainJSON(raw) {
  let converted = '';
  for (let offset = 0; offset < raw.length;) {
    if (raw[offset] === '"') {
      const start = offset++;
      let closed = false;
      while (offset < raw.length) {
        const char = raw[offset++];
        if (char === '\\') offset++;
        else if (char === '"') { closed = true; break; }
      }
      if (!closed) throw new Error('unterminated fixture string');
      converted += raw.slice(start, offset);
    } else if (raw[offset] === '-' || /[0-9]/.test(raw[offset])) {
      const token = raw.slice(offset).match(/^[^\s,}\]]+/)[0];
      canonicalInt64(token);
      converted += JSON.stringify(token);
      offset += token.length;
    } else converted += raw[offset++];
  }
  return JSON.parse(converted);
}
