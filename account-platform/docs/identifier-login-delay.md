# Identifier login delay

The public Identity password grant keeps its existing fifteen-minute failure
counter. Five previous failures cause the next attempt to wait **before**
Keycloak verifies the credential. Attempts six through eleven wait 1, 2, 4, 8,
16 and 30 seconds respectively; subsequent attempts wait 30 seconds while the
counter remains live. A correct credential can still succeed. A successful
attempt does not clear either failure counter.

The source, device, session and provider quotas remain hard admission limits.
The existing ten-failure stable-user lock remains a hard lock. Identity checks
that lock again after waiting, without charging admission quotas twice. Known
and unknown identifiers use the same progressive wait. Cancellation or an
earlier caller deadline stops the wait before sending a credential upstream.
An attempt that waited before verification does not also receive the old
failed-response delay; recording the failed attempt remains unchanged.

Only an admitted password/SMS direct grant needing this wait extends the shared
server's response deadline by that actual delay: the original 30-second
processing budget plus at most 30 seconds. The Keycloak HTTP budget remains 15
seconds. Desktop password/SMS token requests have a 60-second total budget;
refresh and ordinary Desktop APIs retain 30 seconds. The personal NGINX edge
allows 60 seconds between upstream reads only at the exact public token route.
The shared refresh route gains no application processing time or retry. Portal
authorization-code and refresh exchanges still use private Keycloak and their
existing 15-second HTTP budget. Unsupported response-deadline transports fail
closed before starting a progressive wait.

These limits do not add retries, change handle lifetimes, clear counters or
bypass trusted-source extraction. No browser credential persistence is added.
