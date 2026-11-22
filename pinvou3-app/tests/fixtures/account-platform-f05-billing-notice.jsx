import React from 'react';
import { createRoot } from 'react-dom/client';
import { ConversationTimeline } from '../../src/features/conversation/ConversationTimeline.jsx';
import { timelineUserError } from '../../src/features/conversation/deepseek-conversation.js';
import { dict, ensureLanguage } from '../../src/shared/i18n.js';
const root=createRoot(document.querySelector('#root'));
const control={language:'en',completed:true,action:'OPEN_RECHARGE',contextType:'PERSONAL',code:'WALLET_INSUFFICIENT',calls:[]};
control.render=async()=>{await ensureLanguage(control.language); const notice={schema_version:1,code:control.code,stage:'ADDITIONAL_RESERVATION',turn_outcome:control.completed?'COMPLETED':'REJECTED',settlement_status:control.completed?'SETTLED':'NOT_STARTED',action:control.action,context_type:control.contextType,organization_id:control.contextType==='ORGANIZATION'?'0199a100-0001-7000-8000-000000000201':undefined};const turn={id:'public-turn',lifecycleKnown:true,status:control.completed?'completed':'failed',userError:timelineUserError({billing_notice:notice}),items:[],presentation:[],operationCount:0};root.render(<ConversationTimeline turns={[turn]} sessionId="public-session" copy={dict[control.language].uiConversation} onUserErrorAction={error=>control.calls.push({contextType:error.contextType,action:error.action,completed:error.turnOutcome})} />);};
control.copy=()=>dict[control.language].uiConversation;await control.render();window.f05=control;
