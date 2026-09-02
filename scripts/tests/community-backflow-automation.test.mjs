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
const advancedHead = "2".repeat(40);
const merge = "a".repeat(40);

function clone(value) {
  return structuredClone(value);
}

function httpError(status) {
  return new Error(`GitHub API 失败：HTTP ${status}`);
}

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
  const filePages = [[{ filename: release ? "VERSION" : "src/example.rs" }]];
  return { pull, issue, filePages };
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

function fakeApi(data, options = {}) {
  const calls = [];
  const postedComments = [];
  let nextCommentId = 42;
  let remainingCommentResponseLosses = Number(options.commentResponseLosses || 0);
  let branchSha = options.branchSha === undefined ? head : options.branchSha;
  const currentIssue = clone(data.issue);
  const remainingFailures = { ...(options.mutationFailures || {}) };
  const remainingAfterFailures = { ...(options.mutationFailuresAfter || {}) };
  let pullReads = 0;
  let issueReads = 0;
  const failMutation = (name) => {
    if ((remainingFailures[name] || 0) <= 0) return;
    remainingFailures[name] -= 1;
    throw httpError(503);
  };
  const failMutationAfter = (name) => {
    if ((remainingAfterFailures[name] || 0) <= 0) return;
    remainingAfterFailures[name] -= 1;
    throw httpError(503);
  };
  const request = async (_token, method, path, payload) => {
    calls.push({ method, path, payload });
    if (method === "GET" && path === `/repos/${repo}/pulls/${pullNumber}`) {
      pullReads += 1;
      const value = options.pullAtRead?.(pullReads, clone(data.pull)) || data.pull;
      return clone(value);
    }
    if (method === "GET" && path === `/repos/Pinvou/pinvou-agent/pulls/${source}`) {
      return clone(data.communityPull || { merged: true, merged_at: "2026-09-01T08:00:00Z" });
    }
    const filesMatch = path.match(/\/pulls\/393\/files\?per_page=100&page=(\d+)$/u);
    if (method === "GET" && filesMatch) {
      const page = Number(filesMatch[1]);
      const value = data.filePages?.[page - 1];
      return value === undefined ? [] : clone(value);
    }
    if (method === "GET" && path.includes(`/repos/${repo}/issues?`)) {
      const label = decodeURIComponent(new URL(`https://example.invalid${path}`).searchParams.get("labels") || "");
      return currentIssue.labels.some((current) => current.name === label) ? [clone(currentIssue)] : [];
    }
    if (method === "GET" && path === `/repos/${repo}/issues/${issueNumber}`) {
      issueReads += 1;
      if (options.advanceBranchAtIssueRead === issueReads) branchSha = advancedHead;
      const value = options.issueAtRead?.(issueReads, clone(currentIssue)) || currentIssue;
      return clone(value);
    }
    if (method === "GET" && path === `/repos/${repo}/commits/${head}/check-runs?per_page=100&page=1`) {
      return clone(data.checks || {
        check_runs: [{
          name: "required-gate",
          head_sha: head,
          status: "completed",
          conclusion: "success",
          html_url: "https://github.com/checks/required-gate",
        }],
      });
    }
    if (method === "GET" && path.includes(`/issues/${issueNumber}/comments?per_page=100&page=`)) {
      return clone(postedComments);
    }
    if (method === "POST" && path.endsWith(`/issues/${issueNumber}/comments`)) {
      const comment = { id: nextCommentId, body: payload.body };
      nextCommentId += 1;
      postedComments.push(comment);
      if (remainingCommentResponseLosses > 0) {
        remainingCommentResponseLosses -= 1;
        throw new TypeError("socket closed after server committed comment");
      }
      return { id: comment.id };
    }
    const commentMatch = path.match(/\/issues\/comments\/(\d+)$/u);
    if (method === "GET" && commentMatch) {
      const comment = postedComments.find((current) => current.id === Number(commentMatch[1]));
      if (!comment) throw httpError(404);
      return clone(comment);
    }
    if (method === "DELETE" && path.includes(`/issues/${issueNumber}/labels/`)) {
      const encoded = path.slice(path.lastIndexOf("/") + 1);
      const deleted = decodeURIComponent(encoded);
      failMutation(`delete-${deleted}`);
      currentIssue.labels = currentIssue.labels.filter((label) => label.name !== deleted);
      failMutationAfter(`delete-${deleted}`);
      return null;
    }
    if (method === "POST" && path.endsWith(`/issues/${issueNumber}/labels`)) {
      failMutation("add-synced");
      for (const label of payload.labels) {
        if (!currentIssue.labels.some((current) => current.name === label)) {
          currentIssue.labels.push({ name: label });
        }
      }
      failMutationAfter("add-synced");
      return { ok: true };
    }
    if (method === "PATCH" && path.endsWith(`/issues/${issueNumber}`)) {
      failMutation("close");
      if (payload.state) currentIssue.state = payload.state;
      failMutationAfter("close");
      return { ok: true };
    }
    throw new Error(`unexpected request: ${method} ${path}`);
  };
  return {
    request,
    calls,
    postedComment: () => postedComments.at(-1)?.body || "",
    postedComments: () => clone(postedComments),
    branchSha: () => branchSha,
    issue: () => clone(currentIssue),
  };
}

