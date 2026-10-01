# Desktop endpoint enrollment registry

DEV-01 adds the persistent authorization authority consumed later by the Web v2
re-registration and `desktop_pairing_register` handlers. The gated self-hosted
and development dispatcher consumes it in `server.js`. Official live claim
wiring is not installed in this wave: enabling the controller with deployment
`official` fails startup before a listener opens. A tested proof handler alone
is not a production enrollment flow and must not be advertised as one.

## Deployment configuration

Set these variables before opening the registry:

| Variable | Meaning |
|---|---|
| `PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT` | Required: `official`, `self_hosted`, or `development`. |
| `PINVOU_REMOTE_CONTROLLER_STATE_PATH` | Absolute path for the new controller state file. It must not equal the existing `PINVOU_REMOTE_STATE_PATH`. The caller passes this value to `RelayStateStore.open`. |
| `PINVOU_REMOTE_STATE_PATH` | Required absolute path of the legacy Web v2 state. It is used only for alias detection; this module never reads or changes it. |
| `PINVOU_DESKTOP_CONTROLLER_ENABLED` | Set to `1` only when enabling the controller-v1 dispatcher. Any other value preserves legacy Web v2-only behavior. |
| `PINVOU_REMOTE_PAIRING_HMAC_KEY_ID` / `PINVOU_REMOTE_PAIRING_HMAC_KEY` | Stable independent HMAC key used for pairing-code digests and quarantine lookup. Do not reuse the enrollment key. |
| `PINVOU_REMOTE_RELAY_SIGNING_KEY_ID` / `PINVOU_REMOTE_RELAY_SIGNING_PRIVATE_KEY_B64` | Pinned key ID and canonical base64 Ed25519 PKCS#8 DER private key used to sign pairing transcripts. The private value is deployment-only and must never enter logs or repository files. |
| `PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID` | Stable identifier for the active credential-digest key. |
| `PINVOU_REMOTE_ENROLLMENT_HMAC_KEY` | Secret HMAC key, 32–4096 UTF-8 bytes. Supply it through the deployment secret manager, never source control or logs. |
| `PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU` | Optional `1`/`true`; accepted only with `deployment=development` and `NODE_ENV=development` or `test`. |

`official` and `self_hosted` always reject an unknown endpoint. A production
runtime refuses both development deployment mode and the development TOFU
switch. Self-hosted production therefore uses an explicit administrator import
or enrollment operation; the migration CLI and gateway belong to DEV-02.
The config loader rejects lexical aliases, including Windows case-only aliases.
`RelayStateStore.open` receives both paths and also compares resolved paths and
file identities, rejecting junction aliases and hardlinks. The legacy path may
never be anywhere inside the dedicated controller directory or alias the
primary, backup, lock, marker, temporary, stale-lock, or other reserved
management path. Controller state filenames using those reserved suffixes are
also rejected.

## Persistence and concurrency contract

`relay-state-store.js` owns an independent, versioned controller state file and
does not rewrite the strict Web v2 revoke file. The state file must be the child
of a dedicated directory whose parent already exists. The store creates and
protects only that final directory. If it already exists, the store verifies its
owner and protection and never chmods or rewrites a caller-owned parent. A
marker identifies the directory, and unexpected entries fail closed. Each
namespace has a canonical unsigned-decimal revision. A caller reads a namespace
snapshot and commits with
`compareAndSwap(name, expectedRevision, value)`; stale revisions fail without a
write. One process serializes mutations. A complete, synced temporary lock is
protected and published as the exclusive sibling `.lock` with an atomic
hardlink, so this implementation never publishes an empty or partial lock. As
soon as the published link's inode is verified, the store records a provisional
owner and process hold before removing the temporary hardlink or syncing the
directory. The record binds PID, a boot-plus-process-start identity, and a
144-bit per-open session nonce. A live matching process is never preempted, and
PID reuse is detected. Legacy empty/truncated locks and dead owners are re-read
for a bounded interval, then quarantined and identity-checked before removal.
Failures before verified publication close the handle and remove only matching
owned inodes; cleanup failures remain attached to the original primary error.
Failures after verified publication use the same owner-checked release and
abandoned-owner recovery path as initialization failures.

