export const MICROCREDITS_PER_POINT = 10_000;

const locales = {
  zh: 'zh-CN',
  en: 'en-US',
  ja: 'ja-JP',
};

export function microcreditsToPoints(value) {
  if (value == null || value === '') return null;
  const numeric = Number(value);
  return Number.isFinite(numeric) ? numeric / MICROCREDITS_PER_POINT : null;
}

export function parsePointsInputToMicrocredits(value) {
  const match = String(value ?? '').trim().match(/^([+-]?)(\d+)(?:\.(\d{1,4}))?$/);
  if (!match) return null;
  const fraction = (match[3] || '').padEnd(4, '0');
  const absolute = BigInt(match[2]) * BigInt(MICROCREDITS_PER_POINT) + BigInt(fraction || '0');
  const signed = match[1] === '-' ? -absolute : absolute;
  if (signed > BigInt(Number.MAX_SAFE_INTEGER) || signed < BigInt(Number.MIN_SAFE_INTEGER)) return null;
  return Number(signed);
}

export function formatPoints(value, language) {
  const points = microcreditsToPoints(value);
  if (points == null) return '—';
  return points.toLocaleString(locales[language] || language || undefined, {
    minimumFractionDigits: 0,
    maximumFractionDigits: 4,
  });
}

export function formatWholePoints(value, language) {
  const points = microcreditsToPoints(value);
  if (points == null) return '—';
  return Math.floor(points).toLocaleString(locales[language] || language || undefined, {
    minimumFractionDigits: 0,
    maximumFractionDigits: 0,
  });
}

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
