export const MICROCREDITS_PER_POINT = 10_000;

const locales = {
  zh: 'zh-CN',
  en: 'en-US',
  ja: 'ja-JP',
};

export function microcreditsToPoints(value) {
  if (value == null || value === '') return null;
  const numeric = Number(value);
  return Number.isSafeInteger(numeric) ? numeric / MICROCREDITS_PER_POINT : null;
}

function exactInteger(value) {
  if (typeof value === 'bigint') return value;
  if (typeof value === 'number') return Number.isSafeInteger(value) ? BigInt(value) : null;
  if (typeof value !== 'string' || !/^-?\d+$/u.test(value)) return null;
  return BigInt(value);
}

function formatScaledInteger(value, digits, language, fixedFraction) {
  const integer = exactInteger(value);
  if (integer == null) return '—';
  const magnitude = integer < 0n ? -integer : integer;
  const scale = 10n ** BigInt(digits);
  const locale = locales[language] || language || undefined;
  const formatter = new Intl.NumberFormat(locale, { maximumFractionDigits: 0 });
  const sign = integer < 0n ? formatter.formatToParts(-1n).find(part => part.type === 'minusSign').value : '';
  const whole = formatter.format(magnitude / scale);
  let fraction = String(magnitude % scale).padStart(digits, '0');
  if (!fixedFraction) fraction = fraction.replace(/0+$/u, '');
  const decimal = new Intl.NumberFormat(locale).formatToParts(1.1).find(part => part.type === 'decimal').value;
  return `${sign}${whole}${fraction ? decimal + fraction : ''}`;
}

export function formatPoints(value, language) {
  return formatScaledInteger(value, 4, language, false);
}

export function formatMoney(value, language) {
  const formatted = formatScaledInteger(value, 2, language, true);
  return formatted === '—' ? formatted : `¥${formatted}`;
}

export function subtractMicrocredits(posted, reserved) {
  const left = exactInteger(posted);
  const right = exactInteger(reserved);
  return left == null || right == null ? null : String(left - right);
}
