import assert from "node:assert/strict";
import test from "node:test";

import {
  createProvenance,
  encodeProvenanceMarker,
  parseAutomationEvent,
  runAutomation,
} from "../community-backflow.mjs";

const repo = "Pinvou/pinvou3";
const source = 393;
const pullNumber = 923;
const issueNumber = 915;
const head = "9".repeat(40);
const merge = "a".repeat(40);

function fixture({ release = false, merged = false } = {}) {
  const provenance = createProvenance({
    base: "8".repeat(40),
    head,
    source,
    paths: ["文档/说明.md"],
    platform: "win32",
  });
  const pull = {
    number: pullNumber,
    node_id: "PR_node",
    state: merged ? "closed" : "open",
    merged,
    merged_at: merged ? "2026-09-02T08:00:00Z" : null,
    merge_commit_sha: merged ? merge : null,
    draft: false,
    mergeable: true,
    mergeable_state: "clean",
    html_url: `https://github.com/${repo}/pull/${pullNumber}`,
    base: { ref: "main", repo: { full_name: repo } },
    head: { ref: `backflow/community-${source}`, sha: head, repo: { full_name: repo } },
    body: `自动回流\n<!-- community-backflow:Pinvou/pinvou-agent#${source} -->\n${encodeProvenanceMarker(provenance)}`,
  };
  const issue = {
    number: issueNumber,
    state: "open",
    labels: [{ name: "backflow:pending" }, { name: "backflow:processing" }, { name: "产品" }],
    body: `<!-- community-backflow:Pinvou/pinvou-agent#${source} -->\n` +
      `<!-- community-backflow-plan:{"schema":1,"source":${source},"depends_on":[],"release":${release}} -->`,
  };
  return { pull, issue };
}

function workflowEvent() {
  return {
    action: "completed",
    workflow_run: {
      name: "PR Check",
      event: "pull_request",
      conclusion: "success",
      head_sha: head,
      pull_requests: [{ number: pullNumber }],
    },
  };
}

function mergedEvent() {
  return {
    action: "closed",
    pull_request: { number: pullNumber, merged: true, head: { sha: head } },
  };
}

function fakeApi(data) {
  const calls = [];
  let postedComment = "";
  const request = async (_token, method, path, payload) => {
    calls.push({ method, path, payload });
    if (method === "GET" && path === `/repos/${repo}/pulls/${pullNumber}`) return data.pull;
    if (method === "GET" && path === `/repos/Pinvou/pinvou-agent/pulls/${source}`) {
      return data.communityPull || { merged: true, merged_at: "2026-09-01T08:00:00Z" };
    }
    if (method === "GET" && path.includes(`/repos/${repo}/issues?`)) {
      return path.includes("backflow%3Asynced") ? [] : [data.issue];
    }
    if (method === "GET" && path === `/repos/${repo}/commits/${head}/check-runs?per_page=100&page=1`) {
      return data.checks || {
        check_runs: [{
          name: "required-gate",
          head_sha: head,
          status: "completed",
          conclusion: "success",
          html_url: "https://github.com/checks/required-gate",
        }],
      };
    }
    if (method === "DELETE" && path.includes("/git/refs/heads/backflow/community-393")) return null;
    if (method === "GET" && path.includes(`/issues/${issueNumber}/comments?per_page=100&page=`)) return [];
    if (method === "POST" && path.endsWith(`/issues/${issueNumber}/comments`)) {
      postedComment = payload.body;
      return { id: 42 };
    }
    if (method === "GET" && path.endsWith("/issues/comments/42")) return { body: postedComment };
    if (method === "PATCH" && path.endsWith(`/issues/${issueNumber}`)) return { ok: true };
    throw new Error(`unexpected request: ${method} ${path}`);
  };
  return { request, calls, postedComment: () => postedComment };
}

test("事件解析只接受成功 PR Check 与已合并 pull_request_target", () => {
  assert.deepEqual(parseAutomationEvent(workflowEvent()), {
    action: "advance",
    pullNumber,
    expectedHead: head,
  });
  assert.equal(parseAutomationEvent({
    action: "completed",
    workflow_run: { name: "PR Check", event: "pull_request", conclusion: "failure", pull_requests: [] },
  }).action, "skip");
  assert.equal(parseAutomationEvent({
    action: "completed",
    workflow_run: {
      name: "PR Check", event: "push", conclusion: "success", pull_requests: [{ number: pullNumber }],
    },
  }).action, "skip");
  assert.equal(parseAutomationEvent({
    action: "closed",
    pull_request: { number: pullNumber, merged: false },
  }).action, "skip");
  assert.equal(parseAutomationEvent(mergedEvent()).action, "finalize");
});

test("未显式启用时安全跳过且不读取 GitHub", async () => {
  let touched = false;
  const result = await runAutomation({
    event: workflowEvent(),
    repo,
    token: null,
    enabled: undefined,
    request: async () => { touched = true; },
  });
  assert.deepEqual(result, { status: "skipped", reason: "automation-disabled" });
  assert.equal(touched, false);
});

