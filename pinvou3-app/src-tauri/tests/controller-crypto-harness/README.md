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
