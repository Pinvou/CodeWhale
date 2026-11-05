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

## Password reset phase cutover (controlled stop required)

The existing operation and identity fence remain unchanged. The first confirmed
password checkpoint clears only the earlier precautionary session marker.
Idempotent password checkpoints preserve final logout and terminal/repair state.
Logout checkpoint and retry writes bind the phase captured before the RPC; a
late pre-phase callback cannot delete sessions or schedule the post phase into
repair. No database transaction spans Identity RPC, and this is not an
exactly-once remote logout protocol.

Do not roll old/new Account password writers together. The authorized operator
must complete this procedure before starting the new code:

1. Block password-reset and authenticated password-change ingress. Stop every
   old Account replica/process and its password recovery loop (Compose: use the
   deployment's existing project/files with `docker compose stop account-service`;
   Kubernetes: scale the existing Account deployment to zero and wait for all
   old pods to terminate). Verify no old writer remains and Identity password/
   logout requests have drained. Waiting for the 30-second claim is insufficient.
2. Keep ingress blocked. Privately archive IDs/status and the two stage timestamps
   from the SELECT below, without password handles, subjects or mobile numbers.
   Execute on the Account database under the authorized operational actor:

```sql
BEGIN;
LOCK TABLE password_reset_operations IN SHARE ROW EXCLUSIVE MODE;
SELECT id,status,password_updated_at,sessions_revoked_at
FROM password_reset_operations
WHERE password_updated_at IS NOT NULL AND sessions_revoked_at IS NOT NULL
  AND status IN ('PASSWORD_UPDATED','RETRY','REPAIR_REQUIRED') FOR UPDATE;
UPDATE password_reset_operations
SET status='REPAIR_REQUIRED',last_error_code='PASSWORD_SESSION_ORDER_UNPROVEN',
    updated_at=clock_timestamp()
WHERE password_updated_at IS NOT NULL AND sessions_revoked_at IS NOT NULL
  AND status IN ('PASSWORD_UPDATED','RETRY','REPAIR_REQUIRED');
SELECT count(*) AS unsafe_pending FROM password_reset_operations
WHERE password_updated_at IS NOT NULL AND sessions_revoked_at IS NOT NULL
  AND status IN ('PASSWORD_UPDATED','RETRY','REPAIR_REQUIRED')
  AND last_error_code IS DISTINCT FROM 'PASSWORD_SESSION_ORDER_UNPROVEN'; -- must be zero
COMMIT;
```

3. Those pending rows remain quarantined for human investigation. Do not clear
   their marker, press the ordinary retry action, reissue a password or logout
   again automatically: the old data cannot prove whether the logout was pre or
   post. Separate manual disposition requires independent operation evidence.
   COMPLETED rows, including migration backfills, are never rewritten by this
   cutover. Old REPAIR_REQUIRED rows with both markers are equally ambiguous and
   receive this quarantine reason; leaving their ordinary error code would let
   the existing retry action revive them. PW-null repairs retain their original
   manual recovery boundary. This one-time procedure runs before new writers;
   it must not be rerun over newly confirmed post-phase operations.
4. Start only the new Account version, verify old writers remain stopped, then
   reopen the original ingress. A rollback also requires stop/drain; old code
   must not process new phase-bound work. No migration, permission, public API,
   SMS, recent-auth or password/login policy change is introduced.

This is an executable operator precondition, not automatic data repair. It
isolates old PW-non-null/session-marker pending rows before new workers can
mistake them for final evidence. Already-issued remote requests cannot be
recalled; completed replays start no new logout, while an unconfirmed post logout
retains its original retry semantics.
