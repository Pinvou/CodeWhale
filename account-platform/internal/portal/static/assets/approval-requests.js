const root = document.getElementById('approval-requests-root');
const copy = {
  zh: {title:'财务审批', lead:'审批内容、关键版本和脱敏证据由服务端冻结。禁止自审；终态申请不能重开。', refresh:'刷新', all:'全部', type:'命令类型', status:'状态', risk:'风险', requester:'申请人', target:'目标', submitted:'提交 / 到期', submittedFrom:'提交开始', submittedTo:'提交结束', expiresFrom:'到期开始', expiresTo:'到期结束', impact:'预期影响', command:'冻结命令', versions:'关键版本', evidence:'脱敏证据', actions:'操作', approve:'批准', reject:'拒绝', withdraw:'撤回', reason:'审批原因', wallet:'钱包 ID', amount:'调整微积分（非零）', requestReason:'申请原因（至少10字符）', create:'创建并提交调整申请', creating:'正在提交…', created:'调整申请已提交', empty:'暂无审批申请', failed:'审批队列加载失败', policy:'策略', one:'需要一名独立审批人'},
  en: {title:'Financial approvals', lead:'Commands, critical versions, and redacted evidence are frozen by the server. Self-approval is forbidden and terminal requests cannot reopen.', refresh:'Refresh', all:'All', type:'Command type', status:'Status', risk:'Risk', requester:'Requester', target:'Target', submitted:'Submitted / expires', submittedFrom:'Submitted from', submittedTo:'Submitted to', expiresFrom:'Expires from', expiresTo:'Expires to', impact:'Expected impact', command:'Frozen command', versions:'Critical versions', evidence:'Redacted evidence', actions:'Actions', approve:'Approve', reject:'Reject', withdraw:'Withdraw', reason:'Decision reason', wallet:'Wallet ID', amount:'Adjustment microcredits (non-zero)', requestReason:'Request reason (10+ characters)', create:'Create and submit adjustment', creating:'Submitting…', created:'Adjustment submitted', empty:'No approval requests', failed:'Could not load approvals', policy:'Policy', one:'One independent approver required'},
  ja: {title:'財務承認', lead:'コマンド、重要バージョン、マスキング済み証拠はサーバーで固定されます。自己承認は禁止され、終端申請は再開できません。', refresh:'更新', all:'すべて', type:'コマンド種別', status:'状態', risk:'リスク', requester:'申請者', target:'対象', submitted:'申請 / 期限', submittedFrom:'申請開始', submittedTo:'申請終了', expiresFrom:'期限開始', expiresTo:'期限終了', impact:'想定影響', command:'固定コマンド', versions:'重要バージョン', evidence:'マスキング済み証拠', actions:'操作', approve:'承認', reject:'却下', withdraw:'取下げ', reason:'判断理由', wallet:'ウォレット ID', amount:'調整マイクロクレジット（0以外）', requestReason:'申請理由（10文字以上）', create:'調整申請を作成して提出', creating:'送信中…', created:'調整申請を提出しました', empty:'承認申請はありません', failed:'承認一覧を取得できません', policy:'ポリシー', one:'独立した承認者1名が必要'}
};
const decisionCopy={zh:{target:'目标申请',decision:'审批决定',version:'预期版本',help:'至少填写 3 个字符。提交失败时保留输入，不会自动重放审批。',confirmation:'确认后申请将进入终态；批准还会触发独立服务身份按冻结命令执行。',confirm:'确认提交',cancel:'取消',submitting:'正在提交…',refreshStatus:'刷新申请状态',required:'审批原因至少需要 3 个字符。',failed:'审批决定提交失败，请刷新状态后重试。',resultUnknown:'无法确认审批决定是否已生效；为避免重复决策，请仅刷新申请状态。'},en:{target:'Target request',decision:'Decision',version:'Expected version',help:'Enter at least 3 characters. Inputs remain after failure and the decision is never replayed automatically.',confirmation:'The request becomes terminal after confirmation. Approval also authorizes the independent service identity to execute the frozen command.',confirm:'Confirm and submit',cancel:'Cancel',submitting:'Submitting…',refreshStatus:'Refresh request state',required:'The decision reason must contain at least 3 characters.',failed:'The approval decision failed. Refresh the state and retry.',resultUnknown:'The decision result is unknown. Refresh the request state instead of submitting the decision again.'},ja:{target:'対象申請',decision:'承認判断',version:'想定バージョン',help:'3 文字以上入力してください。失敗時も入力を保持し、判断を自動再実行しません。',confirmation:'確認後、申請は終端状態になります。承認時は独立サービス ID が固定コマンドを実行します。',confirm:'確認して送信',cancel:'キャンセル',submitting:'送信中…',refreshStatus:'申請状態を更新',required:'判断理由は 3 文字以上必要です。',failed:'承認判断の送信に失敗しました。状態を更新して再試行してください。',resultUnknown:'判断結果を確認できません。重複を避けるため、再送信せず申請状態のみ更新してください。'}};
let items = [], csrf = '', pendingDecision = null, uncertainDecision = null;
let loadGeneration = 0;
const filters = {command_type:'',status:'',risk_class:'',requester_user_id:'',target:'',submitted_from:'',submitted_to:'',expires_from:'',expires_to:''};
const approvalStatuses=new Set(['DRAFT','SUBMITTED','APPROVED','EXECUTING','EXECUTED','EXECUTION_FAILED','REJECTED','WITHDRAWN','EXPIRED','INVALIDATED']);
function syncRouteFilters(){
  const query=new URLSearchParams(location.search);
  for(const name of Object.keys(filters)) filters[name]='';
  filters.command_type=query.get('approval_command_type')||'';
  filters.target=query.get('approval_target')||'';
}
syncRouteFilters();
const language = () => { const value=localStorage.getItem('pinvou.admin.language')||'zh'; return copy[value]?value:'zh'; };
const t = () => copy[language()];
const esc = value => String(value??'').replace(/[&<>"']/g, c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const date = value => value ? new Date(value).toLocaleString(language()==='zh'?'zh-CN':language()==='ja'?'ja-JP':'en-US') : '—';

async function session(){ const response=await fetch('/api/session',{credentials:'same-origin'}); if(!response.ok) throw new Error('AUTH'); const body=await response.json(); csrf=body.csrf_token; }
async function requestService(service,path, options={}) { const response=await fetch(`/api/admin/${service}${path}`,{credentials:'same-origin',cache:'no-store',...options,headers:{Accept:'application/json',...(options.body?{'Content-Type':'application/json','X-CSRF-Token':csrf}:{}),...(options.headers||{})}}); const body=response.status===204?{}:await response.json().catch(()=>({})); if(!response.ok){const error=new Error(body.code||`HTTP_${response.status}`);error.code=body.code||'';error.status=response.status;error.requestID=body.request_id||'';throw error;} return body; }
const request=(path,options={})=>requestService('account',path,options);

async function load(){
  const generation=++loadGeneration;
  const filterSnapshot={...filters};
  try {
    if(!csrf) await session();
    const params=new URLSearchParams();
    for(const name of Object.keys(filterSnapshot)){if(filterSnapshot[name]) params.set(name,name.includes('_from')||name.includes('_to')?new Date(filterSnapshot[name]).toISOString():filterSnapshot[name]);}
    const body=await request(`/admin/v1/approval-requests?${params}`);
    if(generation!==loadGeneration) return;
    items=body.items||[]; if(!uncertainDecision){pendingDecision=null;render();} return true;
  } catch(error){ if(generation===loadGeneration){if(uncertainDecision){const node=root.querySelector('#approval-decision-error');if(node)node.textContent=`${(decisionCopy[language()]||decisionCopy.zh).resultUnknown} ${t().failed}: ${error.message}`;}else root.innerHTML=`<section class="approval-panel"><p class="approval-error" role="alert">${esc(t().failed)}: ${esc(error.message)}</p></section>`;} return false; }
}

async function refreshUncertainDecision(){
  if(!uncertainDecision)return false;
  const frozen=uncertainDecision,requestID=frozen.item.approval_request_id,dc=decisionCopy[language()]||decisionCopy.zh;
  try{
    const latest=await request(`/admin/v1/approval-requests/${encodeURIComponent(requestID)}`);
    if(uncertainDecision!==frozen)return false;
    const frozenVersion=Number(frozen.item.version);
    if(latest?.approval_request_id!==requestID||!Number.isInteger(latest?.version)||!Number.isInteger(frozenVersion)||latest.version<1||frozenVersion<1||latest.version<frozenVersion||!approvalStatuses.has(latest?.status)||!Array.isArray(latest?.allowed_actions))throw new Error('INVALID_APPROVAL_DETAIL');
    items=[latest,...items.filter(item=>item.approval_request_id!==requestID)];
    uncertainDecision=null;pendingDecision=null;render();return true;
  }catch(error){
    const node=root.querySelector('#approval-decision-error');
    if(node)node.textContent=`${dc.resultUnknown} ${t().failed}: ${error.message}`;
    return false;
  }
}

function openDecision(item, decision){
  if(uncertainDecision)return;
  pendingDecision={item,decision};
  const dialog=root.querySelector('#approval-decision-dialog'),dc=decisionCopy[language()]||decisionCopy.zh;
  root.querySelector('#approval-decision-target').textContent=`${item.command_type} · ${item.target_resource_type} · ${item.target_resource_id}`;
  root.querySelector('#approval-decision-name').textContent=decision==='APPROVED'?t().approve:t().reject;
  root.querySelector('#approval-decision-version').textContent=`v${item.version}`;
  root.querySelector('#approval-decision-error').textContent='';
  root.querySelector('#approval-decision-form').reset();
  dialog.showModal();
  root.querySelector('#approval-decision-form textarea').focus();
}

async function submitDecision(event){
  event.preventDefault();
  if(!pendingDecision)return;
  const dc=decisionCopy[language()]||decisionCopy.zh,form=event.currentTarget,submit=form.querySelector('[type="submit"]'),reason=String(new FormData(form).get('reason')||'').trim();
  if(pendingDecision.outcomeUnknown){submit.disabled=true;submit.textContent=dc.submitting;try{await refreshUncertainDecision();}finally{if(submit.isConnected){submit.disabled=false;submit.textContent=pendingDecision?.outcomeUnknown?dc.refreshStatus:dc.confirm;}}return;}
  if([...reason].length<3){root.querySelector('#approval-decision-error').textContent=dc.required;return;}
  submit.disabled=true;submit.textContent=dc.submitting;root.querySelector('#approval-decision-error').textContent='';
  try{
    const {item,decision}=pendingDecision;
    await request(`/admin/v1/approval-requests/${encodeURIComponent(item.approval_request_id)}/actions/decide`,{method:'POST',body:JSON.stringify({expected_version:item.version,decision,reason,observed_versions:item.critical_resource_versions||{}})});
    pendingDecision=null;root.querySelector('#approval-decision-dialog').close();await load();
  }catch(error){const unknown=!Number.isInteger(error?.status)||error.status>=500;if(unknown){pendingDecision={...pendingDecision,outcomeUnknown:true};uncertainDecision={...pendingDecision};root.querySelectorAll('[data-decision-cancel]').forEach(button=>{button.disabled=true;});}root.querySelector('#approval-decision-error').textContent=unknown?dc.resultUnknown:dc.failed;}
  finally{if(submit.isConnected){submit.disabled=false;submit.textContent=pendingDecision?.outcomeUnknown?dc.refreshStatus:dc.confirm;}}
}

async function withdraw(item){ await request(`/admin/v1/approval-requests/${encodeURIComponent(item.approval_request_id)}/actions/withdraw`,{method:'POST',body:JSON.stringify({expected_version:item.version})}); await load(); }

function render(){
  const c=t(); const types=[...new Set([filters.command_type,...items.map(x=>x.command_type)].filter(Boolean))].sort(); const statuses=[...approvalStatuses];
  const rows=items.map(item=>{const actions=uncertainDecision?[]:item.allowed_actions||[];return `<tr><td><strong>${esc(item.command_type)}</strong><small>${esc(c.policy)} v${esc(item.policy_version)} · ${esc(item.risk_class)} · ${esc(c.one)}</small></td><td><span>${esc(item.status)}</span><small>v${esc(item.version)}</small></td><td><code>${esc(item.requester_user_id)}</code></td><td>${esc(item.target_resource_type)}<small>${esc(item.target_resource_id)}</small></td><td>${esc(date(item.submitted_at||item.created_at))}<small>${esc(date(item.expires_at))}</small></td><td><details><summary>${esc(c.impact)}</summary><pre>${esc(JSON.stringify(item.expected_impact||{},null,2))}</pre></details><details><summary>${esc(c.command)}</summary><pre>${esc(JSON.stringify(item.normalized_command||{},null,2))}</pre></details><details><summary>${esc(c.versions)}</summary><pre>${esc(JSON.stringify(item.critical_resource_versions||{},null,2))}</pre></details><details><summary>${esc(c.evidence)}</summary><pre>${esc(JSON.stringify(item.redacted_evidence||[],null,2))}</pre></details></td><td class="approval-actions">${actions.includes('APPROVE')?`<button data-id="${item.approval_request_id}" data-decision="APPROVED">${esc(c.approve)}</button>`:''}${actions.includes('REJECT')?`<button data-id="${item.approval_request_id}" data-decision="REJECTED">${esc(c.reject)}</button>`:''}${actions.includes('WITHDRAW')?`<button data-id="${item.approval_request_id}" data-withdraw>${esc(c.withdraw)}</button>`:''}</td></tr>`}).join('');
  const select=(name,label,values)=>`<label>${esc(label)}<select name="${name}"><option value="">${esc(c.all)}</option>${values.map(value=>`<option value="${esc(value)}"${filters[name]===value?' selected':''}>${esc(value)}</option>`).join('')}</select></label>`;
  const input=(name,label,type='text')=>`<label>${esc(label)}<input name="${name}" type="${type}" value="${esc(filters[name])}"></label>`;
  root.innerHTML=`<section class="approval-panel"><header><div><h2>${esc(c.title)}</h2><p>${esc(c.lead)}</p></div><button data-refresh>${esc(c.refresh)}</button></header><div class="approval-filters">${select('command_type',c.type,types)}${select('status',c.status,statuses)}${select('risk_class',c.risk,['FINANCIAL_FACT_CHANGE'])}${input('requester_user_id',c.requester)}${input('target',c.target)}${input('submitted_from',c.submittedFrom,'datetime-local')}${input('submitted_to',c.submittedTo,'datetime-local')}${input('expires_from',c.expiresFrom,'datetime-local')}${input('expires_to',c.expiresTo,'datetime-local')}</div>${rows?`<div class="approval-table"><table><thead><tr>${[c.type,c.status,c.requester,c.target,c.submitted,c.impact,c.actions].map(x=>`<th>${esc(x)}</th>`).join('')}</tr></thead><tbody>${rows}</tbody></table></div>`:`<p>${esc(c.empty)}</p>`}</section>`;
  const dc=decisionCopy[language()]||decisionCopy.zh;
  root.innerHTML+=`<dialog id="approval-decision-dialog" class="approval-decision-dialog"><form id="approval-decision-form"><header><h3>${esc(c.reason)}</h3><button type="button" data-decision-cancel>${esc(dc.cancel)}</button></header><dl><div><dt>${esc(dc.target)}</dt><dd id="approval-decision-target"></dd></div><div><dt>${esc(dc.decision)}</dt><dd id="approval-decision-name"></dd></div><div><dt>${esc(dc.version)}</dt><dd id="approval-decision-version"></dd></div></dl><label>${esc(c.reason)}<textarea name="reason" minlength="3" maxlength="2000" rows="4" required aria-describedby="approval-decision-help approval-decision-error"></textarea><small id="approval-decision-help">${esc(dc.help)}</small></label><p id="approval-decision-error" class="approval-error" role="alert" aria-live="assertive"></p><div class="approval-decision-confirm"><strong>${esc(dc.confirm)}</strong><span>${esc(dc.confirmation)}</span></div><footer><button type="button" data-decision-cancel>${esc(dc.cancel)}</button><button type="submit">${esc(dc.confirm)}</button></footer></form></dialog>`;
  root.querySelector('[data-refresh]').onclick=load; root.querySelectorAll('.approval-filters input,.approval-filters select').forEach(node=>node.onchange=()=>{filters[node.name]=node.value;load();});
  root.querySelectorAll('[data-decision]').forEach(button=>button.onclick=()=>openDecision(items.find(x=>x.approval_request_id===button.dataset.id),button.dataset.decision));
  const decisionForm=root.querySelector('#approval-decision-form');if(decisionForm)decisionForm.onsubmit=submitDecision;
  const closeDecision=()=>{const submit=root.querySelector('#approval-decision-form [type="submit"]');if(submit?.disabled||pendingDecision?.outcomeUnknown||uncertainDecision)return;pendingDecision=null;root.querySelector('#approval-decision-dialog')?.close?.();};
  root.querySelectorAll('[data-decision-cancel]').forEach(button=>button.onclick=closeDecision);
  root.querySelector('#approval-decision-dialog')?.addEventListener?.('cancel',event=>{event.preventDefault();closeDecision();});
  root.querySelectorAll('[data-withdraw]').forEach(button=>button.onclick=()=>withdraw(items.find(x=>x.approval_request_id===button.dataset.id)));
}

if(root){
  const routeChanged=()=>{if(new URLSearchParams(location.search).get('section')==='approvals'){syncRouteFilters();load();}};
  render();load();window.addEventListener('pinvou-admin-language-change',()=>{if(!uncertainDecision)render();});window.addEventListener('pinvou-admin-route-change',routeChanged);window.addEventListener('popstate',routeChanged);
}
