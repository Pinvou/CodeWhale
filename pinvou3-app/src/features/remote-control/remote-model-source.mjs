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
    return CONTROLLED_DEVICE_MODEL_SOURCE;
  }
  return normalizeRemoteModelSource(modelSource, connection);
}
