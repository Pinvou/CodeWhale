import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { copyFile, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { validateSpdx, verifyInstallerSupplyChain } from "../scripts/verify-installer-supply-chain.mjs";
import crypto from "node:crypto";

const generator = fileURLToPath(new URL("../../scripts/create-installer-spdx.mjs", import.meta.url));
const commit = "a".repeat(40);

test("SPDX generator inventories npm, Cargo, resources and installer relationships", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-spdx-")); t.after(() => rm(root, { recursive: true, force: true }));
  const installer = join(root, "pinvou3_1.2.3_x64-setup.exe"); const renamedInstaller = join(root, "pinvou-installer.exe");
  const output = join(root, "installer-sbom.spdx.json"); const staging = join(root, "nsis-stage"); const resources = join(staging, "resources");
  await mkdir(resources, { recursive: true }); await writeFile(installer, "installer-bytes"); await writeFile(join(resources, "runtime.dll"), "runtime-bytes");
  await writeFile(join(staging, "tauri.nsis-stage.conf.json"), "{}\n"); await writeFile(join(staging, "installer-hooks.nsh"), "; generated\n");
  await writeFile(join(staging, "manifest.json"), JSON.stringify({ schemaVersion: 3, appVersion: "1.2.3", mainBinary: "target/release/pinvou3-tauri.exe",
    installerHooks: "target/release/nsis-stage/installer-hooks.nsh", resourceFileCount: 1, mappings: [{ source: "private-runtime", destination: "runtime", files: ["runtime.dll"] }], files: ["runtime.dll"] }));
  const mainBinary = join(root, "pinvou3-tauri.exe"); await writeFile(mainBinary, "main-binary");
  await writeFile(join(root, "package-lock.json"), JSON.stringify({ packages: { "": {}, "node_modules/ajv": { version: "8.0.0" } } }));
  await writeFile(join(root, "Cargo.lock"), 'version = 4\n\n[[package]]\nname = "serde"\nversion = "1.0.0"\nsource = "registry+https://github.com/rust-lang/crates.io-index"\n');
  execFileSync(process.execPath, [generator, installer, output, commit, "1.2.3", join(root, "package-lock.json"), join(root, "Cargo.lock"), join(staging, "manifest.json"), staging, mainBinary]);
  const spdx = JSON.parse(await readFile(output, "utf8")); const installerSha256 = crypto.createHash("sha256").update("installer-bytes").digest("hex");
  assert.equal(validateSpdx(spdx, { installerName: "pinvou3_1.2.3_x64-setup.exe", installerSha256, sourceCommit: commit }), true);
  assert.ok(spdx.packages.some((item) => item.name === "ajv")); assert.ok(spdx.packages.some((item) => item.name === "serde"));
  assert.ok(spdx.files.some((item) => item.fileName === "nsis-stage/resources/runtime.dll"));
  assert.match(spdx.files.find((item) => item.fileName === "nsis-stage/resources/runtime.dll").fileComment, /mapping_source=private-runtime/u);
  assert.ok(spdx.files.some((item) => item.fileName === "nsis-stage/manifest.json"));
  assert.ok(spdx.files.some((item) => item.fileName === "bundle/pinvou3-tauri.exe"));
  assert.ok(spdx.relationships.some((item) => item.relationshipType === "DEPENDS_ON"));
  const statement = (name, digest) => ({ mediaType: "application/vnd.dev.sigstore.bundle.v0.3+json", dsseEnvelope: {
    payloadType: "application/vnd.in-toto+json", payload: Buffer.from(JSON.stringify({ _type: "https://in-toto.io/Statement/v1",
      subject: [{ name, digest: { sha256: digest } }], predicate: { buildDefinition: { buildType: "https://actions.github.io/buildtypes/workflow/v1",
        externalParameters: { workflow: { repository: "https://github.com/pinvou/pinvou3", path: ".github/workflows/windows-remote-control-build-evidence.yml" } },
        resolvedDependencies: [{ uri: "https://github.com/pinvou/pinvou3", digest: { gitCommit: commit } }] },
      runDetails: { metadata: { invocationId: "https://github.com/pinvou/pinvou3/actions/runs/123/attempts/1" } } } })).toString("base64"), signatures: [] } });
  const sbomBytes = await readFile(output); const sbomSha256 = crypto.createHash("sha256").update(sbomBytes).digest("hex");
  const installerBundle = join(root, "installer.jsonl"); const sbomBundle = join(root, "sbom.jsonl");
  await writeFile(installerBundle, `${JSON.stringify(statement("pinvou3_1.2.3_x64-setup.exe", installerSha256))}\n`);
  await writeFile(sbomBundle, `${JSON.stringify(statement("installer-sbom.spdx.json", sbomSha256))}\n`);
  const ghBinary = join(root, "trusted-gh.exe");
  await writeFile(ghBinary, "trusted-gh-fixture");
  await copyFile(installer, renamedInstaller);
  const ghCalls = [];
  const verified = await verifyInstallerSupplyChain({ installerPath: renamedInstaller, bundlePath: installerBundle, sbomPath: output,
    sbomBundlePath: sbomBundle, repository: "pinvou/pinvou3", workflow: ".github/workflows/windows-remote-control-build-evidence.yml",
    runId: "123", sourceCommit: commit, outputPath: join(root, "verified.json"), ghBinary,
    ghRunner: async (binary, args) => { ghCalls.push({ binary, args }); } });
  assert.equal(verified.sbom_sha256, sbomSha256); assert.equal(verified.installer_subject_name, "pinvou3_1.2.3_x64-setup.exe"); assert.equal(ghCalls.length, 2);
  assert.ok(ghCalls.every((call) => call.binary === ghBinary
    && call.args[0] === "attestation" && call.args[1] === "verify" && call.args.includes("--bundle")));
  const originalPath = process.env.PATH;
  process.env.PATH = root;
  try {
    await assert.rejects(() => verifyInstallerSupplyChain({ installerPath: renamedInstaller, bundlePath: installerBundle, sbomPath: output,
      sbomBundlePath: sbomBundle, repository: "pinvou/pinvou3", workflow: ".github/workflows/windows-remote-control-build-evidence.yml",
      runId: "123", sourceCommit: commit, outputPath: join(root, "must-not-exist.json"),
      ghRunner: async () => { throw new Error("PATH gh must never execute"); } }), /installer_attestation_gh_configuration_invalid/u);
  } finally {
    if (originalPath === undefined) delete process.env.PATH;
    else process.env.PATH = originalPath;
  }
});

