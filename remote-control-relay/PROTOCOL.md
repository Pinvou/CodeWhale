# WebUI Relay protocol v2

The Relay connects one persistent desktop endpoint to at most one active WebUI
client. It authenticates peers and forwards a small, typed JSON protocol. It does
not interpret RPC commands, store conversation data, assign stream sequence
numbers, or maintain the replay journal.

All protocol messages are UTF-8 JSON objects and must contain the exact numeric
field `v: 2`. This applies both before and after authentication, including revoke
messages. The Relay rejects and closes a socket when the field is missing or has
any other value; it never upgrades an older message by rewriting its version.
Credentials belong only in the authentication messages. The browser link keeps
its credentials in the URL fragment, which is never included in an HTTP request.
On the wire, `access_token` is the only accepted access-token field; legacy
top-level aliases such as `token` and `pairing_token` are explicitly rejected.

## Desktop registration

```json
{
  "v": 2,
  "type": "desktop_endpoint_register",
  "endpoint_id": "endpoint_...",
  "access_token": "...",
  "desktop_secret": "..."
}
```

The first registration creates an in-memory endpoint unless its `endpoint_id`
has a persistent revoke tombstone. Later registrations must match both
credential hashes. A newer authenticated desktop socket replaces the old socket
without changing the endpoint or Web lease. A tombstoned ID is rejected with
`endpoint_revoked`, even when the caller still holds the old credentials.

When a newer registration replaces a still-connected desktop socket, the Relay
sends the old socket a `desktop_endpoint_replaced` notification and closes it
with WebSocket close code 4001. The endpoint identity, its credential hashes,
and any active Web lease are unchanged.

```json
{
  "v": 2,
  "type": "desktop_endpoint_registered",
  "endpoint_id": "endpoint_...",
  "web_client_connected": false,
  "lease_id": null
}
```

The endpoint remains available while its desktop is connected and for
`ENDPOINT_OFFLINE_TTL_MS` after the desktop disconnects (24 hours by default).
Reconnecting within that interval preserves the Web lease. When the TTL expires,
the Relay removes the in-memory endpoint, notifies and closes any Web client with
reason `desktop_offline_ttl_expired`, and releases its capacity. TTL expiry is
not a revoke: the desktop may later recreate the same endpoint with its
persistent credentials, at which point a browser must join again.

Live endpoints are not persisted. After a Relay restart, a non-revoked desktop
recreates its endpoint with the same credentials. Revoked endpoint IDs are the
exception: their credential-free tombstones survive restart and prevent
re-registration.

## Web client join and lease takeover

```json
{
  "v": 2,
  "type": "web_client_join",
  "endpoint_id": "endpoint_...",
  "access_token": "...",
  "stream_epoch": "last-seen-epoch",
  "after_seq": 42
}
```

On success, the Relay assigns an unguessable lease:

```json
{
  "v": 2,
  "type": "web_client_joined",
  "endpoint_id": "endpoint_...",
  "lease_id": "lease_...",
  "desktop_connected": true
}
```

Only one Web lease is active per endpoint. A newer client receives a new lease;
the previous client receives `endpoint_replaced` and is closed. The Relay sends
the desktop a `web_client_connected` status containing the active lease and the
join cursor. The desktop owns replay and decides whether to replay journaled
events, send `stream_reset`, or send `desktop_snapshot`.

## Forwarded messages

After authentication, the Relay accepts only these Web-to-desktop types:

- `rpc_request`
- `event_subscribe`
- `event_unsubscribe`
- `client_ready`

It accepts only these desktop-to-Web types:

- `rpc_response`
- `event`
- `stream_reset`
- `desktop_snapshot`

The original top-level fields are forwarded unchanged except that the Relay
sets authoritative `v`, `endpoint_id`, and `lease_id` fields. Credential fields
(`access_token`, `token`, `pairing_token`, `desktop_secret`) are additionally
stripped from forwarded frames. A desktop message that explicitly targets a
stale lease is dropped so a late response cannot leak into the replacement
client.

The desktop is responsible for command/event policy validation, idempotency via
`client_request_id`, `stream_epoch`, monotonic `seq`, and its bounded replay
journal. The Relay holds none of that business state.

`client_ready` is a two-phase handshake between the shared UI and desktop:

1. The initial `client_ready` carries `state_ready: false`. It negotiates
   capabilities and establishes the replay cursor, but the desktop must not
   deliver business events yet.
2. After the shared UI durably hydrates its local state, it sends
   `client_ready` with `state_ready: true`. Only then may the desktop replay the
   ready-window events after the negotiated cursor and begin live delivery.

Within the forwarded `event` envelope, the authoritative chat-turn allowlist
includes both `chat:user_message` (including `base_transcript_revision`) and
`chat:transcript_committed`. They are authoritative round events, not local UI
hints. The Relay forwards the typed envelope without interpreting either event.

## Connection status and revoke

When the desktop disconnects, the active Web client receives:

```json
{
  "v": 2,
  "type": "desktop_connection_state",
  "endpoint_id": "endpoint_...",
  "lease_id": "lease_...",
  "status": "offline"
}
```

The same message with `status: "connected"` is sent after desktop reconnection.
The desktop receives `web_client_connected` and `web_client_disconnected` with
the corresponding lease.

The desktop invalidates a link with:

```json
{
  "v": 2,
  "type": "desktop_endpoint_revoke",
  "endpoint_id": "endpoint_...",
  "desktop_secret": "...",
  "reason": "refreshed"
}
```

The Relay first writes the endpoint tombstone atomically to
`PINVOU_REMOTE_STATE_PATH`, fsyncs the temporary file, and renames it into place.
Only after that succeeds does the desktop receive `desktop_endpoint_revoked`.
The Web client then receives `endpoint_revoked` and is closed. A later Web join
fails with `endpoint_not_found`; an old desktop registration fails with
`endpoint_revoked`. If persistence fails, the Relay sends
`revoke_persistence_failed`, closes the requester, and leaves the endpoint
usable rather than acknowledging a revoke it cannot preserve.

The state file contains only schema version, endpoint IDs, and revoke times; it
does not contain access tokens, desktop secrets, or their hashes. Tombstones are
bounded by `MAX_REVOKED_ENDPOINTS` and `MAX_RELAY_STATE_BYTES`. Back up the state
file with the deployment's durable data. Deleting it deliberately re-enables old
endpoint IDs.

## Error codes

The Relay reports rejects with a `type: "error"` message carrying a `code`.
Codes enforced by the reject helper also close the socket with WebSocket close
code 1008; the remaining codes are informational errors that leave the
connection open.

