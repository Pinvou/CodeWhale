import assert from "node:assert/strict";
import test from "node:test";
import { mkdtempSync, rmSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import http from "node:http";
import { FailureReportStore, failureGroups, redactDiagnosticText, DIAGNOSTIC_CODES, DIAGNOSTIC_COMPONENTS, DIAGNOSTIC_SITES, DIAGNOSTIC_STAGES } from "../failure-diagnostics.js";
import { BEHAVIOR_TELEMETRY_SCHEMA, validateBehaviorEventShape } from "../behavior-telemetry-schema.js";
import { createTelemetryService, TelemetryStore } from "../telemetry-service.js";

function witness(extra = {}) {
  return { schema_version: 1, event_id: "beh_diagnostic_fixture_1", event_name: "task_finished", event_source: "client",
    occurred_at: Date.now(), collection_period_id: "fixture_collection_period", producer: "pinvou_agent", producer_version: "0.10.12",
    app_version: "0.10.12", platform: "windows", arch: "x86_64", channel: "stable", locale: "en",
    session_id: "session_fixture", attempt_id: "attempt_fixture", turn_id: "turn_fixture", mode: "assistant", initiator_type: "user", input_type: "text", has_attachment: false,
    status: "failed", duration_ms: 1200, error_category: "model_error", stage: "model",
    diagnostic_id: "beh_diagnostic_fixture_1", diagnostic_trace_id: "a".repeat(64), diagnostic_code: "stream_unexpected_eof",
    diagnostic_component: "model", diagnostic_site: "assistant_error", diagnostic_steps: [{ stage: "submission", offset_ms: 0 }, { stage: "model", offset_ms: 100 }], ...extra };
}
const report = (extra = {}) => ({ report_id: "report_diagnostic_fixture", incident_id: "beh_diagnostic_fixture_1", collection_period_id: "fixture_collection_period",
  notice_version: "failure-diagnostics-v1", confirmed_at: Date.now(), notes: "empty stream reproduction", attachments: [], ...extra });

test("diagnostic registries match the wire schema and reject raw/unknown nested content", () => {
  for (const [field, values] of [["diagnostic_code", DIAGNOSTIC_CODES], ["diagnostic_component", DIAGNOSTIC_COMPONENTS], ["diagnostic_site", DIAGNOSTIC_SITES], ["stage", DIAGNOSTIC_STAGES]]) {
    assert.deepEqual(BEHAVIOR_TELEMETRY_SCHEMA.properties[field].enum, values);
  }
  assert.equal(validateBehaviorEventShape(witness()).ok, true);
  for (const extra of [
    { diagnostic_code: "private-provider-error-message" }, { diagnostic_steps: [{ stage: "model", offset_ms: 0, prompt: "private" }] },
    { diagnostic_steps: [{ stage: "model", offset_ms: -1 }] }, { diagnostic_steps: Array(25).fill({stage:"model",offset_ms:0}) },
    { diagnostic_steps: { stage:"model",offset_ms:0 } }, { diagnostic_steps: [{stage:"http://private.invalid",offset_ms:0}] },
    { status:"success",error_category:undefined }, { diagnostic_id: undefined }, { diagnostic_http_status: 600 },
  ]) assert.equal(validateBehaviorEventShape(witness(extra)).ok, false, JSON.stringify(extra));
});

test("grouping merges a fault across versions while preserving filters, privacy threshold and counts", () => {
  const events = [witness({device_id:"A"}), witness({device_id:"B",app_version:"0.10.13"})];
  const rows=failureGroups([...events, ...events]);
  assert.equal(rows.length,1); assert.equal(rows[0].count,2); assert.equal(rows[0].installations,2);
  assert.deepEqual(rows[0].versions,{"0.10.12":1,"0.10.13":1});
  assert.equal(failureGroups(events,{},20).length,0);
  assert.equal(failureGroups(events,{app_version:"0.10.13"})[0].count,1);
  assert.equal(failureGroups(events,{diagnostic_code:"unknown"}).length,0);
  assert.equal(failureGroups(events,{now:Date.now()+36*86400000}).length,0);
});

test("privacy thresholds apply to version subgroups and account subjects",()=>{
  const events=Array.from({length:20},(_,i)=>witness({event_id:`event_privacy_${i}`,diagnostic_id:`event_privacy_${i}`,device_id:`device_${i}`,app_version:i===0?"one-off-version":"0.10.12"}));
  assert.deepEqual(failureGroups(events,{},20)[0].versions,{});
  assert.equal(failureGroups(events.map(e=>({...e,account_subject_id:"same-account"})),{},20).length,0);
  assert.equal(failureGroups(events,{},1)[0].versions["one-off-version"],1);
});

test("deletion remains processing across storage failure and restart until reports are durably removed",()=>{
  const directory=mkdtempSync(join(tmpdir(),"pinvou-diag-delete-recovery-"));
  const originalPrune=FailureReportStore.prototype.prune;
  let store;
  try{
    const pepper="diagnostic-fixture-device-pepper";
    store=new TelemetryStore(directory,pepper);
    const credential=store.register({hardware_claim:"diagnostic-delete-hardware",registration_secret:"diagnostic-delete-registration-secret",hardware_source:"test",identity_quality:"hardware_serial",app_version:"0.10.12",platform:"windows",arch:"x64"},null);
    const device=store.devices.get(credential.device_id);
    const period=store.startBehaviorCollection(device,{request_id:"diagnostic_delete_start"}).collection_period_id;
    const event=witness({collection_period_id:period});store.appendBehaviorEvents(device,[event]);
    store.failureReports.append(device,report({collection_period_id:period}),{...event,device_id:device.device_id});
    const beforeDeletion=readFileSync(join(directory,"failure-diagnostic-reports.json"));
    FailureReportStore.prototype.prune=function(){throw new Error("simulated_disk_failure");};
    const deletion=store.deleteBehaviorData(device,{request_id:"diagnostic_delete_request"}); assert.equal(deletion.status,"processing");
    const restored=new TelemetryStore(directory,pepper);
    assert.equal(restored.deletionStatus(deletion.deletion_request_id,credential.device_token).status,"processing");
    assert.equal(JSON.parse(readFileSync(join(directory,"failure-diagnostic-reports.json"),"utf8")).reports.length,1);
    FailureReportStore.prototype.prune=originalPrune;
    const recovered=new TelemetryStore(directory,pepper);
    assert.equal(recovered.deletionStatus(deletion.deletion_request_id,credential.device_token).status,"completed");
    assert.equal(JSON.parse(readFileSync(join(directory,"failure-diagnostic-reports.json"),"utf8")).reports.length,0);
    writeFileSync(join(directory,"failure-diagnostic-reports.json"),beforeDeletion);
    const restoredBackup=new TelemetryStore(directory,pepper);
    assert.equal(restoredBackup.deletionStatus(deletion.deletion_request_id,credential.device_token).status,"completed");
    assert.equal(JSON.parse(readFileSync(join(directory,"failure-diagnostic-reports.json"),"utf8")).reports.length,0);
  }finally{clearTimeout(store?.saveTimer);FailureReportStore.prototype.prune=originalPrune;rmSync(directory,{recursive:true,force:true});}
});

test("report retries survive credential-period renewal without authorizing another incident",()=>{
  const directory=mkdtempSync(join(tmpdir(),"pinvou-diag-renew-"));
  try{
    const store=new FailureReportStore(directory),incident=witness({device_id:"A"});
    const device={device_id:"A",behavior_collection:{status:"active",collection_period_id:"fixture_collection_period"}};
    const packet=report();store.append(device,packet,incident);
    device.behavior_collection.collection_period_id="renewed_collection_period";
    assert.equal(store.append(device,{...packet,collection_period_id:"renewed_collection_period",incident_period_id:"fixture_collection_period"},incident).status,"received");
    assert.throws(()=>store.append(device,{...packet,collection_period_id:"renewed_collection_period",incident_period_id:"another_incident_period"},incident),/incident_not_received/);
  }finally{rmSync(directory,{recursive:true,force:true});}
});

test("diagnostic proxy upload allowance is bounded to the exact report route",()=>{
  const config=readFileSync(new URL("../nginx-telemetry-stats.conf",import.meta.url),"utf8");
  assert.match(config,/location = \/pinvou3\/stats\/api\/diagnostics\/report\s*\{[^}]*client_max_body_size 2300k;/);
  assert.equal((config.match(/client_max_body_size/g)||[]).length,1);
});

test("a full material quota reserves capacity for mandatory reads and issue updates",()=>{
  const directory=mkdtempSync(join(tmpdir(),"pinvou-diag-quota-"));
  try{
    const store=new FailureReportStore(directory);
    const bytes=Buffer.alloc(512*1024);Buffer.from("89504e470d0a1a0a","hex").copy(bytes);
    const attachments=Array.from({length:3},()=>({kind:"image/png",content:bytes.toString("base64")}));
    let accepted=0;
    for(let index=0;index<20;index++) {
      const device={device_id:`device_quota_${index}`,behavior_collection:{status:"active",collection_period_id:"fixture_collection_period"}};
      try { store.append(device,report({report_id:`report_quota_fixture_${index}`,attachments}),witness({device_id:device.device_id}));accepted++; }
      catch(error){assert.match(error.message,/diagnostic_report_capacity/);break;}
    }
    assert(accepted>=10 && accepted<20);
    const incident=witness({device_id:"device_quota_0"});
    assert.equal(store.detail(incident,"operator").reports.length,1);
    const fingerprint=failureGroups([incident])[0].fingerprint;
    assert.equal(store.update(fingerprint,{status:"fixed",fixed_version:"0.10.13"},"operator").status,"fixed");
    assert.equal(new FailureReportStore(directory).detail(incident,"operator").reports.length,1);
    assert(readFileSync(join(directory,"failure-diagnostic-reports.json")).length<32*1024*1024);
  }finally{rmSync(directory,{recursive:true,force:true});}
});

test("report storage requires incident ownership and active original consent; retries, retention, deletion and corruption are explicit", () => {
  const directory=mkdtempSync(join(tmpdir(),"pinvou-diag-store-"));
  try {
    const device={device_id:"A",behavior_collection:{status:"active",collection_period_id:"fixture_collection_period"}};
    const incident=witness({device_id:"A"}); const store=new FailureReportStore(directory); const packet=report();
    assert.throws(()=>store.append(device,{...packet,notice_version:""},incident),/consent_invalid/);
    assert.throws(()=>store.append(device,packet,{...incident,device_id:"B"}),/incident_not_received/);
    assert.throws(()=>store.append({...device,behavior_collection:{...device.behavior_collection,status:"ended"}},packet,incident),/collection_inactive/);
    assert.throws(()=>store.append(device,{...packet,collection_period_id:"other_collection_period"},incident),/incident_not_received/);
    assert.equal(store.append(device,packet,incident).status,"received");
    assert.equal(store.append(device,packet,incident).status,"received");
    assert.throws(()=>store.append(device,{...packet,notes:"different"},incident),/report_conflict/);
    const reopened=new FailureReportStore(directory);
    assert.equal(reopened.detail(incident,"operator").reports.length,1);
    assert.equal(reopened.state.audit.length,1);
    assert.equal(Object.hasOwn(reopened.detail(incident,"operator").incident,"device_id"),false);
    reopened.update(failureGroups([incident])[0].fingerprint,{status:"fixed",fixed_version:"0.10.13"},"operator");
    assert.throws(()=>reopened.update(failureGroups([incident])[0].fingerprint,{status:"fixed",fixed_version:""},"operator"));
    reopened.prune(new Map([["A",device]]),Date.now()+15*86400000);
    assert.equal(reopened.state.reports.length,0);
    const next=report({report_id:"report_second_fixture"}); reopened.append(device,next,incident);
    reopened.prune(new Map()); assert.equal(new FailureReportStore(directory).state.reports.length,0);
    writeFileSync(join(directory,"failure-diagnostic-reports.json"),"broken");
    assert.throws(()=>new FailureReportStore(directory).detail(incident,"operator"),/storage_unavailable/);
  } finally { rmSync(directory,{recursive:true,force:true}); }
});

test("text defense removes quoted credentials, addresses and paths; report types and size stay bounded", () => {
  const raw='{"api_key": "fixture-private-value"}\nAuthorization: Bearer abc123\nhttps://private.invalid/a\nuser@private.invalid C:\\Users\\person\\file';
  const cleaned=redactDiagnosticText(raw);
  for(const secret of ["fixture-private-value","abc123","private.invalid","person"])assert(!cleaned.includes(secret));
  const directory=mkdtempSync(join(tmpdir(),"pinvou-diag-content-"));
  try {
    const store=new FailureReportStore(directory), device={device_id:"A",behavior_collection:{status:"active",collection_period_id:"fixture_collection_period"}}, incident=witness({device_id:"A"});
    for(const attachments of [[{kind:"image/svg+xml",content:"<svg/>"}],[{kind:"text",content:"x".repeat(65537)}],[{kind:"image/png",content:Buffer.from("not png").toString("base64")} ]]) {
      assert.throws(()=>store.append(device,report({attachments}),incident));
    }
    store.append(device,report({notes:raw,attachments:[{kind:"text",content:raw}]}),incident);
    const persisted=readFileSync(join(directory,"failure-diagnostic-reports.json"),"utf8");
    assert(!persisted.includes("fixture-private-value"));assert(!persisted.includes("abc123"));
  } finally { rmSync(directory,{recursive:true,force:true}); }
});

test("redaction expansion stays within durable report limits after restart",()=>{
  const directory=mkdtempSync(join(tmpdir(),"pinvou-diag-redaction-bound-"));
  try{
    const store=new FailureReportStore(directory),device={device_id:"A",behavior_collection:{status:"active",collection_period_id:"fixture_collection_period"}},incident=witness({device_id:"A"});
    store.append(device,report({notes:"token=x ".repeat(700),attachments:[{kind:"text",content:"token=x ".repeat(8000)}]}),incident);
    const reloaded=new FailureReportStore(directory);
    assert.equal(reloaded.healthy,true);
    const material=reloaded.detail(incident,"operator").reports[0];
    assert(material.notes.length<=6000);assert(Buffer.byteLength(material.attachments[0].content)<=65536);
  }finally{rmSync(directory,{recursive:true,force:true});}
});

test("HTTP closes the collection/authorization/admin/CSRF/delete loop without public incident access", async t => {
  const directory=mkdtempSync(join(tmpdir(),"pinvou-diag-http-"));
  const environment={PINVOU_TELEMETRY_ENROLLMENT_TOKEN:"diagnostic-fixture-enrollment-token",PINVOU_TELEMETRY_DEVICE_PEPPER:"diagnostic-fixture-pepper-value",PINVOU_STATS_ADMIN_PASSWORD:"diagnostic-fixture-admin-password",PINVOU_BEHAVIOR_ANALYTICS_PASSWORD:"diagnostic-fixture-analyst-password",PINVOU_TELEMETRY_DATA_DIR:directory};
  const previous=Object.fromEntries(Object.keys(environment).map(k=>[k,process.env[k]])); Object.assign(process.env,environment);
  const service=createTelemetryService({regionResolver:{enabled:false,lookup:async()=>null}});
  const server=http.createServer((req,res)=>{void service.handleHttp(req,res,new URL(req.url,"http://localhost").pathname).then(handled=>{if(!handled){res.writeHead(404);res.end();}});});
  await new Promise(resolve=>server.listen(0,"127.0.0.1",resolve)); const base=`http://127.0.0.1:${server.address().port}`;
  t.after(async()=>{service.close();await new Promise(resolve=>server.close(resolve));for(const[k,v]of Object.entries(previous)){if(v===undefined)delete process.env[k];else process.env[k]=v;}rmSync(directory,{recursive:true,force:true});});
  async function call(path,body,headers={}) {const response=await fetch(base+path,{method:body===undefined?"GET":"POST",headers:{"content-type":"application/json",...headers},...(body===undefined?{}:{body:JSON.stringify(body)})});return {status:response.status,json:await response.json(),cookie:response.headers.get("set-cookie")?.split(";")[0]};}
  const prefix="/pinvou3/stats/api";
  const registered=await call("/pinvou3/telemetry/v1/register",{enrollment_token:environment.PINVOU_TELEMETRY_ENROLLMENT_TOKEN,hardware_claim:"installation-diag-fixture",hardware_source:"installation_id",registration_secret:"installation-diag-secret-fixture",identity_quality:"installation_only",app_version:"0.10.12",platform:"windows",arch:"x86_64"});
  assert.equal(registered.status,200);
  const creds=registered.json, auth={authorization:`Bearer ${creds.device_token}`};
  const collection=await call(prefix+"/behavior/collection/start",{device_id:creds.device_id,request_id:"fixture_start_request_1"},auth);
  const event=witness({collection_period_id:collection.json.collection_period_id});
  const accepted=await call(prefix+"/behavior/events",{device_id:creds.device_id,events:[event]},auth);assert.equal(accepted.status,200);
  assert.equal((await call(prefix+"/behavior/failures")).status,401);
  const admin=await call(prefix+"/login",{password:environment.PINVOU_STATS_ADMIN_PASSWORD});
  const analyst=await call(prefix+"/login",{password:environment.PINVOU_BEHAVIOR_ANALYTICS_PASSWORD});
  const groups=await call(prefix+"/behavior/failures",undefined,{cookie:admin.cookie});assert.equal(groups.json.rows.length,1);
  assert.equal((await call(prefix+"/behavior/failures",undefined,{cookie:analyst.cookie})).json.rows.length,0);
  const fingerprint=groups.json.rows[0].fingerprint;
  assert.equal((await call(prefix+`/diagnostics/samples?fingerprint=${fingerprint}`,undefined,{cookie:analyst.cookie})).status,403);
  const samples=await call(prefix+`/diagnostics/samples?fingerprint=${fingerprint}`,undefined,{cookie:admin.cookie});assert.equal(samples.json.incidents.length,1);
  const packet=report({collection_period_id:collection.json.collection_period_id});
  assert.equal((await call(prefix+"/diagnostics/report",{device_id:creds.device_id,report:packet},auth)).status,200);
  const detail=await call(prefix+`/diagnostics/incident?id=${samples.json.incidents[0].id}`,undefined,{cookie:admin.cookie});assert.equal(detail.json.reports.length,1);
  assert.equal((await call(prefix+`/diagnostics/incident?id=${event.diagnostic_id}`,undefined,{cookie:admin.cookie})).status,404);
  const update={fingerprint,issue:{status:"reproduced",fixed_version:""}};
  assert.equal((await call(prefix+"/diagnostics/issue",update,{cookie:admin.cookie})).status,403);
  assert.equal((await call(prefix+"/diagnostics/issue",update,{cookie:admin.cookie,"x-pinvou-diagnostics":"1"})).status,200);
  const deleted=await call(prefix+"/behavior/deletion",{device_id:creds.device_id,request_id:"fixture_delete_request_1"},auth);assert.equal(deleted.status,202);
  assert.equal((await call(prefix+`/diagnostics/incident?id=${samples.json.incidents[0].id}`,undefined,{cookie:admin.cookie})).status,404);
  assert.equal(JSON.parse(readFileSync(join(directory,"failure-diagnostic-reports.json"),"utf8")).reports.length,0);
});
