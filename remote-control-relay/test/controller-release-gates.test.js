"use strict";

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { buildReleaseSubprocessEnvironment, validateTrustedGh } from "../scripts/controller-release-gate.mjs";

const read = (relative) => readFileSync(new URL(`../${relative}`, import.meta.url), "utf8");
const runbook = read("../docs/Windows双端远程控制发布与回滚.md");
const checklist = read("../docs/Windows双端远程控制双机演练清单.md");

test("release docs distinguish immutable build eligibility from the runtime switch", () => {
  assert.ok(runbook.includes("仅构建值为 `1` 的 Windows 客户端"));
  assert.ok(runbook.includes("构建值为 `0` 的普通发布产物不能在运行时重新开启"));
  assert.ok(checklist.includes("构建开关 `PINVOU_DESKTOP_CONTROLLER_ENABLED=1`"));
  assert.ok(checklist.includes("构建值为 `0` 的产物不能在运行时重新开启"));
  assert.doesNotMatch(runbook, /必须在 A\/B\/Relay 三端精确设为/u);
  assert.doesNotMatch(checklist, /Windows A\/B 客户端默认开启/u);
});

test("runbook and checklist bind all 27 stable journeys without hidden capabilities", () => {
  for (let index = 1; index <= 27; index += 1) {
    const id = `RC-E2E-${String(index).padStart(3, "0")}`;
    assert.ok(checklist.includes(`\`${id}\``), `checklist must contain ${id}`);
  }
  assert.match(runbook, /RC-E2E-001.*RC-E2E-027/su);
  assert.doesNotMatch(checklist, /首版隐藏|blocked\(首版隐藏\)/u);
  assert.match(runbook, /不声称抵御恶意 Relay 的主动 MITM/u);
  assert.match(runbook, /受信认证控制面/u);
});

test("release gate command executes and accepts only the disabled exact flag", () => {
  const script = fileURLToPath(new URL("../scripts/controller-release-gate.mjs", import.meta.url));
  const disabled = spawnSync(process.execPath, [script], {
    encoding: "utf8", env: { ...process.env, PINVOU_DESKTOP_CONTROLLER_ENABLED: "0" },
  });
  assert.equal(disabled.status, 0, disabled.stderr);
  assert.deepEqual(JSON.parse(disabled.stdout), { ok: true, enabled: false, evidence_required: false });
  const malformed = spawnSync(process.execPath, [script], {
    encoding: "utf8", env: { ...process.env, PINVOU_DESKTOP_CONTROLLER_ENABLED: "true" },
  });
  assert.equal(malformed.status, 1);
  assert.equal(JSON.parse(malformed.stderr).code, "release_feature_flag_invalid");
  const missingEvidence = spawnSync(process.execPath, [script], {
    encoding: "utf8", env: { ...process.env, PINVOU_DESKTOP_CONTROLLER_ENABLED: "1", PINVOU_REMOTE_RELEASE_EVIDENCE: "" },
  });
  assert.equal(missingEvidence.status, 1);
  assert.equal(JSON.parse(missingEvidence.stderr).code, "release_evidence_configuration_missing");
});

