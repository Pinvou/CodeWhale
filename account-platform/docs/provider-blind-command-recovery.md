# Provider-blind command recovery

Human provider-blind commands require `command_protocol: 1` and a stable
`Idempotency-Key`. The client marks only a fresh provider-blind invocation;
existing unresolved intents retain their original facts and key. An old intent
without that protocol marker is not silently upgraded or dispatched.

The receiver checks current authorization and review ownership. A successful
command stores its original request and database case snapshot in immutable
review evidence in the same transaction as the provider pause, event and outbox.
An exact retry returns that receipt without changing a later provider recovery.
Changed facts under the same actor/key conflict. A distinct explicit new key is
a new command and can legitimately pause the provider again.

The protocol field makes an older strict receiver reject the new request before
writing. There is no fallback to the old unkeyed request. Historical reason-only
evidence cannot prove a command identity and is not backfilled or reinterpreted.
This change does not perform provider recovery or bypass its approval contract.
