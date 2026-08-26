const root = document.getElementById("my-role-bindings-root");
const copy = {
  zh:{title:"我的角色授权",lead:"这里只展示你本人的平台与组织角色，以及授权来源和有效期。",role:"角色",scope:"范围",validity:"有效期",source:"来源",status:"状态",none:"暂无角色授权",forever:"永久",failed:"暂时无法读取角色授权。"},
  en:{title:"My role grants",lead:"Only your own platform and organization roles, grant sources, and validity are shown here.",role:"Role",scope:"Scope",validity:"Validity",source:"Source",status:"Status",none:"No role grants",forever:"No expiry",failed:"Role grants are temporarily unavailable."},
  ja:{title:"自分のロール付与",lead:"ここには本人のプラットフォーム／組織ロール、付与元、有効期間のみ表示されます。",role:"ロール",scope:"スコープ",validity:"有効期間",source:"付与元",status:"状態",none:"ロール付与はありません",forever:"無期限",failed:"ロール付与を一時的に取得できません。"}
};
const lang=()=>{const value=localStorage.getItem("pinvou.account.language")||"zh";return copy[value]?value:"zh"};
const esc=value=>String(value??"").replace(/[&<>"']/g,char=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[char]));
const format=value=>value?new Intl.DateTimeFormat(lang()==="zh"?"zh-CN":lang()==="ja"?"ja-JP":"en-US",{dateStyle:"medium",timeStyle:"short"}).format(new Date(value)):copy[lang()].forever;
let items=[];
function render(error=false){if(!root)return;const t=copy[lang()];const cards=items.map(item=>`<article><strong>${esc(item.role_code)}</strong><span>${esc(t.scope)} · ${esc(item.scope)}${item.organization_id?` · ${esc(item.organization_id)}`:""}${item.unit_id?` · ${esc(item.unit_id)}`:""}</span><span>${esc(t.validity)} · ${esc(format(item.starts_at))} — ${esc(format(item.expires_at))}</span><span>${esc(t.source)} · ${esc(item.grant_source)}　${esc(t.status)} · ${esc(item.status)}</span></article>`).join("");root.innerHTML=`<section class="my-role-bindings"><h2>${esc(t.title)}</h2><p>${esc(error?t.failed:t.lead)}</p><div>${cards||`<span>${esc(t.none)}</span>`}</div></section>`}
async function start(){if(!root)return;let session;try{const sessionResponse=await fetch("/api/session",{credentials:"same-origin",cache:"no-store"});if(!sessionResponse.ok)return;session=await sessionResponse.json()}catch{return}if(!session.authenticated)return;try{const response=await fetch("/api/account/v1/me/role-bindings",{credentials:"same-origin",cache:"no-store",headers:{Accept:"application/json"}});if(!response.ok)throw new Error();items=(await response.json()).items||[];render()}catch{render(true)}}
window.addEventListener("storage",event=>{if(event.key==="pinvou.account.language")render()});start();
let renderedLanguage=lang();window.setInterval(()=>{const next=lang();if(next!==renderedLanguage){renderedLanguage=next;render()}},1000);
