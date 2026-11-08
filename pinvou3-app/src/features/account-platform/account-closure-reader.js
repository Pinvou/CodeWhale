import { queryAccountClosure } from './account-closure-flow.js';

const retired = () => new DOMException('Account closure read retired', 'AbortError');

// Initial, manual and background reads share one request for this view's owner.
// Confirmation and mutation invalidate earlier reads without cancelling writes.
export function createAccountClosureReader(client, isCurrent) {
  const epoch = client.financeSessionEpoch || 0;
  let pending = null, generation = 0, closed = false;
  const current = () => !closed && isCurrent() && (client.financeSessionEpoch || 0) === epoch;
  const invalidate = () => {
    generation++;
    pending?.controller.abort();
    pending = null;
  };
  return {
    current,
    read() {
      if (!current()) return Promise.reject(retired());
      if (pending) return pending.promise;
      const controller = new AbortController(), stamp = generation;
      const promise = Promise.resolve().then(() => {
        if (!current() || stamp !== generation) throw retired();
        return queryAccountClosure(client, controller.signal);
      }).then(result => {
        if (!current() || controller.signal.aborted || stamp !== generation) throw retired();
        return result;
      }).finally(() => { if (pending?.promise === promise) pending = null; });
      pending = { promise, controller };
      return promise;
    },
    pending: () => Boolean(pending),
    invalidate,
    dispose() { closed = true; invalidate(); },
  };
}
