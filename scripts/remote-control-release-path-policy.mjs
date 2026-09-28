#!/usr/bin/env node
import { readFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";

export const REMOTE_CONTROL_RELEASE_EXACT_PATHS = Object.freeze([
  ".github/workflows/pr-check.yml",
  ".github/workflows/release-packages.yml",
  ".github/workflows/windows-remote-control-build-evidence.yml",
  ".github/workflows/windows-remote-control-evidence.yml",
  "pinvou3-app/package.json",
  "pinvou3-app/package-lock.json",
  "pinvou3-app/src/components/Toggle.jsx",
  "pinvou3-app/src/features/chat/ChatView.jsx",
  "pinvou3-app/src/app/main.jsx",
  "pinvou3-app/src/features/settings/SettingsView.jsx",
  "pinvou3-app/src/hooks/useBridge.js",
  "pinvou3-app/src/platform/tauri/bridge.js",
  "pinvou3-app/src/shared/platform.js",
  "pinvou3-app/src-tauri/capabilities/default.json",
  "pinvou3-app/src-tauri/src/app/desktop_remote_composition.rs",
  "pinvou3-app/src-tauri/src/app/remote_projection_adapter.rs",
  "pinvou3-app/src-tauri/src/app/commands/remote_control.rs",
  "pinvou3-app/src-tauri/src/features/behavior_telemetry/remote_control.rs",
  "pinvou3-app/src-tauri/src/platform/capabilities.rs",
  "scripts/check-enrollment-gated-v2-rollback.mjs",
  "scripts/create-installer-spdx.mjs",
  "scripts/remote-control-release-path-policy.mjs",
  "scripts/run-windows-remote-control-e2e.ps1",
  "scripts/windows-e2e-orchestrator-helpers.ps1",
  "scripts/windows-e2e-process-guard.ps1",
]);

export const REMOTE_CONTROL_RELEASE_PREFIXES = Object.freeze([
  "scripts/remote-control-",
  "scripts/run-remote-control-",
  "pinvou3-app/scripts/remote-control-",
  "pinvou3-app/src/features/remote-control/",
  "pinvou3-app/src/platform/tauri/bridge/remote-control",
  "pinvou3-app/src/platform/tauri/bridge/desktop-remote-control",
  "pinvou3-app/src-tauri/src/app/commands/",
  "pinvou3-app/src-tauri/src/features/sessions/remote_proxy/",
  "pinvou3-app/src-tauri/src/features/remote_control/",
  "pinvou3-app/src-tauri/packaging/windows/nsis/",
  "pinvou3-app/tests/remote_",
  "pinvou3-app/tests/desktop_remote_",
  "pinvou3-app/tests/connect_remote_device_",
  "remote-control-relay/",
]);

const exactPaths = new Set(REMOTE_CONTROL_RELEASE_EXACT_PATHS);

export function isRemoteControlReleasePath(value) {
  const path = String(value || "").replaceAll("\\", "/").replace(/^\.\//u, "");
  return exactPaths.has(path) || REMOTE_CONTROL_RELEASE_PREFIXES.some((prefix) => path.startsWith(prefix));
}

export function changedPathsRequireRemoteControlRelease(value) {
  return String(value || "").split(/\r?\n/u).filter(Boolean).some(isRemoteControlReleasePath);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const inputPath = process.argv[2];
  const input = inputPath ? await readFile(inputPath, "utf8") : await new Promise((resolve, reject) => {
    let value = "";
    process.stdin.setEncoding("utf8");
    process.stdin.on("data", (chunk) => { value += chunk; });
    process.stdin.on("end", () => resolve(value));
    process.stdin.on("error", reject);
  });
  process.exitCode = changedPathsRequireRemoteControlRelease(input) ? 0 : 1;
}
