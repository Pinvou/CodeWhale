// Only an explicit, allowlisted reason on the authenticated receipt stops
// automatic polling. Older receipts still follow the ordinary recovery path.
export function registrationManualReviewReason(progress) {
  if (progress?.status !== 'REPAIR_REQUIRED' || progress.manual_review_required !== true) return '';
  return ['USERNAME_ALREADY_REGISTERED', 'MOBILE_ALREADY_REGISTERED', 'IDENTITY_CREATE_OUTCOME_UNKNOWN'].includes(progress.reason) ? progress.reason : '';
}
