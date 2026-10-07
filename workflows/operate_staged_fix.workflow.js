/**
 * Operate starter — staged bugfix (worktree implementer → verifier).
 *
 * Dogfood source: docs/examples/dogfood-automatic/wf_a2_staged_bugfix.workflow.js
 * Run: /workflow run workflows/operate_staged_fix.workflow.js
 */
export default async function (args) {
  const target = args?.target ?? "docs/AUTOMATIC_WORKFLOWS.md";
  const change =
    args?.change ??
    "Add a one-line note that #4131 example scenarios live in docs/examples/dogfood-automatic/.";

  phase("Implement");
  const implement = await task({
    description: `Implement minimal docs fix in an isolated worktree: ${target}`,
    label: "implementer",
    type: "implementer",
    // Prefer worktree isolation for write children (product default #4120).
    worktree: true,
    writeAuthority: "worktree_write",
    exactFiles: [target],
    coordinationContracts: ["wf-a2-doc-fix"],
    dependencies: ["The parent workspace must remain unchanged."],
    acceptance: [
      `Only ${target} changes in the isolated worktree.`,
      "The handoff reports the exact path and diff summary.",
    ],
    prompt: [
      `Edit only ${target}.`,
      change,
      "Keep the change minimal and reversible. Do not push. Do not touch unrelated files.",
      "Return: path edited, unified-diff summary, worktree path if any.",
    ].join("\n"),
  });

  phase("Verify");
  const verify = await task({
    description: "Verify the isolated implementer handoff without further edits.",
    label: "verifier",
    type: "verifier",
    worktree: false,
    prompt: [
      "Read the implementer result and validate its reported path and diff summary.",
      "Your posture is read-only: no edit tools and no shell (the verifier role denies bash; only the bounded verification runner may be granted).",
      "Return PASS/FAIL with evidence: read the reported files yourself for each claim you rely on, and mark anything you did not verify as unverified rather than confirmed.",
      "",
      "implementer_result:",
      String(implement ?? "(missing)"),
    ].join("\n"),
  });

  return {
    scenario: "WF-A2",
    target,
    implement,
    verify,
  };
}