test("release gate executes the real lifecycle sensitive-surface scan", () => {
  const result = spawnSync(process.execPath, [
    "--test", "--test-concurrency=1",
    fileURLToPath(new URL("./controller-sensitive-scan.test.js", import.meta.url)),
  ], { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
});

test("release gate passes a purpose-specific environment and never forwards secret material", () => {
  const source = {
    PATH: "safe-path",
    PINVOU_RELEASE_COMMIT_SHA: "a".repeat(40),
    PINVOU_REMOTE_TELEMETRY_SNAPSHOT_URL: "https://telemetry.example.invalid/snapshot",
    PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE: "/protected/token-file",
    PINVOU_REMOTE_ENROLLMENT_HMAC_KEY: "must-not-leak",
    PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64: "must-not-leak",
    PINVOU_STATS_ADMIN_PASSWORD: "must-not-leak",
    PINVOU_E2E_ATTESTATION_PRIVATE_KEY: "must-not-leak",
  };
  const telemetry = buildReleaseSubprocessEnvironment(source, "telemetry");
  assert.deepEqual(Object.keys(telemetry).sort(), [
    "PATH", "PINVOU_RELEASE_COMMIT_SHA", "PINVOU_REMOTE_TELEMETRY_ADMIN_TOKEN_FILE", "PINVOU_REMOTE_TELEMETRY_SNAPSHOT_URL",
  ]);
  for (const key of Object.keys(telemetry)) assert.doesNotMatch(key, /(?:PRIVATE_KEY|HMAC_KEY|PASSWORD)$/u);
  assert.deepEqual(buildReleaseSubprocessEnvironment(source, "test"), { PATH: "safe-path" });
  const gateSource = read("scripts/controller-release-gate.mjs");
  assert.doesNotMatch(gateSource, /Object\.fromEntries\(enabledEntries\)/u);
  for (const purpose of ["telemetry", "rollback"]) {
    assert.ok(gateSource.includes(`buildReleaseSubprocessEnvironment(env, "${purpose}")`));
  }
  assert.throws(() => buildReleaseSubprocessEnvironment(source, "unknown"), /release_subprocess_environment_purpose_invalid/u);
});

test("sensitive Rust evidence names existing exact tests and rejects zero matches", () => {
  for (const [module, name] of [
    ["relay_runtime", "log_allowlist_rejects_identifier_shaped_values_and_secrets"],
    ["relay_runtime", "real_remote_failure_projections_do_not_persist_any_sensitive_category"],
    ["resume", "resume_debug_redacts_current_and_retired_material"],
  ]) {
    const source = read(`../pinvou3-app/src-tauri/src/features/remote_control/controller/${module}.rs`);
    assert.match(source, /#\[cfg\(test\)\]\s*mod tests\s*\{/);
    assert.match(source, new RegExp(`#\\[test\\]\\s*fn ${name}\\(`));
    assert.ok(runbook.includes(`features::remote_control::controller::${module}::tests::${name} -- --exact --test-threads=1`));
  }
  assert.ok(!runbook.includes("cargo test ... controller::telemetry"));
  assert.ok(runbook.includes("1 passed; 0 failed"));
  assert.ok(runbook.includes("零匹配视为失败"));
  assert.ok(runbook.includes("controller-sensitive-scan.test.js"));
});

test("the trusted-Relay residual risk stays declared", () => {
  assert.ok(runbook.includes("不声称抵御恶意 Relay 的主动 MITM"));
  assert.ok(runbook.includes("受信 Relay"));
});

test("release gate ignores PATH injection and requires the configured absolute gh path", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-fake-gh-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const fake = join(root, process.platform === "win32" ? "gh.cmd" : "gh");
  await writeFile(fake, process.platform === "win32" ? "@exit /b 0\r\n" : "#!/bin/sh\nexit 0\n");
  await assert.rejects(() => validateTrustedGh({ PATH: root, Path: root }), /release_gh_tool_configuration_invalid/u);
  const fixed = join(root, "protected-gh.exe");
  await writeFile(fixed, "configured-gh-fixture");
  assert.equal(await validateTrustedGh({ PATH: root, PINVOU_TRUSTED_GH_PATH: fixed }), fixed);
  const gateSource = read("scripts/controller-release-gate.mjs");
  const supplyChainSource = read("scripts/verify-installer-supply-chain.mjs");
  assert.doesNotMatch(gateSource, /run\(["']gh["']/u);
  assert.doesNotMatch(supplyChainSource, /spawn\(["']gh["']/u);
  assert.doesNotMatch(gateSource, /PINVOU_TRUSTED_GH_(?:SHA256|PROVENANCE|SIGNER)/u);
  assert.match(supplyChainSource, /isAbsolute\(ghBinary/u);
});

test("production preflight requires implemented self-hosted claims, feature rollback, TOFU-off and telemetry protection", () => {
  const preflight = read("scripts/deploy-controller-preflight.mjs");
  assert.match(preflight, /NODE_ENV: "production"/u);
  assert.match(preflight, /PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT: "self_hosted"/u);
  assert.match(preflight, /official_controller_claim_dispatcher_unavailable/u);
  assert.match(preflight, /--expect-feature-off/u);
  assert.match(preflight, /PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU: "false"/u);
  assert.match(preflight, /PINVOU_TELEMETRY_ENROLLMENT_TOKEN/u);
  assert.match(preflight, /PINVOU_STATS_ADMIN_PASSWORD/u);
});
