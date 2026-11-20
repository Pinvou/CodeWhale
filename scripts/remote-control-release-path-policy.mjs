#!/usr/bin/env node
import { readFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";

import { REMOTE_CONTROL_RELEASE_EXACT_PATHS, REMOTE_CONTROL_RELEASE_PREFIXES } from "./remote-control-release-paths.mjs";
export { REMOTE_CONTROL_RELEASE_EXACT_PATHS, REMOTE_CONTROL_RELEASE_PREFIXES };

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
