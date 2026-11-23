# Model rate-limit recovery

The transport policy uses 20 total attempts including the first and a 600-second recovery budget. Local exponential equal jitter starts from a 5-second base and stops growing at 60 seconds. With a nonzero server minimum, the larger of the local delay and the minimum receives an additional half-local jitter; requests never retry before that minimum. These defaults are engineering limits, not provider guarantees.

Known quota windows and server minima that cannot fit the remaining operation budget return `QuotaWindow`, including with transport retries disabled. They do not enter short automatic retries. Actual transport failures remain typed when a preceding rate-limit episode exhausts the remaining budget. Ordinary transport recovery also emits the existing recovered receipt.

MiniMax/Zai successful JSON envelopes are inspected before model parsing. Reads are limited to 32 MiB and bounded by the caller's stream-open first-byte deadline and configured stream idle deadline. Existing per-request totals still apply. SSE bodies remain with their existing stream adapters. Native classification combines header, structured JSON, prose, and nested retry hints, and valid JSON string values in the display copy are decoded and redacted before sanitization, with final exact-value redaction afterward. Known native quota windows take precedence over generic billing prose.

The fork additionally owns shared effective-credential/account admission, stable logical-operation budgets, suspendable leases, persisted server reset times, and root/owned-child interruption. A long server reset remains persisted advice for later continuation; it is not shortened to the upstream transport's temporary pause cap. Local token-bucket waits and ordinary network backoff count toward active-work deadlines; provider-directed backoff and shared quota waits use the existing recovery clock.

The cumulative Engine turn budget (`[tui].turn_wall_clock_secs`) measures active
work, excluding its actual provider recovery sleeps. Normal request/generation
time, response-body reads, local admission,
ordinary network backoff and active tool execution remain charged. During
parallel tools, the turn counts time while any owned work remains active; only
intervals when all owned work is blocked on provider recovery are excluded.
The child model's `step_api_timeout` excludes eligible provider-recovery waits
and resets on each inner transport attempt; it is not a cumulative bound on
the entire outer model call. Ordinary generation remains charged, and the
independent operation, turn and task bounds still apply.
Typed rate-limit failures stop that child with an interrupted
checkpoint rather than resetting the transport budget through an outer retry.
Child tool timeouts and the Fleet worker's task-wide deadline remain separate
bounds. Independent child engines have separate clocks and cannot pause an
active parent. Each Engine turn owns a fresh
wait clock, and dropping a cancelled sleep closes its interval synchronously.
This changes the turn's active-work accounting, not its configured finite budget,
step limit, terminal statuses or automatic-continuation policy.

A transport error does not prove a checkpoint was saved. Checkpoint preservation and continued execution belong to the existing Engine and child lifecycle. No new scheduler, tool replay, credential resolver, or task UI protocol is introduced by these fixes. The older fork does not have the newer upstream HTTP-attempt transmission ledger or quota/account-guidance types, so those APIs are not imported independently.

## Configuration

The following TOML table configures HTTP 429 recovery independently of the
ordinary `[retry]` transport settings:

```toml
[retry.rate_limit]
max_attempts = 20
recovery_budget_secs = 600
initial_delay_secs = 5
max_delay_secs = 60
```

`max_attempts` includes the first attempt and accepts 1 through 1000.
`recovery_budget_secs` accepts 1 through 86400. Delay settings must satisfy
`1 <= initial_delay_secs <= max_delay_secs <= 86400`. The maximum local delay
does not cap a provider's minimum. A minimum outside the remaining recovery
budget stops automatic retry; it is still retained for later admission.
`retry.enabled = false` disables automatic transport retries, not known quota
classification or the shared admission pause.

Reset receipts live in `rate-limit-recovery/` under the resolved Codewhale
home: `$CODEWHALE_HOME` when explicitly configured, otherwise `~/.codewhale`.
An embedding host or trusted config can override this with the root-level
`rate_limit_recovery_dir` path; put that key before any TOML table, not inside
`[retry.rate_limit]`. The directory must be private and writable by its owner.
Each `<opaque-scope-key>.json` contains one JSON UTC timestamp, not a credential
or a conversation. The key distinguishes effective endpoint and credential
scopes; treat it and its directory as private metadata.

