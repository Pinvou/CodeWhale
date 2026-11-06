// Only the authenticated registration receipt's explicit, allowlisted rejection
// retires automatic polling. Unknown and older recovery states keep their path.
export function registrationManualReviewReason(progress) {
  if (progress?.status !== 'REPAIR_REQUIRED' || progress.manual_review_required !== true) return '';
  return ['USERNAME_ALREADY_REGISTERED', 'MOBILE_ALREADY_REGISTERED'].includes(progress.reason) ? progress.reason : '';
}
