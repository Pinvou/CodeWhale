/** Translate only device removal errors owned by the model-store boundary. */
export function deviceModelRemovalError(failure, copy) {
  const message = String(failure?.message || failure);
  if (message === 'model_store_weight_removal_busy') return copy.weightRemovalBusy;
  if (message === 'model_store_weight_path_invalid') return copy.weightRemovalPathInvalid;
  const lockPrefix = 'model_store_weight_removal_lock_failed: ';
  if (message.startsWith(lockPrefix)) {
    return copy.weightRemovalLockFailed(message.slice(lockPrefix.length));
  }
  const pathPrefix = 'model_store_weight_path_unavailable: ';
  if (message.startsWith(pathPrefix)) {
    return copy.weightRemovalPathUnavailable(message.slice(pathPrefix.length));
  }
  return message;
}
