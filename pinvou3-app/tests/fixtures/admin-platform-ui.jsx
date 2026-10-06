// Public controlled responses for the real Admin React view. This fixture does
// not contact a BFF, Keycloak, PostgreSQL, or a payment provider. Delayed clients
// intentionally ignore AbortSignal so stale-response protection is exercised.
import React, { useState } from "react";
import { createRoot } from "react-dom/client";
import { AdminPlatformView } from "../../src/features/account-platform/AdminPlatformView.jsx";
import { adminPlatformCopy } from "../../src/shared/admin-platform-i18n.js";

const scenario = new URLSearchParams(location.search).get("scenario");
const pending = new Map();
const counts = {};
let unhandled = 0;
let mutations = 0;
const orderQueries = [];
const commands = [];
let protocolAccessStatus = 0;
window.addEventListener("unhandledrejection", (event) => {
  unhandled += 1;
  event.preventDefault();
});

const ids = {
  user: "00000000-0000-4000-8000-000000000001",
  organization: "00000000-0000-4000-8000-000000000002",
  wallet: "00000000-0000-4000-8000-000000000003",
};
const user = { user_id: ids.user, display_name: "Public User", status: "ACTIVE" };
const organization = { organization_id: ids.organization, name: "Public Organization", status: "ACTIVE" };
const timestamp = "2026-01-01T00:00:00Z";
function failure(status, code) {
  return Object.assign(new Error("Controlled public failure"), { status, code });
}
function delayed(key) {
  if (pending.has(key)) throw new Error("Fixture task already pending");
  return new Promise((resolve, reject) => pending.set(key, { resolve, reject }));
}
function nextCall(key) {
  counts[key] = (counts[key] || 0) + 1;
  return counts[key];
}
function entityID(kind, suffix) {
  return `00000000-0000-4000-8000-${String(kind * 100 + suffix).padStart(12, "0")}`;
}
function orderedRows(key, make) {
  const request = nextCall(key);
  const order = request % 3 === 1 ? [1, 2, 3] : request % 3 === 2 ? [3, 1, 2] : [2, 3, 1];
  return { items: order.map((index) => make(index, String.fromCharCode(64 + index))) };
}
const client = {
  session: async () => ({ authenticated: true }),
  accountOverview: async () => {
    const request = nextCall("overview");
    return { users: scenario === "logout-busy" && request > 1 ? 7 : 1 };
  },
  billingOverview: async () => ({ posted_microcredits: "10000", reserved_microcredits: "0", payment_capabilities: { provider_id: "controlled-public", refund_supported: scenario !== "limited-payment", bill_supported: scenario !== "limited-payment", bill_file_import_supported:false, close_supported: false, notification_supported: false, manual_repair_supported: scenario !== "limited-payment" } }),
  tokenOverview: async () => ({}),
  users: async (search) => {
    const request = nextCall("users");
    if (scenario === "navigation" && request === 1) return delayed("users");
    if (scenario === "search-error" && request > 1) throw failure(503);
    if (scenario === "search-auth" && request > 1) throw failure(401);
    if (scenario === "search-race" && search) return delayed(search);
    return { items: [user] };
  },
  organizations: async () => ({ items: [organization] }),
  roles: async () => ({ items: [] }),
  priceVersions: async () => ({ items: [] }),
  reconciliationRuns: async () => ({ items: [] }),
  wallets: async () => (scenario === "navigation" ? delayed("wallets") : { items: [] }),
  paymentOrders: async (filters = {}) => {
    orderQueries.push({ ...filters });
    if (scenario === "order-search-auth" && filters.q) throw failure(401);
    const result = orderedRows("orders", (index, letter) => ({
      order_id: entityID(1, index),
      wallet_id: ids.wallet,
      requester_id: ids.user,
      merchant_order_no: `Order ${letter}`,
      channel: ["ALIPAY", "WECHAT", "UNIONPAY"][index - 1],
      amount_fen: "100",
      credit_microcredits: "10000",
      status: scenario === "payment-repair" || scenario === "limited-payment" ? "MANUAL_REVIEW" : "CREATED",
      fulfillment_status: scenario === "payment-repair" || scenario === "limited-payment" ? "MANUAL_REVIEW" : "PENDING",
      updated_at: timestamp,
    }));
    if (scenario === "order-filter-completion" && filters.q) result.items = result.items.filter(item => item.merchant_order_no === filters.q);
    return result;
  },
  syncPaymentOrder: async () => { mutations += 1; return delayed("payment-sync"); },
  repairPaymentOrder: async () => { mutations += 1; return delayed("repair"); },
  adjustments: async () =>
    orderedRows("adjustments", (index, letter) => ({
      adjustment_id: entityID(2, index),
      wallet_id: ids.wallet,
      requested_by: ids.user,
      amount_microcredits: "10000",
      reason: `Adjustment ${letter}`,
      status: "PENDING_APPROVAL",
    })),
  holds: async () =>
    orderedRows("holds", (index) => ({
      hold_id: entityID(3, index),
      wallet_id: ids.wallet,
      usage_request_id: entityID(8, index),
      max_microcredits: "10000",
      actual_microcredits: null,
      status: "RESERVED",
      expires_at: timestamp,
    })),
  tokenAccounts: async () =>
    orderedRows("tokens", (index, letter) => ({
      token_account_id: entityID(4, index),
      wallet_id: ids.wallet,
      owner_id: ids.user,
      owner_type: "USER",
      name: `Token ${letter}`,
      status: "ACTIVE",
      share_scope: "OWNER_UNIT_ONLY",
      active_credential_count: 1,
      route_count: 1,
      updated_at: timestamp,
    })),
  usageRequests: async () => ({ items: [] }),
  audits: async () =>
    orderedRows("audits", (index, letter) => ({
      audit_id: entityID(5, index),
      actor_user_id: ids.user,
      active_organization_id: ids.organization,
      action: `Audit ${letter}`,
      resource_type: "ORGANIZATION",
      resource_id: ids.organization,
      result: "SUCCESS",
      request_id: entityID(6, index),
      change_summary: {},
      occurred_at: timestamp,
    })),
  organizationUnits: async () => {
    const request = nextCall("units");
    if (scenario === "mutation-busy" && request > 1) {
      return { items: [{
        unit_id: entityID(7, 2), organization_id: ids.organization, parent_id: null,
        name: "Language read completed", status: "ACTIVE", version: 1,
      }] };
    }
    return scenario === "detail-leave" || scenario === "detail-language"
      ? delayed(`load-${request}`)
      : { items: [] };
  },
  organizationMembers: async () => ({ items: [] }),
  createOrganizationUnit: async () => {
    mutations += 1;
    return delayed("mutation");
  },
  setUserStatus: async () => {
    if (scenario === "directory-mutation") return delayed("mutation");
    throw failure(403);
  },
  logout: async () => {
    if (scenario === "logout-busy") return delayed("logout");
    throw failure(scenario === "csrf" ? 403 : 503, scenario === "csrf" ? "CSRF_REJECTED" : undefined);
  },
};

