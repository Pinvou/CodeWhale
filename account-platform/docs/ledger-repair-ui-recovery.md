# Ledger repair draft recovery

The repair page reads the owner's frozen projection and creates an approval
**DRAFT**. Creating or recovering a draft never submits approval or executes a
financial repair. The existing approval detail remains the next explicit step.

Before the first create request, the page saves the actor, operation, complete
projection, reason and original operator key in session storage and verifies the
stored bytes. The local key is trimmed once and may contain up to 111 UTF-8
bytes. The `approval:` prefix adds nine bytes; the existing public approval key
limit remains 120 bytes. Valid Unicode keys remain supported.

An uncertain create result freezes these facts. Explicit retry uses the original
body and key after refresh or recent authentication, including a new client and
CSRF token. A subsequent 401, 403 or 409 does not discard an earlier uncertain
invocation. Another actor cannot replay it. An unavailable or damaged recovery
record blocks a new write. A first definite rejection with no earlier uncertain
write can release its intent.

Only a validated create response and successful comparison against the complete
original stored record clear the recovery intent. A current list or detail read
is not a create receipt. Comparing only the key is insufficient: a late response
must not clear a newer invocation using the same operator key with different
facts. A comparison or storage-clear failure retains evidence and blocks another
write. Responses from an unmounted view or a replaced client cannot navigate the
new actor's view.

The shared operation hook adds optional intent-construction and complete-record
clearing adapters. Existing account-closure and entitlement-correction callers
retain their original defaults. Browser verification uses built Portal assets
and controlled HTTP responses; it does not establish live identity-provider or
production financial execution coverage. Recovery cannot survive deletion of
the browser's session storage, and this storage does not grant permission.

## Reproducible validation inputs

The browser smoke scripts accept `CHROME` as an explicit Chromium executable and
also discover the existing Chrome/Chromium or Edge paths on Linux, macOS and
Windows. A missing executable fails before the Vite server starts. Run them
through the project npm scripts. Ledger React results are printed to stdout;
`F10_REACT_RESULT` explicitly opts into saving the diagnostic JSON at the chosen
path. No result file is written implicitly over a developer's existing file.

The retained `TestPrivateLedgerSignedCorrection` name identifies the original
signed-direction regression; the fixture now uses the checked-in migrations and
`PINVOU_TEST_SCHEMA_DATABASE_URL`, without a private machine path or fixed port.
Use an explicitly isolated PostgreSQL control identity: the test requires a
superuser session with no other non-system actors. The inherited fixture creates
UUID-owned databases and its own previously absent roles, and checks their
identity, connections and external dependencies before ordinary cleanup. It must
not be run against an instance with existing deployment actors. From the
`account-platform` module, select `go test -count=1 ./internal/billingschema -run '^TestPrivateLedgerSignedCorrection$'`. An absent
connection skips the case; a skip does not establish financial validation.

The checked-in `TestF09PolicyIdentityPublicCoordinator` runs the original policy
identity fixture and requires every selected child case to pass without skips.
From `account-platform`, run
`go test -race -count=1 -mod=readonly ./internal/f09fixture -run '^TestF09PolicyIdentityPublicCoordinator$'`.
Set `PINVOU_TEST_SCHEMA_DATABASE_URL` to the explicitly isolated control
instance, `PINVOU_PRIVATE_GO` to the Go executable, and
`PINVOU_F09_CHILD_STREAM` to a dedicated private diagnostic file. Its inherited
fixture requires its actors to be absent before creating UUID-owned databases
and restores those databases and roles. Do not use a shared or deployed
instance. Missing database opt-in skips the coordinator and is not evidence of
policy validation.

`TestF02ApprovalStopLossExecutionFence` uses that isolated control instance with
explicit `PINVOU_PRIVATE_OWNER_DATA_DIRECTORY` and `PINVOU_PRIVATE_OWNER_USER`
controls. From `account-platform`, run
`go test -race -count=1 -mod=readonly ./internal/billingschema -run '^TestF02ApprovalStopLossExecutionFence$'`.
The data directory must be absolute. Before any fixture writes, the guard
requires a loopback connection to `postgres` with no fallback, the matching
superuser session, actual port and data directory, no other non-system roles,
no application databases and no other client sessions. There is no fixed
private port, directory or account default. Providing a database URL with absent
or mismatching owner controls fails; only absent database opt-in skips the case.
The companion `TestF02StopLossOwnerConfiguration` checks configuration acceptance
and rejection without connecting; it does not establish PostgreSQL execution.


`TestAccountClosedGrantBillingHandler` uses controlled repository responses and
already verified delegation context to cover the existing Billing handler's
validation, closure-authority fence, actor/lifecycle forwarding and receipt. It
does not replace the workload/delegation middleware tests or the real PostgreSQL
reclaim and ledger integration tests. The cross-domain ledger integration also
checks that a requester with a verified approver grant is still refused. In the
existing `PROOF_REQUIRED` mode, self-approval stops at the read-only checkpoint:
its audit set, request, decisions, authorization and financial facts stay
unchanged. The existing public self-rejection path commits only its refusal
audit event. Both preserve the original request version, so an independent
approver can continue the unchanged normal execution chain.

Desktop account-closure recent-auth rejection now requests the existing in-app
username/password login, in the current UI language. The previous Portal-handoff
assertion is superseded by the user's same-actor native re-login decision. The
old business view is retired before native login starts, because native login
clears the previous credentials even when the password grant fails. Passwords
remain only in the current login input and existing native login transport; no
OAuth tokens, passwords or return URLs are stored in the coordinator or intent.

A fresh Account bootstrap must validate the same `user_id` before the new view
is restored. Re-login success is not a closure receipt and sends no automatic
closure POST: the user explicitly retries the original actor/scope/key/facts
under fresh permissions. Cancellation, failed authentication, another actor,
client disposal and late asynchronous results cannot complete the old request.
They retain its proof. A late native login is not treated as cancelled at the
provider and is never followed by a stale logout that could clear a newer
session. Normal Portal opening retains its default and selected language;
Portal profile editing and administration keep their existing online session
flow. The coordinator's controlled transport and browser fixtures verify the
local contract, not a real identity-provider password grant or production
recent-authentication policy.

The active personal recharge participant also snapshots its original product,
channel and request key before this native re-login retires the business view.
The feature-local in-memory record is scoped to the same client and actor; it is
not new persistent storage. Only an exact original context/authorization owner
with current read/create permissions can retry it in the new session epoch.
Changed context or permission isolates the record and blocks fresh order keys.
Cancellation and password failure retain it for the original owner's verified
return. A late authoritative receipt may settle only the matching original key
and complete product/channel facts, without updating the retired UI. This does
not transfer Admin/organization financial operations between session epochs.
An authoritative late receipt updates only the isolated record. The user may
explicitly refresh the current personal recharge view to observe that settlement
and release its form. Refresh cannot release an unresolved record or erase a
separate current pending invocation, and it never sends a recharge POST.
