# Direct payment repair recovery

The Payments repair action sends `command_protocol: 1` and its original
`Idempotency-Key` on both the initial request and explicit UNKNOWN recovery.
Only a fresh repair intent receives the marker. An old unresolved intent retains
its original facts and key; it is not silently upgraded or sent to an older
receiver through an unversioned fallback. Refund and sync protocols are unchanged.

A successful repair stores a separate immutable command receipt in the same
financial transaction as the original repair/audit. Command identity is bound to
actor, order and reason. It is independent of the existing per-transaction repair
audit: neither an old audit nor a currently paid order proves an unknown key.
An exact successful retry returns its original database projection after current
HTTP authorization, without another provider query or another financial effect.
Changed facts conflict, and a new command still requires the existing manual
review state and authoritative matching paid evidence.

Provider reads remain outside the financial transaction. Their existing attempt
and health telemetry is independent of financial commit/rollback. The anomaly
case/key repair protocol and approval flows are unchanged. Migration 900031 adds
only the new command table; historical rows and keys are not inferred/backfilled.
