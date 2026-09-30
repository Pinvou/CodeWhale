#!/usr/bin/env node
import crypto from "node:crypto";
import { spawn } from "node:child_process";
import { readFile, writeFile } from "node:fs/promises";
import { basename, isAbsolute, resolve } from "node:path";
import { pathToFileURL } from "node:url";

function fail(code) { const error = new Error(code); error.code = code; throw error; }
function sha256(value) { return crypto.createHash("sha256").update(value).digest("hex"); }
function arg(name) { const index = process.argv.indexOf(name); return index < 0 ? undefined : process.argv[index + 1]; }
function cleanEnvironment() {
  const result = {};
  for (const key of ["PATH", "Path", "PATHEXT", "SystemRoot", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "HOME"]) {
    if (process.env[key]) result[key] = process.env[key];
  }
  return result;
}
function runGh(binary, args) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(binary, args, { shell: false, windowsHide: true, env: cleanEnvironment(), stdio: ["ignore", "pipe", "pipe"] });
    let stdout = ""; let stderr = "";
    child.stdout.on("data", (chunk) => { stdout += chunk; }); child.stderr.on("data", (chunk) => { stderr += chunk; });
    child.once("error", () => reject(Object.assign(new Error("installer_attestation_gh_unavailable"), { code: "installer_attestation_gh_unavailable" })));
    child.once("close", (code) => code === 0 ? resolvePromise(stdout) : reject(Object.assign(new Error("installer_attestation_verification_failed"), { code: "installer_attestation_verification_failed", cause: stderr })));
  });
}

export function validateSpdx(spdx, { installerName, installerSha256, sourceCommit }) {
  if (!spdx || spdx.spdxVersion !== "SPDX-2.3" || spdx.dataLicense !== "CC0-1.0" || spdx.SPDXID !== "SPDXRef-DOCUMENT"
    || typeof spdx.documentNamespace !== "string" || !spdx.documentNamespace.includes(sourceCommit)
    || !spdx.creationInfo || !Number.isFinite(Date.parse(spdx.creationInfo.created))
    || !Array.isArray(spdx.creationInfo.creators) || !spdx.creationInfo.creators.some((item) => /^Tool:/u.test(item))
    || typeof spdx.documentComment !== "string" || !spdx.documentComment.includes(`source_commit_sha=${sourceCommit}`)
    || !Array.isArray(spdx.packages) || !Array.isArray(spdx.files) || !Array.isArray(spdx.relationships)) fail("installer_spdx_invalid");
  const packageEntry = spdx.packages.find((item) => item?.SPDXID === "SPDXRef-Package-Pinvou" && item.name === "Pinvou Agent");
  const fileEntry = spdx.files.find((item) => item?.fileName === installerName && Array.isArray(item.checksums)
    && item.checksums.some((sum) => sum.algorithm === "SHA256" && String(sum.checksumValue).toLowerCase() === installerSha256));
  const dependencyPackages = spdx.packages.filter((item) => item?.SPDXID !== "SPDXRef-Package-Pinvou");
  const resourceFiles = spdx.files.filter((item) => item !== fileEntry);
  const stagedPaths = resourceFiles.map((item) => item?.fileName);
  if (!packageEntry || !fileEntry || typeof fileEntry.SPDXID !== "string"
    || dependencyPackages.length === 0 || resourceFiles.length === 0
    || !/dependency_inventory_sha256=[a-f0-9]{64};staging_manifest_sha256=[a-f0-9]{64};staged_file_set_sha256=[a-f0-9]{64};mapping_inventory_sha256=[a-f0-9]{64};dependency_count=\d+;staged_file_count=\d+/u.test(spdx.documentComment)
    || new Set(stagedPaths).size !== stagedPaths.length
    || !stagedPaths.includes("nsis-stage/manifest.json") || !stagedPaths.includes("nsis-stage/tauri.nsis-stage.conf.json")
    || !stagedPaths.includes("nsis-stage/installer-hooks.nsh") || !stagedPaths.includes("bundle/pinvou3-tauri.exe")
    || !stagedPaths.some((item) => typeof item === "string" && item.startsWith("nsis-stage/resources/"))
    || resourceFiles.filter((item) => item?.fileName?.startsWith("nsis-stage/resources/"))
      .some((item) => typeof item.fileComment !== "string" || !/pinvou_stage_category=staged-resource;mapping_source=.+;mapping_destination=/u.test(item.fileComment))
    || !spdx.relationships.some((item) => item?.spdxElementId === "SPDXRef-DOCUMENT" && item.relationshipType === "DESCRIBES" && item.relatedSpdxElement === packageEntry.SPDXID)
    || !spdx.relationships.some((item) => item?.spdxElementId === packageEntry.SPDXID && item.relationshipType === "CONTAINS" && item.relatedSpdxElement === fileEntry.SPDXID)
    || dependencyPackages.some((dependency) => !spdx.relationships.some((item) => item?.spdxElementId === packageEntry.SPDXID && item.relationshipType === "DEPENDS_ON" && item.relatedSpdxElement === dependency.SPDXID))
    || resourceFiles.some((resource) => !spdx.relationships.some((item) => item?.spdxElementId === packageEntry.SPDXID && item.relationshipType === "CONTAINS" && item.relatedSpdxElement === resource.SPDXID))) fail("installer_spdx_relationship_invalid");
  return true;
}

