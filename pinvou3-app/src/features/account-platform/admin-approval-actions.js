export function approvalActionsForDetail(detail, currentUserID) {
  if (!detail || !currentUserID || detail.detail_availability === 'DETAIL_PRESENTATION_UNAVAILABLE' ||
    !Array.isArray(detail.allowed_actions)) return [];
  const requester = detail.requester?.user_id || detail.requester_user_id;
  if (!requester) return [];
  return [...new Set(detail.allowed_actions)].filter((name) => {
    if (name === 'APPROVE' || name === 'REJECT') return requester !== currentUserID && detail.status === 'SUBMITTED';
    if (name === 'SUBMIT') return requester === currentUserID && detail.status === 'DRAFT' && detail.version === 1;
    if (name === 'WITHDRAW') return requester === currentUserID && detail.status === 'SUBMITTED';
    return false;
  });
}
