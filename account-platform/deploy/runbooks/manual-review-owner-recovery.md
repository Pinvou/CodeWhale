# Recover an invalid manual-review owner

An open case can have an owner even when `claimed_at` is empty: SLA escalation
assigns the configured on-call user before that user takes action. Platform
permissions do not authorize another administrator to overwrite this ownership.

Before changing production configuration, verify every configured on-call user
against Account's current user state and effective platform permissions. UUID
syntax alone is not proof that the user exists or can handle the domain. Use
the operator-selected real account; never grant permissions or create an account
to make an old example ID valid.

## Resolve retained unknown archives before reassignment

Before changing the on-call configuration or a case owner, inventory any retained
unknown archive with the original request initiator. Keep the original case,
actor, idempotency key and request unchanged. The repository checks current
ownership before looking up its immutable archive receipt: a new owner cannot
recover the original actor's command, and the original actor loses that API
recovery path after reassignment. An on-call change can also cause an automatic
SLA reassignment, so this gate applies before configuration changes as well as
manual owner writes.

While the outcome is unknown, do not transfer the case, substitute another actor
or key, clear the retained intent, or reopen the archive as a new command. Use the
original actor's permitted recovery flow with the original key and request. If
that account no longer has access, do not grant permissions or change ownership
merely to make the retry work; use an authorized operator's read-only Billing
receipt inspection instead.

On a private operator connection to the Billing database, bind `case_id`,
`original_actor_id` and `original_idempotency_key` from the retained invocation.
The following psql query does not change the case, command or financial facts:

```sql
BEGIN READ ONLY;
SELECT command.id AS receipt_id, command.created_at,
       command.request_snapshot = jsonb_build_object(
         'case_id', :'case_id'::uuid,
         'actor_id', :'original_actor_id'::uuid) AS original_request_matches,
       command.after_snapshot->>'case_id' = command.case_id::text
         AS receipt_case_matches,
       command.after_snapshot->>'owner_id' = command.actor_id::text
         AS receipt_actor_matches,
       command.after_snapshot->>'review_status' = 'ARCHIVED_CONFIRMED_UNPAID'
         AS receipt_archive_matches
FROM public.late_payment_review_archive_commands AS command
WHERE command.case_id = :'case_id'::uuid
  AND command.actor_id = :'original_actor_id'::uuid
  AND command.idempotency_key = :'original_idempotency_key';
ROLLBACK;
```

Only an exact single receipt with every match explicitly true confirms that
original archive invocation. Record its reference privately and inspect the
current case/order separately before choosing the approved repair set; a stored
receipt describes the original invocation, not today's state. No row, a mismatch,
a null check or an unavailable read leaves the outcome unresolved: absence is
not proof of NOT_DISPATCHED or permission to submit a new archive. Stop owner and
on-call changes for that case until the original outcome has authoritative
confirmation. If ownership already changed, use this read-only inspection rather
than changing it back to bypass recovery checks. Do not put the original key or
private operator evidence in application logs, PRs or shared transcripts.

For an invalid configured owner:

1. Record the affected services, exact case IDs, old owner, current case state,
   effective service configuration, and rollback configuration privately. Complete
   the unknown-archive gate above before steps that can change ownership.
2. Correct the on-call configuration before repairing cases, so a later SLA tick
   cannot assign them back to the invalid user. Preserve service images and all
   unrelated configuration, and verify health after each affected service update.
3. In one bounded transaction, lock only the explicitly approved open cases.
   Recheck the expected old owner and processing state. A concurrent claim,
   payment or resolution requires a new inspection, not an unconditional retry.
4. Set the selected owner and advance the assignment revision. Append an immutable
   case event recording the old/new owners, reason, and unique recovery reference.
   Preserve old assignment records. An operator correction does not imply that
   the target user claimed, responded to, mitigated, or resolved the case.
5. Verify the exact case count, new owners, appended events, and unchanged order
   and financial facts. Retain a receipt and reconcile uncertain outcomes by
   reading that recovery reference before considering another write.
6. Preserve the corrected effective configuration in the deployment record and
   subsequent deployment/rollback inputs; an old saved Compose/Helm configuration
   may still contain the invalid ID.

The review UI offers Claim only for an unassigned active case. A case assigned
to the current user offers its permitted handling actions; a case assigned to
another user explains the ownership restriction. A concurrent 409 on Claim asks
the user to reload the current owner and status. Transport failures retain the
existing unknown-operation recovery contract.

An owner correction is not an unpaid-payment finding. In particular, a provider
without an authoritative unpaid terminal result cannot be archived as confirmed
unpaid merely because the local payment deadline elapsed.

The UI disables a fresh archive when the stored provider capability explicitly
states that authoritative unpaid terminal results are unavailable, and explains
the waiting-period/evidence prerequisites otherwise. This is an early usability
check, not a replacement for the repository's full transactional admission.
Older/unknown capability snapshots still rely on server validation. A conflict
asks the operator to reload the owner and archive evidence; a retained unknown
archive invocation keeps its original receipt-recovery path and key.