function findDsseStatements(bundle) {
  const candidates = [];
  const visit = (value) => {
    if (!value || typeof value !== "object") return;
    if (typeof value.payload === "string" && (value.payloadType === "application/vnd.in-toto+json" || value.payloadType?.includes("in-toto"))) candidates.push(value.payload);
    for (const item of Object.values(value)) visit(item);
  };
  visit(bundle);
  return candidates.flatMap((payload) => {
    try { return [JSON.parse(Buffer.from(payload, "base64").toString("utf8"))]; } catch { return []; }
  });
}

export function parseSigstoreJsonl(bytes) {
  const lines = String(bytes).split(/\r?\n/u).filter((line) => line.trim() !== "");
  if (lines.length === 0) fail("installer_attestation_bundle_empty");
  const bundles = lines.map((line) => {
    try { return JSON.parse(line); } catch { fail("installer_attestation_jsonl_invalid"); }
  });
  if (bundles.some((value) => !value || typeof value !== "object" || Array.isArray(value))) {
    fail("installer_attestation_jsonl_invalid");
  }
  return bundles;
}

export function uniqueSubjectForDigest(bundles, subjectSha256) {
  const matches = bundles.flatMap(findDsseStatements).flatMap((statement) => (statement.subject || [])
    .filter((item) => String(item?.digest?.sha256).toLowerCase() === subjectSha256)
    .map((subject) => ({ statement, subject })));
  if (matches.length !== 1) fail(matches.length === 0 ? "installer_attestation_statement_missing" : "installer_attestation_statement_ambiguous");
  return matches[0];
}

export function uniqueStatementForDigest(bundles, subjectSha256) {
  return uniqueSubjectForDigest(bundles, subjectSha256).statement;
}

export function validateGitHubBuildIdentity(statement, { repository, workflow, runId, sourceCommit }) {
  const predicate = statement?.predicate;
  const buildDefinition = predicate?.buildDefinition;
  const externalWorkflow = buildDefinition?.externalParameters?.workflow;
  const dependencies = buildDefinition?.resolvedDependencies;
  const invocationId = predicate?.runDetails?.metadata?.invocationId;
  const expectedRepositoryUrl = `https://github.com/${repository}`;
  if (statement?._type !== "https://in-toto.io/Statement/v1"
    || buildDefinition?.buildType !== "https://actions.github.io/buildtypes/workflow/v1"
    || !externalWorkflow || !new Set([repository, expectedRepositoryUrl]).has(externalWorkflow.repository)
    || externalWorkflow.path !== workflow
    || !Array.isArray(dependencies)
    || !dependencies.some((item) => item?.digest?.gitCommit === sourceCommit
      && typeof item.uri === "string" && item.uri.includes(expectedRepositoryUrl))
    || typeof invocationId !== "string"
    || !invocationId.startsWith(`${expectedRepositoryUrl}/actions/runs/${runId}/attempts/`)) {
    fail("installer_attestation_identity_mismatch");
  }
  return true;
}

