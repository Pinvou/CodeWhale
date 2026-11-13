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
