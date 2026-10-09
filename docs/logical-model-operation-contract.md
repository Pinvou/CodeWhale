# Logical model operation and route compatibility

Auto routing retains its existing heuristic fallback when the classifier is
unavailable or its configured provider identity cannot be resolved. Resolution
failure now emits a fixed diagnostic, and the caller reports that it retained
the fallback. Neither diagnostic includes error text, user-owned provider names,
endpoints, request content, or credentials. An empty recommendation still uses
the same fallback without reporting a resolution failure.

Legacy `ollama` routes configured for the recognized Ollama Cloud endpoint keep
their existing compatibility migration. Scoping discloses this migration with a
fixed diagnostic, retains the persisted legacy identity and migration flag, and
does not rewrite configuration files. It does not invent a local Ollama route.

Host request-idempotency opt-in stays scoped to the exact resolved identity:
same built-in identities retain it; foreign built-ins clear it. An unspecified
provider uses the existing default identity rather than granting every route
the opt-in. A conflicting static header is explicitly rejected, rather than
silently overriding either identity. Vision compares effective request endpoints,
including their path construction, before inheriting a route's opt-in.

These contracts do not change heuristic selection, provider authorization,
financial settlement, or the meaning of an uncertain request outcome.

Engine streaming prepares each outbound round once. Opted-in transports return
an opaque request-local payload and identity. Transparent transport retries reuse
that payload and key. An outer retry prepares a new round: the same wire identity
retains the key; a changed wire body gets a new identity. The payload is bound to
its route, authentication, wire
format and path configuration; another route cannot dispatch it. There is no
process-wide preparation cache. Ordinary transports return no operation identity,
so the Engine does not serialize or hash their requests just for unused keys.
Explicit compatibility identity queries share one conservative hashing helper.

Preparation failures use the existing localized authentication decoration and
structured error event. Cancellation has priority before preparation and before
dispatch. A preparation failure emits no RouteDispatched event because no model
request was dispatched. Ordinary streams and their existing error recovery remain
on the same dispatch path. Transports that implement real operation headers must
also implement the prepared-call seam; its default preserves ordinary providers.