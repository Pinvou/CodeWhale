# F18 hold-recovery owned PostgreSQL fixture

The F18 hold-recovery integration test may run only against a disposable PostgreSQL instance whose ownership can be checked before any role or schema is created. A normal developer database, a shared CI service, and a remote URL are not valid substitutes.

## Current implementation and limits

`TestF18HoldRecoveryUsesTrustedFence` in `account-platform/internal/billing/f18_hold_recovery_integration_test.go` reads `PINVOU_TEST_BILLING_DATABASE_URL` and conditionally skips when that URL is unset. With a URL set and `F18_TEST_DATABASE_PROFILE` unset or empty, it retains the fixed legacy fixture identity (user, port and data directory) and attempts the configured connection. An unset profile does not cause a skip. Do not point it at a developer or shared database, or guess another fixture's identity to bypass the guard.

#2244 has implemented the explicit owned configuration. A nonempty profile selects `f18OwnedConnectionConfig` in `account-platform/internal/billing/f18_hold_recovery_config_test.go`, which requires the exact value `local-owned` and consumes `F18_TEST_DATABASE_USER`, `F18_TEST_DATABASE_PORT` and `F18_TEST_DATA_DIRECTORY`. It reuses `f17fixture.OwnedInstance` validation and connects with `pgx.ConnectConfig`. The default branch retains `pgx.Connect`. Unknown profiles, including whitespace-padded values, fail. Both branches probe server identity and empty-instance state before creating billing roles. The current cleanup registers the fixed billing role names after the identity check and attempts to drop those roles. Verification that no owned client sessions remain before DROP ROLE is still a target requirement.

## Owned configuration and pending lifecycle acceptance

The following template documents the implemented explicit owned configuration. Running it requires an external harness to create and own a disposable PostgreSQL instance with this identity and set all five fields. This repository does not provision that F18 instance. #1735 and #1979 track the remaining lifecycle implementation and real owned-PostgreSQL acceptance. Configuration support does not establish the complete cleanup contract below. The CI guard checks documented fields and prose and flags changes to the named test and configuration helper. It does not validate all runtime configurations, a live PostgreSQL fixture or runtime acceptance.

```text
PINVOU_TEST_BILLING_DATABASE_URL=postgres://<owned-user>@127.0.0.1:<port>/postgres?sslmode=disable
F18_TEST_DATABASE_PROFILE=local-owned
F18_TEST_DATABASE_USER=<owned-user>
F18_TEST_DATABASE_PORT=<port>
F18_TEST_DATA_DIRECTORY=<temp>/pinvou-f18-billing-<uuid>/data
```

The URL host must be the literal `127.0.0.1`, with the `postgres` database, the declared user and port, and no TLS, fallback host, or runtime parameter. `<port>` must be canonical decimal in the range 1024–65535 and equal the URL port; the URL user must equal `F18_TEST_DATABASE_USER`. The password-free URL above is an example, not a claim that the Go validator rejects passwords. The server probe requires the same session user, port and canonical data directory, no non-system roles other than the declared fixture coordinator, no non-template databases other than `postgres`, and no other client sessions. The directory must be an absolute path directly below the operating system temporary directory and use the `pinvou-f18-billing-<uuid>/data` shape; generate a new lowercase, canonical, non-nil UUID for every fixture instance. The example sets `sslmode=disable` explicitly. The validator checks the parsed TLS and fallback settings, so `PGSSLMODE=disable` can also satisfy that requirement. Directory comparison uses cleaned paths without resolving symlinks; on macOS, the declared path and server path must use the same temporary-directory spelling (`/var` and `/private/var` are not interchangeable). The shared validator accepts canonical UUID versions other than v4; the documentation guard checks the literal `<uuid>` placeholder, not a replacement runtime UUID parser.

The target lifecycle must create only its own billing roles and record their names before cleanup. Cleanup must handle success, failure, timeout and cancellation, dropping only those roles after verifying that no owned client sessions remain. If any identity, emptiness or ownership check fails, it must stop before DDL. A skip on an unset URL must not be counted as a PostgreSQL pass.

Once the remaining lifecycle is implemented and executed, this contract would verify the local database boundary only. Documentation checks do not establish that boundary and do not replace issuer, NATS or production funds end-to-end acceptance.
