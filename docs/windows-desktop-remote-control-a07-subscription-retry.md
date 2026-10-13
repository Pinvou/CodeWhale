# A07 subscription registration retry

Each of the nine bridge event channels has its own registration attempt.
Successful and pending registrations are not duplicated by another `subscribe()`
call. A synchronous registration exception or rejected promise releases only
that channel's attempt; a later explicit subscription or existing query/connect
path can retry it. There is no automatic timer or unbounded background retry.

`stop()` keeps its existing cleanup and resets the registration bookkeeping for
the next subscription. A rejection from an older attempt cannot clear the new
attempt. Event handlers, lease/generation checks, accepted business operations
and history are unchanged; retrying a listener does not redispatch an old task.

This change covers the registration-failure point of Issue #1390. Retirement of
late successful unlisten handles and old renderer callbacks is a separate point
addressed by PR #1783; this branch does not silently include that patch. Both
changes must be preserved and their tests run after ordinary integration. A
pending SDK promise remains pending rather than being treated as a failed
registration or an authorization to install a second listener.

When integrating the lifecycle patch, keep its subscription owner stable while
retrying channels in the same lifecycle. A channel retry must not retire other
successful callbacks; `stop()` still retires the lifecycle and its late handles.
The channel-attempt identity is only bookkeeping, not business authorization.

The regression loads the real bridge/state source in a VM with controlled
registration promises. It covers partial/all rejection, synchronous exceptions,
pending deduplication, stop/restart bookkeeping and a successful same-binding
status callback. This is not a real dual-host or native event-plugin deployment
test.
