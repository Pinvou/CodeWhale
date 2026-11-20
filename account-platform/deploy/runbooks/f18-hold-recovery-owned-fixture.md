# F18 hold-recovery owned PostgreSQL fixture

The F18 hold-recovery integration test may run only against a disposable PostgreSQL instance whose ownership can be checked before any role or schema is created. A normal developer database, a shared CI service, and a remote URL are not valid substitutes.

## Current implementation and limits

`TestF18HoldRecoveryUsesTrustedFence` in `account-platform/internal/billing/f18_hold_recovery_integration_test.go` reads `PINVOU_TEST_BILLING_DATABASE_URL` and conditionally skips when that URL is unset. With a URL set and `F18_TEST_DATABASE_PROFILE` unset or empty, it retains the fixed legacy fixture identity (user, port and data directory) and attempts the configured connection. An unset profile does not cause a skip. Do not point it at a developer or shared database, or guess another fixture's identity to bypass the guard.

#2244 has implemented the explicit owned configuration. A nonempty profile selects `f18OwnedConnectionConfig` in `account-platform/internal/billing/f18_hold_recovery_config_test.go`. The exact value `local-owned` consumes `F18_TEST_DATABASE_USER`, `F18_TEST_DATABASE_PORT` and `F18_TEST_DATA_DIRECTORY`; it reuses `f17fixture.OwnedInstance` validation and connects with `pgx.ConnectConfig`. The CI profile below uses separate service admission. The default branch retains `pgx.Connect`. Unknown profiles, including whitespace-padded values, fail. All branches probe server identity and empty-instance state before creating billing roles.

The test records each created role's name and PostgreSQL OID. Its cleanup runs after database/pool cleanup, using a fresh ten-second context even if the scenario context failed or was cancelled. Before DROP ROLE it rechecks both session and current user, port, data directory, absence of other clients and business databases, exact recorded role identities, external memberships and cross-database dependencies. A mismatch refuses role deletion and fails the test; it does not kill clients or adopt a same-name replacement. Confirmed roles are dropped in one transaction. Repeating cleanup after confirmed absence is safe. An unrecorded OID or uncertain partial cleanup fails closed; the external owner must inspect and retire its disposable instance.

## Owned configuration and lifecycle acceptance

The following template documents the implemented explicit native owned configuration. Running it requires an external harness to create and own a disposable PostgreSQL instance with this identity and set all five fields. This local template does not provision that F18 instance. #1735 and #1979 track this configuration, guarded cleanup and real owned-PostgreSQL acceptance. The CI guard checks documented fields and prose and flags changes to the named test and configuration helper. Documentation guards do not validate a live PostgreSQL fixture or runtime acceptance.

```text
PINVOU_TEST_BILLING_DATABASE_URL=postgres://<owned-user>@127.0.0.1:<port>/postgres?sslmode=disable
F18_TEST_DATABASE_PROFILE=local-owned
F18_TEST_DATABASE_USER=<owned-user>
F18_TEST_DATABASE_PORT=<port>
F18_TEST_DATA_DIRECTORY=<temp>/pinvou-f18-billing-<uuid>/data
```

The URL host must be the literal `127.0.0.1`, with the `postgres` database, the declared user and port, and no TLS, fallback host, or runtime parameter. `<port>` must be canonical decimal in the range 1024–65535 and equal the URL port; the URL user must equal `F18_TEST_DATABASE_USER`. The password-free URL above is an example, not a claim that the Go validator rejects passwords. The server probe requires the same session user, port and canonical data directory, no non-system roles other than the declared fixture coordinator, no non-template databases other than `postgres`, and no other client sessions. The directory must be an absolute path directly below the operating system temporary directory and use the `pinvou-f18-billing-<uuid>/data` shape; generate a new lowercase, canonical, non-nil UUID for every fixture instance. The example sets `sslmode=disable` explicitly. The validator checks the parsed TLS and fallback settings, so `PGSSLMODE=disable` can also satisfy that requirement. Directory comparison uses cleaned paths without resolving symlinks; on macOS, the declared path and server path must use the same temporary-directory spelling (`/var` and `/private/var` are not interchangeable). The shared validator accepts canonical UUID versions other than v4; the documentation guard checks the literal `<uuid>` placeholder, not a replacement runtime UUID parser.

Run from `account-platform`, under the repository process supervisor with a unique absolute receipt path, after creating the exclusive instance and setting the five fields:

```sh
go test -race -tags=nomsgpack -mod=readonly -count=1 -timeout 4m -v -run '^TestF18(HoldRecoveryUsesTrustedFence|OwnedRoleCleanupPostgres)$' ./internal/billing
```

The original root retains its 150-second budget and all four financial cases: `nil-proof`, `unproven`, `timeout` and `controlled-trusted`. Only the controlled authenticated proof that actually releases the hold permits COMPLETED; the other cases retain ACTIVE, the reserved amount and manual review. `TestF18OwnedRoleCleanupPostgres` exercises normal retirement, partial creation, active clients, external membership, same-name replacement, directory mismatch, cancelled and expired contexts. Refused attempts must preserve role and membership snapshots; a later confirmed cleanup and repeated cleanup must succeed.

Before reporting native acceptance, run the original root against separate disposable instances with an unknown profile, mismatched URL user, mismatched data directory, an extra role and an extra database. Each must stop before DDL and preserve role/database snapshots. The external harness must verify that only its recorded resources remain, then stop its exact data directory. Never clear another process's sessions or sweep roles by prefix. A skip on an unset URL must not be counted as a PostgreSQL pass.

## Admitted CI service

The `ci-owned` profile uses the verified `owners` worker service. Under #1958, the existing parallel financial runner first restores its original owners fixtures, then invokes the additional family entry. Its F18 phase runs both roots before any retained Billing actors exist, with all named financial and cleanup cases required by complete Go JSON lifecycles. It reuses the worker's isolated PostgreSQL16 container; there is no seventh service, additional parallel worker or larger original context budget.

CI consumes `PINVOU_ACCOUNT_CI_PG_PORT`, `PINVOU_ACCOUNT_CI_PG_SYSTEM_ID` and `PINVOU_ACCOUNT_CI_PG_DIRECTORY` from the existing service admission, with `GITHUB_ACTIONS=true`. The client must be password-free `postgres@127.0.0.1:<admitted-port>/postgres` without TLS, fallback or runtime parameters. The server must separately report internal port 5432 and `/var/lib/postgresql/data`. `cifixture.ConnectConfig` verifies the actual system identifier, directory, internal port, database, superuser and both session/current principals on the same backend before F18 DDL. Role cleanup repeats `cifixture.Verify` on that same connection before checking recorded role OIDs and dependencies. Wrong or missing metadata fails; the CI profile never falls back to the legacy or native configuration. The native identity fields above are cleared by the CI runner and cannot override service admission.

The CI profile cannot be used as native-harness evidence by manufacturing GitHub metadata. Native execution still uses the explicit temporary-directory profile. Hosted Linux execution, its exact commit and applicable main combination must be recorded separately before claiming continuous CI acceptance.

These roots have been executed with PostgreSQL 17 on a fresh local-owned Windows instance, including the four financial cases, cleanup scenarios and pre-DDL refusal matrix. This establishes the exercised local database boundary only. Linux/macOS runtime execution remains separate; documentation checks do not establish that boundary and do not replace issuer, NATS or production funds end-to-end acceptance.
