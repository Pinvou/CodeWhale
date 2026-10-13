#!/usr/bin/env node
import crypto from "node:crypto";
import { lstat, readFile, readdir, realpath, writeFile } from "node:fs/promises";
import { basename, dirname, relative, resolve, sep } from "node:path";

const [installer, output, commit, version, packageLockPath, cargoLockPath, stagingManifestPath, stagingRootPath, mainBinaryPath] = process.argv.slice(2);
if (!installer || !output || !packageLockPath || !cargoLockPath || !stagingManifestPath || !stagingRootPath || !mainBinaryPath
  || !/^[a-f0-9]{40}$/u.test(commit || "") || !/^\d+\.\d+\.\d+(?:\.\d+)?$/u.test(version || "")) throw new Error("installer_spdx_arguments_invalid");
const hash = (value) => crypto.createHash("sha256").update(value).digest("hex");
const cleanId = (prefix, value) => `SPDXRef-${prefix}-${hash(value).slice(0, 20)}`;
const ensureInside = (root, candidate) => candidate === root || candidate.startsWith(`${root}${sep}`);
const readRegularInside = async (root, path) => {
  const metadata = await lstat(path); if (!metadata.isFile() || metadata.isSymbolicLink()) throw new Error("installer_spdx_staging_file_invalid");
  const canonical = await realpath(path); if (!ensureInside(root, canonical)) throw new Error("installer_spdx_staging_escape");
  return readFile(canonical);
};

const installerBytes = await readFile(resolve(installer)); const installerDigest = hash(installerBytes);
const packageLockBytes = await readFile(resolve(packageLockPath)); const cargoLockBytes = await readFile(resolve(cargoLockPath));
let packageLock; try { packageLock = JSON.parse(packageLockBytes); } catch { throw new Error("installer_spdx_package_lock_invalid"); }
const npmPackages = Object.entries(packageLock.packages || {}).filter(([path, item]) => path !== "" && path.includes("node_modules/") && item?.version)
  .map(([path, item]) => ({ ecosystem: "npm", name: item.name || path.split("node_modules/").at(-1), version: String(item.version), source: path }));