Calling `close()` synchronously closes read/write admission. A CAS already
performing its durable writes finishes under the owner lock; queued and later
CAS reject with `relay_state_unavailable` during closing and
`relay_state_closed` after successful close. The captured queue drains before
lock release; later queued rejections do not perform persistence. No durable
replacement can overlap owner-lock removal. Concurrent close calls
share the same release attempt.

Lock unlink and directory sync retry bounded Windows sharing failures. `close()`
does not report success until release completes. After an exhausted failure the
object remains in a non-readable `closing` state and retains a process-local
hold; another store in that process cannot take over. The caller must retain the
object, resolve the I/O condition, and retry `close()`. Only that successful
retry releases the process hold and permits reopening. Dropping an object after
a failed close intentionally leaves the store unavailable until process restart.
If initialization itself fails and lock release also exhausts its retries, the
original initialization error remains primary and carries the release error.
The module retains that exact unreturned owner and nonce; a later same-process
open may first retry the owner-verified release after I/O recovers, but can never
clean up a replaced lock or a lock belonging to another owner.

The empty store is durably initialized before it is exposed. Each successful
commit is written to a same-revision primary and `.bak` copy via
a same-directory temporary file, file sync, atomic rename/replace, restrictive
permissions, and directory sync. Rename is retried for the transient sharing
violations used by Windows antivirus and indexers. If either durable write fails,
the store enters a permanent degraded state and rejects all reads and writes;
the process must be restarted after operators resolve the disk problem. On
startup the valid primary and backup must be byte-equivalent after canonical
parsing; a mismatch, a missing peer copy, or a damaged backup fails closed. A
valid backup may restore a malformed primary; a missing primary fails closed so
a staged but uncommitted backup cannot become authoritative. Malformed,
oversized, unsupported, or ambiguous candidates otherwise fail closed.
Operators must back up the primary and `.bak`
together while the Relay is stopped or through a later coordinated snapshot
facility.

If startup reports `relay_state_unavailable`, stop the Relay before touching
either copy and preserve the complete dedicated controller-state directory for
audit. Do not start by deleting `.bak` or by choosing the file with the larger
revision: a newer backup can be only the staged half of a commit that was never
reported successful. Restore the primary and `.bak` together from the same
known-good coordinated snapshot, reapply their documented private permissions,
and validate enrollment before reopening the public route. When no such
snapshot exists, keep the service failed closed and rebuild authorization with
the administrator enrollment/migration flow; never adopt one mismatched copy
automatically.

Pairing registration compacts expired idempotency records and quarantine rows
before allocating new state, including during long-running processes. A row is
retained while its quarantine is live, an idempotency record references it, or
a reservation still needs durable invalidation/notification retries. Reservation
promotion or successful invalidation releases that final retention reference.
New opaque pairing IDs include the monotonically committed namespace revision
and fresh random entropy, so GC cannot reissue an ID even if entropy repeats
within the same store history. Their longer form is disjoint from legacy IDs.

Every atomic state write snapshots the legacy file and every existing managed
file identity before replacement. Its transaction ledger also records each
temporary and retired target inode, then compares the final legacy identity
against the complete pre-write, in-transaction, and post-write set. A hardlink
introduced while backup, primary, or backup-restoration replacement is in
flight therefore cannot turn a successful controller write into a legacy Web
state overwrite: the legacy path is never removed or written, the operation
fails, and a live store becomes unavailable.

### Offline recovery after an interrupted commit

