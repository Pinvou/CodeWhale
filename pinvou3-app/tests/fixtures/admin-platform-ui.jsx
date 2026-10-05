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
  billingOverview: async () => ({ posted_microcredits: "10000", reserved_microcredits: "0" }),
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
  wallets: async () => (scenario === "navigation" ? delayed("wallets") : { items: [] }),
  paymentOrders: async () =>
    orderedRows("orders", (index, letter) => ({
      order_id: entityID(1, index),
      wallet_id: ids.wallet,
      requester_id: ids.user,
      merchant_order_no: `Order ${letter}`,
      channel: "ALIPAY",
      amount_fen: "100",
      credit_microcredits: "10000",
      status: "CREATED",
      fulfillment_status: "PENDING",
      updated_at: timestamp,
    })),
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

window.__adminReview = {
  pending: () => Array.from(pending.keys()),
  unhandled: () => unhandled,
  mutations: () => mutations,
  settle(key, status, name) {
    const task = pending.get(key);
    if (!task) throw new Error("Fixture task is not pending");
    pending.delete(key);
    if (status) {
      task.reject(failure(status));
      return;
    }
    if (key === "wallets") task.resolve({ items: [] });
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
  return (
    <AdminPlatformView
      client={client}
      copy={adminPlatformCopy[language]}
      language={language}
      onLanguageChange={setLanguage}
    />
  );
}
createRoot(document.getElementById("admin-root")).render(<App />);
