# Protocol administration read errors

The authenticated `/admin/v1/protocols` and
`/admin/v1/formulas/{formula_version_id}/preview` routes require
`platform.pricing.read`.

Formula preview rejects a malformed UUID with HTTP 400 and `INVALID_REQUEST`,
using the same identifier validation as other Token administration reads. If
the repository lacks the optional aggregate-list or formula-preview capability,
the corresponding read returns HTTP 503 and `ADMIN_UNAVAILABLE`. These failures
use the standard JSON error envelope, request correlation and `no-store` policy.
Repository capability checks retain their existing precedence over UUID parsing.

The production PostgreSQL repository provides both capabilities. This fallback
error contract does not imply a normal production outage. Successful responses,
backend error mapping and protocol pagination behavior are unchanged.

`TestProtocolAdminRegisteredHTTPErrorResponses` exercises the registered management
HTTP routes with controlled authorization HTTP and repository I/O. It covers
malformed identifiers, missing capabilities, successful reads, backend failures,
missing resources, and authentication/permission rejection. It does not exercise
PostgreSQL effects or deployment infrastructure.
