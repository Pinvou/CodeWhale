# Approved model removal and inflight cancellation

Administrators may enable inflight cancellation only on a REMOVED lifecycle plan.
The default is off. The option is immutable with the plan ID/version and appears
in both the normalized approval command and critical snapshot. Changing it
requires the existing reschedule/new-plan workflow and a new approval.

Approval permissions, independent approval, recent authentication, replacement
readiness, the minimum effective window and all dependency fingerprints remain
required. A true plan removes only the INFLIGHT_REQUESTS_PRESENT blocker. Requests
starting/finishing or changing execution state may invalidate the frozen plan;
normal streaming deltas and dispatch lease renewal do not change its inflight
fingerprint. Route probe facts can also invalidate it. Refresh and reschedule an
invalidated plan instead of bypassing these checks.

After approval and the effective time, the lifecycle worker persists the removal
and a cancellation policy atomically. The existing emergency worker requests
cancellation for nonterminal calls started before the effective time. Provider
cancellation may be unsupported or unconfirmed. This never implies free usage:
predispatch funds are released only after confirmation; dispatched calls retain
the existing usage reconciliation and historical-price settlement evidence.

Each emergency claim returns its incremented attempts as an ownership token.
Request and attempt recording lock the target before the usage row and require
that exact token in LEASED state; retry resets only that same claim. A delayed
worker cannot overwrite a reclaimed target, its usage, or its cancellation
evidence. Lease expiry alone does not revoke an unchanged claim; another worker
must durably reclaim or isolate the target. An external cancellation or idempotent hold release
already started after an owned request commits may still finish after reclaim;
this fencing does not promise exactly-once external effects. Existing USER and
expiry ownership checks and financial confirmation requirements still apply.

On a failed claim, the next automatic attempt is scheduled in Token storage:
5, 10, 20, 40, 80, 160, then at most 300 seconds. If a worker disappears, a
later worker observes the same cooldown after the 30-second lease. Targets in
cooldown do not occupy a claim slot. A worker leases each target just before
processing it, so time spent waiting behind other targets does not use its
budget. Failure or lease expiry on the eighth
automatic claim moves that target to `MANUAL_REVIEW` and records immutable task
evidence with a stable diagnostic. Existing claim generations are preserved;
historical claims do not count against the new retry budget.

Operations can filter the Token reconciliation queue for emergency cancellation
review. There is one such case per usage request. A financial reconciliation
case for the same request has its own severity, deadlines, and resolution;
even confirmed settlement cannot establish that the cancellation succeeded.
For diagnosis, read `model_emergency_cancellations` by
`usage_request_id` and its `MANUAL_REVIEW` evidence by task ID using an approved
read-only connection. Reconcile only through the existing usage and Billing
proof paths. The exhausted task is not retried automatically; exhaustion does
not establish a provider outcome, change the usage state, or release a hold.
Do not reset the task, run the model again, or release funds based on the
retry counter. An already started external action may still finish.

Before enabling this worker version, stop old Token gateway workers, apply and
verify Token migration `900007_emergency_cancellation_retry` with the existing
migration identity, then start the new workers. Old workers do not honor the
new cooldown column. Migration 900007 preserves review evidence and refuses
Down; recover using a corrected compatible version.

The worker attempts local transport cancellation immediately. When another
replica owns the connection, that owner also reads the immutable cancellation
request at its next existing dispatch-lease heartbeat (four-minute cadence,
within the shared ten-second renewal/read budget). This is bounded discovery
under the existing lease and database assumptions, not an immediate broadcast
or provider confirmation. Ordinary USER-only stops remain delivery-only.
Cancellation does not itself prove that funds may be released.

## Strict SAFE release procedure

This release uses lifecycle policy3/snapshot3. Historical policy2/snapshot2
approvals remain byte-identical and can execute only against false plans. No
historical policy, approval, migration checksum or request identity is rewritten.

1. With the installed old release, enter SAFE_ROLLBACK through the existing
   audited governance tool. This pauses all Token-governed administrative and
   configuration writes, not only lifecycle approvals. Existing result, audit
   and idempotent receipt reads remain available.
2. Stop the old writers and deploy the matching Account, Token, consumers,
   Portal and UI. Run Account migration900013 with the dedicated migration
   identity. Verify all installed policies and handlers with the release tools.
3. Use the existing SAFE-to-ACTIVE CAS workflow with a new writer generation
   and explicit `--policy-upgrade model-lifecycle-force-cancel-v1`. This contract
   accepts only the fixed prior complete manifest and fixed new manifest;
   existing epoch, role inventory, writer quiescence and Account policy locks
   remain mandatory. No ACTIVE-to-ACTIVE upgrade is supported.
4. Reopen writers only after activation and readiness checks pass. Preserve
   old v2 false approvals and pending delivery/recovery history.

On failure, remain in SAFE and inspect the original operation receipt. Do not
delete policy3, rewrite a true plan to false, or run its forward-only Down.
An old binary cannot safely take over writes after v3 effects. Use a corrected
compatible release and the existing controlled SAFE activation procedure.