| Code | Trigger |
| --- | --- |
| `bad_json` | A text frame is not valid JSON. |
| `bad_message` | The parsed frame is not a JSON object. |
| `binary_unsupported` | A binary WebSocket frame arrived. |
| `unsupported_protocol_version` | `v` is missing or is not `2`. |
| `legacy_token_alias_unsupported` | The message carries a legacy top-level `token` or `pairing_token` field. |
| `authentication_required` | A non-handshake message arrived before authentication completed. |
| `authentication_timeout` | The socket did not authenticate within the deadline. |
| `bad_desktop_endpoint_register` | Malformed `desktop_endpoint_register`: invalid `endpoint_id`, missing or oversized credentials, or the socket already authenticated. |
| `endpoint_capacity_reached` | Creating a new endpoint would exceed total endpoint capacity. |
| `endpoint_creation_rate_limited` | The per-IP endpoint creation rate was exceeded. |
| `invalid_access_token` | Desktop re-registration whose access token hash does not match the endpoint. |
| `invalid_desktop_secret` | Desktop re-registration or revoke whose desktop secret hash does not match the endpoint. |
| `invalid_token` | `web_client_join` whose access token hash does not match the endpoint. |
| `bad_web_client_join` | Malformed `web_client_join`: invalid `endpoint_id` or missing access token. |
| `endpoint_not_found` | Join, revoke, or forwarded message referenced an endpoint that does not exist. |
| `stale_lease` | A Web message arrived from a socket that no longer holds the active lease. |
| `desktop_endpoint_replaced` | A desktop message arrived from a socket already replaced by a newer registration. |
| `unsupported_message_type` | The authenticated message type is not in that direction's allowlist. |
| `desktop_offline` | A Web-to-desktop message arrived while the desktop socket is not connected. |
| `ingress_rate_limited` | The per-connection ingress message or byte rate was exceeded. |
| `endpoint_revoked` | Desktop registration for a tombstoned endpoint ID. The same name is also the message type delivered to the Web client on revoke or TTL expiry, with `reason` such as `desktop_offline_ttl_expired`. |
| `revocation_in_progress` | A second revoke arrived while one is already in progress for the endpoint. |
| `revoke_persistence_failed` | Persisting the revoke tombstone failed; the requester is closed and the endpoint remains usable. |
| `revoke_failed` | An unexpected error occurred while processing a revoke. |

## HTTP and operational behavior

- `GET /pinvou3/remote/healthz` exposes aggregate counters only.
- `/pinvou3/remote/` serves `web/dist/index.html`; extensionless paths use the
  same SPA fallback.
- Hashed `assets/` files are immutable. HTML is never cached and fixed-name
  scripts must be revalidated so a WebUI update takes effect.
- WebSocket authentication has a deadline. The server also enforces total
  socket capacity, endpoint capacity and creation rate, per-IP connection rate,
  per-connection ingress message and byte rates, maximum message size, proxy
  allowlists, and ping/pong liveness.
- Protocol v2 requires `MAX_PAYLOAD_BYTES` to be at least 4 MiB. Desktop-originated
  frames are bounded by the complete serialized WebSocket message, not just the
  event payload, and stay at or below `4 MiB - 64 KiB`.
- The desktop replay journal retains at most 1,024 complete event frames and
  16 MiB. When either boundary evicts the requested cursor, the desktop rotates
  the stream epoch and sends `stream_reset`; the Relay never fabricates a gap.
- Every outbound send checks `ws.bufferedAmount` plus the next encoded frame
  against `WS_MAX_BUFFERED_BYTES`. A slow recipient is terminated when the
  high-water mark would be crossed; the Relay does not accumulate an unbounded
  per-client queue.
- Browser WebSocket upgrades must use either a same-host `Origin` or an origin
  listed in `PINVOU_REMOTE_ALLOWED_WEB_ORIGINS`. Native desktop clients may omit
  `Origin`. The check assumes the reverse proxy preserves the public `Host` and
  is defense in depth against cross-site WebSocket hijacking.
- Access tokens and desktop secrets are retained only as SHA-256 hashes and are
  compared with constant-time equality.

Relevant limits are configured with `ENDPOINT_OFFLINE_TTL_MS`,
`WS_INGRESS_WINDOW_MS`, `WS_INGRESS_MESSAGE_LIMIT`,
`WS_INGRESS_BYTE_LIMIT`, `MAX_PAYLOAD_BYTES`, `WS_MAX_BUFFERED_BYTES`,
`MAX_REVOKED_ENDPOINTS`, and `MAX_RELAY_STATE_BYTES`. Relay tests always point
`PINVOU_REMOTE_STATE_PATH` at a temporary directory.

Origin checks and caller-supplied first-registration credentials are not a
production enrollment system. Formal desktop enrollment remains a deployment
gate and must be provided before exposing unrestricted endpoint registration to
an untrusted network; protocol v2 intentionally does not design or enable that
registration authority.

## Desktop controller protocol v1 (feature gated)

### DEV-00 normative contract (supersedes summaries below)

The rules in this subsection are normative where an older summary later in
this section differs. DEV-00 freezes the contract; later tasks connect it to
the live WebSocket dispatcher behind `PINVOU_DESKTOP_CONTROLLER_ENABLED=1`.
WebUI v2 behavior remains unchanged when the controller is disabled.

Desktop peers use subprotocol `pinvou-desktop-controller-v1`, JSON `v: 1`, and
the exact capability registry frozen in `protocol-controller.js`. Unknown
versions, message/payload types, fields, enum values, capabilities, duplicates
and role combinations fail closed. Capability negotiation is
intersection-only: Relay sorts the exact A/B intersection bytewise, rejects an
empty intersection, and never inserts a capability absent from either side.
Duplicate object keys are rejected from the original JSON text at every depth,
including escaped spellings of the same key; they fail locally as `invalid_json`
before the schema validator or controller dispatcher sees a collapsed object.
Legacy WebUI v2 keeps its existing duplicate-field parsing, except that any
repeated `v` key (including escaped spellings) is rejected before routing. A
mixed `v: 1` / `v: 2` object can therefore never enter the v2 lane.
General validation has two entry modes. Shape-only parsing checks schema, size,
role and any supplied lease context without assuming negotiation state. Runtime
state-machine dispatch requires a validation context containing the frozen
`negotiated_capabilities`; omitting it fails closed as
`unsupported_capability: negotiated_capabilities`, including for
`controller_capabilities` and `error`. Their shape-only entry point remains
available without runtime context, while their runtime entry point requires an
explicit context whose capability list may be empty. Every message whose schema
contains `controller_lease_id`, and every decrypted payload, additionally
requires a non-empty runtime `lease` context containing the complete
`connection_id + controller_lease_id + lease_epoch + relay_epoch +
remote_control_generation` tuple. Missing lease context is the stable local
`missing_field: runtime_context.lease` failure; a supplied cross-lease tuple is
`stale_context`. Within the general runtime validator, only the manifest's
explicit `runtime_pre_lease_messages` allowlist may be processed without a lease:
`controller_capabilities`,
`desktop_endpoint_claim`, `desktop_pairing_register`,
`desktop_pairing_registered`, `desktop_pairing_revoke`,
`controller_pairing_claim`, `controller_pairing_reserved`,
`finished_receipt_commit`, `encrypted_handshake_finished`,
`finished_receipt_ack`, and `error`. Every encrypted payload is checked
against the negotiated list before decryption dispatch. All numeric counters, `lease_epoch`,
byte counts and timestamps except literal `v`/`protocol_version` use canonical unsigned 64-bit
decimal strings matching `0|[1-9][0-9]{0,19}` and bounded by
`18446744073709551615`; JSON numbers, signs and leading zeroes are invalid.
Nested token counts use the same spelling and are additionally uint32-bounded.
`relay_epoch`, `liveness_epoch_*`, `journal_epoch` and
`remote_control_generation` are explicitly opaque, bounded identifiers rather
than JSON integers; their exact bytes are authenticated and compared.

Every runtime `negotiated_capabilities` list is the bytewise strictly ascending
canonical A/B intersection. Empty is permitted only where the message requires
no specific capability. Unknown names, invalid identifier spelling, duplicates,
or out-of-order entries all fail identically as
`unsupported_capability: negotiated_capabilities`; callers must not sort or
deduplicate an invalid inbound context on its behalf.

The capability hash is SHA-256 of ASCII
`pinvou-controller-capabilities-v1` followed by every negotiated capability in
bytewise sorted order as `LP(UTF-8(value))`. Its complete-v1 digest is
`92368be252a01acfed541dcd7109ec74bb92777728d445b729060b9b67b77d61`.

