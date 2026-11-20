#!/usr/bin/env node
import { readFile, writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import {
  REMOTE_CONTROL_RELEASE_EXACT_PATHS, REMOTE_CONTROL_RELEASE_PREFIXES,
  REMOTE_CONTROL_RELEASE_NATIVE_GLOBS,
  PR_RELEASE_CONTRACT_EXTRA_PATHS, RELEASE_PACKAGES_EXTRA_PATHS,
} from "./remote-control-release-paths.mjs";

export function remoteControlReleasePatterns() {
  return [
    ...REMOTE_CONTROL_RELEASE_EXACT_PATHS,
    ...REMOTE_CONTROL_RELEASE_PREFIXES.flatMap((prefix) => prefix.endsWith("/")
      ? [`${prefix}**`] : [`${prefix}*`, `${prefix}*/**`]),
    ...REMOTE_CONTROL_RELEASE_NATIVE_GLOBS,
  ];
}

function replaceBlock(source, indent, patterns, expectedCount) {
  const expression = new RegExp(`^${indent}# BEGIN GENERATED release paths\\n[\\s\\S]*?^${indent}# END GENERATED release paths\\n`, "gmu");
  const matches = [...source.matchAll(expression)];
  if (matches.length !== expectedCount) throw new Error("release_path_generated_block_missing_or_duplicated");
  const unique = [...new Set(patterns)];
  for (const value of unique) {
    if (!/^[A-Za-z0-9_.\u4e00-\u9fff/*-]+$/u.test(value) || value.startsWith("!") || value.includes("..")) {
      throw new Error("release_path_pattern_invalid");
    }
  }
  const block = `${indent}# BEGIN GENERATED release paths\n`
    + unique.map((value) => `${indent}- '${value}'\n`).join("")
    + `${indent}# END GENERATED release paths\n`;
  return source.replace(expression, () => block);
}

export function renderReleasePathWorkflows(pr, release) {
  const common = remoteControlReleasePatterns();
  return {
    pr: replaceBlock(pr.replaceAll("\r\n", "\n"), "              ", [...common, ...PR_RELEASE_CONTRACT_EXTRA_PATHS], 1),
    release: replaceBlock(release.replaceAll("\r\n", "\n"), "      ", [...common, ...RELEASE_PACKAGES_EXTRA_PATHS], 2),
  };
}

export async function main(args = process.argv.slice(2)) {
  if (args.length !== 1 || !["--check", "--write"].includes(args[0])) {
    throw new Error("usage: generate-remote-control-release-paths.mjs --check|--write");
  }
  const prUrl = new URL("../.github/workflows/pr-check.yml", import.meta.url);
  const releaseUrl = new URL("../.github/workflows/release-packages.yml", import.meta.url);
  const [pr, release] = await Promise.all([readFile(prUrl, "utf8"), readFile(releaseUrl, "utf8")]);
  const rendered = renderReleasePathWorkflows(pr, release);
  if (args[0] === "--check") {
    if (pr.replaceAll("\r\n", "\n") !== rendered.pr || release.replaceAll("\r\n", "\n") !== rendered.release) {
      throw new Error("release_path_workflow_drift: run node scripts/generate-remote-control-release-paths.mjs --write");
    }
  } else {
    await writeFile(prUrl, rendered.pr);
    await writeFile(releaseUrl, rendered.release);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await main();
}
