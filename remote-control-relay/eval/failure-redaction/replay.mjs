// Replay frozen answers only against byte-identical packets; this is not a new blind review.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { copyFileSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

assert(process.argv[2] && process.argv[3], "usage: node replay.mjs <new-run> <frozen-results-directory>");
const work = resolve(process.argv[2]);
const frozen = resolve(process.argv[3]);
const baseline = JSON.parse(readFileSync(join(frozen, "results.json"), "utf8"));
const manifest = JSON.parse(readFileSync(join(work, "manifest.json"), "utf8"));
const hashFile = path => createHash("sha256").update(readFileSync(path)).digest("hex");
// Validate every group before copying any answers.
for (const [arm, entry] of Object.entries(manifest.arms)) {
  const original = baseline.arms[arm];
  assert(original && original.label === entry.label, `arm mismatch: ${arm}`);
  assert.equal(hashFile(entry.file), original.packet_sha256, `packet changed: ${arm}; request a new blind review`);
  assert.equal(hashFile(join(frozen, `answers-${entry.label}.json`)), original.answers_sha256, `frozen answers changed: ${arm}`);
}
for (const entry of Object.values(manifest.arms)) {
  const name = `answers-${entry.label}.json`;
  copyFileSync(join(frozen, name), join(work, name));
}
execFileSync(process.execPath, [fileURLToPath(new URL("./score.mjs", import.meta.url)), work], { stdio: "inherit" });
