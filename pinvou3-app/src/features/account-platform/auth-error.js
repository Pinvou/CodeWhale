import { accountPlatformErrorCodes } from './error-localization.js';

const MAX_RETRY_SECONDS = 86_400;

export function retryAfterSeconds(value, now = Date.now()) {
  if (typeof value === 'number') return Number.isSafeInteger(value) && value > 0 ? Math.min(value, MAX_RETRY_SECONDS) : 0;
  if (typeof value !== 'string' || !value.trim()) return 0;
  const text = value.trim();
  if (/^\d+$/.test(text)) return retryAfterSeconds(Number(text), now);
  // Reject numeric-looking malformed values instead of parsing them as dates.
  if (!/^[A-Za-z]{3}, /.test(text)) return 0;
  const seconds = Math.ceil((Date.parse(text) - now) / 1000);
  return retryAfterSeconds(seconds, now);
}

export function responseRetryAfter(payload, header) {
  return retryAfterSeconds(payload?.details?.retry_after_seconds) || retryAfterSeconds(header);
}

export function normalizeAccountAuthError(reason) {
  if (reason?.name === 'AbortError') return reason;
  const text = typeof reason === 'string' ? reason : typeof reason?.message === 'string' ? reason.message : '';
  const mobile = /^mobile_auth_failed: ([A-Z][A-Z0-9_]{0,127})(?::([0-9]+))?$/.exec(text);
  const codes = accountPlatformErrorCodes(reason);
  const structured = typeof reason?.code === 'string' && /^[A-Za-z][A-Za-z0-9_]{0,127}$/.test(reason.code) && reason.code !== 'REQUEST_FAILED' ? reason.code : '';
  const native = codes.find(value => value !== 'mobile_auth_failed' && /^(oidc_|mobile_auth_|account_platform_|portal_|authentication_required$)/.test(value));
  const code = structured || mobile?.[1] || native || 'REQUEST_FAILED';
  // Preserve only safe identifier candidates for existing nested-error
  // localization, never the provider's raw text or a generic wrapper code.
  const error = new Error(code === 'REQUEST_FAILED' ? codes.join(': ') || code : code);
  error.code = code;
  error.retryAfterSeconds = retryAfterSeconds(reason?.retryAfterSeconds)
    || responseRetryAfter(reason, null)
    || (code === 'SMS_RATE_LIMITED' ? retryAfterSeconds(mobile?.[2]) : 0);
  return error;
}
