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
export const PLAN_MARKER_RE =
  /<!--\s*community-backflow-plan:(\{[^\r\n]*\})\s*-->/;
export const AUTO_FINALIZED_MARKER_PREFIX = "community-backflow-auto-finalized";
export const AUTO_RELEASE_DEFERRED_MARKER_PREFIX = "community-backflow-auto-release-deferred";
export const AUTOMATION_ENV = "COMMUNITY_BACKFLOW_AUTOMATION_ENABLED";
export const SOURCE_REPO = "Pinvou/pinvou-agent";

const RETRYABLE_GITHUB_STATUS = new Set([408, 429, 500, 502, 503, 504]);

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

export function parseQueueIssues(issues) {
  if (!Array.isArray(issues)) fail("Issue 队列必须是数组");
  const items = issues.map((issue) => {
    const issueNumber = Number(issue?.number);
    const match = PLAN_MARKER_RE.exec(String(issue?.body || ""));
    if (!match) fail(`Issue #${issueNumber || "unknown"} 缺少 community-backflow-plan marker`);
    let marker;
    try {
      marker = JSON.parse(match[1]);
    } catch {
      fail(`Issue #${issueNumber || "unknown"} 的 community-backflow-plan marker 无法解析`);
    }
    return {
      source: Number(marker.source),
      issue: issueNumber,
      paths: Array.isArray(marker.paths) ? marker.paths : [],
      release: Boolean(marker.release),
      depends_on: Array.isArray(marker.depends_on) ? marker.depends_on : [],
    };
  });
  const openSources = new Set(items.map((item) => item.source));
  // marker 记录创建批次的完整依赖；已合并并关闭的前置 Issue 不再出现在开放队列中，
  // 聚合时把这些依赖视为已完成，避免下一轮 plan 因“依赖不在计划中”而失效。
  return items.map((item) => ({
    ...item,
    depends_on: item.depends_on.filter((source) => openSources.has(Number(source))),
  }));
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
    env: {
      ...process.env,
      GCM_INTERACTIVE: "Never",
      GIT_TERMINAL_PROMPT: "0",
    },
  });
  if (result.status !== 0) fail("无法从 Git Credential Manager 取得 GitHub 凭据");
  const password = result.stdout
    .split(/\r?\n/u)
    .find((line) => line.startsWith("password="))
    ?.slice("password=".length);
  if (!password) fail(`Git Credential Manager 中没有名为 ${username} 的 GitHub 凭据`);
  return password;
}

export function githubToken(options = {}) {
  const environmentToken = process.env.GITHUB_TOKEN || process.env.GH_TOKEN;
  if (environmentToken) return environmentToken;
  if (typeof options.credential === "string" && options.credential.trim()) {
    return credentialFromGcm(options.credential.trim());
  }
  fail("缺少 GITHUB_TOKEN/GH_TOKEN；如需使用 GCM，请显式传入 --credential <github-username>");
}

function retryDelayMs(response, attempt) {
  const retryAfter = Number(response?.headers?.get("retry-after"));
  if (Number.isFinite(retryAfter) && retryAfter >= 0) return Math.min(retryAfter * 1000, 30_000);
  const base = Number(process.env.COMMUNITY_BACKFLOW_RETRY_BASE_MS || 500);
  return Math.max(0, Number.isFinite(base) ? base : 500) * (2 ** (attempt - 1));
}

function sleep(milliseconds) {
  return new Promise((resolvePromise) => setTimeout(resolvePromise, milliseconds));
}

