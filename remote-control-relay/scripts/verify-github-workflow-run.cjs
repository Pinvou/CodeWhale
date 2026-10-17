"use strict";

function fail(code) {
  const error = new Error(code);
  error.code = code;
  throw error;
}

function splitRunPath(value) {
  if (typeof value !== "string" || value.length === 0) fail("protected_build_run_path_invalid");
  const marker = value.lastIndexOf("@");
  if (marker < 0) return { path: value, ref: null };
  const path = value.slice(0, marker);
  const ref = value.slice(marker + 1);
  if (path.length === 0 || ref.length === 0) fail("protected_build_run_path_invalid");
  return { path, ref };
}

function expectedRefName(ref) {
  const match = /^refs\/(?:heads|tags)\/(.+)$/u.exec(ref || "");
  return match?.[1] || null;
}

function runPathRefMatches(actual, expected) {
  if (actual === null) return true;
  const short = expectedRefName(expected);
  return actual === expected || actual === short;
}

function selectProtectedWorkflowArtifact({ run, workflow, artifacts, expected }) {
  const runPath = splitRunPath(run?.path);
  if (!Number.isSafeInteger(expected?.workflowId) || expected.workflowId <= 0
    || !/^[a-f0-9]{40}$/u.test(expected.sha || "")
    || !/^refs\/(?:heads|tags)\/[A-Za-z0-9._/-]+$/u.test(expected.ref || "")
    || !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/u.test(expected.repository || "")
    || !/^sha256:[a-f0-9]{64}$/u.test(expected.artifactDigest || "")) fail("protected_build_identity_configuration_missing");
  if (workflow?.id !== expected.workflowId || workflow.path !== expected.workflowPath || workflow.state !== "active"
    || run?.workflow_id !== expected.workflowId || runPath.path !== expected.workflowPath
    || !runPathRefMatches(runPath.ref, expected.ref)
    || run?.head_sha !== expected.sha || run?.head_repository?.full_name !== expected.repository
    || run?.event !== "workflow_dispatch" || run?.status !== "completed" || run?.conclusion !== "success"
    || !Number.isSafeInteger(run?.run_attempt) || run.run_attempt < 1
    || expectedRefName(expected.ref) === null || run?.head_branch !== expectedRefName(expected.ref)) {
    fail("protected_build_run_identity_mismatch");
  }
  const named = (artifacts || []).filter((artifact) => artifact?.name === expected.artifactName);
  if (named.length !== 1 || named[0].expired !== false || named[0].digest !== expected.artifactDigest
    || !Number.isSafeInteger(named[0].id) || named[0].id <= 0) fail("protected_build_artifact_identity_mismatch");
  return named[0];
}

module.exports = { selectProtectedWorkflowArtifact, splitRunPath };