export async function verifyInstallerSupplyChain({ installerPath, bundlePath, sbomPath, sbomBundlePath, repository, workflow, runId, sourceCommit, outputPath,
  ghBinary, ghRunner = runGh }) {
  if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/u.test(repository || "") || !/^\.github\/workflows\/[A-Za-z0-9_.-]+\.ya?ml$/u.test(workflow || "")
    || !/^[1-9][0-9]*$/u.test(String(runId || "")) || !/^[a-f0-9]{40}$/u.test(sourceCommit || "")) fail("installer_build_identity_invalid");
  if (!isAbsolute(ghBinary || "")) fail("installer_attestation_gh_configuration_invalid");
  if (!sbomBundlePath) fail("installer_sbom_attestation_missing");
  const [installerBytes, bundleBytes, sbomBytes, sbomBundleBytes] = await Promise.all([
    readFile(resolve(installerPath)), readFile(resolve(bundlePath)), readFile(resolve(sbomPath)), readFile(resolve(sbomBundlePath)),
  ]);
  const installerSha256 = sha256(installerBytes); const sbomSha256 = sha256(sbomBytes); let spdx;
  try { spdx = JSON.parse(sbomBytes); } catch { fail("installer_supply_chain_json_invalid"); }
  const installerAttestation = uniqueSubjectForDigest(parseSigstoreJsonl(bundleBytes), installerSha256);
  const sbomAttestation = uniqueSubjectForDigest(parseSigstoreJsonl(sbomBundleBytes), sbomSha256);
  const statement = installerAttestation.statement; const sbomStatement = sbomAttestation.statement;
  const installerSubjectName = installerAttestation.subject?.name; const sbomSubjectName = sbomAttestation.subject?.name;
  if (typeof installerSubjectName !== "string" || basename(installerSubjectName) !== installerSubjectName
    || typeof sbomSubjectName !== "string" || basename(sbomSubjectName) !== sbomSubjectName) fail("installer_attestation_subject_name_invalid");
  validateGitHubBuildIdentity(statement, { repository, workflow, runId, sourceCommit });
  validateGitHubBuildIdentity(sbomStatement, { repository, workflow, runId, sourceCommit });
  validateSpdx(spdx, { installerName: installerSubjectName, installerSha256, sourceCommit });
  await ghRunner(resolve(ghBinary), ["attestation", "verify", resolve(installerPath), "--bundle", resolve(bundlePath), "--repo", repository,
    "--signer-workflow", workflow, "--format", "json"]);
  await ghRunner(resolve(ghBinary), ["attestation", "verify", resolve(sbomPath), "--bundle", resolve(sbomBundlePath), "--repo", repository,
    "--signer-workflow", workflow, "--format", "json"]);
  const result = {
    schema: "pinvou-installer-supply-chain-verification", version: 1, source_commit_sha: sourceCommit,
    installer_sha256: installerSha256, attestation_bundle_sha256: sha256(bundleBytes), sbom_sha256: sbomSha256,
    sbom_attestation_bundle_sha256: sha256(sbomBundleBytes),
    installer_subject_name: installerSubjectName, sbom_subject_name: sbomSubjectName,
    repository, workflow, build_run_id: Number(runId), protected_environment: "windows-release", verification_tool: "gh-attestation-verify",
    mock: false,
  };
  await writeFile(resolve(outputPath), `${JSON.stringify(result)}\n`, { encoding: "utf8", flag: "wx" });
  return result;
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  verifyInstallerSupplyChain({ installerPath: arg("--installer"), bundlePath: arg("--bundle"), sbomPath: arg("--sbom"), sbomBundlePath: arg("--sbom-bundle"),
    repository: arg("--repository"), workflow: arg("--workflow"), runId: arg("--run-id"), sourceCommit: arg("--source-commit"), outputPath: arg("--output"),
    ghBinary: arg("--gh-binary") })
    .then((value) => process.stdout.write(`${JSON.stringify(value)}\n`)).catch((error) => {
      process.stderr.write(`${JSON.stringify({ ok: false, code: error?.code || "installer_supply_chain_verification_failed" })}\n`); process.exitCode = 1;
    });
}
