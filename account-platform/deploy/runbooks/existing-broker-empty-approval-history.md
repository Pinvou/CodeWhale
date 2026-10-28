# Existing broker with empty SQL approval history

This is an explicitly invoked **models profile** compatibility operation. It
does not change the fresh-only initializer, provision resources during service
startup, or permit a historical approval drain. Use it only after approval of
the exact deployment plan and after independently verifying its public hashes.

## Preconditions and preserved facts

- Use a controlled `account_migrator` login with the existing control roles.
  Supply the database connection through `APPROVAL_INITIALIZER_DATABASE_URL`
  and a protected passfile; never put passwords in command arguments or logs.
- Stop stream publishers and the affected executors, take and verify the required backup, then
  explicitly quiesce and fence the authoritative Account control. Both SQL
  history checks remain empty, including completed requests and delivery facts.
- The fixed stream has no retained authorization or authorization dead-letter
  messages. The only existing consumers are the unused shared Billing durable,
  refund durable, and three Token command durables. Unknown resources stop the
  operation; it never deletes, purges, resets, or recreates existing resources.
- The published legacy consumer has AckWait **1 second**, MaxAckPending **1000**,
  and BackOff **[1,2,5,10,30,60,120,300] seconds**, with the entire remaining
  configuration matching the frozen legacy contract. After its dedicated step,
  the same three Token instances have AckWait **30 seconds**, MaxAckPending
  **64**, and no BackOff. Other configuration, creation identity, delivery/ACK
  positions, and messages are preserved. Pending and ACK-pending must be zero.

Retained-message absence does not prove that a message never existed and was
removed by another administrator. An independently verified history/maintenance
boundary is still required; this tool does not grant authority to erase history.

## Files and permissions

Use the existing `APPROVAL_RESOURCE_CURRENT_PUBLIC_KEYS_FILE/SHA256`, optional
`PREVIOUS_PUBLIC_KEYS_FILE/SHA256`, `MANIFEST_FILE/SHA256`, and
`RECEIPT_FILE/SHA256` variables, all with the `APPROVAL_RESOURCE_` prefix. Supply
`NATS_URL`, `NATS_CA_FILE`, and exactly one private `NATS_NKEY_SEED_FILE` or
`NATS_CREDENTIALS_FILE`. The operator's actual NKey must equal the trusted v2
`event-resource-operator` projection. Keep private directories private; put
resource files in a separate controlled, non-linked directory outside every
private credential directory and the public-identity directory.

`approval-resources render-existing-broker` uses create-only
`APPROVAL_RESOURCE_MANIFEST_FILE` and `APPROVAL_RESOURCE_CONFIGURATION_FILE`.
It retains the existing bounded consumer ACL projection, adds only INFO for
the unused shared Billing durable, and removes stream CREATE and the inherited
refund-consumer CREATE/UPDATE subject. The exact Token
CREATE API subjects also carry JetStream's in-place update requests; runtime
identities receive no CREATE authority. No wildcard API, DELETE, purge, shared
Billing execution, or stream reconfiguration permission is issued. Independently
review the resulting configuration and install/reload it through the controlled
broker procedure while retaining all existing identities and data volumes.

For a verified v1 custody package whose six identities must remain unchanged,
the offline `nats-identities` v2 option `direct_cutover_preserve_account_outbox`
retains all six seeds and creates only a distinct approval publisher. This is a
direct cutover: no previous-v1 overlap file is configured. Independently bind
the embedded prior SHA to the actual trusted v1 bytes, verify all six role/seed
matches and the encrypted recovery package, and check the effective ACL on
existing and newly established connections before resuming any publisher.

JetStream cannot express a field-limited UPDATE-only grant for those three
subjects: someone holding the operator credential could directly issue another
CREATE or UPDATE request for those exact durables. The three-field restriction
is enforced by the reviewed CLI and controlled operator custody, not by NATS
alone. Stop if that operator and maintenance boundary cannot be established.

Plans use `APPROVAL_EXISTING_PLAN_FILE/SHA256`. A prepare operation writes a
new canonical plan and commits a distinct Account audit binding its SHA. The
plan records the operation UUID, full original consumer configurations and
their hashes, creation timestamps, exact integer delivery/ACK positions,
stream creation identity, message count, bytes, first sequence, control facts,
and original high-watermark Q. The stream state must remain identical until
initialization and verification complete; a changed message set stops the work.
Preparation performs INFO calls only. Review the file independently and project
its SHA before any effect operation; hashing a file at service startup does not
create trust. Token upgrade and Billing initialization use **different plans**.

## Optional historical refund upgrade before Token preparation