function fakeGraphql({ mergeMethod = "SQUASH", existing = false, mutate } = {}) {
  const calls = [];
  const graphql = async (_token, query, variables) => {
    calls.push({ query, variables });
    if (query.includes("query BackflowMergeQueuePolicy")) {
      return { repository: { mergeQueue: { configuration: { mergeMethod } } } };
    }
    if (query.includes("query BackflowPullState")) {
      return { node: { mergeQueueEntry: existing ? { id: "existing" } : null, autoMergeRequest: null } };
    }
    if (query.includes("mutation EnqueueBackflowPull")) {
      if (mutate) return mutate({ query, variables });
      return { enqueuePullRequest: { mergeQueueEntry: { id: "MQ_entry" } } };
    }
    throw new Error("unexpected GraphQL operation");
  };
  return { graphql, calls };
}

test("事件解析只接受成功 PR Check 与已合并 pull_request_target", () => {
  assert.deepEqual(parseAutomationEvent(workflowEvent()), { action: "advance", pullNumber, expectedHead: head });
  assert.equal(parseAutomationEvent({ action: "completed", workflow_run: { name: "PR Check", event: "pull_request", conclusion: "failure", pull_requests: [] } }).action, "skip");
  assert.equal(parseAutomationEvent({ action: "completed", workflow_run: { name: "PR Check", event: "push", conclusion: "success", pull_requests: [{ number: pullNumber }] } }).action, "skip");
  assert.equal(parseAutomationEvent({ action: "closed", pull_request: { number: pullNumber, merged: false } }).action, "skip");
  assert.equal(parseAutomationEvent(mergedEvent()).action, "finalize");
});

test("未显式启用时安全跳过且不读取 GitHub", async () => {
  let touched = false;
  const result = await runAutomation({ event: workflowEvent(), repo, token: null, enabled: undefined, request: async () => { touched = true; } });
  assert.deepEqual(result, { status: "skipped", reason: "automation-disabled" });
  assert.equal(touched, false);
});

test("验证 Queue 为 SQUASH 后以 expectedHeadOid 加入 Merge Queue", async () => {
  const api = fakeApi(fixture());
  const graph = fakeGraphql();
  const result = await runAutomation({ event: workflowEvent(), repo, token: "token", enabled: "true", request: api.request, graphql: graph.graphql });
  assert.equal(result.status, "enqueued");
  assert.equal(graph.calls.length, 3);
  assert.deepEqual(graph.calls[0].variables, { owner: "Pinvou", name: "pinvou3", branch: "main" });
  assert.match(graph.calls[2].query, /expectedHeadOid: \$head/u);
  assert.deepEqual(graph.calls[2].variables, { id: "PR_node", head });
});

