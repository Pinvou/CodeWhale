# F18 hold-recovery owned PostgreSQL fixture

The F18 hold-recovery integration test may run only against a disposable PostgreSQL instance whose ownership can be checked before any role or schema is created. A normal developer database, a shared CI service, and a remote URL are not valid substitutes.

Set these values in the process that launches the named test:

```text
PINVOU_TEST_BILLING_DATABASE_URL=postgres://<owned-user>@127.0.0.1:<port>/postgres?sslmode=disable
F18_TEST_DATABASE_PROFILE=local-owned
F18_TEST_DATABASE_USER=<owned-user>
F18_TEST_DATABASE_PORT=<port>
F18_TEST_DATA_DIRECTORY=<temp>/pinvou-f18-billing-<uuid>/data
```

The URL must resolve to loopback, the `postgres` database, the declared user and port, with no TLS, fallback host, or runtime parameter. The server probe must report the same session user, port and canonical data directory, no non-system roles, no non-template databases, and no other client sessions. The directory must be an absolute path directly below the operating system temporary directory and use the `pinvou-f18-billing-<uuid>/data` shape; generate a new UUID for every fixture instance.

The test creates only its own billing roles and records their names before cleanup. Cleanup runs on success, failure, timeout and cancellation and drops only those roles after verifying that no owned client sessions remain. If any identity, emptiness or ownership check fails, the test stops before DDL. An unset profile is reported as a conditional skip and must not be counted as a PostgreSQL pass.

This contract verifies the local database boundary only. It does not replace issuer, NATS or production funds end-to-end acceptance.
