# Pinvou fork merge checks

The `pinvou3-clean` branch uses `.github/workflows/fork-ci.yml`. Upstream
`ci.yml` targets `main` and `master`, so it does not provide this fork's PR
checks. Fork validation runs on every PR update without path filters, against
GitHub's proposed merge tree, and on pushes to the fork base branch.

Branch protection requires these GitHub Actions checks:

| Check | Producer and acceptance condition |
| --- | --- |
| `Check Signed-off-by` | Existing `dco.yml` contributor sign-off workflow (currently advisory internally). |
| `check` | Rust formatting, workspace Clippy, all-feature workspace tests using the existing nextest CI profile, doctests, and lockfile drift check. |
| `Gitleaks` | Checksum-verified Gitleaks CLI scans the complete checked-out tree and introduced commit history with redacted output. Manual runs scan all reachable history. |
| `gate` | Requires both `check` and `Gitleaks` to succeed; failures, cancellations and skipped jobs do not pass. |

`Contribution intake` is the separate contributor welcome/admission workflow,
not the quality gate. DCO remains separately required by branch protection;
the fork workflow does not change its existing sign-off policy.

The fork workflow has read-only repository permissions and no secrets. PR code
is never executed under `pull_request_target`. Checkout does not retain Git
credentials. Secret detection must not be bypassed with broad allowlists or
printed unredacted in public logs. Investigate scanner findings privately
before changing any exception.

Strict branch protection requires checks against the current base. If a PR
predates this workflow, update its branch from `pinvou3-clean` after the CI
repair lands so its merge tree includes the checks. Do not synthesize passing
statuses or bypass protection to compensate for missing workflows.