There is no supported automatic repair CLI. A failed open must not be bypassed
by deleting a candidate, editing JSON, resetting the registry, or enabling TOFU.
Stop the Relay and confirm no process still owns or writes the store before any
recovery. First archive the entire dedicated directory unchanged, including the
primary, `.bak`, lock, marker, temporary/stale files and their owner/permission
metadata. Keep the archive access-restricted; never publish its contents.
Orphaned crash `.tmp` and `.stale` artifacts are not automatically garbage
collected. Monitor disk usage and preserve recovery evidence; do not remove
live-owned artifacts as a shortcut to opening the store.

Commit order is `.bak` replacement first, then primary replacement. If the
process dies between them, the new backup may contain an **uncommitted**
authorization change. It must not automatically become authoritative. A valid
v0 primary and a v0 or v1 backup that normalize to the same complete state can
resume migration automatically; missing, damaged, unsupported or inconsistent
migration peers fail closed before either candidate is rewritten. A single v0
copy is not sufficient for automatic migration.

For non-equivalent copies, a damaged peer or uncertain provenance, restore a
verified pair from the same coordinated offline snapshot. Do not combine
different snapshots or choose a copy solely because its revision is higher.
Alternatively, an administrator may undertake an **offline, audited manual
recovery** only after external commit/incident evidence establishes that the
valid primary is the last committed state and the newer backup was merely
staged. After archiving and validating that decision, reconstruct the backup
from that confirmed primary through a same-directory temporary file, file sync,
atomic replacement and directory sync; preserve and re-verify restrictive
owner/permissions. Leave the primary unchanged. Do not promote the staged
backup or apply this recipe to an unexplained mismatch. Without sufficient
evidence, keep the service stopped and escalate rather than guessing.

Before restarting, verify that both candidates are valid, canonically equivalent
and have the correct owner/protection. Retain the archive and recovery decision
for audit, then verify expected enrollment/revocation records after startup.
Failed commits must not be reported as successful during recovery. Do not
remove a live owner's lock; normal stale-owner handling remains the store's
responsibility. This procedure never touches the legacy Web v2 state file.

On Windows the module disables DACL inheritance on the dedicated directory and
every marker, primary, backup, temporary and lock file. It grants Full Control
only to the Relay process identity, `SYSTEM`, and built-in Administrators, then
reads the DACL back and verifies owner, principals, rights, inheritance flags
and protection. Paths are passed to a constant PowerShell program as environment
data, never interpolated into a command. Any set/read/verification failure keeps
the store closed. On POSIX, a module-created directory is `0700`, files are
`0600`, and existing owner/mode are verified without chmodding parent folders.
The ACL setter itself performs the authoritative readback. Atomic rename/hardlink
then preserves that verified file identity, avoiding duplicate PowerShell
processes for each installed target; a normal CAS invokes the setter once for
each of its two new state files.

The JSON parser rejects unknown schema fields, unsafe object keys, non-integer
numbers, excessive nesting, oversized strings/files, unsafe paths, symlinks,
junctions/reparse points and unsupported schema versions. Every existing path
component is checked and snapshotted by canonical path and file identity.
Critical operations recheck it. Files use `O_NOFOLLOW` where available, are
validated again through `fstat`, read with a hard byte bound, and rechecked after
the read to detect final-component exchange and file-growth races. A narrowly
defined version-0 controller-store shape is migrated atomically to version 1.
This migration is unrelated to the legacy Web v2 Relay state and never reads
it.

## Enrollment data and API

The `endpoint_enrollment` namespace schema is version 3. It stores sorted
endpoint records with status (`active` or terminal `revoked`), endpoint
revision, ordered timestamps, role-domain-separated HMAC-SHA-256 digests in
`access_credential` and `desktop_credential`, and only the audit source/last
action. The desktop digest may be null for an explicitly Web-only enrollment.
Identical plaintext in both roles still produces different digests. The access
digest never authorizes desktop pairing. A sorted migration request ledger
seals stable operations. Earlier development-only namespace versions and old
digest algorithms are rejected; no lazy upgrades or fallback validators exist.
This format is unreleased. Operators testing an earlier format must preserve
their evidence and explicitly rebuild enrollment, not reinterpret old digests.

