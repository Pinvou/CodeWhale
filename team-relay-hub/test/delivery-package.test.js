import assert from "node:assert/strict";
import test from "node:test";
import { normalizeDelivery } from "../src/delivery-package.js";

test("missing delivery package gets an action-specific compatible default", () => {
  assert.equal(normalizeDelivery(undefined, "assign").artifact_role, "input");
  assert.equal(normalizeDelivery(undefined, "return").artifact_role, "intermediate");
  assert.equal(normalizeDelivery(undefined, "final_deliver").artifact_role, "final");
  assert.equal(normalizeDelivery(undefined, "cancel").artifact_role, "evidence");
  assert.equal(normalizeDelivery(undefined, "decline_transfer").artifact_role, "evidence");
});

test("Git delivery references are normalized without accessing the repository", () => {
  const delivery = normalizeDelivery({
    schema_version: 1,
    artifact_role: "final",
    expected_next: " Review and accept. ",
    open_questions: " None. ",
    git_references: [{
      repository_url: "https://github.com/pinvou/example",
      commit_sha: "ABCDEF1234567",
      branch: " main ",
      pull_request_url: "https://github.com/pinvou/example/pull/1",
      paths: [" docs/plan.md "],
      note: " Ready. ",
    }],
  }, "final_deliver");
  assert.equal(delivery.expected_next, "Review and accept.");
  assert.equal(delivery.git_references[0].commit_sha, "abcdef1234567");
  assert.deepEqual(delivery.git_references[0].paths, ["docs/plan.md"]);
});

test("Git references reject credentials and invalid commit identifiers", () => {
  const base = {
    schema_version: 1,
    artifact_role: "final",
    expected_next: "",
    open_questions: "",
    git_references: [{ repository_url: "https://user:secret@example.com/repo", commit_sha: "abcdef1", branch: "", pull_request_url: "", paths: [], note: "" }],
  };
  assert.throws(() => normalizeDelivery(base, "final_deliver"), (error) => error.code === "invalid_request");
  base.git_references[0].repository_url = "https://example.com/repo?token=secret";
  assert.throws(() => normalizeDelivery(base, "final_deliver"), (error) => error.code === "invalid_request");
  base.git_references[0].repository_url = "https://example.com/repo";
  base.git_references[0].commit_sha = "not-a-sha";
  assert.throws(() => normalizeDelivery(base, "final_deliver"), (error) => error.code === "invalid_request");
});
