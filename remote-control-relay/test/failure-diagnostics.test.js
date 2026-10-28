import assert from "node:assert/strict";
import test from "node:test";
import { existsSync, mkdtempSync, rmSync, readFileSync, unlinkSync, writeFileSync } from "node:fs";
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

test("sampled detail counts exclude unsampled faults while fault and subject counts remain complete", () => {
  const events = [true, false, undefined].map((diagnostic_sampled, index) => witness({
    device_id:`sampled_device_${index}`, diagnostic_id:`sampled_incident_${index}`,
    diagnostic_sampled,
  }));
  const group = failureGroups([...events, ...events])[0];
  assert.equal(group.count, 3);
  assert.equal(group.installations, 3);
  assert.equal(group.sampled_details, 2, 'old clients without sampling metadata retain existing detail semantics');
  for (const flag of [true, false]) assert.equal(validateBehaviorEventShape(witness({diagnostic_sampled:flag})).ok, true);
  assert.equal(validateBehaviorEventShape(witness({diagnostic_sampled:'private-flag'})).ok, false);
});

test("report rate and inventory caps reject only new reports and keep prior receipts readable", t => {
  const directory=mkdtempSync(join(tmpdir(),'pinvou-diag-rate-'));
  t.after(()=>{assert(directory.startsWith(join(tmpdir(),'pinvou-diag-rate-')));rmSync(directory,{recursive:true,force:true});});
  const store=new FailureReportStore(directory);
  const device={device_id:'rate_device',behavior_collection:{status:'active',collection_period_id:'fixture_collection_period'}};
  const incident=witness({device_id:device.device_id});
  const packets=Array.from({length:10},(_,i)=>report({report_id:`rate_report_fixture_${i}`}));
  for(const packet of packets)store.append(device,packet,incident);
  assert.throws(()=>store.append(device,report({report_id:'rate_report_fixture_10'}),incident),/report_rate_limited/);
  assert.equal(store.append(device,packets[0],incident).status,'received');
  const other={...device,device_id:'other_device'},otherIncident={...incident,device_id:other.device_id};
  assert.equal(store.append(other,report({report_id:'other_report_fixture'}),otherIncident).status,'received');
  const now=Date.now();
  // The quota is a rolling day, independent of calendar boundaries.
  store.save({...store.state,reports:store.state.reports.map(row=>({...row,received_at:now-86400000-1000}))});
  assert.equal(store.append(device,report({report_id:'rate_report_next_day'}),incident).status,'received');
  // Seed valid persisted material to reach the inventory boundary without 1000
  // unrelated synchronous disk writes; admission still runs production append.
  const seed=store.state.reports[0];
  store.save({...store.state,reports:Array.from({length:1000},(_,i)=>({...seed,
    device_id:`inventory_device_${i}`,report_id:`inventory_report_${i}`}))});
  const full=new FailureReportStore(directory);
  assert.equal(full.healthy,true);
  assert.throws(()=>full.append(other,report({report_id:'inventory_overflow_fixture'}),otherIncident),/report_rate_limited/);
  assert.equal(full.state.reports.length,1000);
  full.audit('detail','inventory_fixture','operator');
  assert.equal(full.state.audit.length,1);
  full.saveMetadata({...full.state,audit:Array.from({length:2000},(_,i)=>({at:now,action:'detail',target:`audit_${i}`,actor:'operator'}))});
  full.audit('detail','audit_newest','operator');
  const reopened=new FailureReportStore(directory);
  assert.equal(reopened.state.audit.length,2000);
  assert.equal(reopened.state.audit[0].target,'audit_1');
  assert.equal(reopened.state.audit.at(-1).target,'audit_newest');
});

