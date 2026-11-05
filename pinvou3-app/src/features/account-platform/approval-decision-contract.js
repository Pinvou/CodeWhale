import { financeNotDispatched } from './finance-operation-state.js';

// UI actions and already canonical callers share the existing server enum.
export function canonicalApprovalDecision(action) {
  switch (action) {
    case 'APPROVE':
    case 'APPROVED':
      return 'APPROVED';
    case 'REJECT':
    case 'REJECTED':
      return 'REJECTED';
    default:
      throw financeNotDispatched(Object.assign(new Error('invalid approval decision'), {
        code: 'INVALID_APPROVAL_DECISION',
      }), true);
  }
}
