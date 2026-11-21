// Unknown input never becomes a new pending action or a false timeout.
export function permissionStateCopyKey(state) {
  if (state === 'allowed') return 'permissionAllowed';
  if (state === 'denied') return 'permissionDenied';
  if (state === 'expired' || state === 'timeout') return 'permissionTimedOut';
  return 'permissionUnknown';
}