`desktop_pairing_register` carries both a transport `request_id` and a stable
`registration_request_id`. B creates a new registration ID only after changing
`remote_control_generation`; reconnect/retry for one generation reuses the
same registration ID while transport request IDs may change. Relay atomically
keys idempotency by `(endpoint_id, remote_control_generation,
registration_request_id)`, seals the first terminal success or failure, and
returns the same sealed outcome for subsequent matching retries: the response
echoes the current transport `request_id`, while every other result field is
byte-identical. Reusing either
member with different registration inputs is `invalid_message`; refreshing or
stopping invalidates the old generation before a new key may be accepted.
`desktop_pairing_registered` echoes the stable ID so B can discard a delayed
result from an older request/generation. A sealed registration failure uses the
ordinary `error` envelope with optional `registration_request_id` and
`remote_control_generation`; those fields are forbidden from changing across
retries just like a successful sealed outcome. When a registration validation
context exists, both fields become mandatory on either success or failure and
must match it. This independent registration context is not a lease context.
Reusing a stable registration ID with a different generation, endpoint,
identity, public key, algorithm or capability list is `invalid_message`.

The claim/reservation authority is connected by the later Relay composition
when the controller feature flag is enabled. A claim
normalizes ASCII case and removes whitespace/hyphen separators before applying
the seven-character alphabet. Source, socket and global fixed-window limits run
before code lookup, and their bounded source/socket maps fail closed when full.
Invalid syntax, absent, expired, consumed, revoked, stale-generation and
rate-limited claims all expose only `pairing_code_invalid`.

Each endpoint has one revisioned interaction slot. Its owner is exactly one of
`web_lease`, `desktop_reservation`, or `controller_lease`; mutation requires a
matching canonical uint64 revision. A free slot may be claimed by either Web or
desktop, a Web owner may be replaced only by the existing Web-to-Web takeover,
and a desktop owner maps a legacy Web join to the existing
`endpoint_not_found` compatibility response. An existing Web/controller owner
makes a new desktop claim `controller_busy`; a losing claim for the same
pairing collapses to `pairing_code_invalid`. Neither failure consumes the code.

A desktop reservation binds the stable claim `request_id`, claim-socket
identity, pairing/endpoint/generation and SHA-256 of A's canonical 32-byte
handshake public key. An exact request retry returns the same reservation;
reusing its request ID with different bindings fails closed. Reserved state has
a 10-second deadline; entering `handshake_pending` starts its separate
20-second deadline. Owner disconnect, timeout, refresh or handshake failure first durably
terminates the one-shot pairing and requests B's replacement-code notification,
then releases the slot. If either durable invalidation or notification fails,
the slot remains closed in retryable `invalidating` state. The module stores no
raw pairing code and emits only fixed audit event names. The notification
receives the pairing, endpoint and generation bindings needed to reject a stale
refresh result.

DEV-06 composes this authority into the live dispatcher only when
`PINVOU_DESKTOP_CONTROLLER_ENABLED=1`. Startup then fails closed unless the
independent pairing digest key and a canonical Ed25519 PKCS#8 signing key are
present alongside the enrollment configuration. Controller v1 and legacy Web
v2 share the same endpoint interaction slot, while their wire versions remain
strictly separate; disabling the controller leaves the v2 rejection and
takeover behavior unchanged.

The Relay signs the canonical transcript after resolving an online enrolled B
socket and immediately moves the reservation into its separate 20-second
handshake window. It forwards commitments and encrypted Finished frames without
decrypting them. Because v1 intentionally defines no second commitment-ACK
shape, Relay confirmation is the byte-equivalent validated commitment echoed to
its sender while the same envelope is forwarded to the peer. A receipt is accepted only from the opposite authenticated
socket after that sender's commitment and Finished; the decoded 32-byte secret
is zeroed immediately after constant-time commitment verification. Only both
opposite receipts invoke PairingStore's revision-CAS consumption. Exactly one
consumer durably terminates the pairing before the in-process lease is exposed;
a crash or injected failure can therefore lose an unannounced lease but can
never revive its one-shot pairing or expose a half-created lease.

The resulting lease binds the connection, lease ID/epoch, Relay epoch,
endpoint/generation, claim identity, transcript hash, exact negotiated
capabilities and both authenticated sockets. A and B receive different random
resume tokens and liveness epochs; Relay retains only role-separated token
digests bound to role, Relay epoch, connection, lease ID/epoch and generation.
Any ordering, binding, role, socket, receipt, persistence or delivery
failure terminates the pairing, releases the shared slot and asks the still
connected B to register a replacement code. Business frames and resume remain
closed until their owning tasks install the frozen router hooks.

`controller_joined` is the sole `lease_install_messages` entry and never
requires a pre-existing lease. It is not accepted by the general runtime
validator. DEV-00 parses the complete raw `controller_pairing_reserved`
envelope into a non-authenticated `ParsedTranscript`: strict schema and role
validation, canonical preimage reconstruction, `transcript_hash` comparison,
and signature algorithm/length validation all run, but a successful parse does
not establish signer identity and cannot authorize lease installation. A
configuration string such as `source=official_enrollment`, raw JSON, an
arbitrary key, or a caller-supplied boolean/callback is never authentication
proof.

The JS module exports only structural parsers, canonical preimage builders,
sealed-code fixture helpers, and Relay-side receipt commitment verification. It does not export
`VerifiedTranscript`, a trust-store/key loader, a signature-to-token function,
or any A/B session-key primitive. Rust exposes `ParsedTranscript` with private
fields and reserves `VerifiedTranscript` behind the single concrete
`TranscriptAuthenticator`. The authenticator's trust state and constructor are
private to `controller::crypto`; there is no trait that another controller
module can implement and no callback/boolean verification escape hatch.

DEV-05 implements that concrete authenticator with Ring Ed25519 verification. Its
input is an opaque `AuthenticatedRelayTrustConfig` whose key ID and 32-byte
public key are atomically bound by a trusted enrollment/configuration loader;
there is deliberately no production raw JSON/string/public-key constructor.
The fixture-only trust configuration constructor is compiled under `cfg(test)`;
the concrete authenticator constructor remains private even in production code. Loading official
embedded pins or administrator-approved self-hosted pins into this opaque type
is owned by the later controller configuration/transport wiring; until that
trusted loader supplies the value, no production caller can obtain a
`VerifiedTranscript`. A claimed source name, arbitrary key, or always-true
callback therefore cannot authenticate a transcript. The controller business
path remains disabled. `LeaseInstallValidationContext` accepts only the opaque
authenticated result and derives `request_id`, expected `pairing_id`, `reservation_id`,
`remote_control_generation`, `relay_epoch`, `transcript_hash`, expected signed
`controller_connection_identity`, and the exact canonical
`negotiated_capabilities + capability_hash` from it. It accepts no independently
overridable copy of those fields. Deleting, adding, replacing, duplicating, or
reordering capabilities therefore requires a fresh valid Relay signature. The
Relay result must echo all seven bindings and requires `pairing_v1`; any
mismatch is `stale_context`. Only after all bindings and the exact result schema
validate does the validator return a new immutable `LeaseContext` containing
`connection_id + controller_lease_id + lease_epoch + relay_epoch +
remote_control_generation`. Thus an attacker cannot make the receiver construct
its expected identity or epoch from the same unverified result being checked.

After reservation, messages carry `pairing_id + reservation_id + relay_epoch +
remote_control_generation + transcript_hash`. After lease creation, every
control message and decrypted business request/event carries `connection_id +
controller_lease_id + lease_epoch + relay_epoch +
remote_control_generation`. Receivers compare every applicable value before
mutation. Refresh, stop, Relay epoch change or lease epoch change closes the
old receive gate, so late frames and replies fail with `stale_context` even if
their cryptography is otherwise valid.

