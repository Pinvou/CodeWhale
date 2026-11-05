// A launch cancellation is itself a distributed outcome. One revoke request
// is permitted, then the immutable result reference is authoritative even if
// the revoke response is lost. This helper deliberately has no launch/create
// callback, so recovery cannot accidentally re-submit a credential handoff.
export async function settleCredentialLaunch(client, launch, reasonCode, validResult, signal, attemptRevoke = true) {
  if (!launch?.result_ref) return null;
  if (attemptRevoke) {
    try { await client.revokeCredentialWindow(launch.result_ref, reasonCode, signal); } catch { /* query below resolves the unknown response */ }
  }
  try {
    const result = await client.credentialIsolationResult(launch.result_ref, signal);
    return validResult(result, launch) ? result : null;
  } catch {
    return null;
  }
}
