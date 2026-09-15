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
| `PINVOU_REMOTE_ENROLLMENT_HMAC_KEY_ID` | Stable identifier for the active credential-digest key. |
| `PINVOU_REMOTE_ENROLLMENT_HMAC_KEY` | Secret HMAC key, 32–4096 UTF-8 bytes. Supply it through the deployment secret manager, never source control or logs. |
| `PINVOU_REMOTE_ENROLLMENT_DEVELOPMENT_TOFU` | Optional `1`/`true`; accepted only with `deployment=development` and `NODE_ENV=development` or `test`. |

`official` and `self_hosted` always reject an unknown endpoint. A production
runtime refuses both development deployment mode and the development TOFU
switch. Self-hosted production therefore uses an explicit administrator import
or enrollment operation; the migration CLI and gateway belong to DEV-02.

## Persistence and concurrency contract

`relay-state-store.js` owns an independent, versioned controller state file and
does not rewrite the strict Web v2 revoke file. Each namespace has a canonical
unsigned-decimal revision. A caller reads a namespace snapshot and commits with
`compareAndSwap(name, expectedRevision, value)`; stale revisions fail without a
write. One process serializes mutations, and an exclusive sibling `.lock` file
prevents concurrent writers in separate processes. A lock owned by a live PID is
never stolen. A dead-PID lock is quarantined before removal.

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
oversized, unsupported, or ambiguous
candidates otherwise fail closed. Operators must back up the primary and `.bak`
together while the Relay is stopped or through a later coordinated snapshot
facility.

The JSON parser rejects unknown schema fields, unsafe object keys, non-integer
numbers, excessive nesting, oversized strings/files, unsafe paths, symlinks and
unsupported schema versions. A narrowly defined version-0 controller-store
shape is migrated atomically to version 1. This migration is unrelated to the
legacy Web v2 Relay state and never reads it.

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