const meteringID = entityID(20, 1);
const protocolID = entityID(21, 1);
const formulaID = entityID(22, 1);
const units = [{ unit_definition_id:meteringID, unit_code:'INPUT', version:1, minimum_precision:1, price_denominator:1000000, rounding_rule:'CEILING', status:'ACTIVE' }];
const prices = [];
const protocols = [{ protocol_definition_id:protocolID, protocol_code:'CHAT', version:1, status:'PUBLISHED', request_schema:{ parameters:['estimated_input_tokens'] } }];
const formulas = [{ formula_version_id:formulaID, protocol_definition_id:protocolID, protocol_code:'CHAT', version:1, status:'PUBLISHED', required_unit_versions:{ INPUT:meteringID }, expression:{ INPUT:'estimated_input_tokens' }, evaluator_version:'SAFE_PARAMETER_MAP_V1' }];
if (scenario.startsWith('protocol-access-')) {
  protocols[0].protocol_code = 'PRIVATE_FORMULA_MARKER';
  formulas[0].protocol_code = 'PRIVATE_FORMULA_MARKER';
  prices.push({ price_version_id:entityID(24, 1), formula_version_id:formulaID, scope_type:'MODEL', scope_id:'controlled-model', protocol_code:'PRIVATE_FORMULA_MARKER', version:1, space_etag:1, lifecycle_status:'DRAFT', derived_status:'DRAFT', units:[] });
}
const lateCase = { case_id:entityID(23, 1), owner_id:null, review_status:'REVIEW_REQUIRED_LATE_PAYMENT', created_at:timestamp, next_review_at:timestamp, provider_capability_snapshot:{ order_query_supported:true }, policy_snapshot:{ grace_hours:72 } };
const reconciliationRun = { reconciliation_run_id:entityID(26, 1), channel:'ALIPAY', business_date:'2030-01-01', source_mode:'FILE_IMPORT', status:'COMPLETED', provider_row_count:1, case_count:1 };
const reconciliationCase = { case_id:entityID(27, 1), reconciliation_run_id:reconciliationRun.reconciliation_run_id, status:'OPEN', severity:'HIGH', version:1, merchant_order_no:'controlled-order', provider_snapshot:{ reference:'provider-evidence' }, internal_snapshot:{ reference:'internal-evidence' }, refund_snapshot:{ reference:'refund-evidence' }, ledger_snapshot:{ reference:'ledger-evidence' } };
Object.assign(client, {
  meteringUnits:async () => { if (scenario === 'metering-denied') throw failure(403); return { items:units }; },
  managedPrices:async () => ({ items:prices }),
  protocols:async () => { if (protocolAccessStatus) throw failure(protocolAccessStatus); return { protocols, formulas }; },
  createMeteringUnit:async input => { commands.push({ kind:'metering', input }); const value = { ...input, unit_definition_id:entityID(20, units.length + 1), version:units.length + 1, status:'ACTIVE' }; units.push(value); return value; },
  createPriceDraft:async input => { commands.push({ kind:'pricing', input }); const value = { ...input, units:input.units.map(item => ({ ...item, fixed_price_per_denominator:String(item.fixed_price_per_denominator) })), price_version_id:entityID(24, 1), lifecycle_status:'DRAFT', derived_status:'DRAFT', version:1, space_etag:input.expected_etag }; prices.push(value); return value; },
  createProtocol:async input => { commands.push({ kind:'protocol', input }); const value = { ...input, protocol_definition_id:entityID(21, protocols.length + 1), version:protocols.length + 1, status:'DRAFT' }; protocols.push(value); return value; },
  previewFormula:async id => scenario === 'protocol-access-late-preview' ? delayed('formula-preview') : ({ formula:formulas.find(value => value.formula_version_id === id), deterministic:true, external_access:false, sample_missing_optional_parameters:{} }),
  previewPrice:async id => ({ price_version_id:id, formula_snapshot:{ protocol_code:'PRIVATE_FORMULA_MARKER' } }),
  hold:async id => ({ hold:{ hold_id:id, usage_request_id:entityID(8, 1), status:'REVIEW', actual_microcredits:null }, component_allocations:[{ component_id:entityID(25, 1), reserved_microcredits:'9223372036854775808' }], member_allocations:[], lease_events:[{ event_type:'LEASE_EXPIRED' }], admin_actions:[] }),
  latePaymentReviews:async filters => { commands.push({ kind:'late-filter', input:filters }); return { items:[lateCase] }; },
  latePaymentReview:async () => ({ case:lateCase, evidence:[{ evidence_key:'public-controlled-evidence' }], events:[{ event_type:'ENTER_REVIEW' }] }),
  claimLatePaymentReview:async () => { commands.push({ kind:'late-claim' }); return { ...lateCase, owner_id:ids.user }; },
});
if (scenario.startsWith('reconciliation-')) Object.assign(client, {
  billingOverview:async () => ({ payment_capabilities:{ provider_id:'controlled-public', bill_supported:false, bill_file_import_supported:scenario === 'reconciliation-file', reconciliation_mode:scenario === 'reconciliation-degraded' ? 'QUERY_DEGRADED' : 'FILE_IMPORT' } }),
  reconciliationRuns:async () => ({ items:scenario === 'reconciliation-cases' ? [reconciliationRun] : [] }),
  reconciliationCases:async () => ({ items:[reconciliationCase] }),
  updateReconciliationCase:async (id, input) => { commands.push({ kind:'reconciliation-command', id, input }); reconciliationCase.owner_id=ids.user; reconciliationCase.version+=1; return { ...reconciliationCase }; },
  importProviderBill:async (channel, businessDate, items) => { commands.push({ kind:'bill-import', channel, businessDate, items }); return { ...reconciliationRun, channel, business_date:businessDate }; },
});