Raw credentials, migration parameters, and the HMAC key are never persisted or
returned. Credential and request-parameter checks use constant-time digest
comparison. A ledger row contains only the stable request ID, operation class,
sorted endpoint IDs, keyed parameter digest, timestamps, state, and a bounded
secret-free outcome.

Callers use:

- `EnrollmentRegistry.enroll` for an already authenticated official/admin flow;
- `authorizeWebRegistration` with `accessToken` for Web joins;
- `authorizeDesktopRegistration` verifies `accessToken` and `desktopSecret`
  together against one registry snapshot before a desktop becomes trusted;
- `authorizePairingRegistration` with `desktopSecret`, only from that trusted
  desktop socket context;
- `rotateAccessCredential` or `rotateDesktopCredential` with the current
  role credential and expected endpoint revision; neither changes the other role;
- `revoke` with the expected endpoint revision.
- `exportSnapshot`, public `validateEnrollmentRegistrySnapshot`, and
  `importSnapshot` for strict backup validation and one-CAS restoration.
  Restoring endpoint records always preserves the current migration ledger;
  snapshot data can never erase or roll back stable request IDs;
- `applyAtomicBatch` for a fully prevalidated allowlist/re-enroll batch or an
  exclusive restore operation. `allowlist` creates a new record only; an optional
  `desktop_credential` requires `expected_revision: "0"`. For an existing active
  Web-only record, `set_desktop_credential` requires the exact endpoint revision,
  increments it, and refuses to overwrite a populated desktop slot. It makes at most one namespace CAS, so no prefix
  of a failed batch becomes durable;
- `beginMigrationRequest` and `completeMigrationRequest` for explicit crash
  boundaries, or `executeMigrationRequest` for the normal composed path.

Migration begin durably creates `pending`. Completion recomputes and verifies
the keyed parameter digest, builds the entire new registry in memory, then
persists endpoint changes and the terminal sealed outcome in the same namespace
CAS. A crash before that CAS leaves only `pending`; a crash after it leaves both
the full endpoint result and terminal outcome. A retry after restart resumes a
matching pending row or returns the original terminal result. The same request
ID with another operation, endpoint set, credential, revision, or snapshot is a
conflict. Concurrent handlers converge through namespace CAS; a storage error
never reports an unpersisted success.

State CAS conflicts and endpoint revision conflicts are explicit.

Authorization caches only a frozen validated registry and its private endpoint
index, not successful credentials. Every request calls the store's lightweight
`readNamespaceRevision`, which checks ready/closing/degraded state. A namespace
revision change reloads the registry; a closed or failed store rejects even a
warm-cache request. Anonymous warm-cache authorization does not clone or scan
the full registry. The lightweight metadata API is part of the required store
contract; a store lacking it is rejected at registry open.

`getControllerReadiness` reports both `web_ready` and dual-role `desktop_ready`.
Rollback inventory coverage counts `migrated` only when both roles are ready
with the configured digest key; `web_migrated` separately counts access readiness.
Active Web-only records remain valid for Web joins but
cannot register a desktop or pair. Development TOFU may create an access-only
record for a Web request, or both roles atomically for a desktop request; a
pairing request can never invent a desktop digest from a single credential.

Digest-key rotation is not endpoint credential rotation. Replacing the deployment
HMAC key (including replacing bytes under the same key ID) invalidates existing
digests; the registry cannot recover them without the previous key. Existing
endpoint IDs cannot be re-enrolled: active conflicting entries are rejected and
revoked entries remain terminal. Do not change the HMAC key as routine maintenance.
An intentional HMAC-key replacement requires re-enrollment of all affected
clients with fresh endpoint IDs and new credentials through the authenticated
admin flow, followed by new pairing and configuration on both peers. Revoke the
old entries through the admin flow; never delete or hand-edit shared state or
reset other namespaces/Legacy Web data. A new key ID must identify new key
material. Before revocation or new enrollments, restoring the previous key and
key ID restores authorization; it does not undo later registry transitions.

