import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { test } from "node:test";

const require = createRequire(import.meta.url);
const { selectProtectedWorkflowArtifact } = require("../scripts/verify-github-workflow-run.cjs");
const sha = "a".repeat(40); const digest = `sha256:${"b".repeat(64)}`;
const expected = { workflowId: 73, workflowPath: ".github/workflows/windows-remote-control-build-evidence.yml",
  sha, ref: "refs/heads/main", repository: "pinvou/pinvou3", artifactName: "windows-remote-control-protected-build", artifactDigest: digest };
const workflow = { id: 73, path: expected.workflowPath, state: "active" };
const run = { workflow_id: 73, path: `${expected.workflowPath}@refs/heads/main`, head_sha: sha, head_branch: "main",
  head_repository: { full_name: "pinvou/pinvou3" }, event: "workflow_dispatch", status: "completed", conclusion: "success", run_attempt: 2 };
const artifacts = [{ id: 991, name: expected.artifactName, expired: false, digest }];

test("protected build REST identity accepts exact workflow, ref, repository, attempt, and artifact", () => {
  assert.equal(selectProtectedWorkflowArtifact({ run, workflow, artifacts, expected }).id, 991);
  assert.equal(selectProtectedWorkflowArtifact({ run: { ...run, path: `${expected.workflowPath}@main` }, workflow, artifacts, expected }).id, 991);
  assert.equal(selectProtectedWorkflowArtifact({ run: { ...run, path: expected.workflowPath }, workflow, artifacts, expected }).id, 991);
});

test("official run.path branch and tag forms are parsed from the final at-sign", () => {
  const pathWithAt = ".github/workflows/windows@remote-control-build-evidence.yml";
  const atWorkflow = { ...workflow, path: pathWithAt };
  const atExpected = { ...expected, workflowPath: pathWithAt };
  for (const path of [`${pathWithAt}@main`, `${pathWithAt}@refs/heads/main`]) {
    assert.equal(selectProtectedWorkflowArtifact({ run: { ...run, path }, workflow: atWorkflow, artifacts, expected: atExpected }).id, 991);
  }
  const tagExpected = { ...expected, ref: "refs/tags/v1.2.3" };
  const tagRun = { ...run, head_branch: "v1.2.3" };
  for (const path of [`${expected.workflowPath}@v1.2.3`, `${expected.workflowPath}@refs/tags/v1.2.3`]) {
    assert.equal(selectProtectedWorkflowArtifact({ run: { ...tagRun, path }, workflow, artifacts, expected: tagExpected }).id, 991);
  }
});

test("protected build REST identity rejects workflow aliases and mutable run metadata", () => {
  for (const mutate of [
    (value) => { value.run.path = `${expected.workflowPath}@refs/heads/release`; },
    (value) => { value.run.path = `${expected.workflowPath}@release`; },
    (value) => { value.run.head_branch = "release"; },
    (value) => { value.run.workflow_id = 72; },
    (value) => { value.workflow.path = `.github/workflows/alias.yml`; },
    (value) => { value.workflow.state = "disabled_manually"; },
    (value) => { value.run.head_repository.full_name = "fork/pinvou3"; },
    (value) => { value.run.event = "push"; },
    (value) => { value.run.run_attempt = 0; },
    (value) => { value.artifacts[0].digest = `sha256:${"c".repeat(64)}`; },
    (value) => { value.artifacts.push({ id: 992, name: expected.artifactName, expired: false, digest }); },
  ]) {
    const fixture = structuredClone({ run, workflow, artifacts }); mutate(fixture);
    assert.throws(() => selectProtectedWorkflowArtifact({ ...fixture, expected }));
  }
});

test("path without an embedded ref is accepted only for an exactly matching branch", () => {
  assert.equal(selectProtectedWorkflowArtifact({
    run: { ...run, path: expected.workflowPath }, workflow, artifacts, expected,
  }).id, 991);
  assert.throws(() => selectProtectedWorkflowArtifact({
    run: { ...run, path: expected.workflowPath, head_branch: "release" }, workflow, artifacts, expected,
  }));
  assert.throws(() => selectProtectedWorkflowArtifact({
    run: { ...run, path: expected.workflowPath }, workflow, artifacts,
    expected: { ...expected, ref: "refs/tags/v1.2.3" },
  }));
});