const cargoPackages = [];
for (const block of String(cargoLockBytes).split(/\r?\n\[\[package\]\]\r?\n/u).slice(1)) {
  const name = /^name\s*=\s*"([^"]+)"/mu.exec(block)?.[1]; const dependencyVersion = /^version\s*=\s*"([^"]+)"/mu.exec(block)?.[1];
  if (name && dependencyVersion) cargoPackages.push({ ecosystem: "cargo", name, version: dependencyVersion,
    checksum: /^checksum\s*=\s*"([a-f0-9]{64})"/mu.exec(block)?.[1], source: /^source\s*=\s*"([^"]+)"/mu.exec(block)?.[1] || "workspace" });
}
if (npmPackages.length === 0 || cargoPackages.length === 0) throw new Error("installer_spdx_dependency_inventory_empty");

const stagingPath = resolve(stagingRootPath);
if (!(await lstat(stagingPath)).isDirectory() || (await lstat(stagingPath)).isSymbolicLink()) throw new Error("installer_spdx_staging_file_invalid");
const stagingRoot = await realpath(stagingPath);
const resourcesPath = resolve(stagingRoot, "resources");
const resourceMetadata = await lstat(resourcesPath);
if (!resourceMetadata.isDirectory() || resourceMetadata.isSymbolicLink()) throw new Error("installer_spdx_resource_symlink_forbidden");
const resourcesRoot = await realpath(resourcesPath);
// Validate the supplied path before canonicalization; otherwise a symlink is
// silently replaced by its regular target and bypasses the no-reparse policy.
const suppliedManifestPath = resolve(stagingManifestPath);
const manifestMetadata = await lstat(suppliedManifestPath);
if (!manifestMetadata.isFile() || manifestMetadata.isSymbolicLink()) throw new Error("installer_spdx_staging_file_invalid");
const manifestPath = await realpath(suppliedManifestPath);
if (!ensureInside(stagingRoot, resourcesRoot) || !ensureInside(stagingRoot, manifestPath) || dirname(manifestPath) !== stagingRoot) throw new Error("installer_spdx_staging_escape");
let stagingManifest; const stagingManifestBytes = await readRegularInside(stagingRoot, manifestPath);
try { stagingManifest = JSON.parse(stagingManifestBytes); } catch { throw new Error("installer_spdx_staging_manifest_invalid"); }
if (stagingManifest?.schemaVersion !== 3 || stagingManifest.appVersion !== version || stagingManifest.mainBinary !== "target/release/pinvou3-tauri.exe"
  || stagingManifest.installerHooks !== "target/release/nsis-stage/installer-hooks.nsh" || !Array.isArray(stagingManifest.files)
  || !Array.isArray(stagingManifest.mappings) || stagingManifest.files.length === 0 || new Set(stagingManifest.files).size !== stagingManifest.files.length) throw new Error("installer_spdx_staging_manifest_invalid");
const fileMappings = new Map();
for (const mapping of stagingManifest.mappings) {
  if (!mapping || Object.keys(mapping).sort().join("\0") !== ["destination", "files", "source"].sort().join("\0")
    || typeof mapping.source !== "string" || !mapping.source || typeof mapping.destination !== "string"
    || !Array.isArray(mapping.files) || mapping.files.length === 0) throw new Error("installer_spdx_staging_mapping_invalid");
  for (const mappedFile of mapping.files) {
    if (typeof mappedFile !== "string" || !mappedFile || fileMappings.has(mappedFile)) throw new Error("installer_spdx_staging_mapping_invalid");
    fileMappings.set(mappedFile, mapping);
  }
}

const actualRelativeFiles = [];
async function collect(path) {
  const metadata = await lstat(path); if (metadata.isSymbolicLink()) throw new Error("installer_spdx_resource_symlink_forbidden");
  const canonical = await realpath(path); if (!ensureInside(resourcesRoot, canonical)) throw new Error("installer_spdx_resource_escape");
  if (metadata.isDirectory()) { for (const entry of await readdir(path)) await collect(resolve(path, entry)); }
  else if (metadata.isFile()) actualRelativeFiles.push(relative(resourcesRoot, path).replaceAll("\\", "/"));
}
await collect(resourcesRoot); actualRelativeFiles.sort();
const declared = [...stagingManifest.files].map(String).sort();
const mapped = [...fileMappings.keys()].sort();
if (JSON.stringify(actualRelativeFiles) !== JSON.stringify(declared) || JSON.stringify(mapped) !== JSON.stringify(declared)
  || stagingManifest.resourceFileCount !== declared.length) throw new Error("installer_spdx_staged_file_set_mismatch");

const stagedFiles = [];
for (const relativePath of declared) {
  if (!relativePath || relativePath.split("/").some((part) => part === "" || part === "." || part === "..")) throw new Error("installer_spdx_staging_manifest_invalid");
  const bytes = await readRegularInside(resourcesRoot, resolve(resourcesRoot, ...relativePath.split("/")));
  const mapping = fileMappings.get(relativePath);
  stagedFiles.push({ path: `nsis-stage/resources/${relativePath}`, sha256: hash(bytes), category: "staged-resource", mappingSource: mapping.source, mappingDestination: mapping.destination });
}
for (const [name, path] of [["nsis-stage/manifest.json", manifestPath], ["nsis-stage/tauri.nsis-stage.conf.json", resolve(stagingRoot, "tauri.nsis-stage.conf.json")], ["nsis-stage/installer-hooks.nsh", resolve(stagingRoot, "installer-hooks.nsh")]]) {
  stagedFiles.push({ path: name, sha256: hash(await readRegularInside(stagingRoot, path)), category: name.endsWith("manifest.json") ? "packaging-manifest" : "generated-packaging" });
}
const mainBinary = resolve(mainBinaryPath); const mainMetadata = await lstat(mainBinary);
if (!mainMetadata.isFile() || mainMetadata.isSymbolicLink() || basename(mainBinary) !== "pinvou3-tauri.exe") throw new Error("installer_spdx_main_binary_invalid");
stagedFiles.push({ path: "bundle/pinvou3-tauri.exe", sha256: hash(await readFile(mainBinary)), category: "main-binary" });
if (new Set(stagedFiles.map((item) => item.path)).size !== stagedFiles.length) throw new Error("installer_spdx_duplicate_staged_file");

const fileId = "SPDXRef-File-Pinvou-Installer"; const packageId = "SPDXRef-Package-Pinvou";
const dependencyPackages = [...npmPackages, ...cargoPackages].map((item) => ({ SPDXID: cleanId("Dependency", `${item.ecosystem}:${item.name}@${item.version}:${item.source}`), name: item.name,
  versionInfo: item.version, downloadLocation: "NOASSERTION", filesAnalyzed: false,
  externalRefs: [{ referenceCategory: "PACKAGE-MANAGER", referenceType: "purl", referenceLocator: `pkg:${item.ecosystem}/${item.name.split("/").map(encodeURIComponent).join("/")}@${encodeURIComponent(item.version)}` }],
  ...(item.checksum ? { checksums: [{ algorithm: "SHA256", checksumValue: item.checksum }] } : {}) }));
const spdxStagedFiles = stagedFiles.map((item) => ({ SPDXID: cleanId("StagedFile", item.path), fileName: item.path,
  checksums: [{ algorithm: "SHA256", checksumValue: item.sha256 }], fileComment: `pinvou_stage_category=${item.category}${item.mappingSource ? `;mapping_source=${item.mappingSource};mapping_destination=${item.mappingDestination}` : ""}`,
  licenseConcluded: "NOASSERTION", copyrightText: "NOASSERTION" }));
const stagedSetDigest = hash(JSON.stringify(stagedFiles)); const mappingDigest = hash(JSON.stringify(stagingManifest.mappings));
const document = {
  spdxVersion: "SPDX-2.3", dataLicense: "CC0-1.0", SPDXID: "SPDXRef-DOCUMENT", name: `Pinvou-Agent-Windows-${version}`,
  documentNamespace: `https://github.com/Pinvou/pinvou3/spdx/${commit}/${installerDigest}`,
  documentComment: `source_commit_sha=${commit};dependency_inventory_sha256=${hash(Buffer.concat([packageLockBytes, cargoLockBytes]))};staging_manifest_sha256=${hash(stagingManifestBytes)};staged_file_set_sha256=${stagedSetDigest};mapping_inventory_sha256=${mappingDigest};dependency_count=${dependencyPackages.length};staged_file_count=${spdxStagedFiles.length}`,
  creationInfo: { created: new Date().toISOString(), creators: ["Tool: pinvou-create-installer-spdx-v3"] },
  packages: [{ SPDXID: packageId, name: "Pinvou Agent", versionInfo: version, downloadLocation: "NOASSERTION", filesAnalyzed: true }, ...dependencyPackages],
  files: [{ SPDXID: fileId, fileName: basename(installer), checksums: [{ algorithm: "SHA256", checksumValue: installerDigest }], licenseConcluded: "NOASSERTION", copyrightText: "NOASSERTION" }, ...spdxStagedFiles],
  relationships: [{ spdxElementId: "SPDXRef-DOCUMENT", relationshipType: "DESCRIBES", relatedSpdxElement: packageId },
    { spdxElementId: packageId, relationshipType: "CONTAINS", relatedSpdxElement: fileId },
    ...dependencyPackages.map((item) => ({ spdxElementId: packageId, relationshipType: "DEPENDS_ON", relatedSpdxElement: item.SPDXID })),
    ...spdxStagedFiles.map((item) => ({ spdxElementId: packageId, relationshipType: "CONTAINS", relatedSpdxElement: item.SPDXID }))],
};
await writeFile(resolve(output), `${JSON.stringify(document)}\n`, { encoding: "utf8", flag: "wx" });