`enrollment_registry_capacity_reached` identifies entry-count or shared byte-budget
rejection, not an infrastructure outage. Capacity validation runs before durable
writes: primary/backup bytes and revisions stay unchanged, and the store remains
usable for smaller writes. Actual persistence failures still degrade the store
and fail closed. Other typed registry validation errors retain their codes.
Batch operation entry-count rejection retains its explicit
`enrollment_capacity_reached` code; it is not an infrastructure outage either.

Audit sources describe admission: explicit development enrollment records
`development`, even with TOFU enabled; only automatic first-registration admission
records `development_tofu`. Rotation and revocation preserve that source.
Endpoint IDs are identifiers, not secrets: unknown, revoked and invalid-credential
errors can reveal enrollment status. Official/self-hosted registration and
development registration with TOFU disabled require an existing active enrollment
and a valid credential. Only explicitly enabled non-production development TOFU
may admit a first registration; callers must apply the protocol layer's rate
limits before exposure.

Recognized lock schemas with an unsupported positive version fail closed with
`relay_state_lock_unsupported`, even with added fields or an apparently dead PID.
They are never quarantined or stolen; use a compatible Relay version or an
operator-controlled offline upgrade. Locks exceeding the 4096-byte read bound,
invalid UTF-8 or a file/identity safety check fail with
`relay_state_lock_unreadable`; other read errors also propagate without stealing
the lock. These files are never quarantined or removed by recovery. Fully read
empty/truncated UTF-8 crash-lock recovery is unchanged.

For `revoke`, the expected endpoint revision is syntax-checked for every call
and compared before an active-to-revoked transition. An already revoked entry
is a read-only idempotent result even if a retry supplies an old or future
well-formed revision: no endpoint/namespace revision or persisted bytes change.

The registry's 100,000-entry ceiling and the store's default 16 MiB serialized
size ceiling are independent safety limits, not a promise to store 100,000
entries. Other namespaces and record sizes consume the same byte budget; the
first applicable limit bounds effective capacity. Set production capacity
targets from deployment-specific load tests, not these hard ceilings.

Registry, storage, digest-key, corruption, and persistence failures reject authorization;
no in-memory success is returned after a failed durable commit. DEV-02 supplies
the migration and rollback perimeter around this authority; DEV-06 still owns
the new desktop protocol routing and production startup wiring.

## DEV-02 migration and legacy rollback

Official migration is accepted only through `EnrollmentMigrationHandler` after
DEV-06 binds an already authenticated legacy desktop socket and verifies an
official device identity. The Relay first issues a random 256-bit, socket-bound
challenge. The client returns an Ed25519-signed, canonical proof containing the
pinned signing-key ID, device/endpoint ID, hashes of the legacy access token and
desktop secret, challenge, issue time and expiry. A challenge is consumed by
the first verification attempt, proofs live for at most five minutes, and a
proof from another socket, endpoint, credential, device key or time window is
rejected. Deployment code constructs the verifier from official public keys;
an authenticated legacy TOFU socket alone is never sufficient to enroll.

The claim request contains only a stable request ID: endpoint ID and the
existing `access_token` and `desktop_secret` come from the opaque socket proof.
The `claim_roles` operation atomically creates both digests, or fills a null
desktop slot only while the access digest still matches. An identical desktop
digest is idempotent; a different digest or revoked record is rejected. Claim outcomes use
the durable migration-request ledger, so a retry after response loss or process
restart returns the original terminal result. Repeating a claim preserves
endpoint ID, token, timestamps and old Web links; a request ID used by another
endpoint or parameter set is rejected. No credential is returned or logged.