export async function githubRequest(token, method, path, payload) {
  const body = payload == null ? undefined : Buffer.from(JSON.stringify(payload), "utf8");
  const timeout = Number(process.env.COMMUNITY_BACKFLOW_GITHUB_TIMEOUT_MS || 15_000);
  const timeoutMs = Number.isFinite(timeout) && timeout > 0 ? timeout : 15_000;
  const maxAttempts = 3;
  let lastError;

  for (let attempt = 1; attempt <= maxAttempts; attempt += 1) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    let response;
    let text;
    try {
      response = await fetch(`${process.env.GITHUB_API_URL || "https://api.github.com"}${path}`, {
        method,
        headers: {
          Accept: "application/vnd.github+json",
          Authorization: `Bearer ${token}`,
          "Content-Type": "application/json; charset=utf-8",
          "X-GitHub-Api-Version": "2022-11-28",
        },
        body,
        signal: controller.signal,
      });
      // 保持 timeout 覆盖响应体读取，避免只收到 headers 后 body 无限挂起。
      text = await response.text();
    } catch (error) {
      lastError = error;
      if (attempt === maxAttempts) {
        fail(`GitHub API ${method} ${path} 连续 ${maxAttempts} 次网络请求失败：${error.message || error}`);
      }
      await sleep(retryDelayMs(null, attempt));
      continue;
    } finally {
      clearTimeout(timer);
    }

    if (response.ok) return text ? JSON.parse(text) : null;
    const rateLimited = response.status === 403 && response.headers.get("x-ratelimit-remaining") === "0";
    if ((RETRYABLE_GITHUB_STATUS.has(response.status) || rateLimited) && attempt < maxAttempts) {
      await sleep(retryDelayMs(response, attempt));
      continue;
    }
    fail(`GitHub API ${method} ${path} 失败：HTTP ${response.status} ${text}`);
  }
  fail(`GitHub API ${method} ${path} 失败：${lastError || "未知错误"}`);
}

export async function githubGraphqlRequest(token, query, variables = {}) {
  const result = await githubRequest(token, "POST", "/graphql", { query, variables });
  if (Array.isArray(result?.errors) && result.errors.length) {
    fail(`GitHub GraphQL 失败：${result.errors.map((error) => error.message).join("；")}`);
  }
  return result?.data;
}

function labelNames(issue) {
  return (issue?.labels || []).map((label) =>
    typeof label === "string" ? label : String(label?.name || ""));
}

export function parseIssuePlan(issue) {
  const match = PLAN_MARKER_RE.exec(String(issue?.body || ""));
  if (!match) return null;
  let marker;
  try {
    marker = JSON.parse(match[1]);
  } catch {
    fail(`Issue #${issue?.number || "unknown"} 的 community-backflow-plan marker 无法解析`);
  }
  if (marker.schema !== 1 || !Number.isInteger(Number(marker.source)) || Number(marker.source) <= 0) {
    fail(`Issue #${issue?.number || "unknown"} 的 community-backflow-plan marker 无效`);
  }
  return {
    source: Number(marker.source),
    release: Boolean(marker.release),
    depends_on: Array.isArray(marker.depends_on) ? marker.depends_on.map(Number) : [],
  };
}

export function parseAutomationEvent(event) {
  if (event?.workflow_run) {
    const run = event.workflow_run;
    if (
      event.action !== "completed" ||
      run.name !== "PR Check" ||
      run.event !== "pull_request" ||
      run.conclusion !== "success"
    ) {
      return { action: "skip", reason: "workflow-run-not-successful-required-gate" };
    }
    const pulls = Array.isArray(run.pull_requests) ? run.pull_requests : [];
    if (pulls.length !== 1 || !Number.isInteger(Number(pulls[0]?.number))) {
      return { action: "skip", reason: "workflow-run-pr-not-unique" };
    }
    return {
      action: "advance",
      pullNumber: Number(pulls[0].number),
      expectedHead: String(run.head_sha || ""),
    };
  }
  if (event?.pull_request && event.action === "closed") {
    if (!event.pull_request.merged) return { action: "skip", reason: "pull-request-not-merged" };
    return {
      action: "finalize",
      pullNumber: Number(event.pull_request.number),
      expectedHead: String(event.pull_request.head?.sha || ""),
    };
  }
  return { action: "skip", reason: "unsupported-event" };
}

async function collectAutomationIssues(request, token, repo) {
  const byNumber = new Map();
  for (const label of ["backflow:pending", "backflow:processing", "backflow:synced"]) {
    for (let page = 1; ; page += 1) {
      const issues = await request(
        token,
        "GET",
        `/repos/${repo}/issues?state=all&labels=${encodeURIComponent(label)}&per_page=100&page=${page}`,
      );
      for (const issue of issues) {
        if (!issue.pull_request) byNumber.set(Number(issue.number), issue);
      }
      if (issues.length < 100) break;
    }
  }
  return [...byNumber.values()];
}