window.__adminReview = {
  ids,
  pending: () => Array.from(pending.keys()),
  unhandled: () => unhandled,
  mutations: () => mutations,
  orderQueries: () => orderQueries,
  commands: () => commands,
  protocolAccess: status => { protocolAccessStatus = status; },
  settle(key, status, name) {
    const task = pending.get(key);
    if (!task) throw new Error("Fixture task is not pending");
    pending.delete(key);
    if (status) {
      task.reject(failure(status));
      return;
    }
    if (key === 'next-client-protocols') task.resolve({ protocols:[], formulas:[] });
    else if (key.startsWith('next-client-')) task.resolve({ items:[] });
    else if (key === 'formula-preview') task.resolve({ formula:formulas[0], deterministic:true, external_access:false, sample_missing_optional_parameters:{} });
    else if (key === "wallets") task.resolve({ items: [] });
    else if (key.startsWith("load-"))
      task.resolve({
        items: [
          {
            unit_id: entityID(7, 1),
            organization_id: ids.organization,
          name: name || "Public Unit", parent_id: null,
          status: "ACTIVE", version: 1,
          },
        ],
      });
    else task.resolve({ items: [{ ...user, display_name: name || "Public User" }] });
  },
};

function App() {
  const [language, setLanguage] = useState("en");
  const [currentClient, setClient] = useState(client);
  window.__adminReview.replaceClient = () => setClient({ ...client,
    meteringUnits:() => delayed('next-client-units'), managedPrices:() => delayed('next-client-prices'), protocols:() => delayed('next-client-protocols') });
  return (
    <AdminPlatformView
      client={currentClient}
      copy={adminPlatformCopy[language]}
      language={language}
      onLanguageChange={setLanguage}
    />
  );
}
createRoot(document.getElementById("admin-root")).render(<App />);
