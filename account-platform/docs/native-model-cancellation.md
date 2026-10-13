# Native model cancellation

The desktop model provider remains Account Platform. The former `llmapi_hub`
and legacy command module are retained as source history but are no longer
compiled: F05 already removed their consumers and invoke registrations, and
their private provisioning API depended on settings fields removed by the
Account Platform migration. This change adds no legacy settings aliases,
changes no invoke registration, and deletes no files or user data. The existing
`client_migration` still owns legacy data migration and credential cleanup.

Each outgoing model invocation records its session, lifecycle generation,
upstream origin, original bearer, and original invocation key in memory. Stop
targets the captured generation. Engine reclaim cancels captured calls while
preserving a reserved generation that has not been submitted. Requests arriving
for a cancelled generation cannot start another outbound invocation.
Forgetting a deleted/reset session synchronously removes its old proxy route
and retires its transport incarnation. A cached old target remains retired even
when the same auxiliary session ID starts again at lifecycle generation one.
The new session receives a new random route and an independent cancellation floor;
old cancellation recovery retains its original origin, bearer, and keys.
Stop also captures the opaque transport incarnation before awaiting lifecycle
arbitration; a delayed old stop cannot cancel the new incarnation's generation
one. Billing notices check both this incarnation's retirement and the turn's
cancellation token, in addition to the existing lifecycle generation guard.

Cancellation stops local delivery immediately and sends the genuine personal
usage cancellation request with a separate stable cancellation key. A pending
model response-header wait is also interrupted by the stop signal;
that interruption does not prove whether the gateway accepted the invocation.
The usage ID comes from `X-Pinvou-Usage-Request-ID`; if that header was lost, a read-only
`GET /v1/usage-requests/by-idempotency-key` uses the original invocation key and
bearer. It never resubmits the model request to discover an ID. A lost cancellation
ACK is retried with the same cancellation key and followed by an authoritative
GET by the original invocation key. This confirmation uses the cancellation
permission and remains available when the current organization selection changes;
it does not widen access to personal usage details. Its returned usage ID must
match the already captured identity. Each HTTP operation has a five-second budget;
recovery makes at most eight attempts. A usage header first arriving after this
budget can start one additional bounded recovery, using the same cancellation key.
Repeated headers cannot create further tasks or replace the captured usage ID.
An unknown lookup, unavailable service, or expired bearer
leaves cancellation unconfirmed. Process restart loses this in-memory recovery;
the service's persisted usage/recovery state remains authoritative.

The desktop never releases a hold, changes a balance, or treats a 404 as proof of
no effect. Provider cancellation reported as `UNSUPPORTED` can stop delivery
while settlement stays `PENDING`; the server retains financial reservations and
handles late provider evidence. Cancellation confirmation and financial terminal
settlement are separate facts. Tracking is bounded to 4096 concurrent local
invocations. Logs contain a fixed diagnostic without bearer, key, payload, or URL.
Cancellation floors belong to live transport incarnations, not a permanent map
of retired session IDs. The retired floor is reclaimed after its frozen references
finish; the retired flag on any remaining reference continues to reject dispatch.
The 4096 limit covers concurrent invocations, not all retained live session routes.
Lifecycle/resource tracking is recorded in [issue #1598](https://github.com/Pinvou/pinvou3/issues/1598).

Regression tests exercise the actual loopback HTTP model proxy, stable invocation
identity, cancellation/reclaim, lost cancellation ACK, read-only lost-header
recovery, unknown lookup, and rejection of late delivery from an old generation.
These fixtures prove the native HTTP/lifecycle boundary, not live provider
cancellation support or production financial settlement.

The existing server admission budget counts `SETTLEMENT_PENDING` among in-flight
states: each personal user has at most five such concurrent invocations, alongside
the unchanged route, model and global limits. A pending settlement still owns an
unconfirmed financial effect. Cancellation, timeout or a stopped response does not
remove it from that budget or authorize releasing its hold. Five pending requests
can therefore keep new invocations at HTTP 429 until their authoritative settlement
states converge; this is the existing contract, not a newly relaxed admission rule.
Recovery observability, persistent retry/backoff and manual operating boundaries
remain tracked in [issue #1632](https://github.com/Pinvou/pinvou3/issues/1632) and
[issue #1644](https://github.com/Pinvou/pinvou3/issues/1644), rather than treating a
successful local cancellation test as their release acceptance.

For a retained pre-dispatch cancellation, an attached canonical hold may enter the
existing expiry-fence recovery only when its original usage/wallet/amount match,
the original user command has exact immutable local pre-dispatch evidence, and no
dispatch intent exists. A pending Billing fence is preserved when the user's first
cancellation command arrives; replay does not overwrite it with `UNSUPPORTED`.
Legacy, unattached or unproved cancellation uncertainty remains unconfirmed and
becomes visible to the existing manual-review case worker without requiring an
ordinary usage reconciliation to exhaust its attempts. A manual case is evidence
for investigation, not proof that funds were released. Ordinary Token notification
delivery and historical retention remain the deferred contracts already recorded
in #1632; this change neither installs a relay nor discards historical events.