If the verified old shared Billing and refund durables both have MaxDeliver 12,
Token preparation cannot start until a separately authorized refund upgrade has
completed. Use this path only under an approved maintenance plan. Stop all
Billing refund writers and outbox publishers; independently check the refund
table and unpublished/in-flight refund outbox, as the Account approval lock
does not fence Billing. Confirm empty required histories and zero consumer
pending/ACK-pending. Back up and independently verify the state first.

The offline `render-refund-upgrade` projection adds only the exact refund
consumer CREATE API subject to the existing operator ACL. That NATS subject
also accepts UPDATE and cannot restrict individual fields: isolate existing
connections, exclusively control the operator credential, independently review
the generated configuration, and use the reviewed CLI. Runtime roles do not
receive this grant. Revoke it by restoring the reviewed `render-existing-broker`
ACL after the operation; reload and verify the effective configuration and that
old connections cannot retain the temporary permission.

After quiesce/fence at version 3, use a **new, distinct** plan file and run
`-action prepare-refund-upgrade -expected-version 3 -operation-id <new-UUID>`.
Independently review its bytes and set `APPROVAL_EXISTING_PLAN_SHA256`. Use
`inspect-refund-upgrade` for INFO-only classification, then
`upgrade-refund-consumer` to change only MaxDeliver 12 to -1 on that same
refund durable. `check-refund-upgrade` performs readback. The shared Billing
consumer stays unchanged at 12, and the three Token consumers stay in their
original legacy shape until the later Token plan. All positions, instances,
other configuration and messages must match the original plan. Stream MaxAge
still applies; -1 does not make messages live indefinitely.

Before attempting the update, the CLI commits a protected audit marker for the
control version and epochs. Once identity, plan, control and database checks pass and the marker is
committed, even a broker preflight failure consumes this one-time attempt.
A failure in those earlier checks writes no marker and sends no broker update. Never rerun the update or prepare a replacement plan in that
epoch after an unknown result: preserve the original plan and use read-only
`inspect-refund-upgrade`; independently determine any further recovery. This
barrier assumes exclusive operator custody and use of the Account-controlled
CLI, not a direct broker API call. Resume the normal sequence below only after
readback is complete, temporary authority is removed, and its own preconditions
are freshly verified. The Token and Billing plans remain separate from the
refund plan.

### Separately authorized one-time refund recovery

After a failed original attempt, an operator may explicitly authorize one
recovery in the same control epoch. Preserve the original plan, attempt audit,
and operation journals. Keep all business writers stopped and retain the
continuous Billing write fence through the new preparation, update, readback,
and temporary-authority revocation. This is not an automatic retry or a way
to change the Account control version.

Set `APPROVAL_REFUND_RECOVERY_ORIGINAL_PLAN_FILE` and
`APPROVAL_REFUND_RECOVERY_ORIGINAL_PLAN_SHA256` to the immutable original
plan. Set `APPROVAL_EXISTING_PLAN_FILE` to a **different, unused** public
plan path and leave its SHA unset. Invoke
`prepare-refund-recovery -expected-version 3 -operation-id <new-UUID>`.
SQL requires exactly one original attempt, its matching prepared plan, no
successful original upgrade, and no previous recovery preparation or attempt
in these control epochs. The broker must still have the original five legacy
consumers, exact creation identities, configuration, and delivery/ACK positions,
the original stream identity and high watermark. A reduced retained prefix
may be captured in this explicitly authorized new plan; the original 30-day
MaxAge is unchanged. New messages or any consumer drift reject recovery.

Independently check the new exact plan and set
`APPROVAL_EXISTING_PLAN_SHA256` to its digest. Invoke
`upgrade-refund-recovery -expected-version 3` once. The recovery has its own
committed, predecessor-bound attempt marker. A subsequent failure consumes
this recovery opportunity even if no UPDATE was sent. All stream state must
remain exactly equal to the **new** plan through its update and verification;
recovery does not relax ongoing drift checks. Neither recovery preparation
nor update accepts `-resume`.

Use `check-refund-recovery -expected-version 3` only for readback against the
same new plan, including after an unknown update result. It sends no UPDATE
and does not require a successful response or successful upgrade audit.
An unchanged legacy consumer causes this check to reject. Never delete either
attempt marker, replace either plan, create/delete a consumer, or move its
positions to make a check pass. Revoke the temporary exact authority on every
aborted operation as well as after success, then independently decide any
further action. Continue the existing Token/Billing sequence only after a
successful recovery readback and final ACL verification.

## Explicit sequence

