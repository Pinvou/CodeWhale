import React, { useEffect, useState } from 'react';
import { tokenManagementCopy, financeCopy } from '../../shared/i18n.js';
import { AdminModelTokenWorkspace } from './AdminModelTokenWorkspace.jsx';
import { AdminModelOperationsWorkspace } from './AdminModelOperationsWorkspace.jsx';
import { PricingLifecycleWorkspace } from './PricingLifecycleWorkspace.jsx';
import { Approvals } from './AdminFinanceWorkspace.jsx';
import './admin-token-management.css';
const defaultOverviewFlags = Object.freeze({});
const tokenApprovalTypes = Object.freeze(['TOKEN_MODEL_LIFECYCLE_CHANGE', 'TOKEN_MODEL_CATALOG_ROLLBACK', 'TOKEN_CREDENTIAL_REVOKE']);
export function AdminManagementWorkspace({ client, copy, language, permissions, owner, credentialOrigin, authorizationVersion, overviewFlags = defaultOverviewFlags }) {
 const [section,setSection]=useState(() => new URL(globalThis.location.href).searchParams.has('usage_request_id') || new URL(globalThis.location.href).searchParams.has('operations_tab') ? 'modelOperations' : new URL(globalThis.location.href).searchParams.has('approval_request_id') ? 'approvals' : new URL(globalThis.location.href).searchParams.has('token_account_id') ? 'tokenResources' : 'models'); const [routeVersion,setRouteVersion]=useState(0);
 const management=tokenManagementCopy[language]; const words={...copy,modelToken:management.modelToken,billing:{...copy.billing,pricingLifecycle:management.pricingLifecycle}};
 const granted=new Set(permissions);const tabs=[['models',granted.has('platform.models.read')||granted.has('platform.pricing.read'),words.modelToken.modelDirectory],['tokenResources',granted.has('platform.tokens.read'),words.modelToken.tokenAccounts],['modelOperations',granted.has('platform.usage.read')||granted.has('platform.tokens.read'),words.modelToken.usageRequests],['pricing',granted.has('platform.pricing.read'),words.billing.pricingLifecycle.priceVersions],['approvals',tokenApprovalTypes.some(type=>granted.has({TOKEN_MODEL_LIFECYCLE_CHANGE:'platform.models.lifecycle.approve',TOKEN_MODEL_CATALOG_ROLLBACK:'platform.models.catalog.rollback.approve',TOKEN_CREDENTIAL_REVOKE:'platform.tokens.credentials.revoke.approve'}[type]))||granted.has('platform.models.lifecycle.request')||granted.has('platform.models.catalog.rollback.request')||granted.has('platform.tokens.credentials.revoke.request'),financeCopy[language].approvals]].filter(([,shown])=>shown);
 const active=tabs.some(([key])=>key===section)?section:tabs[0]?.[0];
 useEffect(()=>{const changed=()=>{if(new URL(globalThis.location.href).searchParams.has('usage_request_id') || new URL(globalThis.location.href).searchParams.has('operations_tab'))setSection('modelOperations');setRouteVersion(v=>v+1);};globalThis.addEventListener('popstate',changed);return()=>globalThis.removeEventListener('popstate',changed);},[]);
 const route=(updates,mode='push')=>{const url=new URL(globalThis.location.href);for(const [key,value]of Object.entries(updates)){if(value===null||value===undefined||value==='')url.searchParams.delete(key);else url.searchParams.set(key,String(value));}globalThis.history[mode==='replace'?'replaceState':'pushState']({},'',url);setRouteVersion(v=>v+1);};
 const navigate=(target,updates={})=>{if(tabs.some(([key])=>key===target)){route({...updates,...(target !== 'modelOperations' ? {operations_tab:null} : {})});setSection(target);}};
 return <section className="adm-panel"><nav>{tabs.map(([key,,label])=><button key={key} aria-pressed={active===key} onClick={()=>setSection(key)}>{label}</button>)}</nav>
 {['models','tokenResources'].includes(active)&&<AdminModelTokenWorkspace section={active} client={client} copy={words} permissions={permissions} routeVersion={routeVersion} onRouteChange={route} onNavigate={navigate} tokenResourceFormsEnabled modelDetailsEnabled modelPricingEditEnabled tokenDetailsEnabled modelTokenActionsEnabled credentialOrigin={credentialOrigin} owner={owner} />}
 {active==='modelOperations'&&<AdminModelOperationsWorkspace key={`${owner}:${client.financeSessionEpoch || 0}`} client={client} copy={words.modelToken} permissions={permissions} routeVersion={routeVersion} onRouteChange={route} onNavigate={navigate} owner={owner} authorizationVersion={authorizationVersion} pageOverviewEnabled={overviewFlags.page === true} operationsOverviewEnabled={overviewFlags.aggregate === true} />}
 {active==='pricing'&&<PricingLifecycleWorkspace key={`${owner}:${client.financeSessionEpoch || 0}`} owner={owner} client={client} copy={words} permissions={permissions} />}
 {active==='approvals'&&<Approvals client={client} copy={financeCopy[language]} permissions={permissions} owner={owner} commandTypes={tokenApprovalTypes} initialRequestID={new URL(globalThis.location.href).searchParams.get('approval_request_id') || ''} />}
 </section>;
}
