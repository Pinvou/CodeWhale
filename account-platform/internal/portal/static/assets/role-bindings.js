const root = document.getElementById("role-bindings-root");

const messages = {
  zh: {
    title: "角色授权生命周期",
    lead: "查看平台与组织角色的授权来源、生效时间和到期时间；普通授权可定时生效、自动到期或单条撤销。超级管理员和所有者使用专用安全流程。",
    user: "用户 ID", role: "角色", organization: "组织 ID（平台角色留空）", unit: "节点 ID（仅 UNIT_ADMIN）",
    starts: "开始时间（留空为立即）", expires: "到期时间（可选）", grant: "授予角色", refresh: "刷新",
    binding: "绑定 ID", scope: "范围", validity: "有效期", source: "授权来源", status: "状态", actions: "操作",
    revoke: "撤销", none: "暂无角色绑定", loading: "正在载入角色绑定…", failed: "角色绑定操作失败，请检查权限或输入。",
    scheduled: "待生效", active: "有效", expired: "已到期", revoked: "已撤销", forever: "永久"
  },
  en: {
    title: "Role grant lifecycle",
    lead: "Review platform and organization role sources and validity. Ordinary grants can start later, expire automatically, or be revoked individually. Super-admin and owner roles use dedicated security flows.",
    user: "User ID", role: "Role", organization: "Organization ID (blank for platform)", unit: "Unit ID (UNIT_ADMIN only)",
    starts: "Starts at (blank means now)", expires: "Expires at (optional)", grant: "Grant role", refresh: "Refresh",
    binding: "Binding ID", scope: "Scope", validity: "Validity", source: "Grant source", status: "Status", actions: "Actions",
    revoke: "Revoke", none: "No role bindings", loading: "Loading role bindings…", failed: "Role binding operation failed. Check permissions and input.",
    scheduled: "Scheduled", active: "Active", expired: "Expired", revoked: "Revoked", forever: "No expiry"
  },
  ja: {
    title: "ロール付与ライフサイクル",
    lead: "プラットフォーム／組織ロールの付与元と有効期間を確認します。通常ロールは開始予約、自動失効、個別取消が可能です。スーパー管理者と所有者は専用の安全フローを使用します。",
    user: "ユーザー ID", role: "ロール", organization: "組織 ID（プラットフォームロールは空欄）", unit: "ユニット ID（UNIT_ADMIN のみ）",
    starts: "開始日時（空欄は即時）", expires: "有効期限（任意）", grant: "ロールを付与", refresh: "更新",
    binding: "バインド ID", scope: "スコープ", validity: "有効期間", source: "付与元", status: "状態", actions: "操作",
    revoke: "取り消す", none: "ロール付与はありません", loading: "ロール付与を読み込み中…", failed: "ロール付与の操作に失敗しました。権限と入力を確認してください。",
    scheduled: "開始待ち", active: "有効", expired: "期限切れ", revoked: "取消済み", forever: "無期限"
  }
};

const roles = [
  "PLATFORM_IDENTITY_ADMIN", "PLATFORM_ORGANIZATION_ADMIN", "PLATFORM_BILLING_OPERATOR", "PLATFORM_BILLING_APPROVER",
  "PLATFORM_PRICING_ADMIN", "PLATFORM_MODEL_ADMIN", "PLATFORM_OPERATIONS_ADMIN", "PLATFORM_AUDITOR",
  "ORG_ADMIN", "BILLING_ADMIN", "AUDITOR", "UNIT_ADMIN"
];

let csrfToken = "";
let items = [];

function language() {
  const value = localStorage.getItem("pinvou.admin.language") || "zh";
  return messages[value] ? value : "zh";
}

