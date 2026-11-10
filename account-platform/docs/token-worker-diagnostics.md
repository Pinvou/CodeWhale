# Token Gateway worker diagnostics

Use the structured `event`, `stage` and `error_code` fields to identify recurring failures in the Gateway logs.

| Event | Stages |
| --- | --- |
| `model_emergency_cancellation_failed` | Target sync and claim, cancellation request, pre-dispatch confirmation, attempt recording, retry persistence and finalization |
| `token_governance_supervisor_failed` | Activation poll and confirmation, worker construction and unexpected exit of either the account or lifecycle worker |
| `usage_history_projection_failed` | Historical projection backfill |

The codes distinguish timeout, cancellation while the worker is still running, rejected state, unexpected results and other operation failures. Routine shutdown of the parent worker is excluded. The events omit identifiers, SQL and dependency error text. A failed emergency retry write leaves recovery to the existing lease policy. A governed worker exit still revokes local readiness before the next poll can restore it.