async function readAutomationContext({ request, token, repo, pullNumber, expectedHead }) {
  const pull = await request(token, "GET", `/repos/${repo}/pulls/${pullNumber}`);
  const source = parseSourceFromBranch(String(pull?.head?.ref || ""));
  if (!source) fail(`PR #${pullNumber} 不是 backflow/community-<source> 分支`);
  if (String(pull?.base?.ref || "") !== "main") fail(`PR #${pullNumber} 的 base 不是 main`);
  const targetRepo = String(repo).toLocaleLowerCase("en-US");
  if (
    String(pull?.base?.repo?.full_name || "").toLocaleLowerCase("en-US") !== targetRepo ||
    String(pull?.head?.repo?.full_name || "").toLocaleLowerCase("en-US") !== targetRepo
  ) {
    fail(`PR #${pullNumber} 不是 Official 同仓 PR`);
  }
  const head = String(pull?.head?.sha || "");
  if (!head || (expectedHead && head !== expectedHead)) {
    fail(`事件 head ${expectedHead || "missing"} 与 PR #${pullNumber} 当前 head ${head || "missing"} 不一致`);
  }
  if (pull.draft) fail(`PR #${pullNumber} 仍是 Draft`);
  if (!pull.merged && pull.mergeable !== true) {
    fail(`PR #${pullNumber} 尚未确认可合并`);
  }
  if (pull.mergeable_state === "dirty") {
    fail(`PR #${pullNumber} 存在合并冲突`);
  }

  const provenance = verifyPullBody({ body: pull.body, expectedHead: head, expectedSource: source });
  const communityPull = await request(token, "GET", `/repos/${SOURCE_REPO}/pulls/${source}`);
  if (!communityPull?.merged || !communityPull?.merged_at) {
    fail(`来源 Community PR #${source} 不是 merged 状态`);
  }

  const issues = await collectAutomationIssues(request, token, repo);
  const matches = issues.filter((issue) => {
    const plan = parseIssuePlan(issue);
    const issueSource = BACKFLOW_MARKER_RE.exec(String(issue?.body || ""));
    return plan?.source === source && Number(issueSource?.[1]) === source;
  });
  if (matches.length !== 1) {
    fail(`来源 Community PR #${source} 必须关联唯一 Official Issue，实际 ${matches.length} 个`);
  }
  const issue = matches[0];
  const plan = parseIssuePlan(issue);
  const labels = labelNames(issue);
  if (labels.includes("backflow:blocked")) fail(`Official Issue #${issue.number} 带有 backflow:blocked`);
  if (labels.includes("backflow:dependency-wait")) {
    fail(`Official Issue #${issue.number} 仍在等待前置回流`);
  }

  const checkRuns = [];
  for (let page = 1; ; page += 1) {
    const checks = await request(
      token,
      "GET",
      `/repos/${repo}/commits/${encodeURIComponent(head)}/check-runs?per_page=100&page=${page}`,
    );
    const current = checks?.check_runs || [];
    checkRuns.push(...current);
    if (current.length < 100) break;
  }
  const requiredGate = checkRuns.find((check) =>
    check.name === "required-gate" &&
    check.head_sha === head &&
    check.status === "completed" &&
    check.conclusion === "success");
  if (!requiredGate) fail(`PR #${pullNumber} 当前 head ${head} 没有成功的 required-gate`);

  return { pull, source, head, provenance, communityPull, issue, plan, labels, requiredGate };
}

async function enqueueBackflow({ graphql, token, context, dryRun }) {
  if (context.pull.state !== "open" || context.pull.merged) {
    return { status: "skipped", reason: "pull-request-not-open" };
  }
  if (context.issue.state !== "open" || !context.labels.includes("backflow:processing")) {
    fail(`Official Issue #${context.issue.number} 不是开放的 processing 状态`);
  }
  if (!context.pull.node_id) fail(`PR #${context.pull.number} 缺少 GraphQL node id`);
  const state = await graphql(token, `
    query BackflowPullState($id: ID!) {
      node(id: $id) {
        ... on PullRequest {
          mergeQueueEntry { id }
          autoMergeRequest { enabledAt }
        }
      }
    }
  `, { id: context.pull.node_id });
  if (state?.node?.mergeQueueEntry || state?.node?.autoMergeRequest) {
    return { status: "already-enabled", pull: context.pull.number, head: context.head };
  }
  if (dryRun) {
    return { status: "dry-run", action: "enqueue", pull: context.pull.number, head: context.head };
  }
  await graphql(token, `
    mutation EnqueueBackflowPull($id: ID!) {
      enqueuePullRequest(input: { pullRequestId: $id }) {
        mergeQueueEntry { id }
      }
    }
  `, { id: context.pull.node_id });
  return { status: "enqueued", pull: context.pull.number, head: context.head };
}

