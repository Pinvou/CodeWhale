import assert from "node:assert/strict";
import test from "node:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import http from "node:http";
import { createTelemetryService, TelemetryStore } from "../telemetry-service.js";
import { diagnosticFingerprint } from "../failure-diagnostics.js";

test("incident HTTP drill-down never associates legacy missing traces and retains explicit shared traces", async t => {
  const directory=mkdtempSync(join(tmpdir(),"pinvou-diag-related-"));
  const environment={PINVOU_TELEMETRY_DATA_DIR:directory,
    PINVOU_TELEMETRY_ENROLLMENT_TOKEN:"related-fixture-enrollment",
    PINVOU_TELEMETRY_DEVICE_PEPPER:"related-fixture-pepper-value",
    PINVOU_STATS_ADMIN_PASSWORD:"related-fixture-admin",
    PINVOU_BEHAVIOR_ANALYTICS_PASSWORD:"related-fixture-analyst"};
  const previous=Object.fromEntries(Object.keys(environment).map(key=>[key,process.env[key]]));
  Object.assign(process.env,environment);
  t.after(()=>{
    for(const [key,value] of Object.entries(previous)) {
      if(value===undefined)delete process.env[key];else process.env[key]=value;
    }
    assert(directory.startsWith(join(tmpdir(),"pinvou-diag-related-")));
    rmSync(directory,{recursive:true,force:true});
  });
  const seed=new TelemetryStore(directory,environment.PINVOU_TELEMETRY_DEVICE_PEPPER);
  const credential=seed.register({hardware_claim:"related-fixture-hardware",
    registration_secret:"related-fixture-registration-secret"},null);
  const device=seed.devices.get(credential.device_id);
  const period=seed.startBehaviorCollection(device,{request_id:"related_fixture_start"}).collection_period_id;
  const events=Array.from({length:4},(_,index)=>({schema_version:1,
    event_id:`beh_related_fixture_${index}`,event_name:"task_finished",event_source:"client",
    occurred_at:Date.now()-1000+index,collection_period_id:period,producer:"pinvou_agent",
    producer_version:"0.10.14",app_version:`0.10.${index+12}`,platform:"windows",arch:"x86_64",
    channel:"stable",locale:"en",session_id:"session_related_fixture",turn_id:`turn_related_${index}`,
    attempt_id:`attempt_related_${index}`,
    mode:"assistant",initiator_type:"user",input_type:"text",has_attachment:false,
    status:"failed",duration_ms:100,error_category:"model_error",stage:"model",
    diagnostic_id:`beh_related_fixture_${index}`,diagnostic_trace_id:(index<2?"a":"b").repeat(64),
    diagnostic_code:"stream_unexpected_eof",diagnostic_component:"model",diagnostic_site:"assistant_error"}));
  const receipt=seed.appendBehaviorEvents(device,events);
  assert(receipt.results.every(row=>row.status==="accepted"),JSON.stringify(receipt));
  assert.equal(seed.behaviorEvents.length,4);
  seed.failureReports.append(device, {
    report_id: "related_export_report", incident_id: events[2].diagnostic_id,
    collection_period_id: period, notice_version: "failure-diagnostics-v1", confirmed_at: Date.now(),
    notes: "export fixture evidence", attachments: [{ kind: "text", content: "redacted fixture" }],
  }, seed.behaviorEvents[2]);
  clearTimeout(seed.saveTimer);seed.saveNow();
  // New ingestion requires a trace. Exercise the defensive route through
  // stored legacy rows, while keeping the normal production loader in the path.
  writeFileSync(seed.behaviorEventsPath,seed.behaviorEvents.map((event,index)=>{
    const row={...event};if(index<2)delete row.diagnostic_trace_id;return JSON.stringify(row);
  }).join("\n")+"\n");
  const service=createTelemetryService({regionResolver:{enabled:false,lookup:async()=>null}});
  const server=http.createServer((req,res)=>{void service.handleHttp(req,res,new URL(req.url,"http://localhost").pathname);});
  t.after(async()=>{service.close();server.closeAllConnections();await new Promise(resolve=>server.close(resolve));});
  await new Promise(resolve=>server.listen(0,"127.0.0.1",resolve));
  const base=`http://127.0.0.1:${server.address().port}/pinvou3/stats/api`;
  const login=await fetch(base+"/login",{method:"POST",headers:{"content-type":"application/json"},
    body:JSON.stringify({password:environment.PINVOU_STATS_ADMIN_PASSWORD})});
  assert.equal(login.status,200);
  const headers={cookie:login.headers.get("set-cookie").split(";")[0]};
  const samplesResponse=await fetch(base+`/diagnostics/samples?fingerprint=${diagnosticFingerprint(events[0])}`,{headers});
  assert.equal(samplesResponse.status,200);
  const {incidents}=await samplesResponse.json();assert.equal(incidents.length,4);
  for(const index of [0,1,2,3]) {
    const sample=incidents.find(row=>row.version===events[index].app_version);
    const response=await fetch(base+`/diagnostics/incident?id=${sample.id}`,{headers});
    assert.equal(response.status,200);
    const detail=await response.json();
    assert.deepEqual(detail.related.map(row=>row.id),index<2?[]:
      [incidents.find(row=>row.version===events[index===2?3:2].app_version).id]);
  }
  const exportUrl=base+"/diagnostics/export";
  const analyst=await fetch(base+"/login",{method:"POST",headers:{"content-type":"application/json"},
    body:JSON.stringify({password:environment.PINVOU_BEHAVIOR_ANALYTICS_PASSWORD})});
  assert.equal(analyst.status,200);
  assert.equal((await fetch(exportUrl)).status,401);
  const analystHeaders={cookie:analyst.headers.get("set-cookie").split(";")[0]};
  assert.equal((await fetch(exportUrl,{headers:analystHeaders})).status,403);
  assert.equal((await fetch(exportUrl+"?group_by=bad",{headers})).status,400);
  assert.equal((await fetch(exportUrl+"?group_by=component&group_value=invalid",{headers})).status,400);
  assert.equal((await (await fetch(exportUrl+"?group_by=component&group_value=model",{headers})).json()).groups[0].samples.length,4);
  const summary=await (await fetch(base+"/behavior/failures",{headers})).json();
  assert.deepEqual(summary.export_groups,{code:["stream_unexpected_eof"],component:["model"],stage:["model"]});
  const defaultFile=await fetch(exportUrl+`?app_version=${events[2].app_version}`,{headers});
  assert.deepEqual((await defaultFile.json()).groups[0].samples[0].reports,[]);
  for(const [mode,key] of [["all","all"],["code","stream_unexpected_eof"],["component","model"],["stage","model"]]) {
    const groupValue=mode==="all"?"":"&group_value="+key;
    const res=await fetch(exportUrl+`?group_by=${mode}&app_version=${events[2].app_version}&include_evidence=1${groupValue}`,{headers});
    assert.equal(res.status,200);
    assert.match(res.headers.get("content-disposition"),/attachment/);
    assert.equal(res.headers.get("cache-control"),"no-store");
    const content=await res.text();
    const {groups}=JSON.parse(content);
    assert.equal(groups.length,1);
    assert.equal(groups[0].key,key);
    assert.equal(groups[0].samples.length,1);
    assert.equal(groups[0].samples[0].reports[0].notes,"export fixture evidence");
    assert.equal(groups[0].samples[0].reports[0].attachments[0].content,"redacted fixture");
    assert(!content.includes(device.device_id));
    assert(!content.includes(period));
    assert(!content.includes("session_related_fixture"));
  }
  const fingerprint=diagnosticFingerprint(events[0]);
  const updated=await fetch(base+"/diagnostics/issue",{method:"POST",headers:{...headers,"content-type":"application/json","x-pinvou-diagnostics":"1"},
    body:JSON.stringify({fingerprint,issue:{status:"fixed",fixed_version:"0.10.20"}})});
  assert.equal(updated.status,200);
  assert.equal((await (await fetch(base+"/behavior/failures",{headers})).json()).rows.length,0);
  assert.equal((await (await fetch(base+"/behavior/failures?include_fixed=1",{headers})).json()).rows[0].status,"fixed");
  assert.deepEqual((await (await fetch(base+`/diagnostics/samples?fingerprint=${fingerprint}`,{headers})).json()).incidents,[]);
  assert.deepEqual((await (await fetch(exportUrl,{headers})).json()).groups,[]);
  assert.equal((await (await fetch(exportUrl+"?include_fixed=1",{headers})).json()).groups[0].samples.length,4);
  const analystSummary=await (await fetch(base+"/behavior/failures",{headers:analystHeaders})).json();
  assert.equal(analystSummary.detail_allowed,false);
  assert.deepEqual(await (await fetch(base+"/behavior/failures?include_fixed=1",{headers:analystHeaders})).json(),analystSummary);
});