test("SPDX generator rejects a staging manifest that omits or duplicates a final resource mapping", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "pinvou-spdx-invalid-")); t.after(() => rm(root, { recursive: true, force: true }));
  const staging = join(root, "nsis-stage"); await mkdir(join(staging, "resources"), { recursive: true });
  await writeFile(join(root, "installer.exe"), "installer"); await writeFile(join(root, "pinvou3-tauri.exe"), "binary");
  await writeFile(join(staging, "resources", "runtime.dll"), "runtime"); await writeFile(join(staging, "tauri.nsis-stage.conf.json"), "{}"); await writeFile(join(staging, "installer-hooks.nsh"), "; hook");
  await writeFile(join(root, "package-lock.json"), JSON.stringify({ packages: { "node_modules/a": { version: "1.0.0" } } }));
  await writeFile(join(root, "Cargo.lock"), 'version = 4\n\n[[package]]\nname = "serde"\nversion = "1.0.0"\n');
  await writeFile(join(staging, "manifest.json"), JSON.stringify({ schemaVersion: 3, appVersion: "1.2.3", mainBinary: "target/release/pinvou3-tauri.exe", installerHooks: "target/release/nsis-stage/installer-hooks.nsh", resourceFileCount: 1, mappings: [{ source: "private-runtime", destination: "runtime", files: ["other.dll"] }], files: ["runtime.dll"] }));
  assert.throws(() => execFileSync(process.execPath, [generator, join(root, "installer.exe"), join(root, "out.json"), commit, "1.2.3", join(root, "package-lock.json"), join(root, "Cargo.lock"), join(staging, "manifest.json"), staging, join(root, "pinvou3-tauri.exe")], { stdio: "pipe" }), /Command failed/u);
});
