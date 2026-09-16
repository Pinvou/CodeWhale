# Desktop endpoint enrollment registry

DEV-01 adds the persistent authorization authority consumed later by the Web v2
re-registration and `desktop_pairing_register` handlers. It is intentionally not
wired into `server.js` yet; DEV-06 owns the single dispatcher integration point.

## Deployment configuration

Set these variables before opening the registry:

| Variable | Meaning |
|---|---|
| `PINVOU_REMOTE_ENROLLMENT_DEPLOYMENT` | Required: `official`, `self_hosted`, or `development`. |
| `PINVOU_REMOTE_CONTROLLER_STATE_PATH` | Absolute path for the new controller state file. It must not equal the existing `PINVOU_REMOTE_STATE_PATH`. The caller passes this value to `RelayStateStore.open`. |
| `PINVOU_REMOTE_STATE_PATH` | Required absolute path of the legacy Web v2 state. It is used only for alias detection; this module never reads or changes it. |
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

Every atomic state write snapshots the legacy file and every existing managed
file identity before replacement. Its transaction ledger also records each
temporary and retired target inode, then compares the final legacy identity
against the complete pre-write, in-transaction, and post-write set. A hardlink
introduced while backup, primary, or backup-restoration replacement is in
flight therefore cannot turn a successful controller write into a legacy Web
state overwrite: the legacy path is never removed or written, the operation
fails, and a live store becomes unavailable.

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

The `endpoint_enrollment` namespace schema is version 2. It stores sorted
endpoint records with status (`active` or terminal `revoked`), endpoint
revision, ordered timestamps, a keyed HMAC-SHA-256 digest, and only the audit
source/last action. It also stores a sorted migration request ledger. Version 1
snapshots are strictly validated and normalized to version 2 with an empty
ledger on read; the next successful mutation persists the upgrade. Unknown or
malformed versions fail closed.

Raw credentials, migration parameters, and the HMAC key are never persisted or
returned. Credential and request-parameter checks use constant-time digest
comparison. A ledger row contains only the stable request ID, operation class,
sorted endpoint IDs, keyed parameter digest, timestamps, state, and a bounded
secret-free outcome.

Callers use:

- `EnrollmentRegistry.enroll` for an already authenticated official/admin flow;
- `authorizeRegistration` with purpose `web_v2_reregister` or
  `desktop_pairing_register` before accepting a registration;
- `rotateCredential` with the current credential and expected endpoint revision;
- `revoke` with the expected endpoint revision.
- `exportSnapshot`, public `validateEnrollmentRegistrySnapshot`, and
  `importSnapshot` for strict backup validation and one-CAS restoration.
  Restoring endpoint records always preserves the current migration ledger;
  snapshot data can never erase or roll back stable request IDs;
- `applyAtomicBatch` for a fully prevalidated allowlist/re-enroll batch or an
  exclusive restore operation. It makes at most one namespace CAS, so no prefix
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

State CAS conflicts and endpoint revision conflicts are explicit. Registry,
storage, digest-key, corruption, and persistence failures reject authorization;
no in-memory success is returned after a failed durable commit. DEV-02 supplies
the migration and rollback perimeter around this authority; DEV-06 still owns
protocol routing, rate limits, origin checks, and production startup wiring.

## DEV-02 migration and legacy rollback

Official migration is accepted only through `EnrollmentMigrationHandler` after
DEV-06 binds an already authenticated legacy desktop socket. The claim request
contains only a stable request ID: endpoint ID and the existing `access_token`
come from the opaque socket proof. Repeating a claim preserves endpoint ID,
token, timestamps, and old Web links; a request ID used by another endpoint is
rejected. No credential is returned or logged.

Self-hosted operators use `scripts/enrollment-migrate.js`. Credentials must be
provided in a private input file and are never accepted as command-line values.
`--dry-run` validates the whole batch without mutation. `--apply` preflights the
whole batch in memory and commits all endpoint changes with one namespace CAS;
there is no durable partial prefix to roll back. Backup restore accepts strict
version 1 or version 2 registry snapshots, normalizes them through the same
validator, and executes as a stable migration request while preserving the
monotonic request ledger.
`re_enroll` rotates the access token and therefore explicitly invalidates old
Web links. Backup files contain keyed digests, not credentials, and still need
mode 0600/private ACL storage.

An old v2 Relay may be used for rollback only on a loopback listener behind
`enrollment-gateway.js`. The gateway validates the first
`desktop_endpoint_register` or `web_client_join` frame against the registry
before forwarding it. Use `nginx-enrollment-gateway.conf` for the public route
and run `scripts/enrollment-gateway-preflight.js` with every public entrypoint.
The preflight fails if a route lacks the gateway marker, is unavailable, or the
legacy Relay URL is not loopback. A bare v2 rollback is never a safe fallback.
`scripts/check-enrollment-gated-v2-rollback.mjs` combines the private endpoint
inventory coverage report and gateway check; deployment automation must require
its zero exit status before starting an old v2 binary.
The preflight checks both the health marker and the WebSocket upgrade marker on
every declared public route, and requires the old Relay's declared bind host to
be literal loopback. `scripts/enrollment-gateway-server.js` is the standalone
gateway entry point; it also refuses a non-loopback listener.
