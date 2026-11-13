export const refundTimeZone = 'Asia/Shanghai';
const clock = new Intl.DateTimeFormat('en-GB', {
  timeZone: refundTimeZone, year: 'numeric', month: '2-digit', day: '2-digit',
  hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23',
});
const wireDate = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

export function refundCreatedControl(value) {
  if (value === '' || value == null) return '';
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return '';
  const parts = Object.fromEntries(clock.formatToParts(date).map(({ type, value: text }) => [type, text]));
  return parts.year.padStart(4, '0') + '-' + parts.month + '-' + parts.day + 'T' + parts.hour + ':' + parts.minute + ':' + parts.second;
}

export function refundCreatedWire(value, reference) {
  if (!value) return '';
  const civil = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2})?$/.test(value) ? value.length === 16 ? value + ':00' : value : '';
  if (!civil) return null;
  if (reference && refundCreatedControl(reference) === civil) return wireDate.test(reference) ? reference : new Date(reference).toISOString();
  const wall = new Date(civil + 'Z');
  if (!Number.isFinite(wall.getTime()) || wall.toISOString().slice(0, 19) !== civil) return null;
  // Reconstruct the timezone's offset instead of relying on the administrator's
  // machine timezone. Check the roundtrip for non-existent civil times.
  let candidate = wall.getTime();
  for (let pass = 0; pass < 3; pass += 1) {
    const actual = refundCreatedControl(candidate);
    if (actual === civil) return new Date(candidate).toISOString();
    candidate += wall.getTime() - Date.parse(actual + 'Z');
  }
  return null;
}
