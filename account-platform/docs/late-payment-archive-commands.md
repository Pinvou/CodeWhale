# Late-payment archive acknowledgement recovery

The installed finance-operation hook creates one key per logical invocation and
retains that key and its original facts while the outcome is unknown. Archive's
UI dispatcher and client forward that key to the existing Billing route.

`POST /admin/v1/late-payment-reviews/{case_id}/archive-command` requires exactly one
`Idempotency-Key` (8–120 characters after trimming). Archive's existing request
has no editable reason or scheduling time; the command binds its actor and case
in the archive namespace. This change adds no new business facts or policy.

The first archive still requires its original active-case, final archive time,
successful authoritative unpaid query, payment/conflict and reviewed-claim
conditions. In the same transaction Billing stores the original immutable
request and before/after receipt with the existing order/manual/event writes.
Migration 900029 retains these facts permanently with no TTL; mutation,
truncation and downgrade reject deletion of the receipt history.

An exact same-key, same-actor, same-case retry returns that invocation's original
stored receipt without rewriting order, case, event or financial facts. A changed
actor/case conflicts. Current authentication, permission and ownership remain
required before receipt disclosure. Current terminal state or a generic 409 is
not proof of the original command: a new key still passes the original admission
and cannot adopt another command's archive. A committed original receipt can be
confirmed after its case or evidence has subsequently changed, without undoing
those changes or re-evaluating them as permission for another archive.

Only definite PostgreSQL serialization/deadlock rollbacks use the existing
bounded retry. Unknown commit outcomes keep their original invocation identity.
The finance hook continues to retain unknown after an unproven 409/403; this
change never clears uncertainty merely from a current terminal status.

This contract covers Archive only. Schedule, ProviderBlind, fresh query, claim,
payment repair and approval commands retain their independent contracts.

Only a new archive invocation explicitly freezes `commandProtocol: 1`. The
client requires that marker with no default. It calls only `archive-command`,
which Billing registers and Portal explicitly allows; old `archive` is retired
in the new routing/allowlist. Older Portal or Billing reject the new path before
repository dispatch, even though the old archive handler ignored request bodies.
There is no write fallback. A legacy pending invocation with no marker keeps its
original facts/key and remains unknown; it is never upgraded or dispatched as a
new command. Unupgraded actions receive no protocol marker.

## Deploy current-query archive admission

Apply the complete Billing migration package, including
`900047_late_payment_query_admission`, through the existing public deployment
identity before starting the new service. Verify the complete applied ledger.
The independent outbox statistics migration owns Billing `900046`; this query
migration owns `900047`. Their SQL contents and rollback contracts stay distinct.
Do not reuse a migration number or edit an applied ledger checksum to combine them.
If an earlier query artifact was already installed as `900046`, stop and plan
compatibility against that installed identity rather than replaying the renamed
package. The verified schema-only production observation for this change had
no `900046` and neither feature schema; it does not attest every deployment.
The migration appends nullable current-query generation/authority facts and does
not reconstruct them from historical timestamps or immutable evidence. Existing
orders therefore need a real new accepted query before a new archive is eligible;
an exact prior archive receipt still follows the original recovery path first.

Drain and stop old Billing writers and their archive entrypoints before enabling
the new service. Old binaries do not maintain the new generation facts and their
old archive API does not gain the new guard merely because the schema exists.
Do not run mixed old/new writers against this admission contract. Deploy the
schema first, then the new writer/entrypoint set; do not roll back to an old
writer or remove the forward-only facts to regain old admission behavior.

The latest query is ordered by its accepted generation, not by response arrival
time or an immutable evidence timestamp. A new accepted query reserves UNKNOWN
before supplier I/O; only that generation can publish its normalized current
result after the original flow succeeds. Failure/cancellation stays unknown;
PENDING/CREATED is not authoritative unpaid. An older reply cannot replace a
newer generation, while every strong payment and immutable observation still
uses the existing financial merge. No database lock spans external I/O.

New archives also reject an unfinished current-generation durable review-query
command and an explicit false frozen capability on either the order or case. A
historical unfinished command remains UNKNOWN permanently, but a separately
accepted, completed explicit command for the same case may restore admission
only when its immutable binding is the current generation and that generation
has a complete authoritative unpaid result. Background/non-command queries and
older completed receipts cannot stand in for that current command. Missing old fields
are not rewritten into a declared capability. These checks do not replace the
original deadline, claim, conflict, financial-fact or current-owner requirements.
Receipt recovery still requires the exact original actor/case/key and current
authorization; it does not re-evaluate today's new-command admission facts.

Apply `900048_late_payment_query_generations` as part of the complete package.
Stop and drain affected old query/archive writers before migration, then start
the compatible new writers. Its deferred constraint rejects a new unbound old
writer command at COMMIT with SQLSTATE `23514`; it does not backfill old UNKNOWN
rows or redispatch them. The published 900030 and 900047 SQL stays unchanged.
900048 is forward-only: an old-binary rollback or mixed writer deployment cannot
restore the former admission contract. Keep affected entrypoints disabled until
a forward-compatible repair is available. No deployment is performed by these
instructions.

Prepare atomically reserves the command binding and current UNKNOWN generation
before supplier I/O. Archive retains the physical case-update snapshot barrier:
even a transaction that began with a previously completed eligible command must
not admit after a newer Prepare commits. Original committed archive receipt
recovery remains first and read-only, including after a late strong SUCCESS
reopens the case. The original receipt does not authorize a new archive key.
