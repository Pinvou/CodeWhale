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

## Durable Profile logout receipts

Account migration `900019_profile_revocation_receipts` persists the operation
receipt. Before granting the first attempt, Account locks USER, profile and a
global admission key, then reserves the receipt in the same transaction as the
grant and its audit. `PROFILE_REVOCATION_CAPACITY` defaults to 4096 unresolved
operations per Account database and accepts 1 through 1000000. Replicas must use
the same capacity. Helm exposes `account.profileRevocationCapacity` and governed
Compose reads the environment variable. Site capacity still requires measurement.
At saturation the original completion endpoint returns 202 with RETRY or
REPAIR_REQUIRED and reason `SESSION_REVOCATION_CAPACITY`. It creates neither
receipt nor first grant and follows the profile retry and SLA policy. Existing
reservations and confirmed replays remain usable when admission is full.

The existing Identity `POST /internal/v1/users/{subject}/profile-revocations`
retains `internal.identity.sessions.revoke`. Account sends the original operation
and required `first_attempt` plus `receipt_protocol` equal to 2. An old Identity
strict decoder rejects that added field before provider dispatch. A legacy DTO
without the field can only replay a confirmed durable receipt. It cannot claim a
new dispatch. The subject-only password-reset logout contract is unchanged.

Identity uses its existing Account URL, CA and rotating workload credential for
`POST /internal/v1/profile-revocation-receipts/claim` and `/confirm`. Only the
Identity workload receives `internal.account.profile-revocation.claim` and
`internal.account.profile-revocation.confirm`. Commands bind the operation,
exact subject and random dispatch token. A domain-separated SHA-256 digest binds
the subject to this operation across later identity anonymization. This retained
verifier is pseudonymous data, scoped to one operation. The receipt copies no raw
subject, mobile number, provider payload or credential from the original profile.

RESERVED permits one atomic claim to DISPATCHED and records the token hash. Even
reclaiming the same token cannot grant dispatch again. Identity calls the provider
only after that committed claim and confirms with the exact token before replying
204. Confirmation gets a bounded five-second attempt even when the caller leaves.
Lost provider ACK, lost claim ACK or failed durable confirmation fails closed and
requires operator investigation. No timeout renews a dispatch grant. Cancellation
before claim leaves RESERVED and can resume the same operation.

CONFIRMED replays 204 across Identity restarts and instances without a provider
call. The original Account checkpoint then clears local sessions under its
existing recovery fence. A lost checkpoint converges from the durable receipt.
Confirmed history stays in Account for replay and releases active admission
capacity. Closure seals a still-RESERVED receipt as NOT_DISPATCHED under the
original USER/profile transaction and releases its admission slot. That durable
terminal state cannot grant logout or be confirmed. DISPATCHED and legacy UNKNOWN
retain capacity slots. Identity has no
process-level receipt map, and no TTL deletes known or unknown outcome evidence.
Aggregate state counts, oldest unresolved time and `pg_total_relation_size` for
this table provide Account capacity inputs. Database storage grows with retained
history. Unknown cases cannot be evicted to create capacity. Manual disposition
still requires authoritative evidence and the existing authorization policy.

## Upgrade and rollback

Block profile ingress, stop all Account profile workers and drain Identity profile
logout requests before installing 900019. Retain backup and package identities.
Install the migration while old writers remain stopped, verify ledger identity
and receipt counts, then update both binaries and the exact Identity-to-Account
workload rules before reopening ingress. Mixed old/new profile writers are
unsupported during cutover. Previous grants become UNKNOWN unless Account already
held `sessions_revoked_at`, which becomes CONFIRMED. A legacy subject already
anonymized before cutover cannot reconstruct its former verification binding.
Migration binds the retained historical subject and never guesses the original.
Migration neither invents an
ACK nor renews a first grant. Unknown cases retain REPAIR_REQUIRED and redacted
audit handling. Never clear an old marker to force a fresh logout.

900019 is forward-only. Application rollback also requires stop/drain and retains
the new table and migration carrier. Old writers must not process new reservations
or dispatches because they do not record durable claims. Registration generation
fences retain their existing contract.

Formal PostgreSQL roots are `TestIdentityRecoveryClaimsRuntimePostgres`,
`TestProfileSessionRevocationRecoveryRuntimePostgres` and
`TestProfileRevocationDurableAdmissionRuntimePostgres`, plus
`TestProfileRevocationClosureRuntimePostgres`. Public migrations and the
non-superuser runtime exercise actual Account and Identity HTTP DTOs with a
controlled provider counter. Live Keycloak and deployed cutover require separate
site evidence.

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
