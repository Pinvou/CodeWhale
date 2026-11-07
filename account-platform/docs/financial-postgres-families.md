# Continuous financial PostgreSQL families

Issue #1958 names 54 original test roots, including coordinator parents and
their child-process roots. They are test identities, not 54 independent
business scenarios. `scripts/account-financial-families.json` records their
reviewed case names and actual packages; the runner never learns a passing
contract from its own output at runtime.

Five roots remain mandatory in `scripts/test-token-financial-postgres.py`:
the original F06 review, quota-batch and expiry roots and the two Account
registration-eligibility roots. Their case contracts now also require complete
Go JSON lifecycles. Other original financial selections remain unchanged.

After that runner's `owners` group restores its fixtures,
`scripts/test-financial-parallel.py` must run
`scripts/test-account-financial-families-postgres.py` on the same admitted
service. The four existing workers and separate service identities remain;
the owners worker cannot succeed if either entry fails. The workflow contains
the complete process tree with `ci-fixture-process.py` and retains its receipt.

The additional entry has nine sequential phases:

| Phase | Original inputs and ownership |
| --- | --- |
| Account F06/F07 | Three roots create and retire their own absent Account actors. |
| Schema F07/F11/F12/F13 | Five parents own their absent actors; F11/F12/F13 invoke the original complete Billing package and keep three separate child logs. |
| F09 policy, prepare, replay, expiry, financial | Five separate coordinator invocations keep unique child streams. The original explicit test files remain; production files come from the declared Go toolchain's platform/tag selection. Child package identity is `command-line-arguments`. |
| Cross-domain repairs | Five Account roots receive both Account and Billing URLs. The runner supplies the three original Billing actors; Account actors remain owned by each root. |
| Retained Billing | Fifteen roots use the same three Billing actors and fresh leased UUID databases. |

Owner-only phases run before Billing actor creation. Creation records role OIDs
and memberships inside its transaction; subsequent checks bind the same
PostgreSQL system, directory, port and coordinator on the same backend as each
operation. Database/role identities, memberships, coordinator objects, clients
and outside dependencies must match before continuation or retirement. There
is no role-name sweep, backend termination or reset of a reused database.
Each coordinator `psql` operation has a 30-second process bound.
Unconfirmed identity or cleanup fails the entry and retains diagnostic facts;
the job's existing service owner handles the disposable instance boundary.

Parents and children use the declared Go binary with actual `-race`, existing
internal context budgets, `-mod=readonly` and the common `nomsgpack` build tag.
Each added parent invocation has a finite 20-minute Go package limit; original
test contexts and the existing job/worker budgets are preserved. Required roots
and every recorded child case must run and pass in the correct package. Missing,
skipped, failed, duplicated, out-of-order or truncated execution cannot be
replaced by a zero exit code or a parent PASS. Complete child logs are validated
separately from parent output.

For a local reproduction on Linux or Windows, use an external harness to create a fresh exclusive
PostgreSQL 16+ instance: only its coordinator role and the three original system
databases, no user objects and no other clients. Set
`PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL` to a password-free
`postgres://<owned-user>@127.0.0.1:<port>/postgres?sslmode=disable`, and declare
absolute `PINVOU_ACCOUNT_TEST_GO` and `PINVOU_ACCOUNT_TEST_PSQL` executables.
Do not set GitHub service metadata to pretend that a local instance is CI-owned.
From the repository checkout, run:

```sh
python3 scripts/ci-fixture-process.py --receipt "<new-absolute-process-receipt.json>" -- python3 scripts/test-account-financial-families-postgres.py
```

`PINVOU_FINANCIAL_CHILD_ARTIFACTS` may name an absolute, not-yet-existing evidence
directory with an existing parent. Otherwise a private temporary directory is
created. Each phase retains its command, Go flags, original exit code, parent
JSON, stderr and child streams. `source.json` records checkout head/tree,
working-tree dirtiness and the actual Go version; `resources.json` records the
admitted instance and before/after resource facts. The parallel worker keeps
these under its existing `financial-parallel-*` diagnostic directory.

The root/case manifest is a reviewed contract: adding or changing an original
case requires updating it and its selection/result regression tests. A local
run on a dirty checkout is diagnostic evidence. Final acceptance binds a fixed
head and the applicable main combination; remote CI, external issuer/NATS,
production funds, capacity and deployment acceptance remain separately owned.
