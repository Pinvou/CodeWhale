export const CONTROLLED_DEVICE_MODEL_SOURCE = 'controlled_device';
export const CONTROLLER_TEMPORARY_MODEL_SOURCE = 'controller_temporary';

export function canUseControllerTemporaryModel(connection) {
  return !!(connection && connection.temporaryModelAvailable === true);
}

export function normalizeRemoteModelSource(modelSource, connection) {
  if (
    modelSource === CONTROLLER_TEMPORARY_MODEL_SOURCE
    && canUseControllerTemporaryModel(connection)
  ) {
    return CONTROLLER_TEMPORARY_MODEL_SOURCE;
  }
  return CONTROLLED_DEVICE_MODEL_SOURCE;
}

export function modelSourceForLease(modelSource, connection, previousLeaseKey, nextLeaseKey) {
  if (String(previousLeaseKey || '') !== String(nextLeaseKey || '')) {
    return canUseControllerTemporaryModel(connection)
      ? CONTROLLER_TEMPORARY_MODEL_SOURCE
      : CONTROLLED_DEVICE_MODEL_SOURCE;
  }
  return normalizeRemoteModelSource(modelSource, connection);
}

/**
 * @param {string} modelSource
 * @param {string | null | undefined} selectedModelId
 */
export function remoteModelSelectionError(modelSource, selectedModelId) {
  return modelSource === CONTROLLER_TEMPORARY_MODEL_SOURCE && !selectedModelId
    ? 'remote_model_required'
    : null;
}

/**
 * @param {{identity?: string, code?: string} | null | undefined} currentError
 * @param {string} identity
 * @param {string | null | undefined} modelError
 */
export function clearResolvedRemoteModelRequiredError(currentError, identity, modelError) {
  return !modelError
    && currentError?.identity === identity
    && currentError?.code === 'remote_model_required'
    ? null
    : currentError;
}