All commands below start with `approval-resources existing-broker`. `status`
without `-expected-version` reads the actual control version and writes no audit.
Every other action requires a positive `-expected-version`. Numbers below show
the initial LEGACY/RUNNING/version1 sequence; a mismatch stops the operation.

| Action | Expected version | Required input/result |
| --- | --- | --- |
| `status` | optional | Authoritative control readback |
| `quiesce` | 1 | QUIESCING, version2 |
| `fence` | 2 | FENCED, version3 |
| `prepare-token-upgrade -operation-id <new-UUID>` | 3 | New Token plan path; plan SHA unset |
| `inspect-token-upgrade` | 3 | Reviewed Token plan and committed prepare audit; read-only classification |
| `upgrade-token-consumers` | 3 | Only the three exact in-place parameter updates |
| `check-token-upgrade` | 3 | All three current; INFO only |
| `prepare-existing-consumers -operation-id <new-UUID>` | 3 | Different new plan path; clear old plan SHA |
| `inspect-consumers` | 3 | Reviewed Billing plan; read-only present/missing classification |
| `initialize-consumers` | 3 | Create missing membership durable and eleven Billing durables; new receipt path |
| `verify-consumers` | 3 | Independently trusted receipt SHA; exact real readback |
| `activate` | 3 | PROOF_REQUIRED, version4; approval epoch advances |
| `resume` | 4 | RUNNING, version5; relay epoch advances |

Use `-action <action> -expected-version <number>` for each row; table values
are not positional arguments. Preparation additionally takes `-operation-id`.
Output is a JSON object containing `control` and, where applicable,
`resource_result`. Plans and receipts are immutable create-only files. The
existing-broker receipt has a distinct schema and embedded original plan; it
cannot impersonate a fresh receipt. The Billing reader accepts its trusted
Q+1 binding, while the original fresh checker still rejects this origin.

## Partial success and unknown outcomes

1. Retain the original operation UUID, plans, hashes, files, process identity,
   and logs. Keep the control FENCED. Query `status`, the SQL audit and the
   matching `inspect-*` action before deciding how to continue. No inspector
   changes a consumer or appends a success audit.
2. After a lost CREATE/UPDATE response, this invocation attempts INFO only. It
   never automatically repeats that mutation. If readback is unavailable or the
   process stopped, the result remains unknown until independent readback.
3. If all visible facts match the original plan and only known effects remain,
   explicitly invoke `upgrade-token-consumers -resume` or
   `initialize-consumers -resume`. These update only still-legacy Token
   instances or create only missing planned resources. They do not recapture Q
   or adopt replacement instances of the original five. Any other drift stops
   the operation.
4. If a plan file was written but its SQL audit did not commit, independently
   approve that original file's SHA and invoke the **same prepare action** with
   `-resume`, omitting `-operation-id`. The complete original pre-effect state
   must still match before the audit can be recorded. The file is not rewritten.
5. If the original receipt is complete but its audit did not commit, project its
   independently verified SHA and invoke `initialize-consumers -resume`. All
   resources and the original receipt must match; this recovery performs no
   broker mutations and never overwrites the receipt. Retain incomplete or
   invalid files. A separately reviewed new receipt output path may be used
   with the **same plan** when no complete trusted receipt exists.
6. If activate/resume has an unknown SQL result, use `status` and audit readback.
   Do not retry a stale CAS or initialize again. A successful control transition
   is not, by itself, complete deployment acceptance.

The operation still requires deployment-specific checks of actual broker ACLs,
the dedicated approval relay, Billing runtime files/role, Identity's exact
`internal.auth.password-preparation.claim` workload permission, source/device
continuity, and service readiness. Preserve existing rules and credentials;
adding proxy trust is a separate decision. Full release acceptance also checks
the database ledgers, governance and credential boundaries.

## Isolated acceptance

Point `PINVOU_TEST_SCHEMA_DATABASE_URL` at an explicitly owned, disposable
PostgreSQL instance with **no pre-existing deployment actors**, and
`PINVOU_TEST_NATS_SERVER_BINARY` at the approved local NATS executable. The
tests create their own TLS NATS listeners, identities, stores, UUID databases
and real migration/runtime roles. They never accept a shared broker URL.

Run `npm --prefix account-platform run test:existing-broker`. This builds the
actual operator and requires the named SDK, CLI, partial-state, lost real
server ACK, drift, fresh-path and exact workload-authority tests to execute and
pass; conditional skips do not count. The TLS transport fault fixture discards
one real server success reply; it covers both an interrupted owned CLI and the
uninterrupted CLI timeout, then verifies readback and original-plan recovery.
This proves fixture
behavior, not production deployment, live ACL reload, or proxy continuity.
