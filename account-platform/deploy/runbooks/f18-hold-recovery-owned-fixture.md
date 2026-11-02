# F18 hold-recovery owned PostgreSQL fixture

The F18 hold-recovery integration test may run only against a disposable PostgreSQL instance whose ownership can be checked before any role or schema is created. A normal developer database, a shared CI service, and a remote URL are not valid substitutes.

## Current implementation and limits

`TestF18HoldRecoveryUsesTrustedFence` in `account-platform/internal/billing/f18_hold_recovery_integration_test.go` currently reads only `PINVOU_TEST_BILLING_DATABASE_URL` for database configuration. It conditionally skips when that URL is unset. When set, it connects and probes a fixed legacy fixture identity (user, port and data directory) before creating billing roles; it does not implement the portable profile below. Do not point it at a developer or shared database, or guess another fixture's identity to bypass the guard.

The four `F18_TEST_*` identity inputs below are not consumed by the current named Go test. Setting them does not select an owned fixture, change its fixed identity, or cause a skip. The current cleanup registers the fixed billing role names after the identity check and attempts to drop those roles; portable UUID ownership checks and the complete cleanup contract below are target requirements, not verified current behavior.

## Target contract — not current runnable configuration

The following template documents the intended owned fixture contract. #1735 and #1979 track the remaining portable fixture implementation and real owned-PostgreSQL acceptance; this PR only pins its documentation. Until the named test consumes and validates these inputs and its lifecycle is verified, this template is not a launch recipe. The CI guard checks documented fields and prose, not a live PostgreSQL fixture or runtime acceptance.

```text
PINVOU_TEST_BILLING_DATABASE_URL=postgres://<owned-user>@127.0.0.1:<port>/postgres?sslmode=disable
F18_TEST_DATABASE_PROFILE=local-owned
F18_TEST_DATABASE_USER=<owned-user>
F18_TEST_DATABASE_PORT=<port>
F18_TEST_DATA_DIRECTORY=<temp>/pinvou-f18-billing-<uuid>/data
```

The URL must resolve to loopback, the `postgres` database, the declared user and port, with no TLS, fallback host, or runtime parameter. The server probe must report the same session user, port and canonical data directory, no non-system roles other than the declared fixture coordinator, no non-template databases other than `postgres`, and no other client sessions. The directory must be an absolute path directly below the operating system temporary directory and use the `pinvou-f18-billing-<uuid>/data` shape; generate a new UUID for every fixture instance.

The target implementation must create only its own billing roles and record their names before cleanup. Cleanup must handle success, failure, timeout and cancellation, dropping only those roles after verifying that no owned client sessions remain. If any identity, emptiness or ownership check fails, it must stop before DDL. In that future profile contract, an unset profile is reported as a conditional skip and must not be counted as a PostgreSQL pass; the current test instead skips on an unset URL as described above.

Once implemented and executed, this contract would verify the local database boundary only. Documentation checks do not establish that boundary and do not replace issuer, NATS or production funds end-to-end acceptance.
