# Inbound transport diagnostics

`ControllerTransportEvent::Frame`, `ControllerLaneEvent::Frame`, and the
`RelayInbound` message variants retain their exact JSON for protocol validation,
reader proof and business consumption. Their `Debug` representations must not
print that JSON: it can contain a replacement resume token, a sealed pairing
code, ciphertext or future credential-bearing fields. Both compact and pretty
Debug redact the entire payload, including unknown fields, rather than trying
to maintain a sensitive-field name list.

The frame variant, priority, generation and lease epoch remain available as
diagnostic metadata, and the real reader receipt retains its existing redacted
Debug representation. Connected/disconnected lifecycle and RTT diagnostics keep
their existing representation. This change does not add a logging call, change
wire JSON, clear tokens, or change business ownership and callback lifetimes.
It does not assert that an actual credential leak occurred or provide a general
redaction guarantee for arbitrary network error strings.

The existing production-module harness tests fake secrets with both `{:?}` and
`{:#?}`, checks unchanged reader JSON and normal lifecycle metadata, and retains
the real inbound protocol/transport tests. This harness is not a full native
IPC/host application test.

The lane event lives in the single production `controller/lane_event.rs` module
because its owned input and diagnostic representation can be tested independently
of the handshake/recovery loop. `controlling_service` re-exports that same type
at its existing path; there is no copied harness enum. The harness compiles the
actual transport, Relay client and lane event, but not the complete controlling
service. Native compilation of its re-export and callers remains a separate gate.

```sh
cargo test --locked --manifest-path pinvou3-app/src-tauri/tests/controller-crypto-harness/Cargo.toml relay_transport::tests
```