test("校验 H 后分支推进到 H2 时 expectedHeadOid 令入队原子失败", async () => {
  const api = fakeApi(fixture());
  const graph = fakeGraphql({ mutate: ({ variables }) => {
    assert.equal(variables.head, head);
    throw new Error(`expectedHeadOid ${head} does not match ${advancedHead}`);
  } });
  await assert.rejects(runAutomation({ event: workflowEvent(), repo, token: "token", enabled: "true", request: api.request, graphql: graph.graphql }), /expectedHeadOid/u);
  assert.equal(graph.calls.filter((call) => call.query.includes("mutation EnqueueBackflowPull")).length, 1);
});

test("Merge Queue 合并方法不是 SQUASH 时拒绝入队且不修改远端规则", async () => {
  const api = fakeApi(fixture());
  const graph = fakeGraphql({ mergeMethod: "MERGE" });
  await assert.rejects(runAutomation({ event: workflowEvent(), repo, token: "token", enabled: "true", request: api.request, graphql: graph.graphql }), /mergeMethod=MERGE.*配置为 SQUASH.*不会修改规则或绕过 Queue/u);
  assert.equal(graph.calls.length, 1);
  assert.equal(graph.calls.some((call) => call.query.includes("mutation")), false);
});

test("dry-run 完成包括 Queue 策略在内的校验但不入队", async () => {
  const api = fakeApi(fixture());
  const graph = fakeGraphql();
  const result = await runAutomation({ event: workflowEvent(), repo, token: "token", enabled: "true", dryRun: true, request: api.request, graphql: graph.graphql });
  assert.equal(result.status, "dry-run");
  assert.equal(result.action, "enqueue");
  assert.equal(graph.calls.some((call) => call.query.includes("mutation")), false);
});

test("workflow_run head 必须等于 PR 当前 head", async () => {
  const api = fakeApi(fixture());
  const event = workflowEvent();
  event.workflow_run.head_sha = "7".repeat(40);
  await assert.rejects(runAutomation({ event, repo, token: "token", enabled: "true", request: api.request, graphql: fakeGraphql().graphql }), /事件 head .* 与 PR .* 当前 head .* 不一致/u);
});

test("已入队时仍先核验 SQUASH 策略后幂等跳过", async () => {
  const api = fakeApi(fixture());
  const graph = fakeGraphql({ existing: true });
  const result = await runAutomation({ event: workflowEvent(), repo, token: "token", enabled: "true", request: api.request, graphql: graph.graphql });
  assert.equal(result.status, "already-enabled");
  assert.equal(graph.calls.length, 2);
});

test("分页读取来源 files，并以根目录 VERSION 与 Issue Release 计划交叉验证", async () => {
  const data = fixture({ release: true, merged: true });
  data.filePages = [Array.from({ length: 100 }, (_, index) => ({ filename: `src/file-${index}.rs` })), [{ filename: "VERSION" }]];
  const api = fakeApi(data);
  const result = await runAutomation({ event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request });
  assert.equal(result.status, "deferred-release");
  assert.equal(api.calls.some((call) => call.path.endsWith("files?per_page=100&page=2")), true);
  assert.equal(api.calls.some((call) => ["PATCH", "DELETE"].includes(call.method)), false);
});

test("来源 VERSION 与 Issue Release 计划不一致或 files 未知时 fail closed", async (t) => {
  const cases = [
    ["漏标 Release", () => fixture(), (data) => { data.filePages = [[{ filename: "VERSION" }]]; }, /Release 计划为 false.*判定为 true/u],
    ["误标 Release", () => fixture({ release: true }), (data) => { data.filePages = [[{ filename: "src/example.rs" }]]; }, /Release 计划为 true.*判定为 false/u],
    ["files 响应未知", () => fixture(), (data) => { data.filePages = [{ unknown: true }]; }, /files 响应未知/u],
    ["changed file 未知", () => fixture(), (data) => { data.filePages = [[{}]]; }, /无法识别的 changed file/u],
    ["Release plan 类型未知", () => fixture(), (data) => { data.issue.body = data.issue.body.replace('"release":false', '"release":"false"'); }, /plan marker 无效/u],
  ];
  for (const [name, create, mutate, pattern] of cases) {
    await t.test(name, async () => {
      const data = create();
      mutate(data);
      const api = fakeApi(data);
      await assert.rejects(runAutomation({ event: workflowEvent(), repo, token: "token", enabled: "true", request: api.request, graphql: fakeGraphql().graphql }), pattern);
    });
  }
});

