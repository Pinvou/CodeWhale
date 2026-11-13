export function normalizeAnomalyStatus(value) {
  return typeof value === 'string' ? value.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, '').toUpperCase() : '';
}

export function anomalyStatusText(copy, value, isScope = false) {
  const status = normalizeAnomalyStatus(value);
  if (status === 'OPEN' || status === 'RESOLVED') return copy[status] + ' (' + status + ')';
  return status ? copy.unknown + ' (' + value + ')' : isScope ? copy.all : copy.unknown;
}
