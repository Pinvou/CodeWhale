/**
 * Translate owned measurement failures; retain unrelated diagnostic detail.
 */
export function versionMeasurementError(failure, copy) {
  const message = String(failure?.message || failure);
  const messages = copy.versionMeasurementErrors;
  return Object.prototype.hasOwnProperty.call(messages, message) ? messages[message] : message;
}
