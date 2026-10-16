# Sealed-code nonce regression

The production sealed-code v1 envelope is frozen as one version byte, a 32-byte
ephemeral public key, a 12-byte nonce, ciphertext and tag. The crypto test checks
`[1..33]` and `[33..45]` independently so a fresh public key cannot hide a fixed
nonce. Both envelopes must still decrypt to the expected code; the existing
vector, malformed-envelope and binding-tamper tests remain unchanged.

Run the actual production-module crypto tests through the existing harness:

```sh
cargo test --locked --manifest-path pinvou3-app/src-tauri/tests/controller-crypto-harness/Cargo.toml crypto::tests
```

For a negative check, temporarily replace the body of `random_nonce` with
`Ok([0_u8; AEAD_NONCE_BYTES])`. The nonce assertion in
`production_sealed_code_sealer_is_one_shot_and_owns_its_nonce` must fail even
though the ephemeral keys differ. Restore the production source afterward and
rerun all crypto tests. Do not commit the mutation or change the random source,
envelope layout, key ownership or existing CI entry point.

This harness does not link the complete native IPC/host application.
