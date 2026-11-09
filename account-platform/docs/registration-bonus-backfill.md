# Permanent registration eligibility backfill

Account prepares the eligibility set before accepting registration traffic. The
1000-row pages share one frozen historical time cutoff. This is an upper bound,
not a database snapshot spanning page transactions; startup preparation still
precedes registration traffic. Each committed PENDING page
stores the fingerprint key ID and a keyed verifier in the same transaction as its
facts and cursor. Resume requires both the same ID and the same key material;
changing the key under an unchanged ID is also rejected. Registration writers
check this binding while the backfill is still PENDING. Award eligibility and
PENDING/READY/BLOCKED registration modes retain their existing meaning.

The snapshot bounds both recoverable and unrecoverable historical counts. Rows
created or committed after it do not change those historical counts. Finalization
also rejects facts with another key ID. Migration 000087 retains its original
identity. Migration 900014 changes the checkpoint invariant and refuses a
destructive rollback after a key binding or permanent facts exist.

Unbound legacy progress or facts cannot safely be attributed to a key from its
ID alone. Preparation fails closed instead of blessing that evidence with the
current key. Do not erase the immutable facts or checkpoints to bypass the check;
restore consistent database/key evidence or use a separately reviewed forward
repair. There is no automatic key rotation or repair of mixed-key history.

## Recover the bound HMAC key

Treat the exact 32-byte key and its key ID as one recovery dependency of the
Account eligibility records, wallet provisioning jobs, and Billing claims,
receipts and ledger. Record the protected secret version and the matching
database recovery point in the controlled recovery set. Keep the encrypted
secret and its recovery authorization under separate custody. Use the site's
existing secret delivery channel for the Account process. Keep the key, its
verifier, fingerprints and mobile numbers out of recovery reports and logs.

If the deployed secret is lost, use the approved maintenance controls to stop
the affected registration and bonus writers and their automatic retries until
the key and data are verified. Include Account's wallet provisioning and
registration recovery workers. They can claim work without an incoming
request, so an ingress fence alone is insufficient. Disabling the Billing
policy while provisioning continues can permanently consume a first
registration as `NOT_GRANTED_DISABLED`. Preserve pending work and completed
receipts, including the original IDs and inputs used to replay them.

1. Restore the matching Account and Billing recovery points in isolation using
   the [backup and restore runbook](../deploy/runbooks/backup-restore.md). Compare
   the restored facts and outstanding jobs with the original recovery record
   using the read-only inspection phase. Retain an untouched paired recovery
   copy and capture its baseline before starting Account. Use the approved
   recovery point to reconcile any work accepted after the snapshot before
   reopening traffic.
2. Restore the protected version of `REGISTRATION_BONUS_FINGERPRINT_HMAC_KEY_BASE64`
   and its `REGISTRATION_BONUS_FINGERPRINT_KEY_ID` to the isolated Account
   process. Before startup, verify the rendered Account key ID against the
   recovery record without printing the secret. Compose accepts a site key ID.
   The standard Helm template pins `registration-bonus-v1`, so a recovery with
   another ID requires the approved matching site override. If the rendered ID
   differs, stop and correct the site configuration while preserving the
   database binding. Use a separate disposable copy for each incorrect ID or
   incorrect key test. Enable mobile registration for these startup checks so
   they exercise key validation. Both negative cases must fail before either
   worker starts and preserve the baseline facts and checkpoint.
3. Start Account with the recovered key and mobile registration enabled only on
   a disposable paired copy in the writable rehearsal phase. Use isolated
   databases, Billing, identity, messaging, and other dependencies. Block
   access to production services before starting either writer. Treat startup
   as a write boundary and reconcile work processed immediately by either
   background worker. For a controlled retry, prepare its test case on a fresh
   disposable copy before starting the workers.
   For a bound `PENDING` checkpoint, verify that the original cutoff and cursor
   resume and that concurrent startup preserves one set of facts. For `READY`,
   check that the Account readiness consumed by Billing reports the expected
   ID and zero unrecoverable registrations. Keep `BLOCKED` on its existing
   stop path. In isolation, replay a completed grant, retry an outstanding job
   with its original input, and exercise concurrent registrations for the same
   test number. Verify one permanent eligibility fact and at most one grant,
   including when the first account has been anonymized.
4. Record the release, recovery point, protected secret version reference,
   aggregate outcomes and unresolved jobs. Resume only after the recovery and
   security owners confirm the recovered material is trusted and the Account
   and Billing evidence agrees. Keep all disposable rehearsal copies out of
   promotion. On the separate recovery candidate, reconcile by registration
   operation. If Account is pending and Billing already has the matching
   receipt, replay the original input through the existing authorized recovery
   path, subject to its normal checks. Ordinary wallet provisioning requires no
   bonus receipt. For a completed operation, use trusted historical evidence
   to determine whether it required bonus evaluation. The completed job clears
   its original bonus mode and fingerprint. If evaluation was required and
   the expected receipt is missing, or if the original mode cannot be verified,
   hold that operation for reconciliation against trusted records from after
   the recovery point before allowing any retry.

## Rotate protection and plan HMAC key replacement

Rotate the storage or wrapping key using the site's approved secret service.
Preserve the underlying HMAC bytes and ID when rewrapping or moving them to a
new protected secret version. Rehearse recovery from the new protection before
retiring the old wrapping material. Include all retained recovery sets in that
check.

Suspected compromise or an unavailable verified backup keeps new bonus writes
stopped. A replacement HMAC key gives the same number a different fingerprint.
An old fingerprint alone cannot be converted into the new fingerprint. A
replacement plan needs a trusted way to resolve both generations before the
permanent eligibility decision, including already-anonymized subjects and
first registrations with no grant. It also needs a joint Account and Billing
cutover fence, unchanged replay of old requests, and conflict and crash tests
for both generations. Keep the immutable facts and receipts under their
original identities. Retire the old HMAC key only after independently verifying
coverage of all permanent facts, outstanding requests and retained recovery
points. The current single-key runtime supports recovery of the bound key.
