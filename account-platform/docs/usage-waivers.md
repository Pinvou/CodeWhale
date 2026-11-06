# Administrator usage waiver

`POST /admin/v1/usage-holds/{hold_id}/release` preserves the existing
`BILLING_USAGE_HOLD_SAFE_RELEASE` / `AUDIT_ONLY` policy and
`platform.billing.holds.release` permission. It requires a verified human
identity, a current Account authorization decision, a reason (1–500 Unicode
characters) and the original `Idempotency-Key` (16–120 characters). A body actor
is rejected. No additional approval type or Portal workflow is introduced.

Billing freezes the original hold, usage, wallet, maximum, source-allocation
digest, reason, key digest, verified actor and authorization version before any
Token request. Repeating the key cannot replace these facts. A current hold
claim prevents a replacement key while the original decision is unknown.

Token accepts the command only under Billing's
`internal.token.usage.waive` workload permission. It commits one immutable
`ADMITTED` or `REJECTED` decision before acknowledging admission. A running
execution, a mismatching original usage/hold tuple, or the existing deadline
release owner can cause a durable rejection. The original rejected command
never reopens automatically. Billing retires only its exact current claim after
receiving that matching persisted rejection. A new key can then freeze a new
decision after eligibility changes. Timeout, HTTP error and malformed response
are unknown outcomes and cannot retire the claim. An old rejected-key replay
cannot remove a newer claim.

The admitted Token coordinator calls
`POST /internal/v1/holds/waiver` under
`internal.billing.hold.waive`. Billing reloads its own immutable command and
uses the same wallet → hold lock order as ordinary settlement. Outcomes are:

| Actual financial winner | Result |
| --- | --- |
| Administrator waiver | Release original reservations, actual zero, one immutable `SAFE_RELEASE` audit action; no journal or posting |
| Earlier ordinary Commit | Return that existing committed amount and original usage authority; no new charge and no successful waiver audit |
| Earlier ordinary expiry/deadline release | `RELEASED_OTHER`; retain the original release reason and zero-charge facts; no invented administrator success |

Member-backed release uses the existing member allocation identity checks and
release implementation. Current suspended buckets, membership status and account
status are preserved. A zero-budget hold requires empty allocation sets and
valid original identity; it creates no zero ledger posting.

Token closes settlement bookkeeping while retaining provider observations,
attempt status, token quantities and trusted usage facts. Later usage following
an admitted intent is evidence only and cannot enter ordinary Extend or Commit.
An already persisted original deadline intent retains recovery priority. When
there is no such intent, an admitted waiver recovers immediately through its
original reference; the automatic deadline waiting branch cannot postpone it.
Both decisions are serialized under the original Token usage-row lock. An unknown
admitted command never extends the existing charging deadline. Continuous
financial HTTP failures retain `ADMIN_WAIVER_RESULT_UNKNOWN` and the pending
original claim, without inventing a confirmed release or loss acknowledgement. Confirmed release after the deadline
retains the platform-loss and no-later-customer-debit boundary. A conflicting
already-prohibited debit remains a manual conflict.

The F16 customer settlement repair precheck and its locked executor reject a
current pending/admitted release-only claim. This is an unresolved coordination
fence, not a claim of successful waiver. A past prepared row whose persisted
rejection retired its claim does not block otherwise eligible F16 processing.
Existing expiry evidence and already committed repair results are retained.
While the original claim is current, a new reservation extension is rejected
under the same wallet → hold lock. An existing successful extension receipt
remains replayable. A matching persisted rejection retires the claim and permits
a new eligible extension; an unknown HTTP outcome cannot do so.

## Recovery and deployment

Configure Billing's `TOKEN_USAGE_WAIVER_URL` with the trusted Token internal base
URL and the existing workload identity trust configuration. The two permissions
are granted only to Billing → Token and Token → Billing respectively. Missing
configuration returns `USAGE_WAIVER_UNAVAILABLE`; unknown acknowledgements return
`USAGE_WAIVER_RESULT_UNKNOWN`. Recover a command by repeating its exact original
human key, reason and authenticated actor. Admitted commands also recover through
the existing Token reconciliation worker. No database transaction spans HTTP.

Billing `900024` and Token `900005` are append-only forward migrations; existing
published migration files retain their identities. The policy registry restores
the existing AuditOnly classification. Release policy digest evidence must be
generated from the resulting complete current registry, following the existing
deployment activation procedure.

The Billing-only verifier recognizes two exact histories for original `026`:
the unchanged mainline SQL and the retained old fixed SQL (LF or CRLF). It checks
the retained resource and original command table structure before applying any
new migration. Unknown history or incompatible structure fails before schema,
ledger or application changes. The generic migration verifier remains strict;
no ledger row is rewritten. Original legacy commands/actions and their source
identities are preserved.

An original completed legacy `SAFE_RELEASE` action is read under its original
hold, actor, reason and key; it is returned as a legacy record without inventing
a modern command reference. A legacy prepared command without a matching
completed action returns `LEGACY_WAIVER_REQUIRES_REVIEW`. Changing the key cannot
bypass that unresolved hold. This release does not infer a financial result or
resume a legacy money action from an error class, reason or absent response.