test("拒绝外部 head、错误 base、过期 gate、blocked Issue 与未合并来源", async (t) => {
  const cases = [
    ["外部 head", (data) => { data.pull.head.repo.full_name = "fork/pinvou3"; }, /不是 Official 同仓 PR/u],
    ["错误 base", (data) => { data.pull.base.ref = "feature"; }, /base 不是 main/u],
    ["过期 gate", (data) => { data.checks = { check_runs: [{ name: "required-gate", head_sha: "old", status: "completed", conclusion: "success" }] }; }, /没有成功的 required-gate/u],
    ["Issue 来源 marker 不一致", (data) => { data.issue.body = data.issue.body.replace("#393", "#392"); }, /必须关联唯一 Official Issue/u],
    ["blocked Issue", (data) => { data.issue.labels.push({ name: "backflow:blocked" }); }, /backflow:blocked/u],
    ["未合并来源", (data) => { data.communityPull = { merged: false, merged_at: null }; }, /不是 merged 状态/u],
  ];
  for (const [name, mutate, pattern] of cases) {
    await t.test(name, async () => {
      const data = fixture();
      mutate(data);
      const api = fakeApi(data);
      await assert.rejects(runAutomation({ event: workflowEvent(), repo, token: "token", enabled: "true", request: api.request, graphql: fakeGraphql().graphql }), pattern);
    });
  }
});

