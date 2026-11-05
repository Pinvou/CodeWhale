// These states describe this invocation's actual progress, not an HTTP code.
// A composed operation must retain an earlier successful write even if its next
// read or local validation fails before another mutation can be sent.
export function financeNotDispatched(error, invalidInput = false) {
  const failure = error instanceof Error ? error : new Error('finance request unavailable');
  if (failure.financeRequestState !== 'WRITE_OBSERVED') failure.financeRequestState = 'NOT_DISPATCHED';
  if (invalidInput) failure.financeInputInvalid = true;
  return failure;
}

export function financeWriteObserved(error) {
  return Object.assign(error instanceof Error ? error : new Error('finance result unavailable'), { financeRequestState: 'WRITE_OBSERVED' });
}

export async function beforeFinanceDispatch(action) {
  try { return await action(); } catch (error) { throw financeNotDispatched(error); }
}