The only minimal outer exception is `encrypted_controller_liveness_a`, which
contains exactly `v`, `type`, routing `connection_id/controller_lease_id/
lease_epoch`, bounded `ciphertext_b64`, and canonical `ciphertext_bytes`.
Relay cannot see its strict inner payload:

```text
{v:1,type:"controller_liveness_a",connection_id,controller_lease_id,
 lease_epoch,relay_epoch,remote_control_generation,role:"A",
 liveness_epoch_a,heartbeat_sequence_a,challenge_id,challenge_b64}
```

Its A-to-B AEAD AAD is ASCII `pinvou-controller-liveness-aad-v1` followed
by LP-encoded `connection_id, controller_lease_id, lease_epoch, relay_epoch,
remote_control_generation, role` in that exact order. B rejects a changed
context, role, liveness epoch or challenge, and compares the sequence with the
last accepted value under this precise rule:
lower rejects with `replay`; equal is an idempotent `same_sequence_tighten`
update that may only move the existing local deadline earlier; greater replaces
the stored sequence and may establish the new conservative deadline.

`controller_liveness_attestation_a` carries full lease context plus
`liveness_epoch_a`, `heartbeat_sequence_a`, `attestation_sequence`,
`challenge_id`, `challenge_b64`, `observed_age_ms`, `remaining_ttl_ms`,
`issued_at_relay`, `relay_signing_key_id`, `signature_algorithm=ed25519`, and
`relay_signature_b64`. The signature preimage is ASCII
`pinvou-controller-liveness-attestation-v1` followed by LP-encoded
`relay_epoch, connection_id, controller_lease_id, lease_epoch,
remote_control_generation, liveness_epoch_a, heartbeat_sequence_a,
attestation_sequence, challenge_b64, observed_age_ms, remaining_ttl_ms,
issued_at_relay, signature_algorithm, relay_signing_key_id`. The shared fixture
freezes the full preimage/signature and tests tampering, replay and cross-lease
rejection. B compares `heartbeat_sequence_a` against A's last accepted
heartbeat using the lower/equal/greater rule above. The independent Relay
`attestation_sequence` is stricter: both lower and equal values are replay;
only a greater value is accepted. The state layer may replace the deadline when
the attestation advances and A's heartbeat advances; an equal A heartbeat may
only tighten the existing deadline.

### A's authenticated proof of B and recovery cutoff

A's own `controller_liveness` transport admission is not evidence that B is
alive and MUST NOT renew A's local lifetime. A instead sends
`controller_liveness_attestation_request_b` to Relay with its current full
lease context, `request_id`, and a fresh one-shot `challenge_id/challenge_b64`.
Only authenticated A may request this proof. Relay signs
`controller_liveness_attestation_b` from its latest strictly increasing
heartbeat received on the currently bound B socket, never from A activity or
the initial sequence-zero seed. B evidence older than 5 seconds is rejected.
This is explicitly a trusted-Relay observation of authenticated B socket
activity, not an end-to-end signature made by B. It neither proves task progress
nor authorizes any business operation. The Relay trust root pinned for pairing
also verifies these proofs for the lifetime of that same connection.

The proof uses domain `pinvou-controller-peer-b-attestation-v1`, followed by
LP-encoded `v` (decimal text), `type, request_id, connection_id,
controller_lease_id, lease_epoch, relay_epoch, remote_control_generation,
liveness_epoch_b, heartbeat_sequence_b, attestation_sequence, challenge_id,
challenge_b64, observed_age_ms, remaining_ttl_ms, issued_at_relay,
relay_signing_key_id, signature_algorithm`. It is distinct from the A-proof
signature domain. B-proof sequences and bounded consumed challenge-ID history
are independent of A-proof state; reusing an ID with different bytes is still
a replay within the 30-second validity window.

A retains at most one pending challenge bound to its local send time, exact
lease/generation, B epoch, and both correlation IDs. It verifies the pinned
Relay signature, full context, schema, capability, sequence floors and pending
challenge before committing any state. The local candidate cutoff is
`receive_time + min(remaining_ttl_ms, 30000 - observed_age_ms) - full_RTT - 1000`;
RTT and observation age may each be at most 5000 ms. Relay wall-clock time is
signed metadata, never a local deadline. The timer is anchored to receipt,
excluding signature-validation and scheduling cost. New B heartbeat sequences
may replace the cutoff; equal sequences may only tighten it, and older B or
replayed proof sequences are rejected. Failed admission, unanswered requests,
bare B challenges and A heartbeats grant no lifetime.
The Rust B worker produces ordinary role-B heartbeats from its verified
joined/final-resumed lease every control poll. The full original operation and
lease stay locked through enqueue; transport disconnect, resume admission and
retired workers cannot publish them. A successful enqueue alone is not proof
for A: only the Relay's authenticated B socket observation can be signed. B's
own watchdog for A is never renewed by these heartbeats. Resume installs B's
validated new epoch and restarts the role sequence only after the final gate.
An exact pending proof-request `error` consumes only that request without
changing the cutoff or failing another capability/journal request. The keeper
pins its original connection intent and transport; a replaced keeper cannot
send through the replacement transport, commit evidence, or clear its lane.

Initial authenticated join starts one finite 30-second cycle. The first
transport disconnect freezes the last valid local cutoff and clears pending
proofs. In-flight proofs, retry traffic and repeated disconnects never extend
it. Only a fully authenticated final resume, consistent with the confirmed B
epoch and successfully reinstalled transport authority, starts a fresh cycle.
This unreleased desktop protocol is upgraded atomically with its contract hash;
mixed implementations fail closed without falling back to self-TX renewal.

B business preparation may run on a serial worker while control/resume polling
continues. Final task admission pins the exact connected five-field lease,
capability revision and session mapping through durable ledger admission and
task-resource installation. Workspace authority is resolved before the session
lock; lock order remains owner → session → ledger. Journal and cleanup callbacks
run only after releasing those locks. A transient disconnect preserves an
already admitted task and its idempotency record; lifecycle cleanup before
dispatch prevents executor entry.

`controller_pairing_reserved` delivers the same strict
`controller_pairing_transcript/v1` object to both peers. Its ordered fields are
`schema, relay_epoch, pairing_id, reservation_id, endpoint_id,
remote_control_generation, controller_connection_identity, role_a,
handshake_public_key_a_b64, role_b, handshake_public_key_b_b64,
protocol_version, negotiated_capabilities, capability_hash, expires_at_ms,
nonce_b64`. `negotiated_capabilities` is the non-empty, unique, bytewise-sorted
A/B intersection and `capability_hash` must equal the hash of that exact list.
Roles are exactly
`A` then `B`; the two 32-byte X25519 public keys are distinct; nonce is 16
bytes. The canonical preimage is unnulled ASCII
`pinvou-controller-pairing-transcript-v1`, then LP encodings of the scalar
fields through `protocol_version` (`1` becomes ASCII `1`), then
`LP(decimal capability count)`, one `LP(capability)` per listed value, and
finally LP encodings of `capability_hash`, `expires_at_ms`, and `nonce_b64`.
Relay signs this complete preimage with
Ed25519. The envelope repeats and validates reservation/generation/Relay
context, `transcript_hash`, `relay_signing_key_id`, algorithm and signature.

The fixed transcript vector is fully listed in the shared fixture. It has
SHA-256 `c98cbd16e78bd50dd49ef2ad0cbdff7e3fa91ec7e33d2e697f002ca7cb408163`,
raw public key `Z/Wupu8RK99T+q0LnTHaE3Iny4RP1NDVkupAQaGUysE=`, and signature
`LtNLmSZGf4nRBQEnJSYocA//IlE+ioVLuadCGEBBQoY63U1IivepwONIwvyYXlPJqTz0u2hDbzZqQX3Wq2WQCw==`.
A and B independently reconstruct the same bytes and verify every field before
key derivation. Shared negative inputs include capability deletion/replacement
with a synchronously recomputed capability hash but the original transcript
authentication, plus a full-transcript signature mutation. Receipt commitment
remains the independent sender-secret LP
algorithm below; Rust constructs it from individual fields rather than hashing
a fixture-supplied preimage.

