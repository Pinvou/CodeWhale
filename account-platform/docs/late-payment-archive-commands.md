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