test("deletion remains processing across storage failure and restart until reports are durably removed",async()=>{
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
    clearTimeout(store.saveTimer);
    FailureReportStore.prototype.prune=function(){throw new Error("simulated_disk_failure");};
    const deletion=store.deleteBehaviorData(device,{request_id:"diagnostic_delete_request"}); assert.equal(deletion.status,"processing");
    const restored=new TelemetryStore(directory,pepper);
    assert.equal(restored.deletionStatus(deletion.deletion_request_id,credential.device_token).status,"processing");
    assert.equal(JSON.parse(readFileSync(join(directory,"failure-diagnostic-reports.json"),"utf8")).reports.length,1);
    FailureReportStore.prototype.prune=originalPrune;
    const recovered=new TelemetryStore(directory,pepper);
    // Windows can briefly refuse a real atomic rename after restart. Exercise
    // the real status-driven recovery, but a sustained failure must still fail.
    const recoveryDeadline=Date.now()+3000;
    let recoveredStatus=recovered.deletionStatus(deletion.deletion_request_id,credential.device_token);
    while(recoveredStatus.status!=="completed" && Date.now()<recoveryDeadline){
      await new Promise(resolve=>setTimeout(resolve,25));
      recoveredStatus=recovered.deletionStatus(deletion.deletion_request_id,credential.device_token);
    }
    assert.equal(recoveredStatus.status,"completed");
    assert.equal(JSON.parse(readFileSync(join(directory,"failure-diagnostic-reports.json"),"utf8")).reports.length,0);
    writeFileSync(join(directory,"failure-diagnostic-reports.json"),beforeDeletion);
    const restoredBackup=new TelemetryStore(directory,pepper);
    assert.equal(restoredBackup.deletionStatus(deletion.deletion_request_id,credential.device_token).status,"completed");
    const backupDeadline=Date.now()+3000;
    while(JSON.parse(readFileSync(join(directory,"failure-diagnostic-reports.json"),"utf8")).reports.length!==0 && Date.now()<backupDeadline){
      await new Promise(resolve=>setTimeout(resolve,25));
      restoredBackup.compactExpiredData();
    }
    assert.equal(JSON.parse(readFileSync(join(directory,"failure-diagnostic-reports.json"),"utf8")).reports.length,0);
  }finally{clearTimeout(store?.saveTimer);FailureReportStore.prototype.prune=originalPrune;rmSync(directory,{recursive:true,force:true});}
});

test("unknown device inventory or deletion ledger cannot destroy an acknowledged report on restart", t => {
  const directory = mkdtempSync(join(tmpdir(), "pinvou-diag-inventory-recovery-"));
  const pepper = "diagnostic-fixture-device-pepper";
  let store;
  t.mock.method(console, "error", () => {});
  try {
    store = new TelemetryStore(directory, pepper);
    const credential = store.register({hardware_claim:"diagnostic-inventory-hardware",registration_secret:"diagnostic-inventory-registration-secret",app_version:"0.10.12",platform:"windows",arch:"x64"}, null);
    const device = store.devices.get(credential.device_id);
    const period = store.startBehaviorCollection(device, {request_id:"diagnostic_inventory_start"}).collection_period_id;
    const event = witness({collection_period_id:period});
    store.appendBehaviorEvents(device, [event]);
    const packet = report({collection_period_id:period});
    const incident = {...event, device_id:device.device_id};
    assert.equal(store.failureReports.append(device, packet, incident).status, "received");
    store.saveDeletionTombstones();
    store.saveNow();
    clearTimeout(store.saveTimer);
    const devicesPath = join(directory, "devices.json");
    const ledgerPath = join(directory, "behavior-deletion-tombstones.json");
    const files = [devicesPath, ledgerPath, join(directory, "failure-diagnostic-reports.json"),
      join(directory, "behavior-events.jsonl"), join(directory, "behavior-pipeline.jsonl")].filter(existsSync);
    const original = new Map(files.map(path => [path, readFileSync(path)]));
    const invalidDeviceRows = [null, ...["device_id", "hardware_hash", "device_token"].map(key => ({...device, [key]:undefined})),
      {...device, device_id:17}, {...device, device_token:""}];
    const corruptions = [
      ...["broken", "null", "{}", '{"devices":{}}', ...invalidDeviceRows.map(row => JSON.stringify({devices:[device, row]})),
        JSON.stringify({devices:[device, {...device}]}),
        JSON.stringify({devices:[device, {...device, device_id:"duplicate_hardware_fixture"}]}),
        ...[false, null, "true"].map(marker => JSON.stringify({devices:[device], deletion_ledger_required:marker}))]
        .map(content => [devicesPath, content]),
      ...["broken", "null", "{}", '{"records":{}}', '{"records":[null]}', '{"records":[{}]}']
        .map(content => [ledgerPath, content]),
      [devicesPath, null],
      [ledgerPath, null],
    ];
    for (const [path, content] of corruptions) {
      if (content === null) unlinkSync(path);
      else writeFileSync(path, content);
      assert.throws(() => new TelemetryStore(directory, pepper), error => error.status === 503
        && error.message === "telemetry_storage_unavailable");
      for (const file of files) {
        if (file === path) {
          if (content === null) assert.equal(existsSync(file), false);
          else assert.equal(readFileSync(file, "utf8"), content, "preserve the corrupt inventory for recovery");
        } else assert.deepEqual(readFileSync(file), original.get(file), "startup failure must not rewrite dependent stores");
      }
      writeFileSync(path, original.get(path));
    }
    const recovered = new TelemetryStore(directory, pepper);
    assert.equal(recovered.failureReports.detail(incident, "operator").reports.length, 1);
    assert.equal(recovered.failureReports.append(recovered.devices.get(device.device_id), packet, incident).status, "received");
    assert.equal(recovered.deleteBehaviorData(recovered.devices.get(device.device_id), {request_id:"diagnostic_inventory_delete"}).status, "completed");
    assert.equal(new FailureReportStore(directory).state.reports.length, 0);
    clearTimeout(recovered.saveTimer);
  } finally { clearTimeout(store?.saveTimer); rmSync(directory, {recursive:true, force:true}); }
});

