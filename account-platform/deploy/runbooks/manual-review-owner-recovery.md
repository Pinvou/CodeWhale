# Recover an invalid manual-review owner

An open case can have an owner even when `claimed_at` is empty: SLA escalation
assigns the configured on-call user before that user takes action. Platform
permissions do not authorize another administrator to overwrite this ownership.

Before changing production configuration, verify every configured on-call user
against Account's current user state and effective platform permissions. UUID
syntax alone is not proof that the user exists or can handle the domain. Use
the operator-selected real account; never grant permissions or create an account
to make an old example ID valid.

For an invalid configured owner:

1. Record the affected services, exact case IDs, old owner, current case state,
   effective service configuration, and rollback configuration privately.
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
