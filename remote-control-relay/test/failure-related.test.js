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
    PINVOU_STATS_ADMIN_PASSWORD:"related-fixture-admin"};
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
    occurred_at:Date.now()+index,collection_period_id:period,producer:"pinvou_agent",
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
});
