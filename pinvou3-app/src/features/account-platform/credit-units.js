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

export function formatPoints(value, language) {
  const points = microcreditsToPoints(value);
  if (points == null) return '—';
  return points.toLocaleString(locales[language] || language || undefined, {
    minimumFractionDigits: 0,
    maximumFractionDigits: 4,
  });
}