## Inspecting and repairing saved state

A long deadline is deliberately preserved across later calls and restarts.
It can outlive one turn's recovery budget. Restarting alone does not clear it.
Values beyond the timestamp representation are conservatively represented by
the maximum UTC timestamp, not treated as permission to retry immediately.
An unreadable, oversized or invalid receipt blocks that scope until its local
storage is repaired; the client does not silently ignore it or erase a reset.

When a storage error asks for repair, inspect the effective directory above
privately. Confirm the provider's reset/account status and the scope that is
affected before changing any deadline. Do not copy credentials, scope keys or
private absolute paths into public reports. A valid future reset should be
waited out, even if it exceeds the automatic recovery budget.

To locate the receipt for the current CLI configuration and selected Fleet
operator, explicitly run:

```sh
codewhale doctor --recovery-json
```

Use the same working directory, config/profile, routing overrides and credential
environment as the affected call. This opt-in output is private: `located`
includes the exact receipt path, even if that file does not currently exist.
Do not paste it into public logs, model prompts or issue reports. Ordinary
doctor output and request errors do not expose this scope identifier.

The locator follows fresh-launch Fleet selection and explicit provider/model
overrides. Interactive project overlays are not resolved: if a workspace has a
project config, it conservatively reports `unavailable`. Use
`--no-project-config` only when that matches the route being repaired; otherwise
identify its effective provider/model and config privately before locating a
receipt. Session-restored routes are not resolved.

The locator uses the dispatch scope recipe, reads credentials through existing
read-only adapters and never sends a provider request, refreshes credentials,
loads a recovery receipt or changes quota state. It reports `unavailable`
instead of guessing when its configuration or read-only credential resolution
fails, or a path cannot be represented losslessly in JSON. Outer CLI argument or
config-parse errors can still exit before the locator emits JSON.
Managed OAuth that may enter a refresh lifecycle is not resolved: owned ChatGPT
credentials and official xAI OAuth routes can therefore be unavailable. Codex
process-token overrides and explicitly consented, valid external Codex
credentials remain supported. No new credential access consent is granted.

This describes a fresh CLI credential snapshot, not every historical account,
an already-running host's frozen route, or an independently configured auxiliary
route. If credentials have rotated or a host chose its own route, do not use
this result to guess which older receipt to change. Consult that host's private
configuration instead. The command only locates a file; it neither confirms
that a provider reset has cleared nor authorizes deleting its saved minimum.

If the provider confirms a reset has cleared, or the local receipt is known to
be invalid, an operator can recover deliberately:

1. Stop all Codewhale clients and embedding hosts using that recovery directory.
   Editing a receipt while a client is running does not clear its in-memory
   pause and can race a later save.
2. Preserve a private backup of the specific affected receipt. Correct storage
   permissions or availability first. Replace invalid content with the verified
   provider reset time as a JSON UTC timestamp; only remove the specific receipt
   when deliberately discarding that saved minimum is justified. Removing it
   permits the next call to learn quota state again and can cause an early 429.
   Do not delete all scopes or guess a provider reset time.
3. Restart those clients and make a new manual call. Loading can be retried after
   a failed read; a successful previous load is cached in the current client.

Saving writes a unique temporary receipt, checks the asynchronous write,
synchronizes the file, closes it and renames it over the saved receipt. A save
failure keeps the in-memory pause and reports an error; it does not promise
that the new minimum survived a restart. Cancellation or interruption can leave
an orphan `<opaque-scope-key>.<unique-id>.tmp`; such files are not loaded as
receipts. Inspect them only after stopping the owning clients. File sync before
rename improves file-data persistence, but there is no directory sync or claim
of complete power-loss durability on every filesystem.

Local loopback and storage tests do not establish live-provider behavior,
real operating-system restart or power-loss recovery, or non-Windows execution.