function text() { return messages[language()]; }
function escapeHTML(value) { return String(value ?? "").replace(/[&<>"']/g, char => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[char])); }
function date(value) { return value ? new Intl.DateTimeFormat(language() === "zh" ? "zh-CN" : language() === "ja" ? "ja-JP" : "en-US", {dateStyle:"medium", timeStyle:"short"}).format(new Date(value)) : text().forever; }
function status(value) { return text()[String(value || "").toLowerCase()] || value; }

async function request(path, options = {}) {
  const response = await fetch(`/api/admin/account${path}`, {
    credentials: "same-origin", cache: "no-store",
    ...options,
    headers: {Accept: "application/json", ...(options.body ? {"Content-Type":"application/json", "X-CSRF-Token":csrfToken} : {}), ...(options.headers || {})}
  });
  const body = response.status === 204 ? {} : await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.code || `HTTP_${response.status}`);
  return body;
}

async function load() {
  const t = text();
  renderMessage(t.loading);
  try {
    const result = await request("/admin/v1/role-bindings");
    items = result.items || [];
    render();
  } catch (error) {
    renderMessage(t.failed, true);
  }
}

function renderMessage(message, failed = false) {
  if (!root) return;
  root.innerHTML = `<section class="role-binding-panel"><p class="${failed ? "role-binding-error" : ""}">${escapeHTML(message)}</p></section>`;
}

function render() {
  const t = text();
  const rows = items.map(item => `<tr>
    <td><code>${escapeHTML(item.user_id)}</code><small>${escapeHTML(item.binding_id)}</small></td>
    <td><strong>${escapeHTML(item.role_code)}</strong><small>${escapeHTML(item.scope)}${item.organization_id ? ` · ${escapeHTML(item.organization_id)}` : ""}${item.unit_id ? ` · ${escapeHTML(item.unit_id)}` : ""}</small></td>
    <td>${escapeHTML(date(item.starts_at))}<small>${escapeHTML(item.expires_at ? date(item.expires_at) : t.forever)}</small></td>
    <td>${escapeHTML(item.grant_source)}</td>
    <td><span class="role-binding-status role-binding-status-${escapeHTML(String(item.status).toLowerCase())}">${escapeHTML(status(item.status))}</span></td>
    <td>${["ACTIVE","SCHEDULED"].includes(item.status) && !["OWNER","MEMBER","PLATFORM_SUPER_ADMIN"].includes(item.role_code) ? `<button type="button" data-revoke="${escapeHTML(item.binding_id)}">${escapeHTML(t.revoke)}</button>` : "—"}</td>
  </tr>`).join("");
  root.innerHTML = `<section class="role-binding-panel">
    <header><div><h2>${escapeHTML(t.title)}</h2><p>${escapeHTML(t.lead)}</p></div><button type="button" data-refresh>${escapeHTML(t.refresh)}</button></header>
    <form id="role-binding-form">
      <label>${escapeHTML(t.user)}<input name="user_id" required pattern="[0-9a-fA-F-]{36}"></label>
      <label>${escapeHTML(t.role)}<select name="role_code">${roles.map(role => `<option>${role}</option>`).join("")}</select></label>
      <label>${escapeHTML(t.organization)}<input name="organization_id" pattern="[0-9a-fA-F-]{36}"></label>
      <label>${escapeHTML(t.unit)}<input name="unit_id" pattern="[0-9a-fA-F-]{36}"></label>
      <label>${escapeHTML(t.starts)}<input name="starts_at" type="datetime-local"></label>
      <label>${escapeHTML(t.expires)}<input name="expires_at" type="datetime-local"></label>
      <button type="submit">${escapeHTML(t.grant)}</button>
    </form>
    <div class="role-binding-table"><table><thead><tr><th>${escapeHTML(t.user)} / ${escapeHTML(t.binding)}</th><th>${escapeHTML(t.role)} / ${escapeHTML(t.scope)}</th><th>${escapeHTML(t.validity)}</th><th>${escapeHTML(t.source)}</th><th>${escapeHTML(t.status)}</th><th>${escapeHTML(t.actions)}</th></tr></thead><tbody>${rows || `<tr><td colspan="6">${escapeHTML(t.none)}</td></tr>`}</tbody></table></div>
    <p class="role-binding-error" id="role-binding-error" hidden>${escapeHTML(t.failed)}</p>
  </section>`;
  root.querySelector("[data-refresh]").addEventListener("click", load);
  root.querySelector("#role-binding-form").addEventListener("submit", grant);
  root.querySelectorAll("[data-revoke]").forEach(button => button.addEventListener("click", () => revoke(button.dataset.revoke)));
}

async function grant(event) {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const starts = form.get("starts_at");
  const expires = form.get("expires_at");
  const body = {
    user_id: form.get("user_id"), role_code: form.get("role_code"),
    organization_id: form.get("organization_id") || null, unit_id: form.get("unit_id") || null,
    starts_at: starts ? new Date(starts).toISOString() : null, expires_at: expires ? new Date(expires).toISOString() : null,
    idempotency_key: `admin-ui-${crypto.randomUUID()}`
  };
  try { await request("/admin/v1/role-bindings", {method:"POST", body:JSON.stringify(body)}); await load(); }
  catch (error) { const target = root.querySelector("#role-binding-error"); target.hidden = false; }
}

async function revoke(id) {
  try { await request(`/admin/v1/role-bindings/${encodeURIComponent(id)}`, {method:"DELETE"}); await load(); }
  catch (error) { const target = root.querySelector("#role-binding-error"); target.hidden = false; }
}

async function start() {
  if (!root) return;
  try {
    const response = await fetch("/api/session", {credentials:"same-origin", cache:"no-store"});
    if (!response.ok) return;
    const session = await response.json();
    if (!session.authenticated) return;
    csrfToken = session.csrf_token || "";
    await load();
  } catch (error) { renderMessage(text().failed, true); }
}

window.addEventListener("storage", event => { if (event.key === "pinvou.admin.language") render(); });
let renderedLanguage = language();
window.setInterval(() => { const next = language(); if (next !== renderedLanguage) { renderedLanguage = next; render(); } }, 1000);
start();
