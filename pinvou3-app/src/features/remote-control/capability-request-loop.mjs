export const REMOTE_CAPABILITY_RETRY_MS = 5_000;

/**
 * Keep capability discovery single-flight for one remote lease/session.
 *
 * The bridge call only confirms that the encrypted request was queued; the
 * capability snapshot arrives later through the remote-control event stream.
 * Treating bridge resolution as completion causes a React effect to resend in
 * a tight loop. This coordinator waits for the snapshot signal (`stop`) and
 * only retries after a bounded delay.
 */
export function createCapabilityRequestLoop({
  send,
  onBusy = function () {},
  onError = function () {},
  schedule = setTimeout,
  cancel = clearTimeout,
  retryMs = REMOTE_CAPABILITY_RETRY_MS,
}) {
  let activeKey = null;
  let activeSessionRef = null;
  let inFlight = false;
  let retryTimer = null;
  let generation = 0;

  const clearRetry = () => {
    if (retryTimer !== null) cancel(retryTimer);
    retryTimer = null;
  };

  const stop = () => {
    generation += 1;
    activeKey = null;
    activeSessionRef = null;
    inFlight = false;
    clearRetry();
    onBusy(false);
  };

  const request = (requestGeneration) => {
    if (requestGeneration !== generation || !activeKey || !activeSessionRef || inFlight) return;
    const sessionReference = activeSessionRef;
    inFlight = true;
    onBusy(true);
    onError(null);
    Promise.resolve()
      .then(() => {
        if (requestGeneration !== generation) return undefined;
        return send(sessionReference);
      })
      .then(
        () => {
          if (requestGeneration === generation) onError(null);
        },
        (failure) => {
          if (requestGeneration === generation) {
            onError(String((failure && failure.code) || 'errorGeneric'));
          }
        },
      )
      .finally(() => {
        if (requestGeneration !== generation) return;
        inFlight = false;
        onBusy(false);
        retryTimer = schedule(() => {
          retryTimer = null;
          request(requestGeneration);
        }, retryMs);
      });
  };

  const update = ({ enabled, key, sessionRef }) => {
    if (!enabled || !key || !sessionRef) {
      stop();
      return;
    }
    if (activeKey === key) return;
    stop();
    activeKey = key;
    activeSessionRef = sessionRef;
    request(generation);
  };

  const dispose = () => {
    generation += 1;
    activeKey = null;
    activeSessionRef = null;
    inFlight = false;
    clearRetry();
  };

  return { update, stop, dispose };
}
