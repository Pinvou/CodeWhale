import assert from "node:assert/strict";
import test from "node:test";
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, statSync, unlinkSync, writeFileSync } from "node:fs";
import http from "node:http";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { DiagnosticError, FailureReportStore, failureGroups } from "../failure-diagnostics.js";
import { createTelemetryService, TelemetryStore } from "../telemetry-service.js";

const period = "review_original_period";
const device = { device_id: "review_device", behavior_collection: { status: "active", collection_period_id: period } };
const incident = { device_id: device.device_id, event_id: "review_incident_id", diagnostic_id: "review_incident_id",
  collection_period_id: period, diagnostic_component: "tool", diagnostic_code: "tool_failed", stage: "tool" };
const packet = extra => ({ report_id: "review_report_id", incident_id: incident.diagnostic_id,
  collection_period_id: period, incident_period_id: period, notice_version: "failure-diagnostics-v1",
  confirmed_at: Date.now(), notes: "error_code=EACCES", attachments: [], ...extra });
function directory(t) {
  const path = mkdtempSync(join(tmpdir(), "pinvou-diagnostic-review-"));
  t.after(() => {
    assert.equal(dirname(resolve(path)), resolve(tmpdir()));
    assert(path.startsWith(join(tmpdir(), "pinvou-diagnostic-review-")));
    rmSync(path, { recursive: true, force: true });
  });
  return path;
}

test("credential renewal changes delivery authorization without changing confirmed report bytes", t => {
  const path = directory(t), store = new FailureReportStore(path), input = packet();
  const exact = JSON.stringify(input), next = "review_renewed_period";
  const renewed = { ...device, behavior_collection: { status: "active", collection_period_id: next } };
  const rebound = { ...incident, collection_period_id: next, diagnostic_origin_period_id: period };
  assert.equal(store.append(renewed, input, rebound, next).status, "received");
  assert.equal(JSON.stringify(input), exact);
  const restored = new FailureReportStore(path);
  // The original event may have been compacted or downgraded by an older
  // collector. An unchanged, already acknowledged report still has a receipt.
  assert.equal(restored.append(renewed, input, undefined, next).status, "received");
  assert.equal(restored.append(renewed, input, { ...rebound, diagnostic_id: undefined }, next).status, "received");
  assert.throws(() => restored.append(renewed, { ...input, notes: "error_code=EPERM" }, undefined, next), /report_conflict/);
  assert.throws(() => restored.append(renewed, input, rebound, period), /collection_inactive/);
  assert.throws(() => restored.append({ ...renewed, behavior_collection: { ...renewed.behavior_collection, status: "ended" } }, input, rebound, next), /collection_inactive/);
  assert.throws(() => restored.append(renewed, packet({ report_id: "review_new_report" }), { ...rebound, device_id: "other" }, next), /incident_not_received/);
  assert.throws(() => restored.append(renewed, packet({ report_id: "review_new_report" }), { ...rebound, diagnostic_origin_period_id: "wrong_original_period" }, next), /incident_period_mismatch/);
  assert.throws(() => restored.append(renewed, packet({ report_id: "review_legacy_report" }), { ...rebound, diagnostic_origin_period_id: undefined }, next), /incident_period_mismatch/);
  assert.throws(() => restored.append(renewed, packet({ report_id: "review_pending_report" }), undefined, next), /incident_not_received/);
  assert.throws(() => restored.append(renewed, packet({ report_id: "review_new_report" }), { ...rebound, diagnostic_id: undefined }, next), /metadata_missing/);
});

test("audit and triage persist before disclosure without rewriting or serializing stored material", t => {
  const path = directory(t), store = new FailureReportStore(path);
  store.append(device, packet(), incident);
  const before = readFileSync(store.path), writes = [];
  const original = store.writePrivate.bind(store);
  t.mock.method(store, "writePrivate", (destination, bytes) => { writes.push(destination); return original(destination, bytes); });
  Object.defineProperty(store.state.reports[0], "toJSON", { value() { throw new Error("old_material_serialized"); } });
  const fingerprint = store.state.reports[0].fingerprint;
  assert.equal(store.detail(incident, "reviewer").reports.length, 1);
  store.update(fingerprint, { status: "fixed", fixed_version: "0.10.13" }, "reviewer");
  assert.deepEqual(writes, [store.metadataPath, store.metadataPath]);
  assert.deepEqual(readFileSync(store.path), before);
  const restored = new FailureReportStore(path);
  assert.equal(restored.state.audit.length, 2);
  assert.equal(restored.issue(fingerprint).status, "fixed");
  // Cached per-record encoding also prevents append from repeatedly
  // serializing the already persisted material for the capacity check.
  store.append(device, packet({ report_id: "review_second_report" }), incident);
  assert.equal(new FailureReportStore(path).state.reports.length, 2);
  const snapshot = JSON.parse(readFileSync(store.path, "utf8"));
  assert.deepEqual(snapshot.audit, []); assert.deepEqual(snapshot.issues, {});
  store.prune(new Map([[device.device_id, device]]), Date.now() + 36 * 86400000);
  const pruned = new FailureReportStore(path);
  assert.deepEqual(pruned.state.audit, []); assert.deepEqual(pruned.state.issues, {});
  assert.deepEqual(pruned.state.reports, []);
});

