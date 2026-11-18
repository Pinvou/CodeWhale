# Pinvou CodeWhale Fork Policy

> Updated: 2026-11-15. Public maintenance 687358f7e sits on 8e8d52fdb and contains [Pinvou/CodeWhale#157](https://github.com/Pinvou/CodeWhale/pull/157) (session-reader read-only reclassification); the parent aligns through the strict public-maintenance-head gate.
> Canonical Chinese policy: [`docs/fork-policy.md`](fork-policy.md). This English page is a condensed summary; the Chinese version is the complete, authoritative process.

Integrated upstream backport and historical provenance: [modification register](fork-modifications.en.md#web-jsonpath-upstream-backport-integrated-historical-candidate-evidence-pinjou-agent756). The public maintenance gate and section 6 authorization requirement continue to apply.

## 0. Current maintenance baseline and validation boundary

- Public maintenance and parent gitlink: `687358f7efde6abd3592737b8ec48adc74ba5a84` (2026-11-15). On top of the former `8e8d52fdb` this consumes the merged [Pinvou/CodeWhale#157](https://github.com/Pinvou/CodeWhale/pull/157) `687358f7e`; earlier batches [Pinvou/CodeWhale#136](https://github.com/Pinvou/CodeWhale/pull/136) `37be2844f`, [Pinvou/CodeWhale#142](https://github.com/Pinvou/CodeWhale/pull/142) `78804c83e`, [Pinvou/CodeWhale#148](https://github.com/Pinvou/CodeWhale/pull/148) `5f15cce57`, [Pinvou/CodeWhale#150](https://github.com/Pinvou/CodeWhale/pull/150) `e972a9a05`, and [Pinvou/CodeWhale#152](https://github.com/Pinvou/CodeWhale/pull/152)/[Pinvou/CodeWhale#156](https://github.com/Pinvou/CodeWhale/pull/156) (`6f7a3a748`, `8e8d52fdb`) remain ancestors. The parent consumes published commits only; it does not advance the maintenance branch or move tags.
- Measured history: 100 commits above upstream v0.9.12 `dcd4c200f`, 85 above r1 `1fafee7e2`; 97 carry DCO sign-offs. The unsigned commits are [Pinvou/CodeWhale#107](https://github.com/Pinvou/CodeWhale/pull/107) `f27672935`, [Pinvou/CodeWhale#116](https://github.com/Pinvou/CodeWhale/pull/116) `e9a7f48da1`, and [Pinvou/CodeWhale#157](https://github.com/Pinvou/CodeWhale/pull/157) `687358f7e` (CodeWhale squash merge carried no sign-off trailer); the unsigned set can no longer be claimed as bounded by the historical two. Published maintenance history is preserved.
- Immutable `pinjou-v0.9.12-r3` remains at `61cb769be5b33abc64f64da4272f5b39a8b6c1fd`; r1 remains at `1fafee7e26b60a59457a43bce50c63aa2ad9dbaf`. The public verifier still requires maintenance-head equality and the frozen r3 tag; no predicate is relaxed.
- Measured drift: 551 files, +121146/-49817, net +71329; 301 distinct `forkguard_*` names (floor remains 64). Earlier native Windows receipts retain their recorded commit boundaries; they do not prove execution on this integrated tree. Current parent CI and final integrated-tree review remain separate requirements.

- The public pre-upgrade rollback point is immutable tag `pinvou-v0.9.5-r13` at `f853f8f1566c57e6be40d5439a222a932aa79ef5`; local `backup/pre-v0.9.12-sync` at the same SHA is only a convenience ref.
- r1 is the protected consumable baseline. At each rN closure the parent gitlink, maintenance branch, and immutable tag resolve to the same commit.
- Transition exemption (from 2026-09-11): between two rN closures the parent gitlink may advance along `pinvou3-clean` ahead of the immutable tag. During the transition `scripts/verify-public-submodule.sh` asserts gitlink equals the public maintenance-branch head and the immutable tag stays pinned at its closure commit; the next rN closure cuts a fresh immutable tag at the merged head and restores three-way equality.
- Keep four long-lived topics, three appended reduction topics, and three merged-into-maintenance topics:

  1. Host embedding and routing boundary
  2. Tool compatibility and command-execution safety
  3. Embedded context and Skill sources
  4. Automation and runtime lifecycle
  5. Session archive export (T5, appended)
  6. Swarm rate-limit governance (T6, appended)
  7. Compaction checkpoint role compatibility (T7, merged into the maintenance branch)
  8. Roster presentation of host profiles (T8, appended)
  9. Custom wire runtime route (T9, merged into the maintenance branch)
  10. Tachyon reasoning-effort dialect (T10, merged into the maintenance branch)

The exact commits and fingerprints are recorded in [`docs/fork-modifications.md`](fork-modifications.md).

## Historical account-host wiring retained

The records below preserve the commit and verification boundaries of their time; the current maintenance baseline is section 0.

> F05 invocation-idempotency registration: the original `8b5a86390` patch and its follow-up fixes were merged into `pinvou3-clean` by an external maintainer through CodeWhale [Pinvou/CodeWhale#92](https://github.com/Pinvou/CodeWhale/pull/92); the public maintenance head synced at the time, `ea9fdc90f`, must retain that capability. The parent keeps the original F05 commit history through an ordinary merge, and the foundation still uses the published maintenance commit — a pre-merge candidate SHA is never treated as the maintenance head. The account Gateway's host opt-in and per-invocation identity are registered under T1 in `docs/fork-modifications.md`. Current gates and actual host verification remain separate requirements; source-code inclusion cannot substitute for either.

> Strict-alignment follow-up after 2026-10-14: the public maintenance branch merged [Pinvou/CodeWhale#107](https://github.com/Pinvou/CodeWhale/pull/107) with external approval; `f27672935` keeps the same wire model/protocol host-freeze restriction, and cross-model requests still re-resolve. Two new result-oriented regressions are consumed with the published commit; the parent modifies no foundation code, moves no tags, and does not relax the public-maintenance-head check; host applicability CI for the new head is verified separately.

> [Pinvou/CodeWhale#54](https://github.com/Pinvou/CodeWhale/pull/54) alignment follow-up after 2026-10-14: `803e672bb` directly inherits the [Pinvou/CodeWhale#107](https://github.com/Pinvou/CodeWhale/pull/107) maintenance commit above and consumes the externally approved workspace-roots foundation capability (T1/T2). The parent keeps a single execution-root input and does not turn the session ledger into additional execution roots; new persisted root fields are redacted item-by-item along the existing Web metadata path while Native keeps raw values. The foundation also changes single-root operand cwd handling and symlink/missing-path `..` resolution, so this cannot be registered as a behavior-neutral gitlink update; actual host verification is recorded separately.


## Rules

- Prefer the app bridge, bundle instructions/Skills, MCP/connectors/plugins, then an upstream contribution. Keep a fork patch only when the behavior must be atomic inside CodeWhale's Engine, SubAgent, Task, or Automation lifecycle.
- Product tool policy, UI, workspace selection, and business routing stay in `pinvou3-app`.
- The current maintenance head and drift are governed by section 0 of `docs/fork-modifications.md`; this round consumes the merged [Pinvou/CodeWhale#136](https://github.com/Pinvou/CodeWhale/pull/136)/[Pinvou/CodeWhale#142](https://github.com/Pinvou/CodeWhale/pull/142)/[Pinvou/CodeWhale#148](https://github.com/Pinvou/CodeWhale/pull/148)/[Pinvou/CodeWhale#150](https://github.com/Pinvou/CodeWhale/pull/150)/[Pinvou/CodeWhale#152](https://github.com/Pinvou/CodeWhale/pull/152) from the public maintenance branch without modifying the branch or moving fixed tags. The historical [Pinvou/CodeWhale#107](https://github.com/Pinvou/CodeWhale/pull/107) maintenance head `f27672935` measures `397 files, +53709/-21547` above upstream v0.9.12, net +32162; its `client.rs` +124/-5 relative to `480bebf61` retains same-model host route limits and two result-oriented regressions, filed under the existing T1 with no new topic.
- The soft drift limits remain 1,500 net added lines and 200 fork-distinct lines per file. The v0.9.12 r1 baseline is 94 files and `+5022/-944` (net 4,078), down from v0.9.5 r13 at 110 files and `+10895/-1195`. Newly touched files include equivalent Rust/rustdoc release-lint adjustments, review-requested result-level lifecycle and evaluation regressions, the API-search fallback reachability/error-guidance fix, removal of an obsolete upstream-comparison test helper, an exact runtime-contract budget ratchet for the official v0.9.12 plus Pinvou r1 model-visible schemas, a bounded macOS cold-build timeout, and overdue one-shot delivery; they add no fork behavior topic. The remaining excess is justified by Engine/Task-atomic steer, final-dispatch security, host prompt/profile/Skills ownership, Automation lifecycle behavior, and their safety tests. Upstreaming priority is generic steer and per-turn security first, Automation lifecycle second, parent migration to narrow re-export APIs followed by retirement of the 18-module compatibility facade, then replacement of prompt/profile/Skills ownership with stable host APIs.
- Fixups are squashed into their owning topic; no long-lived catch-up commit chains are maintained, and generic host configuration, routing, tools, Automation, and OAuth must remain within their owning boundary.
- A fork-distinct change must update the modification register and guard fingerprints, include a result-oriented `forkguard_*` test where applicable, and pass `./scripts/fork-guard.sh --fast`.
- For a large upstream refactor, clean re-fork from the release tag and re-express each surviving topic. Do not preserve merge-conflict batches as long-lived history.
- Candidate review branches may be pushed for a PR. Update the protected maintenance branch and create an immutable tag only after explicit authorization. The published tag, maintenance branch, and parent gitlink resolve to the same commit and must remain aligned.

## Required verification

```bash
./scripts/fork-guard.sh --fast
cargo check --manifest-path CodeWhale/Cargo.toml -p codewhale-tui --lib --locked
cargo test --manifest-path CodeWhale/Cargo.toml -p codewhale-tui --lib --locked \
  forkguard_ -- --test-threads=1
cargo test --manifest-path CodeWhale/Cargo.toml -p codewhale-tui --lib --locked \
  --features benchmark-eval-controls forkguard_benchmark_ -- --test-threads=1
cargo check --manifest-path pinvou3-app/src-tauri/Cargo.toml --locked
cargo check --manifest-path pinvou3-app/src-tauri/Cargo.toml --all-targets \
  --features benchmark-hooks --locked
cargo test --manifest-path pinvou3-app/src-tauri/Cargo.toml --lib --locked \
  -- --test-threads=1
python3 scripts/architecture-guard.py
```

Automated gates do not replace real-model, GUI, MCP/OAuth, and scheduled-task acceptance.
