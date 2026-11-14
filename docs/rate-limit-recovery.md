# Model rate-limit recovery

The transport policy uses 20 total attempts including the first and a 600-second recovery budget. Local exponential equal jitter starts from a 5-second base and stops growing at 60 seconds. With a nonzero server minimum, the larger of the local delay and the minimum receives an additional half-local jitter; requests never retry before that minimum. These defaults are engineering limits, not provider guarantees.

Known quota windows and server minima that cannot fit the remaining operation budget return `QuotaWindow`, including with transport retries disabled. They do not enter short automatic retries. Actual transport failures remain typed when a preceding rate-limit episode exhausts the remaining budget. Ordinary transport recovery also emits the existing recovered receipt.

MiniMax/Zai successful JSON envelopes are inspected before model parsing. Reads are limited to 32 MiB and bounded by the caller's stream-open first-byte deadline and configured stream idle deadline. Existing per-request totals still apply. SSE bodies remain with their existing stream adapters. Native classification combines header, structured JSON, prose, and nested retry hints, and valid JSON string values in the display copy are decoded and redacted before sanitization, with final exact-value redaction afterward. Known native quota windows take precedence over generic billing prose.

The fork additionally owns shared effective-credential/account admission, stable logical-operation budgets, suspendable leases, persisted server reset times, and root/owned-child interruption. A long server reset remains persisted advice for later continuation; it is not shortened to the upstream transport's temporary pause cap. Local token-bucket waits and ordinary network backoff count toward active-work deadlines; provider-directed backoff and shared quota waits use the existing recovery clock.

A transport error does not prove a checkpoint was saved. Checkpoint preservation and continued execution belong to the existing Engine and child lifecycle. No new scheduler, tool replay, credential resolver, or task UI protocol is introduced by these fixes. The older fork does not have the newer upstream HTTP-attempt transmission ledger or quota/account-guidance types, so those APIs are not imported independently.

The branch still requires integration against its current target and applicable CI. Local loopback tests do not establish live-provider behavior, full host restart behavior, or non-Windows execution.
