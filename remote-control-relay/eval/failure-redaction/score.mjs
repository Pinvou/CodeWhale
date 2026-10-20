import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { gradeAnswer } from "./grading.js";
import { taxonomy } from "./cases.js";

assert(process.argv[2], "usage: node eval/failure-redaction/score.mjs <run-directory>");
const work = resolve(process.argv[2]);
const manifest = JSON.parse(readFileSync(join(work, "manifest.json"), "utf8"));
const hashFile = (path) => createHash("sha256").update(readFileSync(path)).digest("hex");
const result = {
  provenance: {
    ...manifest.provenance,
    grading_module_sha256: hashFile(new URL("./grading.js", import.meta.url)),
    score_module_sha256: hashFile(new URL("./score.mjs", import.meta.url)),
    manifest_sha256: hashFile(join(work, "manifest.json")),
    scoring_note:
      "Canary denominators count only distinct declared sensitive substrings present in the original case, including overlapping substrings; these are not independent secrets or a general privacy audit.",
  },
  case_count: manifest.cases.length,
  known_families: manifest.known_families,
  arms: {},
};
for (const [arm, entry] of Object.entries(manifest.arms)) {
  const packetData = JSON.parse(readFileSync(entry.file, "utf8"));
  // PowerShell may write UTF-8 BOM. Ignore it when parsing, never rewrite frozen bytes.
  const answers = JSON.parse(
    readFileSync(join(work, `answers-${entry.label}.json`), "utf8").replace(/^\uFEFF/, ""),
  );
  const rows = Array.isArray(answers) ? answers : answers.answers;
  assert.equal(rows.length, manifest.cases.length, `missing responses ${arm}`);
  assert.equal(new Set(rows.map((r) => r.packet_id)).size, rows.length);
  const scored = rows.map((answer) => {
    assert(Object.hasOwn(taxonomy, answer.root_cause), `invalid root code ${arm}`);
    assert(
      ["confirmed", "likely", "unknown"].includes(answer.confidence),
      `invalid confidence ${arm}`,
    );
    assert(
      Array.isArray(answer.evidence) &&
        answer.evidence.length >= 1 &&
        answer.evidence.length <= 3 &&
        answer.evidence.every((quote) => typeof quote === "string" && quote.length > 0),
      `invalid evidence ${arm}`,
    );
    const id = entry.packet_map[answer.packet_id];
    assert(id, `unknown packet ${answer.packet_id}`);
    const expected = manifest.cases.find((c) => c.id === id),
      packet = packetData.packets.find((p) => p.packet_id === answer.packet_id);
    const mechanical = manifest.mechanical[arm].find((r) => r.case_id === id);
    return {
      case_id: id,
      expected: expected.root,
      predicted: answer.root_cause,
      confidence: answer.confidence,
      ...gradeAnswer(expected, packet, answer),
      ...mechanical,
      sensitive_total: expected.sensitive.filter((value) =>
        `${expected.notes}\n${expected.log}`.includes(value),
      ).length,
    };
  });
  const known = scored.filter((r) => r.expected !== "unknown"),
    unknown = scored.filter((r) => r.expected === "unknown");
  result.arms[arm] = {
    label: entry.label,
    packet_sha256: hashFile(entry.file),
    answers_sha256: hashFile(join(work, `answers-${entry.label}.json`)),
    total: scored.length,
    known: known.length,
    known_correct: known.filter((r) => r.correct).length,
    known_supported_correct: known.filter((r) => r.supported_correct).length,
    unknown_abstained: unknown.filter((r) => r.predicted === "unknown").length,
    unknown_total: unknown.length,
    false_attribution: scored.filter((r) => r.predicted !== "unknown" && !r.correct).length,
    unsupported_attribution: scored.filter((r) => r.predicted !== "unknown" && !r.supported_correct)
      .length,
    all_proof_retained: known.filter((r) => r.all_proof_retained).length,
    proof_kept: known.reduce((n, r) => n + r.proof_kept, 0),
    proof_total: known.reduce((n, r) => n + r.proof_total, 0),
    linkage_preserved: known.filter((r) => r.linkage_preserved).length,
    sensitive_residual: scored.reduce((n, r) => n + r.sensitive_residual, 0),
    sensitive_total: scored.reduce((n, r) => n + r.sensitive_total, 0),
    rows: scored,
  };
}
writeFileSync(join(work, "results.json"), JSON.stringify(result, null, 2));
console.log(
  JSON.stringify(
    Object.fromEntries(
      Object.entries(result.arms).map(([arm, { rows, ...summary }]) => [arm, summary]),
    ),
    null,
    2,
  ),
);