Every decrypted payload carries full lease context. A requests B's runtime
allowlist with `capability_snapshot_request {request_id,session_id,known_revision}`;
`session_id` is A's Rust-generated RemoteProxy reference. B first resolves the
current generation's authoritative local Session and returns
`capability_snapshot {request_id,session_id,controlled_session_id,
capability_revision,expires_at_ms,modes,tools,skills,workspaces}`. A accepts
the B Session mapping only after encrypted authentication and exact request,
reference and lease checks. `valid_for_ms` is optional TTL metadata, not a
mapping fallback. If snapshot construction fails, B returns
`capability_snapshot_error {request_id,session_id,code}` with
the bounded `capability_snapshot_unavailable` code. Tool entries contain only an opaque ID and
display name. Skill entries additionally contain a non-empty, duplicate-free `scopes` array whose
values are `chat` and/or `code`; one opaque skill ID can therefore be advertised for both lanes
without collapsing their authorization scopes. Workspace entries contain only opaque ID, display name and
`code_allowed`; paths, configuration, source and credentials are forbidden.
Lists are bounded to 256 unique entries, revisions are canonical decimal
strings, and a task binds the exact `capability_revision` it consumed.

`task_submit.input` is exactly `{text:string, attachment_ids:string[],
skill_ids:string[], workspace_id?:string}` with 64 KiB UTF-8 text and at
most 32 attachment/Skill references; at least one of text/attachments must be non-empty. Task
status uses `task_status` with exactly `admitted`, `running`,
`waiting_permission`, `succeeded`, `failed`, `cancelled`, `interrupted`, or
`result_unknown`; `completed` is invalid.

For `model_source=controller_temporary`, `task_submit` requires one strict,
64-KiB `temporary_model_package/v1`; it is forbidden for `controlled_device`.
The package requires `schema, package_id, binding, primary,
primary_credential`, with optional one-level `vision_fallback`. Binding is
exactly `connection_id + controller_lease_id + lease_epoch + task_id` and must
equal the containing task. A route requires `display_name, preset, wire_api,
base_url, model, image_capability_override` and only optionally accepts
`provider_kind, vendor, endpoint_mode, context_window_tokens,
max_output_tokens, reasoning_effort`. Presets are `local_vllm`, `deepseek`,
`kimi`, `openai_compatible`, `qwen`, `doubao`, `minimax`, `glm`, `mimo`,
`openai`, `anthropic`, `gemini`, `xai`; Anthropic alone uses
`anthropic_messages`, all others use `openai_chat_completions`. Credentials
are exactly `{kind:"api_key",secret:string}`. OAuth, cookies, subscription
login, opaque SavedModel fields, credential references and recursive fallback
are forbidden.

Model `base_url` has one JS/Rust grammar, with no WHATWG normalization: at most
2048 ASCII bytes; exact lowercase `http://` or `https://`; a lowercase DNS
host (labels 1..63, total at most 253, no edge hyphen/trailing dot) or canonical
dotted IPv4; optional canonical decimal port `1..65535`; no userinfo, bracketed
or bare IPv6, fragment, whitespace, or backslash. The suffix is an optional
`/` path and/or `?` query using RFC 3986 ASCII unreserved/reserved characters;
each percent escape is exactly `%` plus two uppercase hex digits. Lowercase
escapes and malformed escapes fail rather than normalize.

The `artifact_index` operation returns bounded metadata in `body.artifacts`
of a successful `remote_action_result`, never as a standalone payload.
Each item is exactly `artifact_id, task_id, display_name, mime_type,
total_bytes, sha256, preview_kind`; preview kinds are `text`,
`download_only`, or `unsupported`. Subsequent preview and download requests
use `remote_action_request` under the same lease/session context. Preview
selects the opaque `artifact_id`; downloads also bind `task_id`, and their
chunk/abort operations additionally bind `transfer_id`. No host path is
accepted as read authority.

The shared fixture freezes the contract SHA-256, complete registries, exhaustive
outer-message role matrix, canonical transcript/receipt/liveness/attestation
vectors, strict negative categories, public pairing error collapse, and real
legacy WebUI v2 join/register/conflict shapes. Absent, expired, consumed,
revoked and stale-generation codes all map to `pairing_code_invalid` and user
text “连接码无效或已失效”. A legacy conflict remains exactly
`{"v":2,"type":"error","code":"endpoint_not_found","message":"endpoint not found"}`
plus close code `1008`.

The contract digest is
`11c728929280ade5f529efc38a0fca6d618a77839bba1c7e48938b34e781f889`.
It is SHA-256 over canonical JSON (object keys recursively sorted, array order
preserved, compact JSON scalars) of `{contract,fixture}`. `contract` is the
executable manifest containing every schema required/optional field, role
matrix, registry, enum, limit, payload-to-capability mapping, URL grammar and
transcript field order. `fixture` is the complete shared fixture after removing
only `contract.sha256`, avoiding self-reference. The expected digest is
hard-coded in JS and Rust. Both validators and both manifests consume their
production schema descriptors, nested schemas, route fields and capability
bindings. Rust independently rebuilds the canonical manifest from those
required/optional fields, roles, enum registries and limits before hashing the
shared fixture, so either implementation drifting fails tests;
fixture or schema edits must explicitly regenerate and review the digest.

Logs are allowlist projections only. They must exclude all pairing/lease/
generation/endpoint identifiers, keys, nonces, secrets, tokens, ciphertext,
task input, model packages and credentials, attachments, artifacts, snapshots
and journal data.

DEV-00 froze the desktop-to-desktop controller contract before its state
machines and cryptography were implemented. Later tasks connected the protocol
through the Relay and Tauri paths. The Relay routes controller messages only
when `PINVOU_DESKTOP_CONTROLLER_ENABLED=1`; DEV-00 itself enables no business
path.

Controller clients select the WebSocket subprotocol
`pinvou-desktop-controller-v1`. The frozen schema includes an explicit
`controller_capabilities` declaration intended as the first controller-v1
application message. The current feature-gated live router has not connected
that standalone declaration, so clients must not send it to the live router.
Instead, B's `desktop_pairing_register` and A's `controller_pairing_claim`
carry their capability lists; the Relay takes their exact intersection.
When the standalone declaration is connected, it is not authentication. In
that intended v1 flow, before role binding a B candidate may proceed only to
`desktop_endpoint_claim`, and an A
candidate only to `controller_pairing_claim`. B becomes authenticated only when
its enrollment proof succeeds; A becomes the temporary authenticated A identity
only when the Relay atomically binds the successful claim socket to its
reservation. Every controller envelope and decrypted payload has the
exact integer `v: 1`; it is a separate namespace from the existing WebUI v2
protocol. A socket without this subprotocol remains a WebUI v2 socket and its
wire behavior above is unchanged. There is no automatic downgrade between the
two protocols.
In the current live composition, B's enrollment proof is the existing v2
`desktop_endpoint_register` on the negotiated desktop socket. The frozen v1
`desktop_endpoint_claim` schema is not yet a separate live authentication path.
The controlled desktop's persistent endpoint socket negotiates this subprotocol
because it carries controller v1 frames, while its existing v2
`desktop_endpoint_register` and WebUI forwarding continue on that same socket.
An ordinary browser socket does not negotiate the controller subprotocol and
cannot submit controller v1 frames. The Relay binds a controlled controller
identity only to a negotiated desktop socket.
If an older Relay or proxy omits the requested subprotocol in its upgrade
response, the desktop opens a fresh v2-only connection to preserve WebUI
access. That connection cannot carry controller v1 traffic: already queued v1
frames are discarded with a bounded `desktop_controller_unavailable` signal,
and new v1 sends fail immediately. The enrollment gateway negotiates a
controller subprotocol downstream only after its actual upstream connection
has negotiated the same value. Its first-frame authorization and subsequent
text forwarding in both directions reject repeated `v` keys before forwarding
raw JSON; ordinary duplicate v2 fields retain their legacy behavior.

