import { createHash, createHmac, randomBytes } from "node:crypto";
import { chmodSync, existsSync, lstatSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { diagnosticRedactor } from "./diagnostic-redaction.js";

const DAY = 86400000;
// Leave four MiB for the bounded issue registry and mandatory access audit so
// a full material quota can never prevent reading/triaging existing reports.
const REPORT_MATERIAL_BYTES = 28 * 1024 * 1024;
export const DIAGNOSTIC_NOTICE = "failure-diagnostics-v1";
export const DIAGNOSTIC_CODES = Object.freeze([
  "unknown", "permission_denied", "permission_required", "permission_changed", "authentication_failed",
  "configuration_missing", "network_unavailable", "connection_reset", "dns_failed", "tls_failed",
  "request_timeout", "rate_limited", "quota_exhausted", "context_length_exceeded", "model_not_found",
  "provider_unavailable", "stream_unexpected_eof", "response_invalid", "tool_failed", "process_failed",
  "unsupported", "validation_failed", "internal_error", "shutdown_unconfirmed", "capability_stale",
  "remote_disconnected", "remote_result_unknown", "artifact_missing", "attachment_failed", "user_reported_quality",
]);
export const DIAGNOSTIC_COMPONENTS = Object.freeze(["app", "assistant", "acp", "model", "tool", "remote_control", "voice", "configuration", "unknown"]);
export const DIAGNOSTIC_STAGES = Object.freeze(["submission", "queue", "permission", "recording", "recognition", "model", "tool", "output", "shutdown", "unknown"]);
export const DIAGNOSTIC_SITES = Object.freeze(["lifecycle", "assistant_error", "acp_error", "tool_result", "remote_admission", "remote_terminal", "recovery"]);

export class DiagnosticError extends Error {
  constructor(status, code) { super(code); this.status = status; this.code = code; }
}
const fail = (code = "invalid_diagnostic_report", status = 400) => { throw new DiagnosticError(status, code); };
const hash = value => createHash("sha256").update(value).digest("hex");
function reportDigest(report) {
  return hash(JSON.stringify([report.report_id,report.incident_id,report.incident_period_id ?? report.collection_period_id,
    report.notice_version,report.confirmed_at,report.notes,report.attachments.map(a=>[a.kind,a.content])]));
}
export const diagnosticFingerprint = event => hash([event.diagnostic_component, event.stage || "unknown", event.diagnostic_code].join("|"));

// Only for comparing retries of pre-field-redaction stored reports. Never stores material.
function legacyRedactDiagnosticText(value) {
  return String(value || "")
    .replace(/\b(?:Bearer\s+)[^\s"']+/gi, "Bearer [redacted]")
    .replace(/\b(?:sk-[A-Za-z0-9_-]{8,}|gh[pousr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+)/g, "[credential]")
    .replace(/(["']?(?:api[_-]?key|password|passwd|token|secret|authorization|cookie)["']?\s*[=:]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)/gi, "$1[redacted]")
    .replace(/https?:\/\/[^\s<>"']+/gi, "[url]")
    .replace(/\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b/gi, "[email]")
    .replace(/(?:[A-Za-z]:[\\/]|\\\\)[^\s<>"']+/g, "[path]")
    .replace(/(?:~\/|\/(?:Users|home|tmp|var|private|opt|mnt)\/)[^\s<>"']+/g, "[path]")
    .replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/g, "");
}
export function redactDiagnosticText(value, scope, options) {
  return diagnosticRedactor(scope, options)(value);
}
function boundedText(value, max, bytes = false, redact = redactDiagnosticText) {
  const cleaned = redact(value);
  if ((bytes ? Buffer.byteLength(cleaned) : cleaned.length) <= max) return cleaned;
  const suffix="\n[truncated]";
  let count=0, result="";
  for (const char of cleaned) { const size=bytes?Buffer.byteLength(char):char.length; if(count+size>max-suffix.length)break;result+=char;count+=size; }
  return result+suffix;
}

export function failureGroups(events, filters = {}, minimumSubjects = 1) {
  const now = Number.isSafeInteger(filters.now) && filters.now >= 0 ? filters.now : Date.now();
  const days = [1, 7, 30].includes(Number(filters.days)) ? Number(filters.days) : 7;
  const groups = new Map();
  const seen = new Set();
  for (const event of events) {
    if (!event.diagnostic_id || !DIAGNOSTIC_CODES.includes(event.diagnostic_code)) continue;
    if (Number(event.occurred_at) < now - days * DAY || Number(event.occurred_at) > now) continue;
    if (["app_version", "platform", "mode", "provider_key", "diagnostic_code"].some(key => filters[key] && (event[key] ?? event.diagnostic_environment?.[key]) !== filters[key])) continue;
    const unique = `${event.device_id}:${event.diagnostic_id}`;
    if (seen.has(unique)) continue;
    seen.add(unique);
    const fingerprint = diagnosticFingerprint(event);
    const group = groups.get(fingerprint) || { fingerprint, code: event.diagnostic_code,
      component: event.diagnostic_component, stage: event.stage || "unknown", count: 0, sampled_details: 0,
      subjects: new Set(), versions: new Map(), versionSubjects: new Map(), first_seen: event.occurred_at, last_seen: event.occurred_at };
    group.count++;
    if (event.diagnostic_sampled !== false) group.sampled_details++;
    group.subjects.add(event.account_subject_id || event.device_id);
    group.versions.set(event.app_version, (group.versions.get(event.app_version) || 0) + 1);
    if (!group.versionSubjects.has(event.app_version)) group.versionSubjects.set(event.app_version, new Set());
    group.versionSubjects.get(event.app_version).add(event.account_subject_id || event.device_id);
    group.first_seen = Math.min(group.first_seen, event.occurred_at);
    group.last_seen = Math.max(group.last_seen, event.occurred_at);
    groups.set(fingerprint, group);
  }
  return [...groups.values()].filter(g => g.subjects.size >= minimumSubjects)
    .map(({ subjects, versions, versionSubjects, ...g }) => ({ ...g, installations: subjects.size,
      versions: Object.fromEntries([...versions].filter(([version])=>versionSubjects.get(version).size>=minimumSubjects)) }))
    .sort((a, b) => b.count - a.count || b.last_seen - a.last_seen).slice(0, 200);
}

export function failureSamples(events, fingerprint, filters = {}) {
  const days = [1, 7, 30].includes(Number(filters.days)) ? Number(filters.days) : 7;
  const now = Date.now();
  const seen = new Set();
  return events.filter(event => {
    const id = `${event.device_id}:${event.diagnostic_id}`;
    if (!event.diagnostic_id || seen.has(id) || diagnosticFingerprint(event) !== fingerprint
      || event.occurred_at < now - days * DAY || event.occurred_at > now
      || ["app_version", "platform", "mode", "provider_key", "diagnostic_code"].some(key => filters[key] && (event[key] ?? event.diagnostic_environment?.[key]) !== filters[key])) return false;
    seen.add(id); return true;
  }).sort((a, b) => Number(b.diagnostic_sampled !== false) - Number(a.diagnostic_sampled !== false) || b.occurred_at - a.occurred_at).slice(0, 50);
}

function validateReport(input, redact = redactDiagnosticText) {
  const allowed = ["report_id", "incident_id", "collection_period_id", "incident_period_id", "notice_version", "confirmed_at", "notes", "attachments"];
  if (!input || typeof input !== "object" || Array.isArray(input) || Object.keys(input).some(k => !allowed.includes(k))) fail();
  if (![input.report_id, input.incident_id, input.collection_period_id].every(v => typeof v === "string" && /^[A-Za-z0-9_-]{12,128}$/.test(v))) fail();
  if (input.incident_period_id !== undefined && (typeof input.incident_period_id !== "string" || !/^[A-Za-z0-9_-]{12,128}$/.test(input.incident_period_id))) fail();
  if (input.notice_version !== DIAGNOSTIC_NOTICE || !Number.isSafeInteger(input.confirmed_at)
    || input.confirmed_at > Date.now() + 300000 || input.confirmed_at < Date.now() - 14 * DAY) fail("diagnostic_consent_invalid");
  if (typeof input.notes !== "string" || input.notes.length > 6000 || !Array.isArray(input.attachments) || input.attachments.length > 3) fail();
  const attachments = input.attachments.map(attachment => {
    if (!attachment || Object.keys(attachment).some(k => !["kind", "content"].includes(k)) || typeof attachment.content !== "string") fail();
    if (attachment.kind === "text") {
      if (Buffer.byteLength(attachment.content) > 65536) fail("diagnostic_attachment_too_large", 413);
      return { kind: "text", content: boundedText(attachment.content,65536,true,redact) };
    }
    if (!["image/png", "image/jpeg"].includes(attachment.kind) || !/^[A-Za-z0-9+/]*={0,2}$/.test(attachment.content)) fail();
    const bytes = Buffer.from(attachment.content, "base64");
    if (bytes.toString("base64") !== attachment.content || bytes.length > 512 * 1024) fail("diagnostic_attachment_too_large", 413);
    const valid = attachment.kind === "image/png" ? bytes.subarray(0, 8).equals(Buffer.from("89504e470d0a1a0a", "hex"))
      : bytes.length >= 4 && bytes[0] === 255 && bytes[1] === 216 && bytes[2] === 255 && bytes.at(-2) === 255 && bytes.at(-1) === 217;
    if (!valid) fail("diagnostic_image_invalid");
    return { kind: attachment.kind, content: attachment.content };
  });
  return { ...input, notes: boundedText(input.notes,6000,false,redact), attachments };
}

/** Detailed, explicitly confirmed material is separate from the metrics log. */
export class FailureReportStore {
  constructor(directory) {
    this.path = join(directory, "failure-diagnostic-reports.json");
    this.metadataPath = join(directory, "failure-diagnostic-metadata.json");
    this.reportEncoding = new WeakMap();
    this.materialBytes = 2;
    this.state = { reports: [], issues: {}, audit: [], redaction_key: randomBytes(32).toString("hex") };
    this.healthy = true;
    // A surviving access ledger is evidence of an initialized/interrupted
    // store, not permission to replace a missing material snapshot with empty
    // data. Preserve both the orphan and the failure for operator recovery.
    if (!existsSync(this.path) && existsSync(this.metadataPath)) this.healthy = false;
    if (existsSync(this.path)) {
      try {
        const stat = lstatSync(this.path);
        if (!stat.isFile() || stat.isSymbolicLink() || stat.size > 32 * 1024 * 1024) throw new Error("unsafe");
        this.state = JSON.parse(readFileSync(this.path, "utf8"));
        if (!this.state || Object.keys(this.state).some(k => !["reports", "audit", "issues", "redaction_key", "metadata_version"].includes(k))
          || !Array.isArray(this.state.reports) || this.state.reports.length > 1000
          || !Array.isArray(this.state.audit) || this.state.audit.length > 2000
          || !this.state.issues || typeof this.state.issues !== "object" || Array.isArray(this.state.issues)
          || Object.keys(this.state.issues).length > 2000) throw new Error("corrupt");
        let migrated = this.state.metadata_version === undefined;
        if (this.state.metadata_version !== undefined) {
          if (this.state.metadata_version !== 1) throw new Error("corrupt");
          const metadataStat = lstatSync(this.metadataPath);
          if (!metadataStat.isFile() || metadataStat.isSymbolicLink() || metadataStat.size > 4 * 1024 * 1024) throw new Error("unsafe");
          const metadata = JSON.parse(readFileSync(this.metadataPath, "utf8"));
          if (!metadata || metadata.version !== 1 || Object.keys(metadata).some(k => !["version", "audit", "issues"].includes(k))) throw new Error("corrupt");
          this.state.audit = metadata.audit;
          this.state.issues = metadata.issues;
          if (!Array.isArray(this.state.audit) || this.state.audit.length > 2000
            || !this.state.issues || typeof this.state.issues !== "object" || Array.isArray(this.state.issues)
            || Object.keys(this.state.issues).length > 2000) throw new Error("corrupt");
        }
        if (this.state.redaction_key === undefined) {
          this.state.redaction_key = randomBytes(32).toString("hex");
          migrated = true;
        }
        if (typeof this.state.redaction_key !== "string" || !/^[a-f0-9]{64}$/.test(this.state.redaction_key)) throw new Error("corrupt");
        for (const stored of this.state.reports) {
          const {device_id, fingerprint, received_at, digest, redaction_version, ...material} = stored;
          if (typeof device_id !== "string" || !/^[a-f0-9]{64}$/.test(fingerprint) || !/^[a-f0-9]{64}$/.test(digest)
            || !Number.isSafeInteger(received_at) || !Number.isSafeInteger(material.confirmed_at)) throw new Error("corrupt");
          if (redaction_version !== undefined && ![1,2].includes(redaction_version)) throw new Error("corrupt");
          const cleaned = validateReport({...material,confirmed_at:Date.now()}, diagnosticRedactor(this.redactionScope(device_id, material.report_id), {stored:redaction_version !== undefined}));
          if (redaction_version === undefined) {
            // Retain the legacy digest for retry compatibility, but never retain its
            // weaker material. Version 1 means migrated material with a legacy digest.
            stored.notes = cleaned.notes;
            stored.attachments = cleaned.attachments;
            stored.redaction_version = 1;
            migrated = true;
          }
        }
        for (const [key, issue] of Object.entries(this.state.issues)) {
          if (!/^[a-f0-9]{64}$/.test(key) || !issue || Object.keys(issue).some(k=>!["status","fixed_version","updated_at"].includes(k))
            || !["open","reproduced","fixed"].includes(issue.status) || typeof issue.fixed_version !== "string" || !/^[A-Za-z0-9.+_-]{0,48}$/.test(issue.fixed_version)
            || !Number.isSafeInteger(issue.updated_at)) throw new Error("corrupt");
        }
        for (const entry of this.state.audit) {
          if (!entry || Object.keys(entry).some(k=>!["at","action","target","actor"].includes(k)) || !Number.isSafeInteger(entry.at)
            || !["samples_read","incident_read","issue_update"].includes(entry.action)
            || ![entry.target,entry.actor].every(s=>typeof s==="string" && s.length<=128)) throw new Error("corrupt");
        }
        if (migrated) this.save(this.state);
        else this.materialBytes = this.encodedReports(this.state.reports).size;
      } catch { this.healthy = false; }
    }
  }
  ensureHealthy() {
    if (!this.healthy) fail("diagnostic_storage_unavailable", 503);
    if (this.state.metadata_version === 1) {
      try {
        const stat = lstatSync(this.metadataPath);
        if (!stat.isFile() || stat.isSymbolicLink()) throw new Error("unsafe");
      } catch { this.healthy = false; fail("diagnostic_storage_unavailable", 503); }
    }
  }
  redactionScope(device, report) {
    return createHmac("sha256",this.state.redaction_key).update(JSON.stringify([device,report])).digest("hex");
  }
  writePrivate(path, bytes) {
    if (existsSync(path) && (!lstatSync(path).isFile() || lstatSync(path).isSymbolicLink())) fail("diagnostic_storage_unavailable", 503);
    const temporary = `${path}.${randomBytes(8).toString("hex")}.tmp`;
    try {
      writeFileSync(temporary, bytes, { mode: 0o600, flag: "wx" });
      renameSync(temporary, path);
      chmodSync(path, 0o600);
    } catch { try { unlinkSync(temporary); } catch { /* original storage failure */ } fail("diagnostic_storage_unavailable", 503); }
  }
  encodedReports(reports) {
    let size = 2 + Math.max(0, reports.length - 1);
    const records = reports.map(report => {
      let encoded = this.reportEncoding.get(report);
      if (encoded === undefined) { encoded = JSON.stringify(report); this.reportEncoding.set(report, encoded); }
      size += Buffer.byteLength(encoded);
      return encoded;
    });
    return { records, size };
  }
  metadataBytes(next) {
    const bytes = JSON.stringify({ version: 1, audit: next.audit, issues: next.issues });
    if (Buffer.byteLength(bytes) > 4 * 1024 * 1024) fail("diagnostic_storage_capacity", 503);
    return bytes;
  }
  save(next) {
    this.ensureHealthy();
    const encoded = this.encodedReports(next.reports);
    if (encoded.size > REPORT_MATERIAL_BYTES) fail("diagnostic_report_capacity", 503);
    const { reports, audit, issues, ...control } = next; void reports; void audit; void issues;
    // Control data has exactly one authoritative on-disk copy. Leaving stale
    // audit/issues in the material snapshot would retain them past pruning.
    const bytes = `${JSON.stringify({ ...control, audit: [], issues: {}, metadata_version: 1 }).slice(0, -1)},"reports":[${encoded.records.join(",")}]}`;
    if (Buffer.byteLength(bytes) > 32 * 1024 * 1024) fail("diagnostic_storage_capacity", 503);
    // Publish metadata before its snapshot marker. An interrupted migration
    // retains the old self-contained snapshot, while a marked snapshot never
    // silently replaces a missing or corrupt access ledger with an empty one.
    this.writePrivate(this.metadataPath, this.metadataBytes(next));
    this.writePrivate(this.path, bytes);
    this.state = { ...next, metadata_version: 1 };
    this.materialBytes = encoded.size;
  }
  saveMetadata(next) {
    this.ensureHealthy();
    if (this.state.metadata_version !== 1) { this.save(next); return; }
    // Mandatory audit remains synchronous and must succeed before disclosing
    // material. Only this small bounded ledger is rewritten on read/triage.
    this.writePrivate(this.metadataPath, this.metadataBytes(next));
    this.state = next;
  }
  prune(devices, now = Date.now()) {
    this.ensureHealthy();
    const reports = this.state.reports.filter(r => devices.has(r.device_id) && r.received_at >= now - 14 * DAY);
    const audit = this.state.audit.filter(r => r.at >= now - 14 * DAY).slice(-2000);
    const issues = Object.fromEntries(Object.entries(this.state.issues).filter(([, issue]) => issue.updated_at >= now - 35 * DAY));
    if (reports.length !== this.state.reports.length) this.save({ ...this.state, reports, issues, audit });
    else if (audit.length !== this.state.audit.length || Object.keys(issues).length !== Object.keys(this.state.issues).length) this.saveMetadata({ ...this.state, issues, audit });
  }
  append(device, input, incident, deliveryPeriod = input?.collection_period_id) {
    this.ensureHealthy();
    const report = validateReport(input, diagnosticRedactor(this.redactionScope(device.device_id,input?.report_id)));
    if (device.behavior_collection?.status !== "active" || deliveryPeriod !== device.behavior_collection.collection_period_id) fail("collection_inactive", 409);
    const prior = this.state.reports.find(r => r.device_id === device.device_id && r.report_id === report.report_id);
    if (prior) {
      const currentMatches = prior.redaction_version === 2 && prior.digest === reportDigest(report);
      const legacyMatches = prior.redaction_version === 1 && prior.digest === reportDigest(validateReport(input,legacyRedactDiagnosticText));
      if (!currentMatches && !legacyMatches) fail("diagnostic_report_conflict", 409);
      return { report_id: prior.report_id, status: "received" };
    }
    if (incident?.device_id === device.device_id && incident.event_id === report.incident_id && !incident.diagnostic_id) fail("diagnostic_incident_metadata_missing", 422);
    if (!incident || incident.device_id !== device.device_id || incident.diagnostic_id !== report.incident_id) fail("diagnostic_incident_not_received", 409);
    if ((incident.diagnostic_origin_period_id ?? incident.collection_period_id) !== (report.incident_period_id ?? report.collection_period_id)) fail("diagnostic_incident_period_mismatch", 409);
    const today = Date.now() - DAY;
    if (this.state.reports.filter(r => r.device_id === device.device_id && r.received_at >= today).length >= 10
      || this.state.reports.length >= 1000) fail("diagnostic_report_rate_limited", 429);
    const stored = { ...report, device_id: device.device_id, fingerprint: diagnosticFingerprint(incident),
      received_at: Date.now(), digest: reportDigest(report), redaction_version: 2 };
    const reports=[...this.state.reports, stored];
    const encoded = JSON.stringify(stored);
    if (this.materialBytes + Number(this.state.reports.length > 0) + Buffer.byteLength(encoded) > REPORT_MATERIAL_BYTES) fail("diagnostic_report_capacity",503);
    this.reportEncoding.set(stored, encoded);
    this.save({ ...this.state, reports });
    return { report_id: report.report_id, status: "received" };
  }
  audit(action, target, actor) {
    this.saveMetadata({ ...this.state, audit: [...this.state.audit, { at: Date.now(), action, target, actor }].slice(-2000) });
  }
  issue(fingerprint) { this.ensureHealthy(); return this.state.issues[fingerprint] || { status: "open", fixed_version: "" }; }
  update(fingerprint, value, actor) {
    if (!/^[a-f0-9]{64}$/.test(fingerprint) || !value || Object.keys(value).some(k => !["status", "fixed_version"].includes(k))
      || !["open", "reproduced", "fixed"].includes(value.status) || typeof value.fixed_version !== "string"
      || !/^[A-Za-z0-9.+_-]{0,48}$/.test(value.fixed_version) || (value.status === "fixed" && !value.fixed_version)) fail();
    const issue = { ...value, updated_at: Date.now() };
    const issues = Object.fromEntries(Object.entries({ ...this.state.issues, [fingerprint]: issue }).sort((a,b)=>b[1].updated_at-a[1].updated_at).slice(0,2000));
    this.saveMetadata({ ...this.state, issues, audit:[...this.state.audit,{at:Date.now(),action:"issue_update",target:fingerprint,actor}].slice(-2000) });
    return issue;
  }
  detail(incident, actor) {
    this.ensureHealthy();
    this.audit("incident_read", incident.diagnostic_id, actor);
    const { device_id, identity_id, account_subject_id, session_id, turn_id, ...safe } = incident;
    void device_id; void identity_id; void account_subject_id; void session_id; void turn_id;
    return { incident: safe, reports: this.state.reports.filter(r => r.incident_id === incident.diagnostic_id && r.device_id === incident.device_id)
      .map(({ device_id, digest, ...report }) => { void device_id; void digest; return report; }) };
  }
}
