# Identity recovery claims and profile session revocation

The Account registration and profile workers use durable `recovery_generation` and
`recovery_lease_until` from migration `900007_identity_recovery_claims`. Each leased
item carries those exact facts in its context. Local transitions, including
`WithoutCancel` failure cleanup, validate the current generation and expiry under
the same row lock/transaction as the write. A scheduling timestamp or business
version alone does not grant ownership. Existing USER → profile → sessions lock
order is retained, including audited profile writes: lock USER before profile,
then validate the claim after both locks, before the audit FK can request USER.
A batch item whose lease has expired cannot dispatch recovery.
Ordinary non-worker requests retain their existing interfaces.

Profile logout is a separate internal command:

- `POST /internal/v1/users/{subject}/profile-revocations`
- Exact workload permission: `internal.identity.sessions.revoke`.
- Required JSON: `{"operation_id":"<uuid>","first_attempt":false}`. The boolean is
  required, not inferred from its default. Account durably records the first grant
  before making this request; subsequent recovery always sends `false`.
- `204`: an ACK for that operation and exact subject; a confirmed duplicate does
  not call the provider again. `409 PROFILE_REVOCATION_CONFLICT`: subject conflict.
- `503 PROFILE_REVOCATION_UNCONFIRMED`: no confirmed receipt for a non-first command
  or a prior remote dispatch has an unknown outcome. No further logout is guessed.

Adapter receipts are process-local and serialized per operation. An adapter
restart does not turn an unknown operation into a new first attempt. Account sends
non-first after its durable grant, so a missing post-restart receipt fails closed.
The sender must not replay a stored first-attempt grant as a fresh command after
restart; it resumes from Account's durable state. This is not a durable distributed
exactly-once guarantee or a claim of complete automatic recovery after unknown ACK.

An unknown receipt puts the Account profile into `REPAIR_REQUIRED`, stops automatic
worker claims, and records a redacted `account.profile_change.repair_required`
audit. Merely scheduling an operator retry cannot invent a remote ACK: missing or
unknown receipts still fail closed. Operator reconciliation of unknown remote
facts remains necessary; this change adds no override or guessed-success action.
The durable first grant has its own `account.profile_change.revocation_started`
audit. A lost Account checkpoint can converge from a confirmed adapter receipt.
Password-reset uses its existing subject-only `/revoke-sessions` path; profile
commands never fall back to that path.

## Upgrade and rollback

Stop old Account/profile workers before migration and restart only the new
binaries after it succeeds. Old binaries do not know the new claim fence; mixed
old/new writers are unsupported during this repair rollout.

The user-approved conservative migration marks pre-upgrade profiles with
`account_committed_at` as possibly already dispatched, using the existing committed
or acknowledged timestamp as `sessions_revocation_started_at`. It does not invent
an ACK. If Account has no ACK and the adapter has no confirmed receipt, these rows
require manual investigation and cannot automatically issue a new logout. Earlier
uncommitted rows have not reached logout and retain an empty first-grant field.
Published SQL identities remain unchanged; new generation and dispatch facts are
forward-only, and public DownOne refuses to remove them.

Formal PostgreSQL roots are `TestIdentityRecoveryClaimsRuntimePostgres` and
`TestProfileSessionRevocationRecoveryRuntimePostgres`. They use public migrations
and a genuine non-superuser `account_service` runtime, then send the production
HTTP DTO through the actual adapter handler with a controlled directory side
effect counter. This is not a live Keycloak/provider or deployed rollout proof.