test("a fresh store and an authoritative empty device inventory remain valid", () => {
  const directory = mkdtempSync(join(tmpdir(), "pinvou-diag-empty-inventory-"));
  try {
    assert.equal(new TelemetryStore(directory, "diagnostic-fixture-device-pepper").devices.size, 0);
    assert.equal(new TelemetryStore(directory, "diagnostic-fixture-device-pepper").devices.size, 0);
    writeFileSync(join(directory, "devices.json"), JSON.stringify({version:2, devices:[]}));
    assert.equal(new TelemetryStore(directory, "diagnostic-fixture-device-pepper").devices.size, 0);
  } finally { rmSync(directory, {recursive:true, force:true}); }
});

test("legacy ledger migration is durable, repeatable and preserves report evidence on write failure", t => {
  const directory = mkdtempSync(join(tmpdir(), "pinvou-diag-ledger-migration-"));
  const pepper = "diagnostic-fixture-device-pepper";
  let store;
  try {
    store = new TelemetryStore(directory, pepper);
    const credential = store.register({hardware_claim:"diagnostic-legacy-hardware",registration_secret:"diagnostic-legacy-registration-secret"}, null);
    const device = store.devices.get(credential.device_id);
    const period = store.startBehaviorCollection(device, {request_id:"diagnostic_legacy_start"}).collection_period_id;
    const event = witness({collection_period_id:period});
    store.appendBehaviorEvents(device, [event]);
    store.failureReports.append(device, report({collection_period_id:period}), {...event, device_id:device.device_id});
    store.saveNow(); clearTimeout(store.saveTimer);
    const devicesPath = join(directory, "devices.json"), ledgerPath = join(directory, "behavior-deletion-tombstones.json");
    const reportPath = join(directory, "failure-diagnostic-reports.json");
    const legacy = JSON.parse(readFileSync(devicesPath, "utf8"));
    delete legacy.deletion_ledger_required;
    writeFileSync(devicesPath, JSON.stringify(legacy)); unlinkSync(ledgerPath);
    const originalDevices = readFileSync(devicesPath), originalReports = readFileSync(reportPath);
    const failedLedger = t.mock.method(TelemetryStore.prototype, "saveDeletionTombstones", () => { throw new Error("simulated_ledger_write_failure"); });
    assert.throws(() => new TelemetryStore(directory, pepper), /simulated_ledger_write_failure/);
    assert.deepEqual(readFileSync(devicesPath), originalDevices);
    assert.deepEqual(readFileSync(reportPath), originalReports);
    assert.equal(existsSync(ledgerPath), false);
    failedLedger.mock.restore();
    const failedMarker = t.mock.method(TelemetryStore.prototype, "saveNow", () => { throw new Error("simulated_marker_write_failure"); });
    assert.throws(() => new TelemetryStore(directory, pepper), /simulated_marker_write_failure/);
    assert.deepEqual(readFileSync(devicesPath), originalDevices);
    assert.deepEqual(readFileSync(reportPath), originalReports);
    assert.deepEqual(JSON.parse(readFileSync(ledgerPath, "utf8")).records, []);
    failedMarker.mock.restore();
    const migrated = new TelemetryStore(directory, pepper);
    assert.equal(JSON.parse(readFileSync(devicesPath, "utf8")).deletion_ledger_required, true);
    assert.equal(migrated.failureReports.state.reports.length, 1);
    assert.equal(new TelemetryStore(directory, pepper).failureReports.state.reports.length, 1);
    assert.deepEqual(readFileSync(reportPath), originalReports);
  } finally { clearTimeout(store?.saveTimer); rmSync(directory, {recursive:true, force:true}); }
});

