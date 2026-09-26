import { authorizationVersionChanged, authorizationVersions } from './authorization-ui.js';

export function createAccountSynchronizationQueue() {
  /** @type {undefined | (() => void | Promise<void>)} */
  let runner;
  let isRefreshing = false;
  let contextMutationDepth = 0;
  let isPending = false;
  let isDisposed = false;
  const idleWaiters = new Set();

  const settleIdleWaiters = () => {
    if (isRefreshing || isPending || contextMutationDepth > 0) return;
    for (const resolve of idleWaiters) resolve();
    idleWaiters.clear();
  };

  const drain = () => {
    if (isDisposed || isRefreshing || contextMutationDepth > 0 || !isPending || !runner) {
      settleIdleWaiters();
      return;
    }
    isPending = false;
    isRefreshing = true;
    const activeRunner = runner;
    Promise.resolve()
      .then(activeRunner)
      .catch(() => {})
      .finally(() => {
        isRefreshing = false;
        drain();
      });
  };

  return {
    request(/** @type {() => void | Promise<void>} */ nextRunner) {
      if (isDisposed) return;
      runner = nextRunner;
      isPending = true;
      drain();
    },
    retry() {
      if (isDisposed || !runner) return;
      isPending = true;
      drain();
    },
    beginContextMutation() {
      if (isDisposed) return;
      contextMutationDepth += 1;
    },
    endContextMutation() {
      if (isDisposed) return;
      contextMutationDepth = Math.max(0, contextMutationDepth - 1);
      drain();
    },
    isContextMutationInProgress() {
      return contextMutationDepth > 0;
    },
    whenIdle() {
      if (isDisposed || (!isRefreshing && !isPending && contextMutationDepth === 0)) return Promise.resolve();
      return new Promise((resolve) => idleWaiters.add(resolve));
    },
    dispose() {
      isDisposed = true;
      isPending = false;
      runner = undefined;
      for (const resolve of idleWaiters) resolve();
      idleWaiters.clear();
    },
  };
}

/**
 * @param {Record<string, any>} requestAccount
 * @param {Record<string, any>} currentAccount
 * @param {Record<string, any>} nextAccount
 * @param {boolean} isContextMutationInProgress
 */
export function resolveAccountSynchronization(requestAccount, currentAccount, nextAccount, isContextMutationInProgress = false) {
  if (isContextMutationInProgress || currentAccount !== requestAccount) {
    return {
      account: currentAccount,
      applyAccount: false,
      reloadContext: false,
    };
  }

  return {
    account: nextAccount,
    applyAccount: true,
    reloadContext: authorizationVersionChanged(authorizationVersions(currentAccount), authorizationVersions(nextAccount)),
  };
}
