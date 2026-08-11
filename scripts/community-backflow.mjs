#!/usr/bin/env node

/**
 * Community 回流的跨平台快速路径。
 *
 * 核心逻辑只依赖 Node.js 与 git，避免把 PR 创建平台绑定到 Bash、PowerShell
 * 或某一种 runner。重型产品验证仍由按路径路由的 GitHub Actions 完成。
 */

import { execFileSync, spawnSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const PROVENANCE_SCHEMA = 1;
export const CLI_VERSION = "1";
export const BACKFLOW_MARKER_RE =
  /<!--\s*community-backflow:Pinvou\/pinvou-agent#(\d+)\s*-->/;
export const PROVENANCE_MARKER_RE =
  /<!--\s*community-backflow-provenance:([A-Za-z0-9_-]+)\s*-->/;

const WINDOWS_RESERVED_NAMES = new Set([
  "con",
  "prn",
  "aux",
  "nul",
  ...Array.from({ length: 9 }, (_, index) => `com${index + 1}`),
  ...Array.from({ length: 9 }, (_, index) => `lpt${index + 1}`),
]);

function fail(message) {
  throw new Error(message);
}

function runGit(args, options = {}) {
  try {
    return execFileSync("git", args, {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
      ...options,
    }).trim();
  } catch (error) {
    const detail = String(error.stderr || error.message || error).trim();
    fail(`git ${args.join(" ")} 失败：${detail}`);
  }
}

function platformName(platform = process.platform) {
  if (platform === "win32") return "windows";
  if (platform === "darwin") return "macos";
  if (platform === "linux") return "linux";
  return platform;
}

function parseArgs(argv) {
  const [command = "help", ...rest] = argv;
  const options = {};
  for (let index = 0; index < rest.length; index += 1) {
    const token = rest[index];
    if (!token.startsWith("--")) fail(`无法识别参数：${token}`);
    const key = token.slice(2);
    if (key.startsWith("no-")) {
      options[key.slice(3)] = false;
      continue;
    }
    const next = rest[index + 1];
    if (!next || next.startsWith("--")) {
      options[key] = true;
      continue;
    }
    options[key] = next;
    index += 1;
  }
  return { command, options };
}

function parseSourceFromBranch(branch) {
  const match = /^backflow\/community-(\d+)$/.exec(branch);
  return match ? Number(match[1]) : null;
}

function normalizePath(value) {
  return String(value).replaceAll("\\", "/").replace(/^\.\//, "");
}

export function validateChangedPaths(paths) {
  const errors = [];
  const folded = new Map();
  for (const rawPath of paths) {
    const path = normalizePath(rawPath);
    if (!path || path.includes("\uFFFD")) {
      errors.push(`文件路径不是有效 UTF-8：${JSON.stringify(rawPath)}`);
      continue;
    }
    const key = path.toLocaleLowerCase("en-US");
    const previous = folded.get(key);
    if (previous && previous !== path) {
      errors.push(`文件路径仅大小写不同：${previous} / ${path}`);
    } else {
      folded.set(key, path);
    }
    for (const part of path.split("/")) {
      const stem = part.split(".", 1)[0].toLocaleLowerCase("en-US");
      if (/[<>:"|?*\u0000-\u001f]/u.test(part)) {
        errors.push(`Windows 不可用文件名：${path}`);
        break;
      }
      if (/[ .]$/u.test(part) || WINDOWS_RESERVED_NAMES.has(stem)) {
        errors.push(`跨平台不安全文件名：${path}`);
        break;
      }
    }
  }
  return [...new Set(errors)];
}

export function buildSchedule(items) {
  if (!Array.isArray(items)) fail("调度输入必须是数组");
  const normalized = items.map((item, index) => ({
    source: Number(item.source),
    issue: item.issue == null ? null : Number(item.issue),
    paths: [...new Set((item.paths || []).map(normalizePath))].sort(),
    release: Boolean(item.release),
    depends_on: [...new Set((item.depends_on || []).map(Number))],
    order: index,
  }));
  const bySource = new Map(normalized.map((item) => [item.source, item]));
  if (
    bySource.size !== normalized.length ||
    normalized.some((item) => !Number.isInteger(item.source) || item.source <= 0)
  ) {
    fail("每个调度项都必须有唯一的正整数 source");
  }

  for (const item of normalized) {
    for (const dependency of item.depends_on) {
      if (!Number.isInteger(dependency) || dependency <= 0) {
        fail(`回流 #${item.source} 包含无效依赖：${dependency}`);
      }
      if (!bySource.has(dependency)) fail(`回流 #${item.source} 的依赖 #${dependency} 不在计划中`);
      if (dependency === item.source) fail(`回流 #${item.source} 不能依赖自身`);
    }
  }

  // 同批次修改相同路径时保持来源合并顺序，避免并发 PR 反复 rebase。
  // Release 即使较早合并，也始终让位于功能回流。
  for (let right = 0; right < normalized.length; right += 1) {
    const current = normalized[right];
    const currentPaths = new Set(current.paths);
    for (let left = 0; left < right; left += 1) {
      const earlier = normalized[left];
      if (earlier.paths.some((path) => currentPaths.has(path))) {
        if (earlier.release && !current.release) {
          earlier.depends_on.push(current.source);
        } else {
          current.depends_on.push(earlier.source);
        }
      }
    }
  }
  // Release/版本回流最后处理，避免功能 PR 合入后再次修改版本合同。
  const featureSources = normalized.filter((item) => !item.release).map((item) => item.source);
  for (const current of normalized) {
    if (current.release) current.depends_on.push(...featureSources);
    current.depends_on = [...new Set(current.depends_on)].sort((a, b) => a - b);
  }

  const remaining = new Map(normalized.map((item) => [item.source, item]));
  const completed = new Set();
  const waves = [];
  while (remaining.size) {
    const ready = [...remaining.values()]
      .filter((item) => item.depends_on.every((source) => completed.has(source)))
      .sort((a, b) => a.order - b.order);
    if (!ready.length) {
      const cycle = [...remaining.values()].map((item) => `#${item.source}->[${item.depends_on.join(",")}]`);
      fail(`回流依赖存在环：${cycle.join(" ")}`);
    }
    waves.push(ready.map((item) => item.source));
    for (const item of ready) {
      remaining.delete(item.source);
      completed.add(item.source);
    }
  }

  return {
    schema: 1,
    waves,
    items: normalized.map(({ order: _order, ...item }) => item),
  };
}

export function encodeProvenanceMarker(provenance) {
  const encoded = Buffer.from(JSON.stringify(provenance), "utf8").toString("base64url");
  return `<!-- community-backflow-provenance:${encoded} -->`;
}

export function parseProvenanceMarker(body) {
  const match = PROVENANCE_MARKER_RE.exec(String(body || ""));
  if (!match) return null;
  try {
    return JSON.parse(Buffer.from(match[1], "base64url").toString("utf8"));
  } catch {
    fail("PR 正文中的回流 provenance 无法解析");
  }
}

function changedPaths(base, head) {
  const output = execFileSync("git", ["diff", "--name-only", "-z", base, head], {
    encoding: "utf8",
  });
  return output.split("\0").filter(Boolean).map(normalizePath);
}

export function createProvenance({ base, head, source, paths, platform = process.platform }) {
  return {
    schema: PROVENANCE_SCHEMA,
    cli_version: CLI_VERSION,
    processor_os: platformName(platform),
    source: source == null ? null : Number(source),
    base_sha: base,
    head_sha: head,
    changed_files: paths.length,
    checks: ["git-diff-check", "portable-paths"],
    created_at: new Date().toISOString(),
  };
}

function preflight(options = {}) {
  const base = String(options.base || process.env.BASE_SHA || "origin/main");
  const head = String(options.head || process.env.HEAD_SHA || "HEAD");
  const branch = runGit(["branch", "--show-current"]);
  const source = options.source || process.env.SOURCE_NUMBER || parseSourceFromBranch(branch);

  runGit(["diff", "--check", base, head]);
  const paths = changedPaths(base, head);
  const pathErrors = validateChangedPaths(paths);
  if (pathErrors.length) fail(pathErrors.join("\n"));

  const resolvedBase = runGit(["rev-parse", base]);
  const resolvedHead = runGit(["rev-parse", head]);
  const provenance = createProvenance({
    base: resolvedBase,
    head: resolvedHead,
    source,
    paths,
  });

  if (options.output) {
    writeFileSync(resolve(String(options.output)), `${JSON.stringify(provenance, null, 2)}\n`, "utf8");
  }
  if (process.env.GITHUB_OUTPUT) {
    writeFileSync(process.env.GITHUB_OUTPUT, `processor_os=${provenance.processor_os}\n`, {
      encoding: "utf8",
      flag: "a",
    });
  }
  return provenance;
}

export function verifyPullBody({ body, expectedHead, expectedSource }) {
  const sourceMatch = BACKFLOW_MARKER_RE.exec(String(body || ""));
  if (!sourceMatch) fail("PR 正文缺少 Community 回流来源 marker");
  const provenance = parseProvenanceMarker(body);
  if (!provenance) fail("PR 正文缺少实际处理平台的 provenance marker");
  if (provenance.schema !== PROVENANCE_SCHEMA) fail(`不支持的 provenance schema：${provenance.schema}`);
  if (!new Set(["windows", "macos", "linux"]).has(provenance.processor_os)) {
    fail(`无效的 processor_os：${provenance.processor_os}`);
  }
  if (expectedHead && provenance.head_sha !== expectedHead) {
    fail(`provenance head ${provenance.head_sha} 与 PR head ${expectedHead} 不一致`);
  }
  const markerSource = Number(sourceMatch[1]);
  if (provenance.source !== markerSource) fail("provenance 来源 PR 与来源 marker 不一致");
  if (expectedSource && markerSource !== Number(expectedSource)) fail("来源 PR 与预期不一致");
  return provenance;
}

function credentialFromGcm(username) {
  const result = spawnSync("git", ["credential", "fill"], {
    input: `protocol=https\nhost=github.com\nusername=${username}\n\n`,
    encoding: "utf8",
  });
  if (result.status !== 0) fail("无法从 Git Credential Manager 取得 GitHub 凭据");
  const password = result.stdout
    .split(/\r?\n/u)
    .find((line) => line.startsWith("password="))
    ?.slice("password=".length);
  if (!password) fail(`Git Credential Manager 中没有名为 ${username} 的 GitHub 凭据`);
  return password;
}

function githubToken(options) {
  return (
    process.env.GITHUB_TOKEN ||
    process.env.GH_TOKEN ||
    credentialFromGcm(String(options.credential || "zhuowp"))
  );
}

async function githubRequest(token, method, path, payload) {
  const body = payload == null ? undefined : Buffer.from(JSON.stringify(payload), "utf8");
  const response = await fetch(`${process.env.GITHUB_API_URL || "https://api.github.com"}${path}`, {
    method,
    headers: {
      Accept: "application/vnd.github+json",
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json; charset=utf-8",
      "X-GitHub-Api-Version": "2022-11-28",
    },
    body,
  });
  const text = await response.text();
  if (!response.ok) fail(`GitHub API ${method} ${path} 失败：HTTP ${response.status} ${text}`);
  return text ? JSON.parse(text) : null;
}

function appendMarker(body, markerRegex, marker) {
  const normalized = String(body || "").trimEnd();
  return markerRegex.test(normalized)
    ? `${normalized.replace(markerRegex, marker)}\n`
    : `${normalized}\n\n${marker}\n`;
}

async function submit(options) {
  for (const required of ["repo", "issue", "source", "title", "body-file"]) {
    if (!options[required]) fail(`submit 缺少 --${required}`);
  }
  const branch = runGit(["branch", "--show-current"]);
  const expectedBranch = `backflow/community-${options.source}`;
  if (branch !== expectedBranch) fail(`当前分支必须是 ${expectedBranch}，实际为 ${branch || "detached HEAD"}`);
  const targetBase = String(options.base || "main");
  const localBase = options["base-ref"] || (targetBase.includes("/") ? targetBase : `origin/${targetBase}`);
  const provenance = preflight({ base: localBase, source: options.source });
  const marker = encodeProvenanceMarker(provenance);
  let body = readFileSync(resolve(String(options["body-file"])), "utf8");
  if (!BACKFLOW_MARKER_RE.test(body)) {
    body = `${body.trimEnd()}\n\n<!-- community-backflow:Pinvou/pinvou-agent#${options.source} -->\n`;
  }
  body = appendMarker(body, PROVENANCE_MARKER_RE, marker);

  const token = githubToken(options);
  const [owner] = String(options.repo).split("/");
  const existing = await githubRequest(
    token,
    "GET",
    `/repos/${options.repo}/pulls?state=open&head=${encodeURIComponent(`${owner}:${branch}`)}`,
  );
  const payload = { title: String(options.title), body, base: targetBase };
  let pull;
  if (existing.length) {
    // 先更新 provenance，再 push 新 head；随后 synchronize 事件读取的一定是新正文，
    // 无需为所有 PR body 编辑额外重跑整套产品 CI。
    pull = await githubRequest(
      token,
      "PATCH",
      `/repos/${options.repo}/pulls/${existing[0].number}`,
      payload,
    );
    if (options.push !== false) runGit(["push", "--set-upstream", "origin", branch]);
  } else {
    if (options.push !== false) runGit(["push", "--set-upstream", "origin", branch]);
    pull = await githubRequest(token, "POST", `/repos/${options.repo}/pulls`, {
      ...payload,
      head: branch,
    });
  }

  // 写后回读必须逐字一致，防止任意处理平台在 API 边界破坏非 ASCII 文本。
  const roundTrip = await githubRequest(token, "GET", `/repos/${options.repo}/pulls/${pull.number}`);
  if (roundTrip.title !== payload.title || roundTrip.body !== payload.body) {
    fail("PR 标题或正文写后回读不一致");
  }
  await githubRequest(token, "POST", `/repos/${options.repo}/issues/${options.issue}/labels`, {
    labels: ["backflow:processing"],
  });
  const commentBody = `已创建或更新回流 PR：${pull.html_url}\n\n当前 head：\`${provenance.head_sha}\`\n处理平台：\`${provenance.processor_os}\``;
  const comment = await githubRequest(
    token,
    "POST",
    `/repos/${options.repo}/issues/${options.issue}/comments`,
    { body: commentBody },
  );
  const commentRoundTrip = await githubRequest(
    token,
    "GET",
    `/repos/${options.repo}/issues/comments/${comment.id}`,
  );
  if (commentRoundTrip.body !== commentBody) fail("Issue 评论写后回读不一致");
  return { url: pull.html_url, number: pull.number, provenance };
}

function help() {
  return `用法：
  node scripts/community-backflow.mjs plan --input queue.json [--output plan.json]
  node scripts/community-backflow.mjs preflight [--base origin/main] [--head HEAD] [--source N]
  node scripts/community-backflow.mjs verify --body-file pr-body.txt --head SHA [--source N]
  node scripts/community-backflow.mjs submit --repo OWNER/REPO --issue N --source N --title TITLE --body-file FILE [--credential zhuowp] [--no-push]
`;
}

async function main(argv = process.argv.slice(2)) {
  const { command, options } = parseArgs(argv);
  if (command === "help" || command === "--help") {
    process.stdout.write(help());
    return;
  }
  if (command === "plan") {
    if (!options.input) fail("plan 缺少 --input");
    const plan = buildSchedule(JSON.parse(readFileSync(resolve(String(options.input)), "utf8")));
    const output = `${JSON.stringify(plan, null, 2)}\n`;
    if (options.output) writeFileSync(resolve(String(options.output)), output, "utf8");
    process.stdout.write(output);
    return;
  }
  if (command === "preflight") {
    process.stdout.write(`${JSON.stringify(preflight(options), null, 2)}\n`);
    return;
  }
  if (command === "verify") {
    const body = options["body-file"]
      ? readFileSync(resolve(String(options["body-file"])), "utf8")
      : process.env.GITHUB_PR_BODY;
    process.stdout.write(`${JSON.stringify(verifyPullBody({
      body,
      expectedHead: options.head || process.env.HEAD_SHA,
      expectedSource: options.source || process.env.SOURCE_NUMBER,
    }), null, 2)}\n`);
    return;
  }
  if (command === "submit") {
    process.stdout.write(`${JSON.stringify(await submit(options), null, 2)}\n`);
    return;
  }
  fail(`未知命令：${command}\n${help()}`);
}

const isEntryPoint = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isEntryPoint) {
  main().catch((error) => {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  });
}
