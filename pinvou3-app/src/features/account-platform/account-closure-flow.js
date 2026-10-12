import { accountPlatformErrorCodes } from './error-localization.js';

export const accountClosureQueryStatus = Object.freeze({
  loading: 'loading',
  ready: 'ready',
  error: 'error',
});

export async function queryAccountClosure(client, signal) {
  try {
    return {
      status: accountClosureQueryStatus.ready,
      closure: await client.accountClosure(signal),
    };
  } catch (reason) {
    if (reason?.status === 404) {
      return { status: accountClosureQueryStatus.ready, closure: null };
    }
    if (reason?.name === 'AbortError') {
      return { status: accountClosureQueryStatus.loading, closure: null };
    }
    return { status: accountClosureQueryStatus.error, closure: null, reason };
  }
}

export function accountClosureResumeAction({ requestedClosure, queryStatus, closure, resumed }) {
  if (requestedClosure !== 'confirm' || queryStatus !== accountClosureQueryStatus.ready || resumed) {
    return { clearMarker: false, prompt: false };
  }
  return { clearMarker: true, prompt: !closure };
}

export function accountClosurePresentation({ queryStatus = accountClosureQueryStatus.ready, closure }) {
  const hasClosure = Boolean(closure);
  return {
    state: hasClosure ? 'existing' : queryStatus,
    canRequest: !hasClosure && queryStatus === accountClosureQueryStatus.ready,
    canRetry: queryStatus === accountClosureQueryStatus.error,
  };
}

export async function submitAccountClosure({ client, confirmation, key, returnTo }) {
  if (confirmation !== 'CLOSE_MY_ACCOUNT') {
    return { status: confirmation === null ? 'cancelled' : 'confirmation-mismatch' };
  }
  try {
    const closure = await client.createAccountClosure(confirmation, key);
    return { status: 'created', closure };
  } catch (reason) {
    if (!accountPlatformErrorCodes(reason).includes('RECENT_AUTHENTICATION_REQUIRED')) throw reason;
    if (typeof client.reauthenticate !== 'function') return { status: 'recent-authentication-required' };
    await client.reauthenticate(returnTo);
    return { status: 'reauthenticating' };
  }
}