test("missing device inventory with dependent history is not a fresh deployment", () => {
  for (const file of ["usage-events.jsonl", "behavior-events.jsonl", "behavior-pipeline.jsonl", "failure-diagnostic-reports.json", "behavior-deletion-tombstones.json"]) {
    const directory = mkdtempSync(join(tmpdir(), "pinvou-diag-missing-inventory-"));
    try {
      writeFileSync(join(directory, file), "historic evidence");
      assert.throws(() => new TelemetryStore(directory, "diagnostic-fixture-device-pepper"), /telemetry_storage_unavailable/);
      assert.equal(readFileSync(join(directory, file), "utf8"), "historic evidence");
      assert.equal(existsSync(join(directory, "devices.json")), false);
    } finally { rmSync(directory, {recursive:true, force:true}); }
  }
});

test("first startup can repeat an interrupted marker write and routine saves do not rewrite the ledger", t => {
  const directory = mkdtempSync(join(tmpdir(), "pinvou-diag-ledger-initialization-"));
  const pepper = "diagnostic-fixture-device-pepper";
  try {
    const failedMarker = t.mock.method(TelemetryStore.prototype, "saveNow", () => { throw new Error("simulated_marker_write_failure"); });
    assert.throws(() => new TelemetryStore(directory, pepper), /simulated_marker_write_failure/);
    const devicesPath = join(directory, "devices.json"), ledgerPath = join(directory, "behavior-deletion-tombstones.json");
    assert.deepEqual(JSON.parse(readFileSync(devicesPath, "utf8")), {version:2, devices:[]});
    assert.deepEqual(JSON.parse(readFileSync(ledgerPath, "utf8")).records, []);
    failedMarker.mock.restore();
    const recovered = new TelemetryStore(directory, pepper);
    const ledgerWrites = t.mock.method(recovered, "saveDeletionTombstones");
    const originalLedger = readFileSync(ledgerPath);
    recovered.saveNow(); recovered.saveNow();
    assert.equal(ledgerWrites.mock.callCount(), 0);
    assert.deepEqual(readFileSync(ledgerPath), originalLedger);
    unlinkSync(ledgerPath);
    recovered.saveNow();
    assert.equal(ledgerWrites.mock.callCount(), 1);
    assert.deepEqual(readFileSync(ledgerPath), originalLedger);
    assert.equal(JSON.parse(readFileSync(devicesPath, "utf8")).deletion_ledger_required, true);
  } finally { rmSync(directory, {recursive:true, force:true}); }
});