test("audit write failure refuses material and does not acknowledge an in-memory access", t => {
  const store = new FailureReportStore(directory(t));
  store.append(device, packet(), incident);
  const original = store.writePrivate.bind(store), count = store.state.audit.length;
  t.mock.method(store, "writePrivate", (destination, bytes) => {
    if (destination === store.metadataPath) throw new DiagnosticError(503, "diagnostic_storage_unavailable");
    return original(destination, bytes);
  });
  assert.throws(() => store.detail(incident, "reviewer"), /storage_unavailable/);
  assert.equal(store.state.audit.length, count);
  assert.equal(new FailureReportStore(dirname(store.path)).state.audit.length, count);
});

test("legacy single-file migration is durable and marked metadata loss/corruption fails closed", t => {
  const path = directory(t), store = new FailureReportStore(path);
  store.append(device, packet(), incident);
  store.detail(incident, "reviewer");
  const legacy = { ...store.state }; delete legacy.metadata_version;
  writeFileSync(store.path, JSON.stringify(legacy)); unlinkSync(store.metadataPath);
  const migrated = new FailureReportStore(path);
  assert.equal(migrated.healthy, true);
  assert.equal(migrated.state.audit.length, 1);
  assert.equal(migrated.state.metadata_version, 1);
  assert.equal(existsSync(migrated.metadataPath), true);
  const snapshot = readFileSync(migrated.path), metadata = readFileSync(migrated.metadataPath);
  for (const invalid of [null, "broken", JSON.stringify({ version: 1, audit: null, issues: {} })]) {
    if (invalid === null) unlinkSync(migrated.metadataPath);
    else writeFileSync(migrated.metadataPath, invalid);
    const failed = new FailureReportStore(path);
    assert.equal(failed.healthy, false);
    assert.throws(() => failed.detail(incident, "reviewer"), /storage_unavailable/);
    assert.deepEqual(readFileSync(migrated.path), snapshot);
    writeFileSync(migrated.metadataPath, metadata);
  }
  unlinkSync(migrated.metadataPath);
  assert.throws(() => migrated.detail(incident, "reviewer"), /storage_unavailable/);
  assert.equal(existsSync(migrated.metadataPath), false);
});

test("an orphan access ledger cannot turn a lost material snapshot into a fresh empty store", t => {
  const path = directory(t), store = new FailureReportStore(path);
  store.append(device, packet(), incident);
  store.detail(incident, "reviewer");
  const metadata = readFileSync(store.metadataPath);
  unlinkSync(store.path);
  const failed = new FailureReportStore(path);
  assert.equal(failed.healthy, false);
  assert.throws(() => failed.append(device, packet(), incident), /storage_unavailable/);
  assert.deepEqual(readFileSync(store.metadataPath), metadata);
  assert.equal(existsSync(store.path), false);
});

test("missing identity inventory with only metadata remaining rejects startup before creating files", t => {
  const path = directory(t), metadataPath = join(path, "failure-diagnostic-metadata.json");
  const bytes = JSON.stringify({ version: 1, audit: [], issues: {} });
  writeFileSync(metadataPath, bytes);
  assert.throws(() => new TelemetryStore(path, "review-missing-inventory-pepper"), /telemetry_storage_unavailable/);
  assert.equal(existsSync(join(path, "devices.json")), false);
  assert.equal(existsSync(join(path, "failure-diagnostic-reports.json")), false);
  assert.equal(readFileSync(metadataPath, "utf8"), bytes);
});

test("metadata migration interruption retains the old self-contained report and retries safely", t => {
  const path = directory(t), store = new FailureReportStore(path);
  store.append(device, packet(), incident);
  const legacy = { ...store.state }; delete legacy.metadata_version;
  writeFileSync(store.path, JSON.stringify(legacy)); unlinkSync(store.metadataPath);
  const original = FailureReportStore.prototype.writePrivate;
  const mock = t.mock.method(FailureReportStore.prototype, "writePrivate", function(destination, bytes) {
    if (destination === this.path) throw new DiagnosticError(503, "diagnostic_storage_unavailable");
    return original.call(this, destination, bytes);
  });
  assert.equal(new FailureReportStore(path).healthy, false);
  assert.deepEqual(JSON.parse(readFileSync(store.path, "utf8")), legacy);
  mock.mock.restore();
  const recovered = new FailureReportStore(path);
  assert.equal(recovered.healthy, true);
  assert.equal(recovered.append(device, packet({ confirmed_at: legacy.reports[0].confirmed_at }), incident).status, "received");
});

