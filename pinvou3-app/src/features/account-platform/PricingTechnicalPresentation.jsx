import React from 'react';
import './admin-pricing-technical.css';

const decimal = value => {
  if (typeof value === 'string' && /^(0|[1-9]\d*)$/.test(value)) return value;
  if (typeof value === 'number' && Number.isSafeInteger(value) && value >= 0) return String(value);
  if (typeof value === 'bigint' && value >= 0n) return String(value);
  return '';
};
const tokenUnits = new Set(['INPUT_TOKEN', 'CACHED_INPUT_TOKEN', 'OUTPUT_TOKEN']);
export const exactPricingInteger = decimal;

function supportedExample(unit, p, d) {
  if (!tokenUnits.has(unit.unit_code) || !p || !d || p === '0' || d === '0') return null;
  const rule = unit.rounding_rule;
  if (!['CEILING', 'FLOOR', 'HALF_UP'].includes(rule)) return null;
  // Illustrative input to the production token-metering algorithm. Quantity
  // first takes whole precision steps, then the rate is applied and rounded.
  const quantity = 5n, price = 3n, precision = BigInt(p), divisor = BigInt(d);
  const max = (1n << 63n) - 1n;
  if (precision > max || divisor > max || quantity > max / precision) return null;
  const steps = (quantity + precision - 1n) / precision;
  if (steps > max / price) return null;
  const numerator = steps * price;
  if (rule === 'CEILING' && numerator > max - (divisor - 1n)
    || rule === 'HALF_UP' && numerator > max - divisor / 2n) return null;
  const quotient = numerator / divisor, remainder = numerator % divisor;
  const adjust = rule === 'CEILING' ? remainder !== 0n : rule === 'HALF_UP' && remainder * 2n >= divisor;
  return { q: quantity, p: price, m: precision, s: steps, n: numerator, d: divisor, r: quotient + (adjust ? 1n : 0n) };
}

export function MeteringBasis({ unit, labels }) {
  const words = labels.technicalPresentation;
  const precision = decimal(unit.minimum_precision), denominator = decimal(unit.price_denominator);
  const round = Object.prototype.hasOwnProperty.call(words.rounding, unit.rounding_rule) ? words.rounding[unit.rounding_rule] : '';
  const sample = supportedExample(unit, precision, denominator);
  return <div className="adm-pricing-metering-basis">
    {precision && precision !== '0' && <span>{words.precision(precision)}</span>}
    {denominator && denominator !== '0' && <span>{words.denominator(denominator)}</span>}
    {unit.rounding_rule && <span>{round ? round + ' · ' : ''}<code>{unit.rounding_rule}</code></span>}
    {(!precision || precision === '0' || !denominator || denominator === '0' || !round) && <small>{words.unknownBasis}</small>}
    {sample ? <details className="adm-price-identifiers"><summary>{words.example}</summary><p>{words.exampleText(sample)}</p></details> : <small>{words.exampleUnavailable}</small>}
  </div>;
}

export function PricingReadProgress({ loaded, pending, failed, children, words }) {
  if (failed) return <small className="adm-pricing-read-progress">{words.updateFailed}</small>;
  if (pending || !loaded) return <small className="adm-pricing-read-progress" role="status">{pending ? words.refreshing : words.initial}</small>;
  return <small className="adm-pricing-read-progress" role="status">{children}</small>;
}