The two authenticated desktop roles are:

- `A` / `desktop_controller`: the computer initiating remote control.
- `B` / `desktop_controlled_endpoint`: the computer being controlled.

The Relay is the only sender represented by the internal `relay` transport
role. A role may send only the message types listed for it below. A claimed
`sender_role`, `receiver_role`, frame direction, connection, generation or
lease never changes the role authenticated by the socket.

### Version, capabilities and limits

The frozen capability values are `pairing_v1`, `encrypted_frames_v1`,
`finished_receipt_v1`, `role_liveness_v1`, `resume_v1`, `task_v1`,
`temporary_model_package_v1`,
`permission_v1`, `attachment_v1`, `artifact_v1` and
`snapshot_journal_v1`. Unknown capabilities, message types, payload types,
enum values or object fields are errors. All required fields must be present;
`null` is not a substitute for omission or a typed value.

| Item | Limit |
| --- | ---: |
| Non-frame controller JSON message | 64 KiB encoded UTF-8 |
| Encrypted frame ciphertext | 2 MiB decoded bytes |
| Decrypted payload JSON | 512 KiB encoded UTF-8 |
| Attachment/artifact chunk | 256 KiB decoded bytes |
| Identifier | 128 ASCII characters |
| File name | 255 Unicode scalar values |
| MIME type | 128 ASCII characters |

Base64 is standard padded RFC 4648 and must round-trip to the canonical spelling.
SHA-256 values and commitments are exactly 64 lowercase hexadecimal characters.
Identifiers begin with an ASCII alphanumeric and then contain only ASCII
alphanumerics, `.`, `_`, `:`, or `-`. The pairing code is exactly seven
characters from `23456789ABCDEFGHJKMNPQRSTUVWXYZ`.

Every message schema is an exact object. The authoritative field lists and
role allowlists are executable in `protocol-controller.js` and the shared
`test/fixtures/controller-protocol-v1.json`; this table summarizes their state
transitions.

| Sender | Message | Purpose |
| --- | --- | --- |
| A/B | `controller_capabilities` | Bind authenticated role, protocol version and exact capability list. |
| B | `desktop_endpoint_claim` | Present an enrollment proof for an existing endpoint. |
| B | `desktop_pairing_register` | Register one generation with stable `registration_request_id` and independent code-wrap/handshake keys. |
| Relay | `desktop_pairing_registered` | Echo the registration ID and return its sealed result, `pairing_id`, expiry and `relay_epoch`. |
| B | `desktop_pairing_revoke` | Retire the generation before refresh or stop. |
| A | `controller_pairing_claim` | Claim a seven-character code with a stable `claim_request_id`. |
| Relay | `controller_pairing_reserved` | Bind the claim socket to one reservation and signed transcript. |
| A/B | `finished_receipt_commit` | Commit the role's independently random receipt secret. |
| A/B | `encrypted_handshake_finished` | Deliver that sender's Finished and receipt secret only inside ciphertext. |
| A/B | `finished_receipt_ack` | The opposite receiver returns the received secret to the Relay exactly once. |
| Relay | `controller_joined` | Echo request/pairing/reservation/transcript bindings and atomically announce the consumed pairing and newly installed controller lease. |
| A/B | `controller_liveness` | Advance only that transport role's epoch and monotonic sequence. |
| B | `controller_liveness_challenge_a` | Create a one-use challenge for direct A-to-B proof. |
| A | `encrypted_controller_liveness_a` | Relay-blind E2E liveness response bound to connection, lease and challenge. |
| B | `controller_liveness_attestation_request_a` | Request the bounded Relay-signed fallback proof. |
| Relay | `controller_liveness_attestation_a` | Return observed age, remaining TTL, sequence and signature. |
| A/B | `controller_resume_prepare` | Consume neither role token; prepare a role-bound resume candidate. |
| Relay | `controller_resume_provisional` | Announce a candidate on which business frames are forbidden. |
| A/B | `encrypted_resume_confirm` | Cross-confirm the candidate under the old E2E context. |
| B | `controller_resume_activation_ack` | Confirm B accepted the candidate before activation. |
| Relay | `controller_resume_activation_pending` | Reserve replacement material while the old watchdog remains active. |
| B | `controller_post_activation_liveness` | Supply the first new B liveness for the activation. |
| Relay | `controller_post_activation_liveness_confirmed` | Cross-notify the exact accepted B liveness. |
| A/B | `controller_resume_ready_ack` | Each role independently acknowledges the same activation and liveness. |
| Relay | `controller_resumed` | Atomically rotate the recipient's role token, return the next cycle's `resume_deadline_ms`, and open the business gate. |

`resume_deadline_ms` and echoed `original_deadline_ms` are opaque Relay-clock
cycle identities. Endpoints must never compare them with local monotonic time.
At the first actual transport interruption, each endpoint freezes the cutoff
from its last valid authenticated role-liveness evidence; Relay synchronously
snapshots both role cutoffs before close hooks remove that evidence. All resume
phases share that remaining budget: retries, B-only activity, and intermediate
resume frames cannot extend it. A complete authenticated join or final resumed
activation establishes a finite new cycle (at most 30 seconds); the final frame
and persisted lease use exactly the same next-cycle identity.
| A/B | `encrypted_frame` | Carry one encrypted typed payload in the direction fixed by socket role. |
| A/B | `controller_disconnect` | End the current lease; B local stop remains authoritative. |
| Relay | `error` | Return a bounded generic error from the controller error registry. |

`remote_control_generation` is owned by B. Pairing, reservation, connection and
controller lease identifiers are distinct domains. `lease_epoch` prevents an
old connection from addressing a replacement lease. Messages with an old or
cross-bound identifier are rejected by the later state machine even when their
JSON shape is valid. Pairing registration additionally uses the stable
`registration_request_id`; the tuple with generation is its idempotency key and
`desktop_pairing_registered` echoes it. A transport reconnect may change only
`request_id`, while refresh/stop must change generation and registration ID.

The frozen decrypted `encrypted_frame` payload kinds are
`capability_snapshot_request`, `capability_snapshot`, `capability_snapshot_error`, `task_submit`,
`task_cancel`, `task_status`, `remote_action_request`, `remote_action_result`,
`session_snapshot_request`, `session_snapshot`, `journal_replay_request` and
`journal_event`. A sends capability, task, cancel, closed operation-specific
actions and replay requests. B sends capability snapshots/errors, task states,
correlated action results, snapshots and journal events. The parser
rejects these payloads when presented by the opposite role. Every task,
permission, attachment, artifact, snapshot and journal operation is bound to
the same `connection_id + controller_lease_id + lease_epoch`; task and transfer
messages additionally carry their exact task/session/request identifiers. The
action envelope requires `task_v1`; after decryption, permission, attachment
and artifact operations also require their own negotiated capability.
The outer gate maps pairing messages to `pairing_v1`, receipt/Finished messages
to `finished_receipt_v1`, liveness messages to `role_liveness_v1`, resume
messages to `resume_v1`, and encrypted frames/disconnect to
`encrypted_frames_v1`. Capability announcement and generic errors are the only
outer messages without a particular negotiated capability requirement; their
runtime validation still requires an explicit capability context. Capability
snapshot and task payloads require negotiated `task_v1`; permission,
attachment and artifact payloads require their same-named `*_v1`; snapshots and
journal payloads require `snapshot_journal_v1`. `encrypted_frame` requires both
`encrypted_frames_v1` and the capability for its declared `payload_kind` before
forwarding.

