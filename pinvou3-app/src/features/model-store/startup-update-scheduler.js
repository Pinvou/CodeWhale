export const STARTUP_UPDATE_DELAY_MS = 15000;

/** Never expose backend or chunk-loader error details in the console. */
export function warnStartupUpdateFailure() {
  console.warn('model_store_startup_update_check failed');
}

/** Paint first, then capture the earliest check time without blocking rendering. */
export function prepareAfterStartupPaint(prepare, host = globalThis) {
  let secondFrame;
  const firstFrame = host.requestAnimationFrame(() => {
    secondFrame = host.requestAnimationFrame(() => {
      prepare(host.performance.now() + STARTUP_UPDATE_DELAY_MS);
    });
  });
  return () => {
    host.cancelAnimationFrame(firstFrame);
    if (secondFrame !== undefined) host.cancelAnimationFrame(secondFrame);
  };
}
