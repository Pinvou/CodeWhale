# Historical Account 034 rollback

Account's public `DownOne` preserves the frozen 034 SQL files, version and ledger
checksums. An execution hook accepts only the exact frozen Up/Down identity and
the existing LF or CRLF applied Up digest. It repeats the original retired-cache
cleanup immediately after the original binding invalidation loop, before the
two-scope constraint is restored. This removes UNIT cache entries recreated by
that loop while retaining its ORGANIZATION and PLATFORM invalidations.

UNIT_ADMIN assignments, users, organizations and other historical ledger rows
are preserved. Original removal of ACCOUNT_LIFECYCLE PERSONAL_USER assignments
is unchanged. This is not a general downgrade guarantee: an ORG_MODEL_ADMIN
binding still prevents the original role deletion through its foreign key, and
the transaction preserves all data on failure. No role binding is deleted to
bypass that blocker. The separate 032 and 039 rollback policies are unchanged.

The required PostgreSQL public-history test runs the original generic 1..34 Up
and its real ledger before calling Account `DownOne` with the complete current
package. Its `three_scope_public_down` LF/CRLF cases are nested in the existing
CI-selected `TestAuditExportPublicDownRetainsOriginalHistory`. They assert the
original SQL's UNIT constraint failure, exact identity rejection without state
changes, preserved ORG_MODEL_ADMIN foreign-key rejection, and successful adapted
rollback with original data, cache invalidation and ledger verification.
