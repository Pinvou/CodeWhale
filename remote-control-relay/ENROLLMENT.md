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
hardlink, so this implementation never publishes an empty or partial lock. The
record binds PID, a boot-plus-process-start identity, and a 144-bit per-open
session nonce. A live matching process is never preempted, and PID reuse is
detected. Legacy empty/truncated locks and dead owners are re-read for a bounded
interval, then quarantined and identity-checked before removal. Any
create/write/sync/publish failure closes the handle and removes files owned by
the failed attempt.

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

The `endpoint_enrollment` namespace stores sorted endpoint records with status
(`active` or terminal `revoked`), endpoint revision, timestamps, a keyed
HMAC-SHA-256 digest, and only the audit source/last action. Raw credentials and
the HMAC key are never persisted or returned. Credential checks use a constant-
time digest comparison.

Callers use:

- `EnrollmentRegistry.enroll` for an already authenticated official/admin flow;
- `authorizeRegistration` with purpose `web_v2_reregister` or
  `desktop_pairing_register` before accepting a registration;
- `rotateCredential` with the current credential and expected endpoint revision;
- `revoke` with the expected endpoint revision.

State CAS conflicts and endpoint revision conflicts are explicit. Registry,
storage, digest-key, corruption, and persistence failures reject authorization;
no in-memory success is returned after a failed durable commit. DEV-02 supplies
authenticated migration/admin tooling, and DEV-06 supplies protocol routing,
rate limits, origin checks, and production startup wiring.
