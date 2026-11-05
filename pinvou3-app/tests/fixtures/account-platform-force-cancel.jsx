import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminModelDetailWorkspace } from '../../src/features/account-platform/AdminModelDetailWorkspace.jsx';
import { tokenManagementCopy } from '../../src/shared/i18n.js';

const modelID = '11111111-1111-4111-8111-111111111111';
const replacementID = '22222222-2222-4222-8222-222222222222';
const planID = '33333333-3333-4333-8333-333333333333';
const approvalID = '44444444-4444-4444-8444-444444444444';
const hash = 'a'.repeat(64);
const now = new Date().toISOString();
const collection = items => ({ items, as_of: now, has_more: false, next_cursor: null });
const language = new URLSearchParams(location.search).get('language') || 'en';
const copy = tokenManagementCopy[language].modelToken;
const control = { calls: [], plan: null, draft: null, submitted: false, copy };
function impact(target, planned) {
  const plan = planned ? control.plan : null;
  const dependencies = Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map(key => [key, 0]));
  dependencies.inflight_requests = 1;
  return { action: `MODEL_LIFECYCLE_${target}`, precheck_fingerprint: hash, requires_cas_revalidation: true, snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS', blockers: [], token_observed_at: now, billing_observed_at: now, evidence: { resource_type: 'MODEL', resource_id: modelID, current_status: 'PUBLISHED', current_version: { evidence_etag: `"impact-${hash}"`, evidence_set_sha256: hash }, dependencies }, critical_resource_versions: { model_id: modelID, catalog_version: 1, current_lifecycle: 'PUBLISHED', target_lifecycle: target, lifecycle_plan_version: plan ? 1 : 0, effective_at: plan?.effective_at || now, existing_session_cutoff_at: plan?.existing_session_cutoff_at || null, replacement_model_id: plan?.replacement_model_id || null, billing_price_version: 5, route_set_fingerprint: hash, provisioning_set_fingerprint: hash, inflight_request_set_fingerprint: hash, policy_version: plan ? 3 : 2, ...(plan ? { force_cancel_inflight: plan.force_cancel_inflight } : {}) }, financial_snapshot: { billing_price_revision: 5, price_set_fingerprint: hash, models_checked: 2, models_covered: 2, fully_covered: true, billing_as_of: now } };
}
const client = {
  catalogModel: async () => ({ model: { model_id: modelID, display_name: 'Controlled model', lifecycle_status: 'PUBLISHED', catalog_version: 1 }, as_of: now }),
  modelLifecyclePlans: async () => collection(control.plan ? [control.plan] : []),
  catalogModels: async () => collection([{ model_id: replacementID, display_name: 'Controlled replacement', lifecycle_status: 'PUBLISHED' }]),
  modelLifecycleImpact: async (_id, target, _replacement, _signal, plan) => impact(target, Boolean(plan)),
  createModelLifecyclePlan: async (_id, input, key) => {
    control.calls.push({ kind: 'plan', input: structuredClone(input), key });
    control.plan = { ...input, model_id: modelID, lifecycle_plan_id: planID, lifecycle_plan_version: 1, status: 'PENDING_APPROVAL' };
    return control.plan;
  },
  governedApprovalRequests: async () => collection([]),
  createGovernedApprovalDraft: async draft => { control.draft = structuredClone(draft); return approvalID; },
  governedApprovalRequest: async () => ({ approval_request_id: approvalID, command_type: 'TOKEN_MODEL_LIFECYCLE_CHANGE', policy_version: 3, approval_mode: 'APPROVAL_REQUIRED', version: control.submitted ? 2 : 1, action_control_version: 1, detail_availability: 'AVAILABLE', allowed_actions: control.submitted ? [] : ['SUBMIT'], status: control.submitted ? 'SUBMITTED' : 'DRAFT', target: { resource_type: 'MODEL', resource_id: modelID }, command: control.draft?.normalized_command }),
  submitGovernedApprovalRequest: async () => { control.submitted = true; },
};
const root = createRoot(document.querySelector('#root'));
control.render = () => root.render(<AdminModelDetailWorkspace owner={modelID} modelID={modelID} client={client} copy={copy} permissions={['platform.models.read', 'platform.models.lifecycle.request']} actionsEnabled routeVersion={1} onRouteChange={() => {}} onQueryChange={() => {}} cursorHistory={{ current: {} }} />);
globalThis.forceCancelFixture = control;
control.render();
