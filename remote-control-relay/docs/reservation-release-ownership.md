# Reservation retention ownership

`retainForReservation` and `releaseReservation` accept the same exact frozen
controller identifier domain. Invalid and non-string input fails with
`pairing_id_invalid`; identifiers are not coerced or normalized. Releasing a valid
unknown or already released ID remains an idempotent no-op.

Release changes only the matching in-memory retention owner. It does not erase
durable pairing evidence, affect another retained pairing, or skip the existing
quarantine/expiry conditions for later GC. Normal production release continues
to use the previously validated pairing ID saved in its reservation.

On a controller-protocol WebSocket, desktop registration, controller claim and
resume preparation reserve their role while awaiting authorization or lease
work. A conflicting role request is rejected before it can enroll or replace a
desktop endpoint or take a controller reservation. Same-role claim retries
continue to share the lease manager's pending request. When an attempt fails,
its role reservation is released. A closed socket cannot commit a late result.
During B reconnect, a prepare for a lease of the same endpoint may wait for
registration to succeed. Only one distinct prepare can wait on that socket.
Legacy Web v2 sockets retain their separate registration and join flow.
