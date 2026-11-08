// Payment and provider bills use the same Shanghai half-open natural day.
// Assemble ISO fields explicitly; locale display ordering is not an API contract.
export function shanghaiBusinessDate(instant) {
  if (!(instant instanceof Date) || !Number.isFinite(instant.getTime())) {
    throw new TypeError('a valid business instant is required');
  }
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: 'Asia/Shanghai', calendar: 'gregory', numberingSystem: 'latn',
    year: 'numeric', month: '2-digit', day: '2-digit',
  }).formatToParts(instant);
  const field = (name) => parts.find((part) => part.type === name)?.value;
  const date = `${field('year')}-${field('month')}-${field('day')}`;
  if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) throw new TypeError('business date formatting failed');
  return date;
}
