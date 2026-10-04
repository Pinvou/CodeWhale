export function needsAccountLogin(error) {
  return error?.status === 401 || error?.code === 'LOGIN_REQUIRED' || error?.code === 'AUTH_REQUIRED';
}

export function accountErrorMessage(error, copy) {
  if (needsAccountLogin(error)) return copy.loginRequired;
  if (error?.status === 403) return copy.permissionDenied;
  if (error?.status === 400 || error?.status === 422) return copy.invalidInput;
  if (error?.status === 409) return copy.stateConflict;
  return copy.requestFailed;
}
