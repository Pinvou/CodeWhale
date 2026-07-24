# Pinvou Fork Baseline

This repository is the public CodeWhale fork embedded by
[Pinvou Agent](https://github.com/Pinvou/pinvou-agent). CodeWhale remains an
independent upstream project; general-purpose fixes should be contributed to
[Hmbown/CodeWhale](https://github.com/Hmbown/CodeWhale) whenever possible.

## Pinned baseline

- Upstream release: `v0.9.0`
- Upstream commit: `d167c07c96282411956ea7f35ddb8227afa1402f`
- Pinvou maintenance branch: `pinvou3-clean`
- Pinvou baseline tag: `pinvou-v0.9.0-r1`

Pinvou Agent pins the exact commit identified by the baseline tag. It does not
follow the maintenance branch automatically.

The fork was rebuilt on the public upstream release history. It preserves
upstream commits, authorship, copyright, and the MIT license; it does not reuse
the history of Pinvou's former private fork.

## Maintained patch themes

Pinvou keeps six long-lived patch themes:

1. A library facade for embedding CodeWhale in a desktop host.
2. Host-specific tool exposure, bounded file writes, and execution safety.
3. A sealed prompt composer with host-controlled context and skill sources.
4. Automation scheduling, stable conversation identity, and run retention.
5. Host orchestration, structured workflow completion, cancellation, and OAuth
   cancellation.
6. Runtime route, context-budget, and shared automation APIs for the host.

Small fixes are folded into the nearest theme instead of creating an
unbounded patch stack. Product UI, platform integration, connectors, and
distribution logic stay in Pinvou Agent rather than this fork.

## Sync policy

For an upstream update:

1. Fetch upstream tags and select a public release baseline.
2. Compare each Pinvou theme with the new upstream behavior.
3. Remove patches already implemented upstream.
4. Move host-only behavior to Pinvou Agent when it does not require an atomic
   CodeWhale lifecycle change.
5. Reapply the remaining themes as reviewable commits on the new baseline.
6. Run formatting, all-target checks, `forkguard_` tests, and a full history
   secret scan.
7. Create a new immutable `pinvou-v<upstream>-r<N>` tag and update Pinvou
   Agent's submodule pointer in a separate pull request.

Never move or reuse an existing Pinvou baseline tag.

## Verification

The fork CI runs:

```bash
cargo fmt --all -- --check
cargo check --workspace --all-targets --locked
cargo test -p codewhale-tui --lib forkguard_ --locked
gitleaks detect --source=. --redact --verbose
```

The parent repository additionally verifies that the pinned gitlink is
reachable from this public repository and that a recursive clone succeeds.

## License and attribution

CodeWhale and these fork changes are available under the repository's
[MIT License](LICENSE). Upstream copyright and contributor attribution are
preserved.
