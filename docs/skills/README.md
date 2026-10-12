# Maintainer / agent skills

GitHub-stewardship and release-QA workflows for maintaining Codewhale, codified as
`SKILL.md` skills (same format Claude Code and Codewhale both load). They encode the
issue-triage, PR-harvest, credit, and release-QA workflows the maintainers run each
release.

For end-user Skills Manager behavior (ownership, audit, import, trust), see
[../SKILLS.md](../SKILLS.md).

To activate:
- **Claude Code:** copy a skill dir into `.claude/skills/` (project) or your user skills dir.
- **Codewhale:** copy into a Codewhale-owned root (e.g. `~/.codewhale/skills/`), import via
  `/skills`, or bundle into `crates/tui/assets/skills/` + register in
  `crates/tui/src/skills/system.rs` to ship it.

Skills: gh-file-issue, gh-compile-issues, gh-assign-issues, gh-find-prs,
gh-treasure-hunt, gh-close-issues, gh-credit-harvest, codew-release-qa-sweep.

Loop skills (the cw-* ladder):

- cw-orient — establish live repo truth (checkout, branch, worktree) at the start of any session, before reading a plan or editing.
- cw-slice — find the existing owner of the behavior and bound the change to one reviewable slice with its evidence bar before writing code.
- cw-gates — the focused-to-broad verification ladder, the budget checks CI enforces, and the rules for what counts as a passing test.
- cw-dogfood — prove a change in the real product: stamped release build, atomic install, fresh-shell verification, and manual QA.
- cw-land — turn verified work into commits, branches, or a merge: direct-main vs. worktree vs. integration branch, with contributor credit preserved.
- cw-handoff — a paste-ready takeover prompt or end-of-session summary grounded in live state, done/suspected/blocked kept separate.
