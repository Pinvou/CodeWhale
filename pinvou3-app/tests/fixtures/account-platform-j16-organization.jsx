import React, { useState, useRef } from 'react';
import { createRoot } from 'react-dom/client';
import { ConversationTimeline } from '../../src/features/conversation/ConversationTimeline.jsx';
import { timelineUserError } from '../../src/features/conversation/deepseek-conversation.js';
import { AccountPlatformPopover } from '../../src/features/account-platform/AccountPlatformPopover.jsx';
import { walletRechargeTarget } from '../../src/features/conversation/billing-notice.js';
import { organizationRechargeTargetAllowed, requirePersonalAccount } from '../../src/features/account-platform/personal-contract.js';
import { dict, ensureLanguage, accountPlatformCopy, organizationCopy } from '../../src/shared/i18n.js';
import mainSource from '../../src/app/main.jsx?raw';
const org='0199a100-0001-7000-8000-000000000201', user='0199a100-0001-7000-8000-000000000202';
const control={org,user,language:'en',calls:[],authenticated:true,denied:false,other:false};
const profile=()=>({user_id:user,status:'ACTIVE',personal_permissions:[],platform_permissions:[],active_context:{context_type:'ORGANIZATION',organization_id:control.other?user:org,context_version:1,membership_version:1,organization_authorization_version:1,permissions:control.denied?[]:['org.entitlements.purchase']}});
const client={financeSessionEpoch:0,versionObservationEpoch:0,contextVersion:1,authorizationVersions:{},session:async()=>({authenticated:control.authenticated}),bootstrap:async()=>{control.bootstrapReads=(control.bootstrapReads||0)+1;return profile();},request:async(area,path,options={})=>{
 control.calls.push({area,path,method:options.method||'GET',key:options.idempotencyKey,body:options.body});
 if(path==='/v1/me/organizations')return {items:[{organization_id:org,version:1,status:'ACTIVE',name:'Controlled Org'}]};
 if(path.endsWith('/recharge/products'))return {items:[{product_id:'current-product',amount_fen:'100',channels:['ALIPAY'],availability_status:'AVAILABLE',plan_name:'Current product'}]};
 if(path.endsWith('/payment-capabilities'))return {channels:['ALIPAY'],test_only:true,formal_collection_admitted:false};
 if(path.endsWith('/recharge/orders')&&options.method==='POST')throw Object.assign(Error('controlled unknown'),{code:'COMMAND_RESULT_UNKNOWN',financeRequestState:'WRITE_OBSERVED'});
 throw Error('unexpected controlled path '+path);
}};
function Fixture(){
 const [open,setOpen]=useState(false),[request,setRequest]=useState(null),[error,setError]=useState('');
 const seq=useRef(0),[tick,setTick]=useState(0);control.update=()=>setTick(tick+1);
 const language=control.language;
 const values={walletRechargeTarget,organizationRechargeTargetAllowed,requirePersonalAccount,personalAccountAvailable:true,personalAccountClient:client,accountRechargeSequence:seq,setAccountRechargeRequest:setRequest,setPersonalAccountOpen:setOpen,setSettingsToast:setError,organizationCopy,language};
 const start=mainSource.indexOf('onOpenAccountRecharge: async userError => {'),end=mainSource.indexOf('        onGotoTools:',start);
 const callback=new Function(...Object.keys(values),'return ('+mainSource.slice(start,end).replace(/^onOpenAccountRecharge: /,'').trim().replace(/,$/,'')+')')(...Object.values(values));
 const notice={schema_version:1,code:'WALLET_INSUFFICIENT',stage:'INITIAL_RESERVATION',turn_outcome:'REJECTED',settlement_status:'NOT_STARTED',action:'OPEN_ORGANIZATION_RECHARGE',context_type:'ORGANIZATION',organization_id:org};
 const turn={id:'controlled-turn',lifecycleKnown:true,status:'failed',userError:timelineUserError({billing_notice:notice}),items:[],presentation:[],operationCount:0};
 return <><p role="status" data-host-error>{error}</p><ConversationTimeline turns={[turn]} sessionId="controlled-session" copy={dict[language].uiConversation} onUserErrorAction={callback}/><AccountPlatformPopover open={open} client={client} copy={accountPlatformCopy[language]} language={language} onLanguageChange={()=>{}} rechargeRequest={request} onClose={()=>{seq.current++;setRequest(null);setOpen(false);}}/></>;
}
await ensureLanguage('en');createRoot(document.querySelector('#root')).render(<Fixture/>);window.j16=control;