test("report retries survive credential-period renewal without authorizing another incident",()=>{
  const directory=mkdtempSync(join(tmpdir(),"pinvou-diag-renew-"));
  try{
    const store=new FailureReportStore(directory),incident=witness({device_id:"A"});
    const device={device_id:"A",behavior_collection:{status:"active",collection_period_id:"fixture_collection_period"}};
    const packet=report();store.append(device,packet,incident);
    device.behavior_collection.collection_period_id="renewed_collection_period";
    assert.equal(store.append(device,{...packet,collection_period_id:"renewed_collection_period",incident_period_id:"fixture_collection_period"},incident).status,"received");
    assert.throws(()=>store.append(device,{...packet,collection_period_id:"renewed_collection_period",incident_period_id:"another_incident_period"},incident),/report_conflict/);
    assert.throws(()=>store.append(device,{...packet,report_id:"report_other_period_fixture",collection_period_id:"renewed_collection_period",incident_period_id:"another_incident_period"},incident),/incident_period_mismatch/);
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
    // Fourteen sets already exceed 28 MiB from base64 alone, before JSON
    // metadata. A 32 MiB mutation admits fifteen and must fail this contract.
    assert.equal(accepted, 13);
    assert.equal(store.state.reports.length, 13);
    assert(Buffer.byteLength(JSON.stringify(store.state.reports)) <= 28*1024*1024);
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
    assert.throws(()=>store.append(device,{...packet,collection_period_id:"other_collection_period"},incident),/collection_inactive/);
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
  async function call(path,body,headers={}) {const response=await fetch(base+path,{method:body===undefined?"GET":"POST",headers:{"content-type":"application/json",connection:"close",...headers},...(body===undefined?{}:{body:JSON.stringify(body)})});return {status:response.status,json:await response.json(),cookie:response.headers.get("set-cookie")?.split(";")[0]};}
  const prefix="/pinvou3/stats/api";
  const registered=await call("/pinvou3/telemetry/v1/register",{enrollment_token:environment.PINVOU_TELEMETRY_ENROLLMENT_TOKEN,hardware_claim:"installation-diag-fixture",hardware_source:"installation_id",registration_secret:"installation-diag-secret-fixture",identity_quality:"installation_only",app_version:"0.10.12",platform:"windows",arch:"x86_64"});
  assert.equal(registered.status,200);
  const creds=registered.json, auth={authorization:`Bearer ${creds.device_token}`};
  for (const malformed of [null, [], "invalid"]) {
    const result=await call(prefix+"/diagnostics/report",malformed,auth);
    assert.equal(result.status,400);
    assert.equal(result.json.error,'invalid_diagnostic_report');
  }
  const collection=await call(prefix+"/behavior/collection/start",{device_id:creds.device_id,request_id:"fixture_start_request_1"},auth);
  const event=witness({collection_period_id:collection.json.collection_period_id});
  const accepted=await call(prefix+"/behavior/events",{device_id:creds.device_id,events:[event]},auth);assert.equal(accepted.status,200);
  assert.equal((await call(prefix+"/behavior/failures")).status,401);
  const admin=await call(prefix+"/login",{password:environment.PINVOU_STATS_ADMIN_PASSWORD});
  const analyst=await call(prefix+"/login",{password:environment.PINVOU_BEHAVIOR_ANALYTICS_PASSWORD});
  const groups=await call(prefix+"/behavior/failures",undefined,{cookie:admin.cookie});assert.equal(groups.json.rows.length,1);
  for (const now of ["0", "abc", "9999999999999"]) {
    const bounded = await call(prefix+`/behavior/failures?now=${now}`,undefined,{cookie:admin.cookie});
    assert.equal(bounded.status,200);assert.deepEqual(bounded.json.rows,groups.json.rows);
  }
  assert.equal((await call(prefix+"/behavior/failures",undefined,{cookie:analyst.cookie})).json.rows.length,0);
  const fingerprint=groups.json.rows[0].fingerprint;
  assert.equal((await call(prefix+`/diagnostics/samples?fingerprint=${fingerprint}`,undefined,{cookie:analyst.cookie})).status,403);
  const samples=await call(prefix+`/diagnostics/samples?fingerprint=${fingerprint}`,undefined,{cookie:admin.cookie});assert.equal(samples.json.incidents.length,1);
  const png=Buffer.alloc(512*1024,0);Buffer.from('89504e470d0a1a0a','hex').copy(png);
  const packet=report({collection_period_id:collection.json.collection_period_id,
    attachments:Array.from({length:3},()=>({kind:'image/png',content:png.toString('base64')}))});
  const wire=JSON.stringify({device_id:creds.device_id,report:packet});
  assert(Buffer.byteLength(wire)>512*1024 && Buffer.byteLength(wire)<=2300*1024);
  assert.equal((await call(prefix+"/diagnostics/report",{device_id:creds.device_id,report:packet},auth)).status,200);
  // Breaking the request iterator on overflow may close the connection before
  // the 413 reaches fetch. Both outcomes reject the upload; neither may append.
  try {
    assert.equal((await call(prefix+"/diagnostics/report",{device_id:creds.device_id,
      report:{...packet,report_id:'oversized_report_fixture',notes:'x'.repeat(2300*1024)}},auth)).status,413);
  } catch(error) {
    assert.equal(error?.cause?.code,'ECONNRESET');
  }
  assert.equal(JSON.parse(readFileSync(join(directory,'failure-diagnostic-reports.json'),'utf8')).reports.length,1);
  const rebound = witness({event_id:"beh_renewed_incident_fixture",diagnostic_id:"beh_renewed_incident_fixture",
    collection_period_id:collection.json.collection_period_id,diagnostic_origin_period_id:"fixture_original_period"});
  const reboundReceipt = await call(prefix+"/behavior/events",{device_id:creds.device_id,events:[rebound]},auth);
  assert.equal(reboundReceipt.status,200);assert.equal(reboundReceipt.json.results[0].status,"accepted");
  const frozen = report({report_id:"report_renewed_incident_fixture",incident_id:rebound.diagnostic_id,
    collection_period_id:"fixture_original_period",incident_period_id:"fixture_original_period"});
  const exact = JSON.stringify(frozen);
  const renewed = {device_id:creds.device_id,collection_period_id:collection.json.collection_period_id,report:frozen};
  assert.equal((await call(prefix+"/diagnostics/report",renewed,auth)).status,200);
  assert.equal((await call(prefix+"/diagnostics/report",renewed,auth)).status,200);
  assert.equal(JSON.stringify(frozen),exact);
  assert.equal((await call(prefix+"/diagnostics/report",{...renewed,collection_period_id:"fixture_original_period"},auth)).status,409);
  const detail=await call(prefix+`/diagnostics/incident?id=${samples.json.incidents[0].id}`,undefined,{cookie:admin.cookie});assert.equal(detail.json.reports.length,1);
  assert.equal(detail.json.timeline_context.incident_included,true);
  assert.equal(detail.json.timeline_context.task_terminal_included,true);
  assert(detail.json.timeline.some(item=>item.diagnostic_code===event.diagnostic_code));
  assert.equal((await call(prefix+`/diagnostics/incident?id=${event.diagnostic_id}`,undefined,{cookie:admin.cookie})).status,404);
  const update={fingerprint,issue:{status:"reproduced",fixed_version:""}};
  assert.equal((await call(prefix+"/diagnostics/issue",update,{cookie:admin.cookie})).status,403);
  assert.equal((await call(prefix+"/diagnostics/issue",update,{cookie:admin.cookie,"x-pinvou-diagnostics":"1"})).status,200);
  const deleted=await call(prefix+"/behavior/deletion",{device_id:creds.device_id,request_id:"fixture_delete_request_1"},auth);assert.equal(deleted.status,202);
  assert.equal((await call(prefix+`/diagnostics/incident?id=${samples.json.incidents[0].id}`,undefined,{cookie:admin.cookie})).status,404);
  assert.equal(JSON.parse(readFileSync(join(directory,"failure-diagnostic-reports.json"),"utf8")).reports.length,0);
});
