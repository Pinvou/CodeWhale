// Controlled DTOs copied from the published owner schema, not the UI validator.
const contracts = {
 provisioning_pending_count:['POINT_IN_TIME',['PENDING'],null],provisioning_attention_count:['POINT_IN_TIME',['FAILED','REPAIR_REQUIRED'],null],
 usage_request_count:['WINDOW',['CREATED','RESERVED','UPSTREAM_ACTIVE','SETTLEMENT_PENDING','COMMITTED','RELEASED','REVIEW'],null],
 reconciliation_pending_count:['POINT_IN_TIME',['SETTLEMENT_PENDING','REVIEW'],null],reconciliation_stopped_count:['POINT_IN_TIME',['SETTLEMENT_PENDING','REVIEW'],null],
 published_model_count:['POINT_IN_TIME',['PUBLISHED'],null],deprecated_model_count:['POINT_IN_TIME',['DEPRECATED'],null],model_draft_count:['POINT_IN_TIME',['DRAFT'],null],
 stable_route_count:['POINT_IN_TIME',['RESOLVED'],null],healthy_route_count:['POINT_IN_TIME',['HEALTHY'],'stable_route_count'],route_attention_count:['POINT_IN_TIME',['UNKNOWN','PROBING','DEGRADED','UNHEALTHY','STALE','EXPIRED_HEALTH'],'stable_route_count'],
};
const metric = key => ({key,value:key==='usage_request_count'?1:key==='reconciliation_stopped_count'?0:2,unit:'COUNT',aggregation:contracts[key][0],included_statuses:contracts[key][1],denominator_key:contracts[key][2]});
export function overviewDTO(view='TOKEN_AND_USAGE',duration='24h',aggregate=false) {
 const as_of='2026-10-09T00:00:00.000Z';const days=duration==='7d'?7:duration==='30d'?30:1;const bucket=days===1?3600:86400;
 const from=new Date(Date.parse(as_of)-days*86400e3).toISOString();const window={from,to:as_of,timezone:'UTC'};
 const trend={bucket_seconds:bucket,points:Array.from({length:days===1?24:days},(_,index)=>({from:new Date(Date.parse(from)+index*bucket*1000).toISOString(),to:new Date(Date.parse(from)+(index+1)*bucket*1000).toISOString(),usage_request_count:index===0?1:0,reconciliation_attention_count:index===0?1:0}))};
 if(aggregate) return {as_of,window,metrics:Object.keys(contracts).map(key=>{const item=metric(key);if(item.denominator_key===null) delete item.denominator_key;return item;}),trend};
 return {schema_version:'1',as_of,permission_view:view,...(view!=='USAGE_ONLY'?{provisioning:{metrics:['provisioning_pending_count','provisioning_attention_count'].map(metric)}}:{}),...(view!=='TOKEN_ONLY'?{usage:{window,metrics:['usage_request_count','reconciliation_pending_count','reconciliation_stopped_count'].map(metric),trend}}:{})};
}
