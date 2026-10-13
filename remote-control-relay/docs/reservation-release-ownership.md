# Reservation retention ownership

`retainForReservation` and `releaseReservation` accept the same exact frozen
controller identifier domain. Invalid and non-string input fails with
`pairing_id_invalid`; identifiers are not coerced or normalized. Releasing a valid
unknown or already released ID remains an idempotent no-op.

Release changes only the matching in-memory retention owner. It does not erase
durable pairing evidence, affect another retained pairing, or skip the existing
quarantine/expiry conditions for later GC. Normal production release continues
to use the previously validated pairing ID saved in its reservation.
