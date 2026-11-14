import { isProxy } from 'node:util/types';

// A closed diagnostic for fixture failures. Never include tool/host text or paths.
export function heldRequestResultDiagnostic(response) {
  try {
    if (!response || typeof response !== 'object' || Array.isArray(response)) {
      return { kind: 'unclassified' };
    }
    const error = response.error;
    if (error && typeof error === 'object' && !Array.isArray(error)) {
      const code = error.code;
      return {
        kind: 'rpc_error',
        rpcCode: Number.isInteger(code) && code >= -(2 ** 31) && code <= 2 ** 31 - 1
          ? code : 'unknown',
      };
    }
    const result = response.result;
    if (!result || typeof result !== 'object' || Array.isArray(result)) {
      return { kind: 'unclassified' };
    }
    if (result.isError !== true) return { kind: 'tool_result' };
    const structured = result.structuredContent;
    const code = structured?.errorCode;
    const retryable = structured?.retryable;
    return {
      kind: 'tool_error',
      errorCode: code === 'browser/action-commit-unknown-after-authorization-loss'
        ? code : 'unknown',
      retryable: typeof retryable === 'boolean' ? retryable : 'unknown',
    };
  } catch {
    return { kind: 'unclassified' };
  }
}

function ownData(record, key) {
  if (!record || typeof record !== 'object' || isProxy(record) || Array.isArray(record)) return undefined;
  const prototype = Object.getPrototypeOf(record);
  if (prototype !== Object.prototype && prototype !== null) return undefined;
  const descriptor = Object.getOwnPropertyDescriptor(record, key);
  return descriptor && Object.hasOwn(descriptor, 'value') ? descriptor.value : undefined;
}

// Separate from the result-class diagnostic: inspect only inert JSON data and
// classify reviewed producer messages. A reported message is not timing proof.
export function heldRequestAuthorizationDiagnostic(response) {
  try {
    if (!response || typeof response !== 'object' || isProxy(response) || Array.isArray(response)
      || Object.hasOwn(response, 'error')) return { reason: 'unknown' };
    const result = ownData(response, 'result');
    if (ownData(result, 'isError') !== true) return { reason: 'unknown' };
    const structured = ownData(result, 'structuredContent');
    if (ownData(structured, 'errorCode') !== 'browser/action-commit-unknown-after-authorization-loss') {
      return { reason: 'unknown' };
    }
    const message = ownData(structured, 'authorizationError');
    switch (message) {
      case 'browser/host-request-timeout: refresh_agent_input exceeded 400ms':
        return { reason: 'reported_host_timeout', operation: 'refresh_agent_input', timeoutMs: 400 };
      case 'browser/host-request-timeout: refresh_agent_operation exceeded 5000ms':
        return { reason: 'reported_host_timeout', operation: 'refresh_agent_operation', timeoutMs: 5000 };
      case 'The host-visible tab changed before tool execution':
        return { reason: 'reported_workspace_change' };
      case 'Browser tool call was cancelled':
        return { reason: 'reported_cancellation' };
      default:
        return { reason: 'unknown' };
    }
  } catch {
    return { reason: 'unknown' };
  }
}
