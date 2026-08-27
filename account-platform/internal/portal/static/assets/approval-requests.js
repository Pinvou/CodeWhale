const root = document.getElementById('approval-requests-root');
const copy = {
  zh: {title:'财务审批', lead:'审批内容、关键版本和脱敏证据由服务端冻结。禁止自审；终态申请不能重开。', refresh:'刷新', all:'全部', type:'命令类型', status:'状态', risk:'风险', requester:'申请人', target:'目标', submitted:'提交 / 到期', submittedFrom:'提交开始', submittedTo:'提交结束', expiresFrom:'到期开始', expiresTo:'到期结束', impact:'预期影响', command:'冻结命令', versions:'关键版本', evidence:'脱敏证据', actions:'操作', approve:'批准', reject:'拒绝', withdraw:'撤回', reason:'审批原因', wallet:'钱包 ID', amount:'调整微积分（非零）', requestReason:'申请原因（至少10字符）', create:'创建并提交调整申请', creating:'正在提交…', created:'调整申请已提交', empty:'暂无审批申请', failed:'审批队列加载失败', policy:'策略', one:'需要一名独立审批人'},
  en: {title:'Financial approvals', lead:'Commands, critical versions, and redacted evidence are frozen by the server. Self-approval is forbidden and terminal requests cannot reopen.', refresh:'Refresh', all:'All', type:'Command type', status:'Status', risk:'Risk', requester:'Requester', target:'Target', submitted:'Submitted / expires', submittedFrom:'Submitted from', submittedTo:'Submitted to', expiresFrom:'Expires from', expiresTo:'Expires to', impact:'Expected impact', command:'Frozen command', versions:'Critical versions', evidence:'Redacted evidence', actions:'Actions', approve:'Approve', reject:'Reject', withdraw:'Withdraw', reason:'Decision reason', wallet:'Wallet ID', amount:'Adjustment microcredits (non-zero)', requestReason:'Request reason (10+ characters)', create:'Create and submit adjustment', creating:'Submitting…', created:'Adjustment submitted', empty:'No approval requests', failed:'Could not load approvals', policy:'Policy', one:'One independent approver required'},
  ja: {title:'財務承認', lead:'コマンド、重要バージョン、マスキング済み証拠はサーバーで固定されます。自己承認は禁止され、終端申請は再開できません。', refresh:'更新', all:'すべて', type:'コマンド種別', status:'状態', risk:'リスク', requester:'申請者', target:'対象', submitted:'申請 / 期限', submittedFrom:'申請開始', submittedTo:'申請終了', expiresFrom:'期限開始', expiresTo:'期限終了', impact:'想定影響', command:'固定コマンド', versions:'重要バージョン', evidence:'マスキング済み証拠', actions:'操作', approve:'承認', reject:'却下', withdraw:'取下げ', reason:'判断理由', wallet:'ウォレット ID', amount:'調整マイクロクレジット（0以外）', requestReason:'申請理由（10文字以上）', create:'調整申請を作成して提出', creating:'送信中…', created:'調整申請を提出しました', empty:'承認申請はありません', failed:'承認一覧を取得できません', policy:'ポリシー', one:'独立した承認者1名が必要'}
};
Object.assign(copy.zh,{paymentControls:'支付渠道控制',requestResume:'申请恢复支付渠道',resumeReason:'恢复原因（至少10个字符）',resumeCreated:'支付渠道恢复申请已提交',resumeUnavailable:'当前没有满足申请条件的暂停支付渠道'});
Object.assign(copy.en,{paymentControls:'Payment channel controls',requestResume:'Request payment channel recovery',resumeReason:'Recovery reason (10+ characters)',resumeCreated:'Payment channel recovery submitted',resumeUnavailable:'No paused payment channel is currently eligible for recovery'});
Object.assign(copy.ja,{paymentControls:'決済チャネル制御',requestResume:'決済チャネルの復旧を申請',resumeReason:'復旧理由（10文字以上）',resumeCreated:'決済チャネル復旧申請を送信しました',resumeUnavailable:'現在、復旧申請可能な停止中の決済チャネルはありません'});
Object.assign(copy.zh,{providerRecovery:'Provider 恢复',productRecovery:'商品恢复',productResumeCreated:'商品恢复申请已提交',productResumeUnavailable:'Provider 必须先恢复，且商品需满足准入、观察缺口与现场探测条件'});
Object.assign(copy.en,{providerRecovery:'Provider recovery',productRecovery:'Product recovery',productResumeCreated:'Product recovery submitted',productResumeUnavailable:'The Provider must be available and the product must pass admission, observation-gap, and live-probe checks'});
Object.assign(copy.ja,{providerRecovery:'Provider 復旧',productRecovery:'商品の復旧',productResumeCreated:'商品の復旧申請を送信しました',productResumeUnavailable:'Provider の復旧後、商品が審査、観測ギャップ、現地プローブの条件を満たす必要があります'});
let items = [], providerControl = null, csrf = '';
let adjustmentRequestKey = crypto.randomUUID();
const filters = {command_type:'',status:'',risk_class:'',requester_user_id:'',target:'',submitted_from:'',submitted_to:'',expires_from:'',expires_to:''};
const language = () => { const value=localStorage.getItem('pinvou.admin.language')||'zh'; return copy[value]?value:'zh'; };
const t = () => copy[language()];
const esc = value => String(value??'').replace(/[&<>"']/g, c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const date = value => value ? new Date(value).toLocaleString(language()==='zh'?'zh-CN':language()==='ja'?'ja-JP':'en-US') : '—';

async function session(){ const response=await fetch('/api/session',{credentials:'same-origin'}); if(!response.ok) throw new Error('AUTH'); const body=await response.json(); csrf=body.csrf_token; }
async function requestService(service,path, options={}) { const response=await fetch(`/api/admin/${service}${path}`,{credentials:'same-origin',cache:'no-store',...options,headers:{Accept:'application/json',...(options.body?{'Content-Type':'application/json','X-CSRF-Token':csrf}:{}),...(options.headers||{})}}); const body=response.status===204?{}:await response.json().catch(()=>({})); if(!response.ok) throw new Error(body.code||`HTTP_${response.status}`); return body; }
const request=(path,options={})=>requestService('account',path,options);

async function load(){
  try {
    if(!csrf) await session();
    const params=new URLSearchParams();
    for(const name of Object.keys(filters)){if(filters[name]) params.set(name,name.includes('_from')||name.includes('_to')?new Date(filters[name]).toISOString():filters[name]);}
    const [body,control]=await Promise.all([request(`/admin/v1/approval-requests?${params}`),requestService('billing','/admin/v1/payment-provider-controls')]); items=body.items||[]; providerControl=control; render();
  } catch(error){ root.innerHTML=`<section class="approval-panel"><p class="approval-error">${esc(t().failed)}: ${esc(error.message)}</p></section>`; }
}

async function action(item, decision){
  const reason=prompt(t().reason); if(!reason) return;
  await request(`/admin/v1/approval-requests/${encodeURIComponent(item.approval_request_id)}/actions/decide`,{method:'POST',body:JSON.stringify({expected_version:item.version,decision,reason,observed_versions:item.critical_resource_versions||{}})});
  await load();
}

async function withdraw(item){ await request(`/admin/v1/approval-requests/${encodeURIComponent(item.approval_request_id)}/actions/withdraw`,{method:'POST',body:JSON.stringify({expected_version:item.version})}); await load(); }

async function createAdjustment(event){
  event.preventDefault(); const c=t(); const form=event.currentTarget; const button=form.querySelector('button'); const wallet=form.wallet_id.value.trim(); const amount=Number(form.amount_microcredits.value); const reason=form.reason.value.trim();
  if(!wallet||!Number.isSafeInteger(amount)||amount===0||reason.length<10) return;
  button.disabled=true; button.textContent=c.creating;
  try {
    const adjustment=await requestService('billing','/admin/v1/adjustments',{method:'POST',body:JSON.stringify({wallet_id:wallet,amount_microcredits:amount,reason,idempotency_key:adjustmentRequestKey})});
    const draft=await request('/admin/v1/approval-requests',{method:'POST',body:JSON.stringify({command_type:'WALLET_ADJUSTMENT',idempotency_key:`approval:${adjustmentRequestKey}`,normalized_command:{adjustment_id:adjustment.adjustment_id,wallet_id:adjustment.wallet_id,amount_microcredits:adjustment.amount_microcredits,requested_by:adjustment.requested_by},target_resource_type:'MANUAL_ADJUSTMENT',target_resource_id:adjustment.adjustment_id,reason,redacted_evidence:[],expected_impact:{wallet_id:adjustment.wallet_id,delta_microcredits:adjustment.amount_microcredits},critical_resource_versions:{adjustment_version:adjustment.version},financial_snapshot:{wallet_id:adjustment.wallet_id,delta_microcredits:adjustment.amount_microcredits}})});
    if(draft.status==='DRAFT') await request(`/admin/v1/approval-requests/${encodeURIComponent(draft.approval_request_id)}/actions/submit`,{method:'POST',body:JSON.stringify({expected_version:draft.version})}); else if(draft.status!=='SUBMITTED') throw new Error(`APPROVAL_${draft.status}`); adjustmentRequestKey=crypto.randomUUID(); form.reset(); await load(); alert(c.created);
  } catch(error){alert(`${c.failed}: ${error.message}`);} finally {button.disabled=false;button.textContent=c.create;}
}

async function createControlResume(event){
  event.preventDefault(); const c=t(); const form=event.currentTarget; const reason=form.resume_reason.value.trim(); const productID=form.dataset.productId||''; const product=productID?(providerControl?.products||[]).find(item=>item.product_id===productID):null; const draftInput=product?product.resume_draft:providerControl?.resume_draft;
  if(!draftInput||reason.length<10){alert(productID?c.productResumeUnavailable:c.resumeUnavailable);return;}
  const button=form.querySelector('button'); button.disabled=true; button.textContent=c.creating;
  try {
    const key=`payment-control-resume-${crypto.randomUUID()}`;
    const draft=await request('/admin/v1/approval-requests',{method:'POST',body:JSON.stringify({...draftInput,idempotency_key:key,reason,redacted_evidence:[],expected_impact:{provider_id:providerControl.provider_id,product_id:productID||null,new_orders_enabled:true},financial_snapshot:{new_orders_enabled:false,provider_control_version:providerControl.provider_control_version,product_control_version:product?.version||null}})});
    if(draft.status==='DRAFT') await request(`/admin/v1/approval-requests/${encodeURIComponent(draft.approval_request_id)}/actions/submit`,{method:'POST',body:JSON.stringify({expected_version:draft.version})}); else if(draft.status!=='SUBMITTED') throw new Error(`APPROVAL_${draft.status}`);
    form.reset(); await load(); alert(productID?c.productResumeCreated:c.resumeCreated);
  } catch(error){alert(`${c.failed}: ${error.message}`);} finally {button.disabled=false;button.textContent=productID?c.productRecovery:c.requestResume;}
}

function render(){
  const c=t(); const types=[...new Set(items.map(x=>x.command_type))].sort(); const statuses=['DRAFT','SUBMITTED','APPROVED','EXECUTING','EXECUTED','EXECUTION_FAILED','REJECTED','WITHDRAWN','EXPIRED','INVALIDATED'];
  const rows=items.map(item=>{const actions=item.allowed_actions||[];return `<tr><td><strong>${esc(item.command_type)}</strong><small>${esc(c.policy)} v${esc(item.policy_version)} · ${esc(item.risk_class)} · ${esc(c.one)}</small></td><td><span>${esc(item.status)}</span><small>v${esc(item.version)}</small></td><td><code>${esc(item.requester_user_id)}</code></td><td>${esc(item.target_resource_type)}<small>${esc(item.target_resource_id)}</small></td><td>${esc(date(item.submitted_at||item.created_at))}<small>${esc(date(item.expires_at))}</small></td><td><details><summary>${esc(c.impact)}</summary><pre>${esc(JSON.stringify(item.expected_impact||{},null,2))}</pre></details><details><summary>${esc(c.command)}</summary><pre>${esc(JSON.stringify(item.normalized_command||{},null,2))}</pre></details><details><summary>${esc(c.versions)}</summary><pre>${esc(JSON.stringify(item.critical_resource_versions||{},null,2))}</pre></details><details><summary>${esc(c.evidence)}</summary><pre>${esc(JSON.stringify(item.redacted_evidence||[],null,2))}</pre></details></td><td class="approval-actions">${actions.includes('APPROVE')?`<button data-id="${item.approval_request_id}" data-decision="APPROVED">${esc(c.approve)}</button>`:''}${actions.includes('REJECT')?`<button data-id="${item.approval_request_id}" data-decision="REJECTED">${esc(c.reject)}</button>`:''}${actions.includes('WITHDRAW')?`<button data-id="${item.approval_request_id}" data-withdraw>${esc(c.withdraw)}</button>`:''}</td></tr>`}).join('');
  const select=(name,label,values)=>`<label>${esc(label)}<select name="${name}"><option value="">${esc(c.all)}</option>${values.map(value=>`<option value="${esc(value)}"${filters[name]===value?' selected':''}>${esc(value)}</option>`).join('')}</select></label>`;
  const input=(name,label,type='text')=>`<label>${esc(label)}<input name="${name}" type="${type}" value="${esc(filters[name])}"></label>`;
  const productResumeForms=(providerControl?.products||[]).filter(product=>!product.new_orders_enabled).map(product=>`<form class="approval-control-resume" data-product-id="${esc(product.product_id)}"><strong>${esc(c.productRecovery)} · ${esc(product.product_id)}</strong><label>${esc(c.resumeReason)}<input name="resume_reason" minlength="10" maxlength="2000" required></label><button${product.resume_draft?'':' disabled'}>${esc(c.productRecovery)}</button></form>`).join('');
  root.innerHTML=`<section class="approval-panel"><header><div><h2>${esc(c.title)}</h2><p>${esc(c.lead)}</p></div><button data-refresh>${esc(c.refresh)}</button></header><details class="approval-control-snapshot"><summary>${esc(c.paymentControls)}</summary><pre>${esc(JSON.stringify(providerControl||{},null,2))}</pre></details><form class="approval-control-resume"><strong>${esc(c.providerRecovery)}</strong><label>${esc(c.resumeReason)}<input name="resume_reason" minlength="10" maxlength="2000" required></label><button${providerControl?.resume_draft?'':' disabled'}>${esc(c.requestResume)}</button></form>${productResumeForms}<form class="approval-create"><label>${esc(c.wallet)}<input name="wallet_id" required></label><label>${esc(c.amount)}<input name="amount_microcredits" type="number" step="1" required></label><label>${esc(c.requestReason)}<input name="reason" minlength="10" maxlength="2000" required></label><button>${esc(c.create)}</button></form><div class="approval-filters">${select('command_type',c.type,types)}${select('status',c.status,statuses)}${select('risk_class',c.risk,['FINANCIAL_FACT_CHANGE'])}${input('requester_user_id',c.requester)}${input('target',c.target)}${input('submitted_from',c.submittedFrom,'datetime-local')}${input('submitted_to',c.submittedTo,'datetime-local')}${input('expires_from',c.expiresFrom,'datetime-local')}${input('expires_to',c.expiresTo,'datetime-local')}</div>${rows?`<div class="approval-table"><table><thead><tr>${[c.type,c.status,c.requester,c.target,c.submitted,c.impact,c.actions].map(x=>`<th>${esc(x)}</th>`).join('')}</tr></thead><tbody>${rows}</tbody></table></div>`:`<p>${esc(c.empty)}</p>`}</section>`;
  root.querySelector('[data-refresh]').onclick=load; root.querySelectorAll('.approval-filters input,.approval-filters select').forEach(node=>node.onchange=()=>{filters[node.name]=node.value;load();});
  root.querySelectorAll('[data-decision]').forEach(button=>button.onclick=()=>action(items.find(x=>x.approval_request_id===button.dataset.id),button.dataset.decision));
  root.querySelectorAll('[data-withdraw]').forEach(button=>button.onclick=()=>withdraw(items.find(x=>x.approval_request_id===button.dataset.id)));
  root.querySelector('.approval-create').onsubmit=createAdjustment;
  root.querySelectorAll('.approval-control-resume').forEach(form=>form.onsubmit=createControlResume);
}

if(root){render();load();window.addEventListener('pinvou-admin-language-change',render);}
