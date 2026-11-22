# Late-payment review scheduling

`POST /admin/v1/late-payment-reviews/{case_id}/schedule` requires one
`Idempotency-Key` (8–120 characters after trimming). The existing finance-operation
hook generates one key per logical invocation and retains that key and its frozen
facts while an outcome is unknown. The UI and client forward it to Billing.

Billing binds the key to the actor, case, complete UTC timestamp and trimmed
reason. A new key is a new schedule command. Reusing a committed key with the
same facts returns its original stored receipt, even after a later command has
scheduled a different time or the original scheduled time has passed. It does
not update the current plan or append another scheduling event. Reusing the key
with different facts conflicts. Sub-microsecond changes are different facts;
successful receipts reflect PostgreSQL's stored microsecond precision.

Current authentication, permission and case ownership still apply to receipt
recovery. An old receipt does not grant access after permission retirement or
owner reassignment. Active-case and future-time bounds admit only a new command:
an exact existing receipt remains recoverable after case termination without
changing terminal facts. A denied or conflicting
response cannot establish that an earlier unknown invocation never committed.

Migration `900027` retains the immutable request and before/after receipts with
the existing financial command retention convention: no TTL or history deletion.
Plan, scheduling event and receipt commit in the same transaction. Only definite
serialization/deadlock rollbacks use the existing bounded transaction retry;
an unknown commit outcome is recovered with the same original key and facts.

This contract covers scheduling only. Claim, fresh query, archive, provider-blind,
payment repair and approval actions retain their existing behavior.

The new invocation explicitly freezes `commandProtocol: 1` only for schedule.
The client requires that exact marker without a default and sends required
`command_protocol: 1`; Billing includes it in the stored request identity. An
older Billing strict decoder rejects the unknown field before dispatch. Missing
legacy pending markers are never filled in or converted to a new invocation.
Unupgraded query/claim/resolve/other actions receive no protocol marker.

A fresh UI schedule converts the displayed datetime-local value in the current
user time zone exactly once, before command facts are retained. The retained
nextReview is the absolute UTC instant; retries never reinterpret that local
form value after a system time-zone change. The client requires a timestamp
with explicit Z or UTC offset. Existing ambiguous pending values are not
guessed into a different instant or a new command, and remain unconfirmed.
The local input remains a local form display; no global time policy changes.
