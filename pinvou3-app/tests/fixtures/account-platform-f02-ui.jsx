import React from 'react';
import { createRoot } from 'react-dom/client';
import { PersonalFinanceWorkspace } from '../../src/features/account-platform/PersonalFinanceWorkspace.jsx';
import { AdminFinanceWorkspace } from '../../src/features/account-platform/AdminFinanceWorkspace.jsx';
import { RechargeProductWorkspace } from '../../src/features/account-platform/RechargeProductWorkspace.jsx';
import { AccountPlatformBffClient } from '../../src/features/account-platform/bff-client.js';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { financeCopy } from '../../src/shared/i18n.js';
import { parseFinanceJSON } from '../../src/features/account-platform/finance-contract.js';
const root = createRoot(document.querySelector('#root'));
const id = '11111111-1111-4111-8111-111111111111';
const other = '22222222-2222-4222-8222-222222222222';
const json = (value, status = 200) => new Response(status === 204 ? null : JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
const control = { id, other, user: id, calls: [], kind: 'personal', language: 'en', unknownCreate: false, unknownRefund: false, wrongRefund: false, plan: false, delayedCreate: false, approvalStatus: 'DRAFT', approvalVersion: 1, approvalCommand: 'WALLET_ADJUSTMENT', approval202: false, approvalCreated: false, publishOnly: false };
Object.assign(control, { financeRecovery: false, expiredCursor: false, replacementRequester: id, replacementWallet: id, unknownAdjustment: false });
const personal = new AccountPlatformBffClient();
const admin = new AdminPlatformClient(); admin.csrfToken = 'CONTROL_SYNTHETIC_CSRF';
const order = () => ({ order_id: id, amount_fen: '9007199254740993', status: 'PAID', fulfillment_status: 'CREDITED', attention_summary: 'NORMAL' });
const pkg = () => ({ package_id: id, source_payment_transaction_id: id, source_type: 'PAYMENT', status: 'ACTIVE', points_remaining_microcredits: '1000000', points_used_microcredits: '0', components: [{ component_type: control.plan ? 'PLAN' : 'BASE' }] });
const late = () => ({ case_id: id, payment_order_id: id, owner_id: id, review_status: 'REVIEW_REQUIRED_LATE_PAYMENT', severity: 'P2', next_review_at: '2099-01-01T00:00:00Z' });
const product = { catalog_item_id: id, product_id: 'personal-base', display_name_en: 'Controlled product', display_name_zh_cn: '受控商品', display_name_ja: 'テスト商品', display_order: 1, base_points: '100', bonus_points: '0', validity_days: 30, invoice_product_name: 'Controlled invoice', lifecycle_status: 'DRAFT', version: 1, mapping: { draft_version: 1, provider_id: 'controlled', provider_version: 'v1', sku_bom: 'sku-base', amount_fen: '100', currency: 'CNY', channels: ['ALIPAY'], admission_class: 'TEST_ONLY' } };
const recoveryControl = () => ({ provider_id: 'controlled', provider_control_version: 3, resume_draft: { command_type: 'PAYMENT_PROVIDER_RESUME', target_resource_type: 'PAYMENT_PROVIDER', target_resource_id: 'controlled', normalized_command: { provider_id: 'controlled', provider_control_version: 3 }, critical_resource_versions: { provider_control_version: 3 } }, products: [{ product_id: 'personal-base', version: 4, resume_draft: { command_type: 'PAYMENT_PRODUCT_RESUME', target_resource_type: 'PAYMENT_PRODUCT', target_resource_id: 'personal-base', normalized_command: { provider_id: 'controlled', product_id: 'personal-base', provider_control_version: 3, product_control_version: 4 }, critical_resource_versions: { provider_control_version: 3, product_control_version: 4 } } }] });
const approval = () => ({ approval_request_id: id, command_type: control.approvalCommand, status: control.approvalStatus, version: control.approvalVersion, target: control.approvalCommand === 'WALLET_ADJUSTMENT' ? { resource_type: 'MANUAL_ADJUSTMENT', resource_id: id } : control.approvalCommand === 'PAYMENT_PROVIDER_RESUME' ? { resource_type: 'PAYMENT_PROVIDER', resource_id: 'controlled' } : { resource_type: 'PAYMENT_PRODUCT', resource_id: 'personal-base' }, detail_availability: 'AVAILABLE', requester: { user_id: id }, allowed_actions: control.approvalStatus === 'DRAFT' ? ['EDIT', 'SUBMIT'] : control.approvalStatus === 'SUBMITTED' ? ['WITHDRAW', 'APPROVE', 'REJECT'] : [], action_control_version: 1, command: { adjustment_id: id, wallet_id: id, amount_microcredits: '9007199254740993' }, execution: { attempt_status: 'PENDING', error_class: null, next_retry_at: null, execution_deadline: '2099-01-01T00:00:00Z' } });
globalThis.fetch = async (url, options = {}) => {
  const method = options.method || 'GET'; control.calls.push({ url, method, body: options.body, key: options.headers?.['Idempotency-Key'] });
  if (control.financeRecovery) {
    if (url.includes('/payment-orders?')) { const cursor = new URL(url, location.origin).searchParams.get('cursor'); return control.expiredCursor && cursor ? json({ code: 'PAGINATION_CURSOR_EXPIRED' }, 410) : json({ items: [order()], next_cursor: cursor ? '' : 'controlled-next', snapshot_as_of: '2099-01-01T00:00:00Z' }); }
    if (url.includes('/late-payment-reviews?')) { const cursor = new URL(url, location.origin).searchParams.get('cursor'); return control.expiredCursor && cursor ? json({ code: 'PAGINATION_CURSOR_EXPIRED' }, 410) : json({ items: [late()], next_cursor: cursor ? '' : 'controlled-review-next', snapshot_as_of: '2099-01-01T00:00:00Z' }); }
    if (url.includes('/wallets?')) return json({ items: [{ wallet_id: id, posted_microcredits: '1000000', available_microcredits: '1000000', status: 'ACTIVE', financial_status: 'NORMAL' }] });
    if (url.includes('/adjustments?')) return control.changedAdjustment ? json({ items: [] }) : json({ items: [{ adjustment_id: id, wallet_id: id, status: 'PENDING_APPROVAL', amount_microcredits: '9007199254740993', requested_by: id, reason: 'Controlled original adjustment reason', request_idempotency_key: 'controlled-adjustment-key', version: 1 }] });
    if (url.endsWith('/adjustments') && method === 'POST') {
      const input = parseFinanceJSON(options.body); control.newAdjustment = { ...input, adjustment_id: other, requested_by: id, status: 'PENDING_APPROVAL', request_idempotency_key: input.idempotency_key, version: 1 };
      return control.unknownAdjustment ? json({ code: 'UPSTREAM_UNAVAILABLE' }, 503) : json(control.newAdjustment, 201);
    }
    if (url.includes('/api/admin/approvals/requests?')) { const target = new URL(url, location.origin).searchParams.get('target'); return json({ items: target === other || !control.approvalCreated ? [] : [approval()], next_cursor: '', has_more: control.incompleteApprovalHistory === true }); }
    if (url.endsWith(`/api/admin/approvals/requests/${id}`) && method === 'GET' && !control.newApproval) return json({ ...approval(), requester: { user_id: control.replacementRequester }, command: { adjustment_id: id, wallet_id: control.replacementWallet, amount_microcredits: '9007199254740993' } });
    if (url.endsWith('/api/admin/approvals/requests') && method === 'POST') { control.newApproval = parseFinanceJSON(options.body); control.approvalCreated = true; control.approvalStatus = 'DRAFT'; control.approvalVersion = 1; return json({ approval_request_id: other }, 201); }
    if (url.endsWith(`/api/admin/approvals/requests/${other}`)) return json({ ...approval(), approval_request_id: other, target: { resource_type: 'MANUAL_ADJUSTMENT', resource_id: control.newApproval.target_resource_id }, command: control.newApproval.normalized_command });
    if (url.includes(`/api/admin/approvals/requests/${other}/actions/`)) { control.approvalStatus = 'SUBMITTED'; control.approvalVersion++; return json(null, 204); }
  }
  if (url.includes('/recharge/products')) return json({ items: [{ product_id: 'personal-base', display_name_en: 'Controlled product', display_name_zh_cn: '受控商品', display_name_ja: 'テスト商品', amount_fen: '100', channels: ['ALIPAY'] }] });
  if (url.includes('/payment-capabilities')) return json({});
  if (url.endsWith('/v1/me/entitlement-packages')) return json({ items: [pkg()] });
  if (url.endsWith('/v1/recharge/orders') && method === 'POST') {
    const respond = () => control.unknownCreate ? json({ code: 'UPSTREAM_UNAVAILABLE' }, 503) : json({ ...order(), status: 'PENDING', close_status: 'OPEN', expires_at: '2099-01-01T00:00:00Z', payment_url: 'javascript:alert(1)', qr_data: 'data:image/svg+xml;base64,PHN2Zz4=' }, 201);
    return control.delayedCreate ? new Promise(resolve => { control.releaseCreate = () => resolve(respond()); }) : respond();
  }
  if (url.endsWith('/v1/recharge/orders')) return json({ items: [] });
  if (url.includes('/payment-orders?')) return json({ items: [order()] });
  if (url.endsWith('/admin/v1/overview')) return json({ payment_capabilities: { normal_refund_creation_enabled: true } });
  if (url.endsWith(`/payment-orders/${id}`)) return json({ ...order(), transactions: [{ payment_transaction_id: id, payment_order_id: id, status: 'PAID', fulfillment_status: 'CREDITED', amount_fen: '9007199254740993' }], entitlement_packages: [pkg()] });
  if (url.endsWith(`/payment-orders/${id}/refunds`)) { const facts = JSON.parse(options.body); return control.unknownRefund ? json({ code: 'UPSTREAM_UNAVAILABLE' }, 503) : json({ refund_id: id, payment_order_id: id, ...facts, amount_fen: control.wrongRefund ? '1' : '9007199254740993' }, 201); }
  if (url.includes('/late-payment-reviews?')) return json({ items: [late()] });
  if (url.endsWith(`/late-payment-reviews/${id}`)) return json({ case: late(), events: [] });
  if (url.endsWith(`/late-payment-reviews/${id}/query`)) return json(late());
  if (url.includes('/recharge-products?')) return json({ items: [product] });
  if (url.endsWith('/recharge-product-create-options')) return json({ adapters: [{ provider_id: 'controlled', provider_version: 'v1', channels: ['ALIPAY'] }], invoice_product_names: ['Controlled invoice'] });
  if (url.endsWith('/admission-verifications')) return json({ items: [] });
  if (url.endsWith('/display-names')) return json({ ...product, ...JSON.parse(options.body), version: 2 });
  if (url.endsWith('/api/admin/approvals/capability')) return json({ session_approval_wire_version: 2, control: { version: 1, epoch: 1, authorization_relay_epoch: 1 } });
  if (url.endsWith('/payment-provider-controls')) return json(recoveryControl());
  if (url.includes('/api/admin/approvals/requests?')) return json({ items: url.includes('command_type=') && !control.approvalCreated ? [] : [approval()] });
  if (url.endsWith('/api/admin/approvals/requests') && method === 'POST') { control.approvalCreated = true; control.approvalCommand = JSON.parse(options.body).command_type; return json({ approval_request_id: id }, 201); }
  if (url.endsWith(`/api/admin/approvals/requests/${id}`)) {
    if (method === 'PUT') { control.approvalVersion++; return json(null, 204); }
    return json(approval());
  }
  if (url.includes(`/api/admin/approvals/requests/${id}/actions/`)) {
    if (control.approval202) return json({ approval_request_id: id }, 202);
    const action = url.split('/').pop(); control.approvalStatus = action === 'submit' ? 'SUBMITTED' : action === 'withdraw' ? 'WITHDRAWN' : JSON.parse(options.body).decision === 'APPROVE' ? 'APPROVED' : 'REJECTED'; control.approvalVersion++; return json(null, 204);
  }
  if (url.includes('/adjustments?')) return json({ items: [{ adjustment_id: id, wallet_id: id, status: 'PENDING_APPROVAL', amount_microcredits: '9007199254740993', requested_by: id, request_idempotency_key: 'controlled-adjustment-key', version: 1 }] });
  if (url.includes('/wallets?') || url.includes('/requests?')) return json({ items: [] });
  throw new Error('UNEXPECTED_CONTROLLED_ROUTE');
};
function render() {
  const me = { user_id: control.user, platform_authorization_version: 1, personal_permissions: ['personal.billing.read', 'personal.recharge.create'], platform_permissions: ['platform.billing.wallets.read', 'platform.billing.orders.sync', 'platform.billing.repairs.request', 'platform.billing.adjustments.approve', 'platform.billing.payment-controls.resume.approve', 'platform.billing.payment-controls.resume.request', 'platform.products.read', 'platform.products.create', 'platform.products.publish'], active_context: { scope: 'PERSONAL', context_version: 1, personal_authorization_version: 1, personal_permissions: ['personal.billing.read', 'personal.recharge.create'] } };
  if (control.publishOnly) me.platform_permissions = ['platform.products.read', 'platform.products.publish'];
  const client = control.kind === 'personal' ? personal : admin;
  root.render(<><select aria-label="fixture-language" value={control.language} onChange={event => { control.language = event.target.value; render(); }}>{['en', 'zh', 'ja'].map(value => <option key={value}>{value}</option>)}</select>{control.kind === 'personal' ? <PersonalFinanceWorkspace key={control.user} {...{ client, me }} language={control.language} /> : control.kind === 'products' ? <RechargeProductWorkspace {...{ client, me }} copy={financeCopy[control.language]} language={control.language} /> : <AdminFinanceWorkspace {...{ client, me }} language={control.language} />}</>);
}
control.render = kind => { control.kind = kind; render(); };
control.languageCopy = () => financeCopy[control.language];
control.switchIdentity = () => { control.user = other; render(); };
window.f02 = control; render();