test("成功 required-gate 对应当前 head 时进入 Merge Queue", async () => {
  const data = fixture();
  const api = fakeApi(data);
  const graphqlCalls = [];
  const graphql = async (_token, query, variables) => {
    graphqlCalls.push({ query, variables });
    if (query.includes("query BackflowPullState")) {
      return { node: { mergeQueueEntry: null, autoMergeRequest: null } };
    }
    return { enqueuePullRequest: { mergeQueueEntry: { id: "MQ_entry" } } };
  };
  const result = await runAutomation({
    event: workflowEvent(), repo, token: "token", enabled: "true", request: api.request, graphql,
  });
  assert.equal(result.status, "enqueued");
  assert.equal(graphqlCalls.length, 2);
  assert.match(graphqlCalls[1].query, /enqueuePullRequest/);
  assert.deepEqual(graphqlCalls[1].variables, { id: "PR_node" });
});

test("dry-run 完成全部校验但不入队", async () => {
  const data = fixture();
  const api = fakeApi(data);
  let mutation = false;
  const result = await runAutomation({
    event: workflowEvent(),
    repo,
    token: "token",
    enabled: "true",
    dryRun: true,
    request: api.request,
    graphql: async (_token, query) => {
      if (query.includes("mutation")) mutation = true;
      return { node: { mergeQueueEntry: null, autoMergeRequest: null } };
    },
  });
  assert.equal(result.status, "dry-run");
  assert.equal(result.action, "enqueue");
  assert.equal(mutation, false);
});

test("workflow_run head 必须等于 PR 当前 head", async () => {
  const data = fixture();
  const api = fakeApi(data);
  const event = workflowEvent();
  event.workflow_run.head_sha = "7".repeat(40);
  await assert.rejects(runAutomation({
    event,
    repo,
    token: "token",
    enabled: "true",
    request: api.request,
    graphql: async () => ({ node: {} }),
  }), /事件 head .* 与 PR .* 当前 head .* 不一致/);
});

test("已入队或已启用 auto-merge 时幂等跳过", async () => {
  const data = fixture();
  const api = fakeApi(data);
  const result = await runAutomation({
    event: workflowEvent(), repo, token: "token", enabled: "true", request: api.request,
    graphql: async () => ({ node: { mergeQueueEntry: { id: "existing" }, autoMergeRequest: null } }),
  });
  assert.equal(result.status, "already-enabled");
});

test("拒绝外部 head、错误 base、过期 gate、blocked Issue 与未合并来源", async (t) => {
  const cases = [
    ["外部 head", (data) => { data.pull.head.repo.full_name = "fork/pinvou3"; }, /不是 Official 同仓 PR/],
    ["错误 base", (data) => { data.pull.base.ref = "feature"; }, /base 不是 main/],
    ["过期 gate", (data) => { data.checks = { check_runs: [{ name: "required-gate", head_sha: "old", status: "completed", conclusion: "success" }] }; }, /没有成功的 required-gate/],
    ["Issue 来源 marker 不一致", (data) => { data.issue.body = data.issue.body.replace("#393", "#392"); }, /必须关联唯一 Official Issue/],
    ["blocked Issue", (data) => { data.issue.labels.push({ name: "backflow:blocked" }); }, /backflow:blocked/],
    ["未合并来源", (data) => { data.communityPull = { merged: false, merged_at: null }; }, /不是 merged 状态/],
  ];
  for (const [name, mutate, pattern] of cases) {
    await t.test(name, async () => {
      const data = fixture();
      mutate(data);
      const api = fakeApi(data);
      await assert.rejects(runAutomation({
        event: workflowEvent(), repo, token: "token", enabled: "true", request: api.request,
        graphql: async () => ({ node: {} }),
      }), pattern);
    });
  }
});

test("普通回流合并后删除分支、写 UTF-8 评论并关闭为 synced", async () => {
  const data = fixture({ merged: true });
  const api = fakeApi(data);
  const result = await runAutomation({
    event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request,
  });
  assert.equal(result.status, "finalized");
  assert.equal(api.calls.some((call) => call.method === "DELETE"), true);
  assert.match(api.postedComment(), /回流 PR 已合并并自动收尾/);
  const patch = api.calls.find((call) => call.method === "PATCH");
  assert.deepEqual(patch.payload.labels.sort(), ["backflow:synced", "产品"].sort());
  assert.equal(patch.payload.state, "closed");
});

test("Release 回流提示后续固件构建，不删除分支或错误关闭 Issue", async () => {
  const data = fixture({ release: true, merged: true });
  const api = fakeApi(data);
  const result = await runAutomation({
    event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request,
  });
  assert.equal(result.status, "deferred-release");
  assert.match(api.postedComment(), /不会跳过固件构建契约/);
  assert.equal(api.calls.some((call) => ["PATCH", "DELETE"].includes(call.method)), false);
});