function finalizedMarker(context) {
  return `<!-- ${AUTO_FINALIZED_MARKER_PREFIX}:${SOURCE_REPO}#${context.source}:${context.pull.merge_commit_sha} -->`;
}

function releaseDeferredMarker(context) {
  return `<!-- ${AUTO_RELEASE_DEFERRED_MARKER_PREFIX}:${SOURCE_REPO}#${context.source}:${context.pull.merge_commit_sha} -->`;
}

async function deleteBackflowBranch(request, token, repo, branch) {
  const ref = branch.split("/").map(encodeURIComponent).join("/");
  try {
    await request(token, "DELETE", `/repos/${repo}/git/refs/heads/${ref}`);
    return true;
  } catch (error) {
    if (/HTTP (404|422)\b/u.test(String(error?.message || error))) return false;
    throw error;
  }
}

async function collectIssueComments(request, token, repo, issueNumber) {
  const comments = [];
  for (let page = 1; ; page += 1) {
    const current = await request(
      token,
      "GET",
      `/repos/${repo}/issues/${issueNumber}/comments?per_page=100&page=${page}`,
    );
    comments.push(...current);
    if (current.length < 100) return comments;
  }
}

async function finalizeBackflow({ request, token, repo, context, dryRun }) {
  if (!context.pull.merged || !context.pull.merge_commit_sha) {
    return { status: "skipped", reason: "pull-request-not-merged" };
  }
  if (context.plan.release) {
    const marker = releaseDeferredMarker(context);
    if (!dryRun) {
      const comments = await collectIssueComments(request, token, repo, context.issue.number);
      if (!comments.some((comment) => String(comment.body || "").includes(marker))) {
        const body = `${marker}\nRelease 回流 PR 已合并，但自动器不会跳过固件构建契约。` +
          `请按 Release 回流流程核对 main 版本、复用或触发 \`release-packages.yml\`，` +
          `记录 run URL 后再标记 synced 并关闭。\n\n` +
          `- PR：${context.pull.html_url}\n` +
          `- merge commit：\`${context.pull.merge_commit_sha}\``;
        const comment = await request(
          token,
          "POST",
          `/repos/${repo}/issues/${context.issue.number}/comments`,
          { body },
        );
        const roundTrip = await request(token, "GET", `/repos/${repo}/issues/comments/${comment.id}`);
        if (roundTrip.body !== body) fail("Release 延后评论写后回读不一致");
      }
    }
    return {
      status: dryRun ? "dry-run" : "deferred-release",
      pull: context.pull.number,
      issue: context.issue.number,
      reason: "release-packages-contract-requires-follow-up",
    };
  }
  const marker = finalizedMarker(context);
  const synced = context.labels.includes("backflow:synced");
  if (context.issue.state === "closed" && synced) {
    return { status: "already-finalized", pull: context.pull.number, issue: context.issue.number };
  }
  const nextLabels = [
    ...context.labels.filter((label) => !label.startsWith("backflow:")),
    "backflow:synced",
  ];
  const commentBody = `${marker}\n回流 PR 已合并并自动收尾：${context.pull.html_url}\n\n` +
    `- 来源：${SOURCE_REPO}#${context.source}\n` +
    `- PR head：\`${context.head}\`\n` +
    `- merge commit：\`${context.pull.merge_commit_sha}\`\n` +
    `- required-gate：${context.requiredGate.html_url || "success"}`;
  if (dryRun) {
    return {
      status: "dry-run",
      action: "finalize",
      pull: context.pull.number,
      issue: context.issue.number,
      labels: nextLabels,
    };
  }

  await deleteBackflowBranch(request, token, repo, context.pull.head.ref);
  const comments = await collectIssueComments(request, token, repo, context.issue.number);
  if (!comments.some((comment) => String(comment.body || "").includes(marker))) {
    const comment = await request(
      token,
      "POST",
      `/repos/${repo}/issues/${context.issue.number}/comments`,
      { body: commentBody },
    );
    const roundTrip = await request(token, "GET", `/repos/${repo}/issues/comments/${comment.id}`);
    if (roundTrip.body !== commentBody) fail("自动收尾 Issue 评论写后回读不一致");
  }
  await request(token, "PATCH", `/repos/${repo}/issues/${context.issue.number}`, {
    labels: nextLabels,
    state: "closed",
    state_reason: "completed",
  });
  return { status: "finalized", pull: context.pull.number, issue: context.issue.number };
}

