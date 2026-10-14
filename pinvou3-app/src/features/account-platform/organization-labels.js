// Translate display text only. Protocol values and unknown diagnostic values
// remain unchanged in requests, comparisons and responses.
export function organizationLabel(copy, group, value) {
  const key = `enum${group}_${value}`;
  return Object.prototype.hasOwnProperty.call(copy, key) ? copy[key] : value;
}
