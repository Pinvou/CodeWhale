# Controller crypto harness

This harness compiles the actual controller crypto module without linking the
complete Tauri application. It does not replace native IPC or host validation.

The actual product filesystem module and its filesystem-only test primitives
are linked by path. FIFO planting, permission probes and the existing five-second
bounded-refusal assertion are shared with the full application's tests, not
copied or replaced with successful stubs. The harness does not import application
home paths, preferences, keyring state or unrelated ZIP test support. Existing
application test-helper paths remain re-exports of the same implementation.

`crypto::tests::session_keys_cannot_be_cloned` is a compilation-level ownership
guard for `SessionKeys`. Its two trait implementations deliberately become
ambiguous if the key owner gains `Clone`, including through `#[derive(Clone)]`.
No production key type is made cloneable, and `into_role_ciphers` continues to
consume the key owner by value.

Run the crypto regressions with:

```sh
cargo test --locked --manifest-path pinvou3-app/src-tauri/tests/controller-crypto-harness/Cargo.toml crypto::tests
```

To verify the negative guard, temporarily add `#[derive(Clone)]` immediately
above `SessionKeys` in the production crypto source and run the same command.
Compilation must fail at `AmbiguousIfClone<_>`. Restore the source and rerun;
the regular key ownership, directional cipher and sequence tests must pass.
Never commit the mutation. This guard does not change nonce generation.

The harness also links the production controller transport. Run its deadline,
cancellation, queue and loopback WebSocket regressions with:

```sh
cargo test --locked --manifest-path pinvou3-app/src-tauri/tests/controller-crypto-harness/Cargo.toml relay_transport::tests:: -- --test-threads=1
```

The harness substitutes Tokio for Tauri task spawning and does not include
`controlling_service`. The real A port/handshake integration is a separate
application test, selected by
`features::remote_control::controller::controlling_service::tests::connect_attempt_`.
Windows native CI executes that selector and the transport selector against the
compiled application test executable, requiring a nonzero match. These tests
use a loopback Relay fixture and real handshake cryptography, not a deployed
Relay or two physical devices.