export async function runAutomation({
  event,
  repo,
  token,
  dryRun = false,
  enabled = process.env[AUTOMATION_ENV],
  request = githubRequest,
  graphql = githubGraphqlRequest,
}) {
  if (enabled !== "true") return { status: "skipped", reason: "automation-disabled" };
  if (!repo || !/^[-A-Za-z0-9_.]+\/[-A-Za-z0-9_.]+$/u.test(repo)) fail("automation 缺少有效 --repo");
  if (!token) fail("automation 启用后缺少 GITHUB_TOKEN/GH_TOKEN");
  const parsed = parseAutomationEvent(event);
  if (parsed.action === "skip") return { status: "skipped", reason: parsed.reason };
  const context = await readAutomationContext({
    request,
    token,
    repo,
    pullNumber: parsed.pullNumber,
    expectedHead: parsed.expectedHead,
  });
  if (parsed.action === "advance") return enqueueBackflow({ graphql, token, context, dryRun });
  return finalizeBackflow({ request, token, repo, context, dryRun });
}

async function automation(options) {
  if (!options["event-file"]) fail("automation 缺少 --event-file");
  const event = JSON.parse(readFileSync(resolve(String(options["event-file"])), "utf8"));
  const enabled = process.env[AUTOMATION_ENV];
  const token = enabled === "true" ? githubToken(options) : null;
  return runAutomation({
    event,
    repo: String(options.repo || process.env.GITHUB_REPOSITORY || ""),
    token,
    dryRun: Boolean(options["dry-run"]),
    enabled,
  });
}

async function collectQueue(options) {
  if (!options.repo) fail("queue 缺少 --repo");
  const token = githubToken(options);
  const byNumber = new Map();
  for (const label of ["backflow:pending", "backflow:processing"]) {
    for (let page = 1; ; page += 1) {
      const issues = await githubRequest(
        token,
        "GET",
        `/repos/${options.repo}/issues?state=open&labels=${encodeURIComponent(label)}&per_page=100&page=${page}`,
      );
      for (const issue of issues) {
        if (!issue.pull_request) byNumber.set(Number(issue.number), issue);
      }
      if (issues.length < 100) break;
    }
  }
  const issues = [...byNumber.values()].sort((left, right) =>
    String(left.created_at || "").localeCompare(String(right.created_at || "")) ||
    Number(left.number) - Number(right.number));
  return parseQueueIssues(issues);
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
  node scripts/community-backflow.mjs queue --repo OWNER/REPO [--output queue.json] [--credential USERNAME]
  node scripts/community-backflow.mjs plan --input queue.json [--output plan.json]
  node scripts/community-backflow.mjs preflight [--base origin/main] [--head HEAD] [--source N]
  node scripts/community-backflow.mjs verify --body-file pr-body.txt --head SHA [--source N]
  node scripts/community-backflow.mjs submit --repo OWNER/REPO --issue N --source N --title TITLE --body-file FILE [--credential USERNAME] [--no-push]
  node scripts/community-backflow.mjs automation --repo OWNER/REPO --event-file EVENT.json [--dry-run]
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
  if (command === "queue") {
    const output = `${JSON.stringify(await collectQueue(options), null, 2)}\n`;
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
  if (command === "automation") {
    process.stdout.write(`${JSON.stringify(await automation(options), null, 2)}\n`);
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
