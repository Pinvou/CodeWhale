import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { FailureReportStore } from "../../failure-diagnostics.js";
import { makeCases, taxonomy } from "./cases.js";
import { fieldRedactor } from "./candidate.js";
import { nativeRedact, rustPath } from "./native-runner.mjs";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const directory = resolve(process.argv[2] || join(root, ".cache/failure-redaction-comparison"));
mkdirSync(directory, { recursive: true });
const work = mkdtempSync(join(directory, "run-"));
const hash = (value) => createHash("sha256").update(value).digest("hex");
const { cases, observations, known_families } = await makeCases(
  join(work, "does-not-exist.fixture"),
);

// Execute the exact production module; no JavaScript port of the client.
const nativeResults = nativeRedact(
  cases.flatMap((c) =>
    ["notes", "log"].map((section) => ({
      id: c.id,
      section,
      scope: `fixture-only-scope:${c.id}`,
      text: c[section],
    })),
  ),
  work,
);
const native = new Map(
  cases.map((c) => [
    c.id,
    { notes: nativeResults.get(`${c.id}:notes`), log: nativeResults.get(`${c.id}:log`) },
  ]),
);
const serverStore = new FailureReportStore(work);
// Synthetic scope only: production stores generate and privately persist their own key.
serverStore.state.redaction_key = hash("offline-fixture-private-scope-key");
function productionServer(c, material) {
  const period = "period_fixture_comparison",
    id = `incident_fixture_${c.id}`,
    device = {
      device_id: `device_fixture_${c.id}`,
      behavior_collection: { status: "active", collection_period_id: period },
    };
  serverStore.append(
    device,
    {
      report_id: `report_fixture_${c.id}`,
      incident_id: id,
      collection_period_id: period,
      notice_version: "failure-diagnostics-v1",
      confirmed_at: Date.now(),
      notes: material.notes,
      attachments: [{ kind: "text", content: material.log }],
    },
    {
      device_id: device.device_id,
      diagnostic_id: id,
      collection_period_id: period,
      diagnostic_code: "unknown",
      diagnostic_component: "app",
      stage: "unknown",
    },
  );
  const report = serverStore.state.reports.at(-1);
  return { notes: report.notes, log: report.attachments[0].content };
}
const arms = { raw: [], production: [], candidate: [] };
const codebook = {
  classification: taxonomy,
  instructions:
    "For each packet select one root-cause code or unknown. Do not guess beyond evidence. Quote only literal non-sensitive fault facts from the packet, not simulated private values. Distinguish the observed symptom from an attributed cause. Confidence: confirmed/likely/unknown. Give a short next verification step.",
};
for (const c of cases) {
  arms.raw.push({ id: c.id, notes: c.notes, log: c.log });
  arms.production.push({ id: c.id, ...productionServer(c, native.get(c.id)) });
  const redact = fieldRedactor(`offline-report-salt:${c.id}`);
  const once = { notes: redact(c.notes), log: redact(c.log) };
  const twice = { notes: redact(once.notes), log: redact(once.log) };
  assert.deepEqual(twice, once, `prototype redaction must be idempotent for ${c.id}`);
  arms.candidate.push({ id: c.id, ...twice });
}
const manifest = {
  version: 1,
  cases,
  known_families,
  unknown_controls: cases.filter((c) => c.root === "unknown").length,
  observations,
  provenance: {
    production_rust_file:
      "pinvou3-app/src-tauri/src/features/behavior_telemetry/diagnostic_redaction.rs",
    rust_module_sha256: hash(readFileSync(rustPath)),
    rust_dependency_lock_sha256: hash(readFileSync(join(work, "native-runner/Cargo.lock"))),
    redaction_server_module_sha256: hash(
      readFileSync(new URL("../../diagnostic-redaction.js", import.meta.url)),
    ),
    server_module_sha256: hash(
      readFileSync(join(root, "remote-control-relay/failure-diagnostics.js")),
    ),
    candidate_module_sha256: hash(readFileSync(new URL("./candidate.js", import.meta.url))),
    runtime: {
      node: process.version,
      rustc: execFileSync("rustc", ["--version"], { encoding: "utf8" }).trim(),
    },
    note: "Detailed text/report comparison only; automatic diagnostic metadata and screenshots are held outside this experiment. Local HTTP/exit/missing-file observations are executed; proxy attribution, filesystem access denial and dual-device journals are explicit synthetic adapter fixtures.",
  },
  arms: {},
  mechanical: {},
};
for (const [arm, records] of Object.entries(arms)) {
  const label = hash(`arm-label|${arm}`).slice(0, 8),
    packetMap = {};
  const packets = records
    .map((record) => {
      const packetId = hash(`packet|${arm}|${record.id}`).slice(0, 16);
      packetMap[packetId] = record.id;
      return { packet_id: packetId, notes: record.notes, log: record.log };
    })
    .sort((a, b) => hash(`shuffle|${a.packet_id}`).localeCompare(hash(`shuffle|${b.packet_id}`)));
  const packetFile = join(work, `set-${label}.json`);
  writeFileSync(packetFile, JSON.stringify({ codebook, packets }, null, 2));
  manifest.arms[arm] = { label, file: packetFile, packet_map: packetMap };
  manifest.mechanical[arm] = records.map((record) => {
    const c = cases.find((c) => c.id === record.id),
      text = `${record.notes}\n${record.log}`;
    const matchingAliases = text.match(/\[id:[a-f0-9]{12}(?:[a-f0-9]{12})?\]/g) || [];
    const linked =
      c.linkage.length === 0
        ? null
        : arm === "raw"
          ? c.linkage.every((value) => text.split(value).length - 1 >= 2)
          : new Set(
              matchingAliases.filter(
                (value) => matchingAliases.filter((v) => v === value).length >= 2,
              ),
            ).size === c.linkage.length;
    return {
      case_id: c.id,
      root: c.root,
      layout: c.layout,
      proof_total: c.proof.length,
      proof_kept: c.proof.filter((value) => text.includes(value)).length,
      all_proof_retained:
        c.proof.length === 0 ? null : c.proof.every((value) => text.includes(value)),
      linkage_preserved: linked,
      sensitive_total: c.sensitive.filter((value) => `${c.notes}\n${c.log}`.includes(value)).length,
      sensitive_residual: c.sensitive.filter((value) => text.includes(value)).length,
      bytes: Buffer.byteLength(text),
    };
  });
}
writeFileSync(join(work, "manifest.json"), JSON.stringify(manifest, null, 2));
writeFileSync(join(work, "mechanical.json"), JSON.stringify(manifest.mechanical, null, 2));
writeFileSync(
  join(directory, "latest.json"),
  JSON.stringify({ work, manifest: join(work, "manifest.json") }, null, 2),
);
console.log(
  JSON.stringify(
    {
      work,
      cases: cases.length,
      known_families,
      unknown_controls: manifest.unknown_controls,
      packets: Object.values(manifest.arms).map(({ label, file }) => ({ label, file })),
      mechanical: Object.fromEntries(
        Object.entries(manifest.mechanical).map(([arm, rows]) => [
          arm,
          {
            complete_evidence: rows.filter((r) => r.all_proof_retained).length,
            known: rows.filter((r) => r.all_proof_retained !== null).length,
            linked: rows.filter((r) => r.linkage_preserved).length,
            sensitive_residual: rows.reduce((n, r) => n + r.sensitive_residual, 0),
          },
        ]),
      ),
    },
    null,
    2,
  ),
);