Self-hosted operators use `scripts/enrollment-migrate.js`. Credentials must be
provided in a private input file and are never accepted as command-line values.
`--dry-run` validates the whole batch without mutation. Desktop credential
backfill is explicitly reported as `would_set_desktop_credential`, not as an
already enrolled no-op. Each version-1 batch
document contains a stable `request_id`; `--apply` records it in the durable
migration ledger and commits all endpoint changes plus the terminal result with
one namespace CAS. A retry returns that sealed result and a different batch
under the same ID conflicts. A failed request may persist a secret-free failed
ledger row, but no endpoint-operation prefix can persist. Backup restore accepts
only the current strict registry format and executes as a stable migration
request while preserving the monotonic request ledger.
The ledger has a separate 100,000-request safety ceiling and shares the store's
16 MiB default byte budget with other namespaces; byte capacity can be reached
first. There is no automatic ledger pruning. Never delete terminal IDs merely
to free space: that would permit old operations to execute again. Capacity,
safe archival and warning policy are tracked in #1371.
`--restore --input PRIVATE_BACKUP --request-id RESTORE_OPERATION_ID` requires an
explicit operation ID. A first applied restore returns `applied: true`,
`restored: true`, `replayed: false`; response-loss retries with the same ID return
`applied: false`, `restored: false`, `replayed: true` without writing anything.
If records subsequently change, the old ID still only replays its sealed outcome
and does not restore those changed records. Use a new explicit operation ID to
intentionally apply the same backup again. Reusing an ID with another backup is
a conflict. Do not generate a fresh ID automatically on each retry.
Restore intentionally replaces endpoint records, not just missing entries.
It currently has no backup/current-revision fence: a stale backup can remove
enrollments created since that backup. Review that destructive scope explicitly
before a maintenance restore; the durable request ledger alone does not protect
newer endpoint records. A reused-ID conflict is currently exposed as
`migration_restore_failed` with its original conflict retained as the cause.
`re_enroll` rotates the access token and therefore explicitly invalidates old
Web links. Backup files contain keyed digests, not credentials, and still need
mode 0600/private ACL storage. The CLI rejects symlink/reparse-point ancestors,
requires input and pre-existing output files to have private ownership/ACLs,
creates backup output as mode 0600 on POSIX, and applies a protected Windows
DACL before writing any bytes.

An old v2 Relay may be used for rollback only on a literal IPv4 or IPv6 loopback
listener behind `enrollment-gateway.js`; `localhost` is not accepted as proof of
loopback binding. The gateway validates the first
`desktop_endpoint_register` (both credential roles) or `web_client_join` (access
role only) frame against the registry
before forwarding it. The first authentication frame is independently capped
at 16 KiB, while later valid business frames retain the legacy Relay's 4 MiB
default payload range. The gateway also enforces browser Origin policy,
connection/ingress limits, a bounded source bucket table and a strict trusted
proxy allowlist; untrusted forwarding headers are rejected. Use
`nginx-enrollment-gateway.conf` for the public route and run
`scripts/enrollment-gateway-preflight.js` with every public entrypoint. The
preflight fails if a route lacks the gateway marker, is unavailable, the
anonymous active-denial probe is not rejected, the Relay URL is not literal
loopback, or OS listener inspection finds a wildcard/non-loopback bind. A bare
v2 rollback is never a safe fallback.
`scripts/check-enrollment-gated-v2-rollback.mjs` combines the private endpoint
inventory coverage report and gateway check; deployment automation must require
its zero exit status before starting an old v2 binary. Coverage must have a
positive integer total, all entries migrated, no unmigrated entries and exactly
100 percent coverage; an empty inventory is not evidence of safe rollback.
The gateway holds the controller-state writer lock. During rollback, stop the
controller dispatcher before opening that gateway; two independent processes
must not open the same store as concurrent writers.
The preflight checks the health marker, WebSocket upgrade marker and an
anonymous registration denial on every declared public route. The declared
bind host remains a required assertion, but does not replace OS listener
inspection. `scripts/enrollment-gateway-server.js` is the standalone gateway
entry point; it also refuses a non-loopback listener. The example deployment
forwards a single nginx-derived client address instead of accepting a caller's
incoming forwarding chain.
