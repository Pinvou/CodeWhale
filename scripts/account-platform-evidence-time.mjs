// RFC3339 calendar validation shared by offline evidence gates. No I/O.
export function evidenceTimestamp(value, name) {
  const match = typeof value === 'string' && value.match(/^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|[+-]\d{2}:\d{2})$/i);
  const fail = () => { throw new Error(`${name} must be a valid RFC3339 timestamp`); };
  if (!match) fail();
  const [,year,month,day,hour,minute,second,zone] = match;
  const y = Number(year), m = Number(month), d = Number(day);
  const leap = y % 4 === 0 && (y % 100 !== 0 || y % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  if (m < 1 || m > 12 || d < 1 || d > days[m-1] || Number(hour) > 23 || Number(minute) > 59 || Number(second) > 59) fail();
  if (zone.toUpperCase() !== 'Z' && (Number(zone.slice(1,3)) > 23 || Number(zone.slice(4)) > 59)) fail();
  const parsed = Date.parse(value);
  if (!Number.isFinite(parsed)) fail();
  return parsed;
}
