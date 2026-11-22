# Token streaming transport

## Stream state reads

The streaming checkpoints read `status` and `delivery_status` directly from the
owning usage row through `GetUsageStreamState`. The pre-persistence checkpoint
observes COMMITTED and RELEASED. Connected delivery checks USER_STOPPED after
each persisted CHUNK. Numeric frames and EOF also check terminal state before
the final evidence and settlement paths. Inconclusive read errors follow the
existing checkpoint error handling.

The observation bound is the next applicable stream checkpoint. An upstream line
or database/network I/O can delay that checkpoint in wall-clock time. After a stop
is acknowledged and the next frame arrives, its delivery checkpoint observes the
durable stop before client delivery. Cancellation latency samples therefore need
their frame cadence, database load and timing boundary recorded.

`TestStreamStatePollingPublicPostgres` exercises the public inference, cancellation
and replay routes with the shared HTTP middleware, actual PostgreSQL migrations
and `token_gateway`. It checks connected checkpoint counts, post-stop delivery,
detached completion, replay events and single dispatch/settlement counts. The
provider, authorization, tariff and external money boundaries use controlled
fixtures. The financial runner requires all three named scenarios.

## HTTP transport

Token gateway explicitly opts its application handler into `httpserver.WithStreamingWriteDeadline`. Only an actual `text/event-stream` response receives the policy, including dispatch, live replay and retained replay. Each response write/header flush has the existing30-second bounded write budget; the deadline is cleared between writes so valid generation can last beyond30 seconds. Unsupported deadline transports reject the stream before committing its requested status. Transport/deadline errors remain sticky and cancel the HTTP request context so existing delivery tracking observes disconnects. The metrics writer forwards flush errors rather than hiding them.

All shared servers retain read-header5s, read30s, write30s and idle60s defaults. Non-streaming responses and other services retain the absolute response write deadline. Token request-body/header/security middleware, two-hour upstream cap, explicit registered cancellation, detached dispatch and settlement/reconciliation rules are unchanged. A client disconnect still does not invent cancellation proof or release funds.

Billing notices are platform metadata. Supplier `X-Pinvou-Billing-*` headers and
the reserved top-level `billing_notice` in supplier 402 JSON responses are not
forwarded as platform notices. Retained supplier 402 responses are filtered on
delivery using their persisted dispatch hold; their stored ciphertext is unchanged.
Platform initial reservation failures retain their own notice receipts.

All gateway-framed SSE writers use the exact `Content-Type: text/event-stream`.
The desktop qualifies SSE billing notices only on successful responses with this
exact writer representation; ordinary SSE transport keeps its existing permissive
MIME detection. Error SSE and raw historical MIME aliases cannot become platform
notices. The controlled proxy path preserves this header value; changing it needs
a coordinated review of notice qualification. Status, stream bytes, EOF checks and
financial settlement rules are not changed by this qualification.

Run `go test -tags=nomsgpack ./internal/platform/httpserver ./internal/token -run "^(TestStreamingWriteDeadlineTransportContract|TestSharedApplicationServerTimeoutDefaults|TestSharedOrdinaryResponseRetainsAbsoluteWriteTimeout|TestTokenSSESharedHTTPExceedsThirtySeconds|TestTokenSSETransportRetainsRegisteredUpstreamCancellation|TestTokenSSETransportDisconnectRetainsDetachedDispatch)$" -count=1`. The actualHTTP/1.1 long-response cases take at least31.5 seconds and use the same shared server constructor, real Token routes/adapter/SSE logic with controlled in-memory repository/Billing/authorization facts and a loopback HTTP provider. They are not real PostgreSQL/funds/provider/issuer E2E.
