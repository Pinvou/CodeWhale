# Personal zero-budget Hold release

BILL-25 treats an explicitly free price as a valid priced request. The ordinary
personal reservation path accepts a maximum of zero and creates no allocation
rows. Its release must still produce the normal RELEASED receipt, including
pre-dispatch cancellation and the trusted expiry recovery path in BILL-35.

Under the existing wallet/hold locks, release first retains the member allocation
identity check. A persisted PERSONAL Hold whose maximum is zero may then use the
empty-allocation path only when both usage and member allocation tables have no
rows for that Hold. Any open or historically closed allocation is inconsistent
with this free reservation and fails with ErrConflict. Positive-budget missing
allocations, generic organization Holds and existing member behavior keep their
current checks. No migrations, allocation amounts or ledger policy change.

This financial no-op does not establish that a request was never dispatched.
Token's canonical identity, dispatch fence, original cancellation proof and lease
rules still govern whether the release RPC can be called. Streaming or uncertain
requests stay under their existing recovery/review rules even when their maximum
is zero. A lost release acknowledgement keeps durable recovery evidence and may
only replay release for the original Hold; it does not create another reservation.

The strict financial PostgreSQL runner selects both
TestPersonalZeroBudgetHoldReleasePostgres and
TestPersonalZeroBudgetCancellationAndExpiryPostgres and requires completed child
cases. The tests use public migrations and restricted Billing/Token runtimes;
provider/request facts and the internal test transport are controlled fixtures,
not live model/provider or workload identity deployment acceptance.
