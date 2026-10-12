export function profileCaptchaSubmissionOffset(candidate, fallback = 0) {
  const value = typeof candidate === 'number' ? candidate : Number(fallback);
  return Number.isSafeInteger(value) && value >= 0 ? value : 0;
}
