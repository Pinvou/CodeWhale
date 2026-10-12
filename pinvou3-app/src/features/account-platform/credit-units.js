const locales = {
  zh: 'zh-CN',
  en: 'en-US',
  ja: 'ja-JP',
};

export function formatUsagePoints(value, language) {
  if (typeof value === 'number' && !Number.isSafeInteger(value)) return '—';
  if (!/^(?:0|[1-9]\d*)$/.test(String(value ?? ''))) return '—';
  // Preserve the transport's original int64 digits, then round once to the
  // existing two-decimal presentation without passing through binary Number.
  const hundredths = (BigInt(value) + 50n) / 100n;
  const locale = locales[language] || language || undefined;
  const decimal = (1.1).toLocaleString(locale).replace(/\d/g, '');
  return `${(hundredths / 100n).toLocaleString(locale)}${decimal}${String(hundredths % 100n).padStart(2, '0')}`;
}