test("PR 与 Issue 的来源、provenance、plan marker 必须各恰好一个且互相一致", async (t) => {
  const provenanceMarker = /<!-- community-backflow-provenance:[A-Za-z0-9_-]+ -->/u;
  const cases = [
    ["重复 PR 来源 marker", (data) => {
      data.pull.body += `\n<!-- community-backflow:Pinvou/pinvou-agent#${source} -->`;
    }, /PR 来源 marker.*实际 2 次/u],
    ["重复 PR provenance marker", (data) => {
      data.pull.body += `\n${data.pull.body.match(provenanceMarker)[0]}`;
    }, /PR provenance marker.*实际 2 次/u],
    ["重复 Issue 来源 marker", (data) => {
      data.issue.body += `\n<!-- community-backflow:Pinvou/pinvou-agent#${source} -->`;
    }, /Issue #915 的来源 marker.*实际 2 次/u],
    ["重复 Issue plan marker", (data) => {
      const plan = data.issue.body.match(/<!-- community-backflow-plan:[^\r\n]+ -->/u)[0];
      data.issue.body += `\n${plan}`;
    }, /Issue #915 的 community-backflow-plan marker.*实际 2 次/u],
    ["PR 来源与分支冲突", (data) => {
      data.pull.body = data.pull.body.replace(
        `community-backflow:Pinvou/pinvou-agent#${source}`,
        "community-backflow:Pinvou/pinvou-agent#392",
      );
    }, /provenance 来源 PR 与来源 marker 不一致/u],
    ["PR provenance 与来源冲突", (data) => {
      const conflicting = createProvenance({
        base: "8".repeat(40), head, source: 392, paths: ["文档/说明.md"], platform: "win32",
      });
      data.pull.body = data.pull.body.replace(provenanceMarker, encodeProvenanceMarker(conflicting));
    }, /provenance 来源 PR 与来源 marker 不一致/u],
    ["Issue plan 与来源冲突", (data) => {
      data.issue.body = data.issue.body.replace(`"source":${source}`, '"source":392');
    }, /必须关联唯一 Official Issue/u],
  ];
  for (const [name, mutate, pattern] of cases) {
    await t.test(name, async () => {
      const data = fixture();
      mutate(data);
      const api = fakeApi(data);
      await assert.rejects(runAutomation({
        event: workflowEvent(), repo, token: "token", enabled: "true", request: api.request,
        graphql: fakeGraphql().graphql,
      }), pattern);
    });
  }
});

test("普通回流先确认 synced、最后移除 processing 并关闭，且不自动删除分支", async () => {
  const api = fakeApi(fixture({ merged: true }));
  const result = await runAutomation({ event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request });
  assert.equal(result.status, "finalized");
  assert.equal(api.calls.some((call) => call.path.includes("/git/ref")), false);
  assert.equal(api.branchSha(), head);
  assert.match(api.postedComment(), /回流 PR 已合并并自动收尾/u);
  assert.match(api.postedComment(), /不执行无 SHA 原子绑定的 ref 删除/u);
  const issuePatch = api.calls.find((call) => call.method === "PATCH");
  assert.equal("labels" in issuePatch.payload, false);
  assert.equal(issuePatch.payload.state, "closed");
  assert.equal(api.calls.filter((call) => call.method === "DELETE" && call.path.includes("/labels/")).length, 2);
  const addLabels = api.calls.find((call) => call.method === "POST" && call.path.endsWith(`/issues/${issueNumber}/labels`));
  assert.deepEqual(addLabels.payload, { labels: ["backflow:synced"] });
  const addIndex = api.calls.indexOf(addLabels);
  const removeProcessingIndex = api.calls.findIndex((call) =>
    call.method === "DELETE" && call.path.endsWith("backflow%3Aprocessing"));
  assert.ok(addIndex >= 0 && removeProcessingIndex > addIndex);
  assert.deepEqual(api.issue().labels.map((label) => label.name).sort(), ["backflow:synced", "产品"].sort());
});

test("普通收尾评论已落库但响应丢失时重放只保留一条 stable marker 评论", async () => {
  const api = fakeApi(fixture({ merged: true }), { commentResponseLosses: 1 });
  await assert.rejects(
    runAutomation({ event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request }),
    /socket closed after server committed comment/u,
  );
  assert.equal(api.postedComments().length, 1);
  assert.equal(api.issue().state, "open");

  const result = await runAutomation({
    event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request,
  });
  assert.equal(result.status, "finalized");
  assert.equal(api.postedComments().length, 1);
  assert.equal(api.calls.filter((call) =>
    call.method === "POST" && call.path.endsWith(`/issues/${issueNumber}/comments`)).length, 1);
});

test("校验 H 后远端分支正好推进到 H2 时收尾不触发无 CAS 的 ref DELETE", async () => {
  const api = fakeApi(fixture({ merged: true }), { advanceBranchAtIssueRead: 2 });
  const result = await runAutomation({ event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request });
  assert.equal(result.status, "finalized");
  assert.equal(api.branchSha(), advancedHead);
  assert.equal(api.calls.some((call) => call.path.includes("/git/ref")), false);
});

test("每个标签或关闭 mutation 失败后重跑都从可恢复状态收敛", async (t) => {
  const cases = [
    ["添加 synced 前失败", "mutationFailures", "add-synced", "open", ["backflow:pending", "backflow:processing", "产品"]],
    ["添加 synced 后响应失败", "mutationFailuresAfter", "add-synced", "open", ["backflow:pending", "backflow:processing", "backflow:synced", "产品"]],
    ["移除 pending 前失败", "mutationFailures", "delete-backflow:pending", "open", ["backflow:pending", "backflow:processing", "backflow:synced", "产品"]],
    ["移除 pending 后响应失败", "mutationFailuresAfter", "delete-backflow:pending", "open", ["backflow:processing", "backflow:synced", "产品"]],
    ["移除 processing 前失败", "mutationFailures", "delete-backflow:processing", "open", ["backflow:processing", "backflow:synced", "产品"]],
    ["移除 processing 后响应失败", "mutationFailuresAfter", "delete-backflow:processing", "open", ["backflow:synced", "产品"]],
    ["关闭 Issue 前失败", "mutationFailures", "close", "open", ["backflow:synced", "产品"]],
    ["关闭 Issue 后响应失败", "mutationFailuresAfter", "close", "closed", ["backflow:synced", "产品"]],
  ];
  for (const [name, failureOption, mutation, expectedState, expectedAfterFailure] of cases) {
    await t.test(name, async () => {
      const api = fakeApi(fixture({ merged: true }), { [failureOption]: { [mutation]: 1 } });
      await assert.rejects(runAutomation({
        event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request,
      }), /HTTP 503/u);
      assert.equal(api.issue().state, expectedState);
      assert.deepEqual(
        api.issue().labels.map((label) => label.name).sort(),
        [...expectedAfterFailure].sort(),
      );

      const result = await runAutomation({
        event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request,
      });
      assert.ok(["finalized", "already-finalized"].includes(result.status));
      assert.equal(api.issue().state, "closed");
      assert.deepEqual(api.issue().labels.map((label) => label.name).sort(), ["backflow:synced", "产品"].sort());
    });
  }
});

test("收尾前 PR head 变化立即停止且不执行写 mutation", async () => {
  const api = fakeApi(fixture({ merged: true }), { pullAtRead: (read, pull) => {
    if (read === 2) pull.head.sha = advancedHead;
    return pull;
  } });
  await assert.rejects(runAutomation({ event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request }), /merged\/head\/branch\/merge commit 已变化/u);
  assert.equal(api.calls.some((call) => call.method === "DELETE"), false);
});

test("收尾前 PR 或 Issue 状态变化以及新增 blocker 都立即停止", async (t) => {
  const cases = [
    ["PR 不再 merged", {
      pullAtRead: (read, pull) => {
        if (read === 2) pull.merged = false;
        return pull;
      },
    }, /merged\/head\/branch\/merge commit 已变化/u],
    ["Issue 被并发关闭", {
      issueAtRead: (read, issue) => {
        if (read === 1) issue.state = "closed";
        return issue;
      },
    }, /不在可恢复的 open processing\/synced 状态/u],
    ["Issue 新增 blocker", {
      issueAtRead: (read, issue) => {
        if (read === 1) issue.labels.push({ name: "backflow:blocked" });
        return issue;
      },
    }, /新增 blocker\/dependency-wait/u],
  ];
  for (const [name, options, pattern] of cases) {
    await t.test(name, async () => {
      const api = fakeApi(fixture({ merged: true }), options);
      await assert.rejects(runAutomation({ event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request }), pattern);
      assert.equal(api.calls.some((call) => call.method === "DELETE"), false);
      assert.equal(api.calls.some((call) => call.method === "PATCH"), false);
    });
  }
});

test("各收尾阶段 Issue 标签变化立即停止，绝不用旧标签覆盖", async (t) => {
  for (const [name, changedRead] of [
    ["首次复核", 1],
    ["标签更新前", 2],
    ["synced 确认", 3],
    ["最终关闭前", 4],
  ]) {
    await t.test(name, async () => {
      const api = fakeApi(fixture({ merged: true }), { issueAtRead: (read, issue) => {
        if (read === changedRead) issue.labels.push({ name: "并发审查" });
        return issue;
      } });
      await assert.rejects(runAutomation({ event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request }), /标签已变化/u);
      assert.equal(api.calls.some((call) => call.method === "PATCH"), false);
      if (changedRead === 1) assert.equal(api.calls.some((call) => call.method === "DELETE"), false);
    });
  }
});

test("Release 回流提示后续固件构建，不删除分支或错误关闭 Issue", async () => {
  const api = fakeApi(fixture({ release: true, merged: true }));
  const result = await runAutomation({ event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request });
  assert.equal(result.status, "deferred-release");
  assert.match(api.postedComment(), /不会跳过固件构建契约/u);
  assert.equal(api.calls.some((call) => ["PATCH", "DELETE"].includes(call.method)), false);
});

test("Release 延后评论已落库但响应丢失时重放仍幂等且不收尾", async () => {
  const api = fakeApi(fixture({ release: true, merged: true }), { commentResponseLosses: 1 });
  await assert.rejects(
    runAutomation({ event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request }),
    /socket closed after server committed comment/u,
  );
  assert.equal(api.postedComments().length, 1);

  for (let replay = 0; replay < 2; replay += 1) {
    const result = await runAutomation({
      event: mergedEvent(), repo, token: "token", enabled: "true", request: api.request,
    });
    assert.equal(result.status, "deferred-release");
  }
  assert.equal(api.postedComments().length, 1);
  assert.equal(api.issue().state, "open");
  assert.equal(api.calls.some((call) => ["PATCH", "DELETE"].includes(call.method)), false);
});