### Pairing, Finished receipt and lease transition

The state order is `waiting_for_pairing` → `pairing_reserved` →
`handshake_pending` → `controller_connected`. Claim creates only a reservation.
The pairing code is consumed and the lease is created in one atomic transition
only after the Relay validates both role commitments and both receipts from the
opposite authenticated sockets. Timeout, disconnect, bad transcript, wrong
role, replay or a partial handshake terminates the pairing instance and creates
no lease. The resulting `controller_joined` additionally carries and validates
the original reservation's `request_id + pairing_id + reservation_id +
transcript_hash + relay_epoch + remote_control_generation` and the receiver's
pre-established signed `controller_connection_identity` before installing the
new Relay-assigned `connection_id` and returned lease.

Each sender creates an independent 32-byte CSPRNG receipt secret. Its commitment
has exactly one encoding:

```text
SHA-256(
  "pinvou-finished-receipt-commit-v1" ||
  LP(relay_epoch) || LP(pairing_id) || LP(reservation_id) ||
  LP(transcript_hash) || LP(sender_role) || LP(receipt_secret)
)
```

`LP(x)` is a four-byte unsigned big-endian byte length followed by the bytes.
The first five LP values use UTF-8; `transcript_hash` is its 64-byte lowercase
hexadecimal ASCII spelling, `sender_role` is the single byte `A` or `B`, and
the final LP value is the raw 32-byte secret. The domain is unprefixed ASCII.
The sender submits only the commitment before sending Finished. The secret is
sent to its peer only in `encrypted_handshake_finished`; after verification,
the opposite receiver returns it to the Relay in `finished_receipt_ack` over
its role-authenticated socket. The Relay deletes receipt secrets after the
atomic result and cannot use them to derive any Finished or business key.

The fixed compatibility vector is:

- `relay_epoch`: `epoch-test-1`
- `pairing_id`: `pairing-0001`
- `reservation_id`: `reservation-0001`
- `transcript_hash`: 64 ASCII `0` characters
- `sender_role`: `A`
- secret hex: `000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f`
- preimage length: 194 bytes
- commitment: `ed19ab422bae98c4f52b2e18665df08b9109eb916c85812678cfde57448f3a53`

The exact preimage hex is frozen in the shared fixture and asserted by both JS
and Rust tests.

### DEV-05 cryptographic profile (feature gated in the live path)

The Rust controller crypto module uses X25519, HKDF-SHA-256,
AES-256-GCM, Ed25519 verification, an OS CSPRNG, and zeroizing secret owners.
Later tasks connected them to the Relay and Tauri command path behind the
controller feature gate. Production Relay JavaScript receives no A/B handshake private key,
HKDF output, Finished key, business key, resume secret, or business plaintext.
Its helpers recompute and constant-time compare the receipt commitment and
validate the frozen sealed-code structure/vector. DEV-03 provides the one-shot
Relay sealed-code encryptor, persistent pairing store and registration handler;
later Relay composition connects them when the feature gate is enabled.
The handler first runs the injected authenticated-endpoint gate. A successful
allocation atomically persists the stable registration result and a code-digest
tombstone before returning the code to the handler; persistence failure returns
no code. Stable registration retries echo the new transport request ID while
reusing the byte-identical sealed-code value and terminal fields. Collision
exhaustion is also a durable terminal result.

The pairing store uses exactly
`23456789ABCDEFGHJKMNPQRSTUVWXYZ`, rejection sampling from the OS CSPRNG and at
most 32 collision retries. It compares a candidate under every retained digest
key. A code is quarantined for ten minutes after issue and at least two minutes
after a terminal transition. Pairing tombstones are retained through
`reuse_after_ms` and while a retained registration or live reservation references
them; compaction then removes unreferenced rows, not merely their code digests.
An old digest key may be removed only after its last reference expires. Library
open can reconstruct still-valid pairing registrations; live Relay startup calls
`invalidateForRelayRestart()` before serving requests, so pre-restart codes and
memory-only claim/reservation/lease ownership do not resume across a restart.

Stable claim retries on the same owner/code/public key/capabilities share one
in-flight preparation, with a separate transport `request_id` on each response.
Changed retry parameters and late/replayed handshake frames are rejected without
canceling an incumbent handshake or established lease. Disconnect, timeout and
failure to deliver an accepted handshake operation invalidate only its owned
reservation; actual socket closure retains the explicit lease/resume-hook rules.
Each asynchronous preparation/commit boundary rechecks ownership and deadlines;
no callback may publish a reservation released during its await.

`code_wrap_keypair` and `handshake_keypair` are distinct Rust types and are
generated independently; their public-key wrapper types are also distinct, and
`desktop_pairing_register` rejects byte-identical code-wrap and handshake
public keys as `invalid_field: message.key_purpose_reuse`. Both X25519 paths
reject a non-contributory result with `SharedSecret::was_contributory()` before
exporting the shared bytes; the all-zero and known low-order `u=1` peer points
are frozen negative tests.

`sealed_code_v1` has exactly 68 binary bytes:

```text
version=0x01 | relay_ephemeral_x25519_public_key[32] | nonce[12] |
pairing_code_ciphertext[7] | aes_gcm_tag[16]
```

Its JSON field is canonical base64 of those complete bytes. The Relay sealer is
one-shot: it consumes its ephemeral private-key owner, obtains the 12-byte nonce
internally from the OS CSPRNG, and exposes no production API for caller-supplied
nonce or AAD. The receiver requires exactly 68 decoded bytes and version 1
before deriving or decrypting. Code-wrap key derivation is HKDF-SHA-256 over the
X25519 shared secret with ASCII salt `pinvou-code-wrap-salt-v1` and info
`pinvou-code-wrap-key-v1 || LP(registration_request_id) || LP(pairing_id) ||
LP(remote_control_generation) || LP(relay_epoch) || LP(expires_at_ms)`.
The AES-GCM AAD is ASCII `pinvou-sealed-code-aad-v1` followed by the same five
LP fields in the same order. All timestamps use their canonical decimal-string
wire spelling. Binding, version, public key, nonce, ciphertext or tag mutation
fails closed; reusing the same key/nonce with different ciphertext fails the
independent Node authentication test. The shared fixture freezes both binary
hex and JSON base64 encodings.

Handshake derivation uses the raw 32-byte
`transcript_hash` as salt and info
`pinvou-controller-handshake-kdf-v1 || LP(relay_epoch) || LP(pairing_id) ||
LP(reservation_id) || LP(transcript_hash) || LP(purpose)`. Purpose is exactly
one of `finished/A`, `finished/B`, `business/A-to-B`, `business/B-to-A`, or
`resume`. The independent `resume` output is a zeroizing `ResumeSecret`; it is
not interchangeable with a Finished or business key and is included in the
cross-language fixed vector.
Consequently a key or ciphertext from one role, direction, lease, or purpose
cannot be substituted for another.

The handshake key pair is consumed when producing `SessionKeys`, and
`SessionKeys` is neither cloneable nor reusable: `into_role_ciphers(local_role,
binding)` consumes it exactly once and yields one role-bound `SendCipher`, one
opposite-direction `ReceiveCipher`, one Finished sender/receiver pair, and one
resume secret. There is no `&self` cipher factory. Rebuilding ciphers requires a
new handshake key pair and a new lease epoch. A sender sequence must strictly
increase; after successfully using `u64::MAX`, that sender remains permanently
`sequence_exhausted` and cannot rewind or emit another frame.

A Finished plaintext is
`pinvou-controller-finished-v1 || LP(raw transcript_hash) || LP(sender_role) ||
LP(raw 32-byte receipt_secret)`. Its AES-GCM AAD is
`pinvou-controller-finished-aad-v1 || LP(raw transcript_hash) ||
LP(sender_role)` and its wire bytes are `12-byte nonce || ciphertext ||
16-byte tag`; the production sealer obtains the nonce directly from the OS
CSPRNG rather than accepting a caller-supplied nonce. The receiver rejects a wrong role, transcript, domain, length,
tag, or trailing byte before returning the receipt secret. Receipt secrets are
zeroizing values and the bounded receipt replay guard accepts each commitment
at most once.

For business traffic, the direction-specific handshake root is expanded again
with salt `transcript_hash` and info
`pinvou-controller-business-key-v1 || LP(connection_id) ||
LP(controller_lease_id) || LP(lease_epoch) || LP(relay_epoch) ||
LP(remote_control_generation) || LP(direction)`. The 12-byte nonce is the first
four bytes of SHA-256 over
`pinvou-controller-business-nonce-v1 || direction || u64be(lease_epoch)`,
followed by `u64be(frame_sequence)`. The AAD is
`pinvou-controller-business-aad-v1` followed by LP encoding of the same five
lease fields, direction, decimal sequence, and `payload_kind`. The receive
primitive rejects a zero, repeated, or decreasing outbound sequence before
encryption, preventing nonce reuse under one derived lease key. The receive
window accepts authenticated out-of-order frames only within 64 sequence
numbers, rejects zero, duplicates and older frames, and commits a sequence only
after successful AEAD authentication. A lease epoch mismatch is rejected before
decryption. Ciphertext length is bounded against the maximum plaintext plus tag
before allocation/decryption.

All LP lengths are unsigned four-byte big-endian values. Identifiers are
non-empty and bounded before derivation. Private X25519 scalars, HKDF results,
AEAD keys, resume secrets and receipt secrets use zeroizing owners; temporary Finished
plaintext/preimage buffers are explicitly zeroized after use.
`SensitiveString::new` returns an error for empty values or UTF-8 content over
8192 bytes, has no `Clone` or Serde output, prints `[REDACTED]` for
`Display`/`Debug`, and owns a zeroizing string whose drop path is covered by a
cleanup hook test. The shared fixture freezes X25519 public/shared values, derived
Finished, business and resume keys, nonce, AAD and ciphertext; Rust reconstructs every
stage and Node validates the complete contract hash without implementing A/B
session cryptography.

### Liveness and resume invariants

`last_liveness_a` and `last_liveness_b` have separate epochs and monotonic
sequences; one role's transport traffic never advances the other. For an active
remote task, B advances its at-most-30-second controller watchdog only from a
valid `encrypted_controller_liveness_a` or the conservative signed attestation
fallback. Lower heartbeat sequences are rejected; equal heartbeat sequences
can only tighten the existing deadline; a greater heartbeat may replace it.
Relay attestation tracks a separate sequence that must strictly increase;
equal or lower attestations are replay. Relay transport traffic alone cannot
extend the deadline.

Resume passes through `resume_provisional` and
`resume_activated_pending_liveness`; neither state accepts, buffers or forwards
business frames. A and B have independent single-use resume tokens. Final
activation occurs only before B's original local deadline, after B's activation
ACK, the first post-activation B liveness, Relay confirmation and both role
ready ACKs. Only then are tokens atomically consumed and rotated and business
frames reopened. Any timeout, missing ACK, Relay restart or ambiguous result
terminates the candidate and prevents reuse of both old and proposed material.

On transport loss, clients revoke runtime/TX authority and retain the old full
lease tuple only as a recovery binding. Only the explicit resume-control family
may use that binding; ordinary liveness and business frames remain closed.
The strictly validated final `controller_resumed` grants one local installation
proof bound to the receiving socket epoch, role, request ID, activation ID and
old tuple. A further disconnect, stop or ordinary lease installation invalidates
that proof. The domain resume state machine must independently validate the
E2E activation before consuming it; the transport proof is not E2E trust.
Only `encrypted_controller_liveness_a` uses the frozen three-field routing
exception, and only with an active authenticated lease.

Relay retains one completed-attempt tombstone per live lease: at most eight
canonical accepted-message digests bound to type, role, request and socket.
No old token or ciphertext is retained in the tombstone. Exact retries on the
current role's socket are empty no-ops, not a second activation or a timer
extension; changed or cross-socket retired traffic is rejected and has no
cleanup authority over the new revision. New resume attempts replace the
tombstone only after successful commit.

Final deliveries precede the Relay's atomic token/epoch rotation so a failed
delivery can still abort the old revision. This is not an atomic distributed
commit: one peer may have seen a final before the other delivery fails. Any such
uncertainty terminates the lease rather than opening business on the Relay.
Accepted-result cleanup is fenced to the exact immutable lease revision; once
router post-processing finishes, that result's abort authority is retired.
Authoritative termination releases the interaction slot, clears all lease peer
links and notifies stateful hooks even if replacement notification fails. Hook
cleanup is owner-fenced, including a bounded immediate predecessor during a
partial post-rotation hook installation.

### Errors, legacy Web conflict and logging

Controller error envelopes use only these codes: `pairing_code_invalid`,
`pairing_code_generation_failed`, `controller_busy`, `version_incompatible`, `endpoint_offline`,
`enrollment_required`, `invalid_message`, `unsupported_capability`,
`permission_already_decided`, `permission_expired`, `result_unknown` and
`capability_snapshot_unavailable`.
Malformed schema errors are local parser failures and are mapped to the bounded
`invalid_message` envelope before the later dispatcher closes the socket.
Non-local pairing failures (missing, expired, consumed, revoked, wrong
generation or a losing concurrent claim) all produce the same user-facing
`pairing_code_invalid` message: “连接码无效或已失效”.

When a legacy WebUI v2 `web_client_join` conflicts with a desktop reservation
or controller lease, compatibility requires the exact existing response and no
desktop details:

```json
{"v":2,"type":"error","code":"endpoint_not_found","message":"endpoint not found"}
```

The Relay closes it with code 1008. With no desktop conflict, all WebUI v2
messages and Web-to-Web takeover semantics remain byte-for-byte compatible.
The shared fixture is copied from the live v2 dispatcher and uses only its real
types: request/response pairs `desktop_endpoint_register` /
`desktop_endpoint_registered` and `web_client_join` / `web_client_joined`,
including endpoint, token/secret, stream cursor, connection flags, nullable or
actual `lease_id`, and the complete error envelope. It freezes `accept_v2` for
no controller conflict and the generic `endpoint_not_found` result for
every slot-occupying state: `pairing_reserved`, `handshake_pending`,
`controller_connected`, `reconnecting`, `resume_provisional`, and
`resume_activated_pending_liveness`. Every state freezes the complete error
envelope and close code 1008; it does not invent controller-only v2 messages.

Protocol logs and telemetry must be allowlist projections, never serialization
of the source message. They may contain only protocol version, message type,
bounded request ID, declared sender role and generic error code. They must not
contain pairing codes, sealed codes, endpoint/connection/pairing/reservation/
lease/generation identifiers, ciphertext, receipt secrets, resume tokens,
public or private key material, enrollment proofs, credential references,
tasks, prompts, tool arguments/results, filenames, attachment/artifact bytes,
snapshots or journal contents. Validation errors identify only the bounded field
name, not its supplied value.