test("tombstone compaction preserves every credential and registration block in both row orders", t => {
  const path = directory(t), pepper = "review-fixture-pepper";
  let store = new TelemetryStore(path, pepper);
  t.after(() => clearTimeout(store.saveTimer));
  const registration = { hardware_claim: "review-hardware-claim", registration_secret: "review-registration-secret-fixture-123456789", platform: "windows", app_version: "0.10.12" };
  const credential = store.register(registration, null);
  const deletion = store.deleteBehaviorData(store.devices.get(credential.device_id), { request_id: "review_delete_request" });
  const full = store.deletionTombstones.find(row => row.token_hash === store.deletionTokenHash(credential.device_token));
  const minimal = { subject_hash: full.subject_hash, removal_reason: "deleted" };
  const other = { ...full, token_hash: "e".repeat(64), hardware_hash: "c".repeat(64), registration_secret_hash: "d".repeat(64) };
  const expired = { ...full, removal_reason: "expired" };
  const future = deletion.accepted_at + 36 * 86400000;
  for (const rows of [[minimal, full, other, expired], [expired, other, full, minimal]]) {
    store.deletionTombstones = rows;
    for (let i = 0; i < 3; i++) store.compactDeletionTombstones(future);
    assert.equal(store.removedCredentialReasons.has(full.token_hash), true);
    assert.equal(store.removedCredentialReasons.get(full.token_hash), "deleted");
    assert.equal(store.removedCredentialReasons.has(other.token_hash), true);
    assert.equal(store.registrationWasDeleted(full.hardware_hash, full.registration_secret_hash), true);
    assert.equal(store.registrationWasDeleted(other.hardware_hash, other.registration_secret_hash), true);
    clearTimeout(store.saveTimer); store.saveNow();
    store = new TelemetryStore(path, pepper);
    assert.equal(store.removedCredentialReasons.get(full.token_hash), "deleted");
    assert.equal(store.registrationWasDeleted(full.hardware_hash, full.registration_secret_hash), true);
    assert.equal(store.registrationWasDeleted(other.hardware_hash, other.registration_secret_hash), true);
    assert.throws(() => store.register(registration, null), /installation_data_deleted/);
  }
});

test("invalid internal clock input cannot remove the bounded grouping window", () => {
  const event = { ...incident, occurred_at: Date.now() - 40 * 86400000, app_version: "0.10.12" };
  for (const now of ["abc", NaN, Infinity, -1, "0"]) assert.deepEqual(failureGroups([event], { now, days: 30 }), []);
});

test("startup hardens existing report and metadata permissions", { skip: process.platform === "win32" }, t => {
  const path = directory(t), store = new TelemetryStore(path, "review-permission-pepper");
  t.after(() => clearTimeout(store.saveTimer));
  store.failureReports.append(device, packet(), incident);
  for (const file of [store.failureReports.path, store.failureReports.metadataPath]) chmodSync(file, 0o666);
  const restored = new TelemetryStore(path, "review-permission-pepper");
  t.after(() => clearTimeout(restored.saveTimer));
  for (const file of [restored.failureReports.path, restored.failureReports.metadataPath]) assert.equal(statSync(file).mode & 0o777, 0o600);
});

test("report body idle timeout closes a stalled unauthenticated upload", { timeout: 20000 }, async t => {
  const path = directory(t), environment = {
    PINVOU_TELEMETRY_DATA_DIR: path, PINVOU_TELEMETRY_DEVICE_PEPPER: "review-http-pepper-fixture",
    PINVOU_TELEMETRY_ENROLLMENT_TOKEN: "review-http-enrollment-fixture",
    PINVOU_STATS_ADMIN_PASSWORD: "review-http-admin-fixture",
  };
  const previous = Object.fromEntries(Object.keys(environment).map(key => [key, process.env[key]]));
  Object.assign(process.env, environment);
  const service = createTelemetryService({ regionResolver: { enabled: false, lookup: async () => null } });
  const server = http.createServer((req, res) => { void service.handleHttp(req, res, new URL(req.url, "http://localhost").pathname); });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  t.after(async () => {
    service.close(); server.closeAllConnections(); await new Promise(resolve => server.close(resolve));
    for (const [key, value] of Object.entries(previous)) { if (value === undefined) delete process.env[key]; else process.env[key] = value; }
  });
  const started = Date.now();
  const error = await new Promise(resolve => {
    const req = http.request({ host: "127.0.0.1", port: server.address().port,
      path: "/pinvou3/stats/api/diagnostics/report", method: "POST", headers: { "content-length": 1000 } });
    req.on("error", resolve); req.write("{");
  });
  assert.equal(error.code, "ECONNRESET");
  assert(Date.now() - started >= 14000 && Date.now() - started < 19500);
});
