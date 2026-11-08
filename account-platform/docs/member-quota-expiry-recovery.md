# Member quota restoration at package expiry

When a removed member rejoins after an entitlement deadline, positive remaining
quota uses the ordinary whole-package expiry transaction. The wallet lock comes
before the package expiry key in both lifecycle projection and the worker. The
transaction checks the package's actual wallet, status and deadline, releases
only the existing `ACTIVE` usage holds, and applies the original balanced
`EXPIRY` journal, credit-lot decrement and wallet decrement. Cumulative
`expired_microcredits` is evidence of past transitions, not a new amount to burn.

`REVIEW` reservations remain reserved. A package with an outstanding review
allocation is deferred by the worker so another due package can proceed. A
restoration encountering reserved capacity fails atomically; it does not release
review funds. Unknown reservation inconsistencies still fail instead of being
silently skipped. A grant deadline alone does not authorize expiry of a package
whose deadline has not arrived, or whose status is frozen.

## Historical-data limitation and read-only diagnosis

Older restoration code could move quota to an expired bucket without reducing
the credit lot and posted wallet balance. A later worker could mark the package
`CONSUMED`, or write a partial expiry and mark it `EXPIRED`. The runtime correction
does not rewrite these terminal packages, replace immutable journals, refund
users, or automatically charge any historical residual a second time.

Run the following only through an authorized read-only connection. It lists
suspected mismatches for investigation, including active packages not yet seen
by the old worker; it does not establish an approved repair amount:

```sql
BEGIN READ ONLY;
SELECT package.id AS package_id, package.status AS package_status,
       component.id AS component_id, component.status AS component_status,
       component.credit_lot_id,
       lot.remaining_microcredits,
       component.available_microcredits::numeric + component.active_grant_microcredits
         + component.suspended_grant_microcredits
         + component.reserved_microcredits AS current_backed_capacity,
       component.expired_microcredits,
       journal.id AS existing_expiry_journal_id,
       EXISTS (SELECT 1 FROM member_quota_grants quota
               WHERE quota.component_id = component.id
                 AND quota.status = 'SUSPENDED') AS has_suspended_grant
FROM entitlement_components component
JOIN entitlement_packages package ON package.id = component.package_id
JOIN credit_lots lot ON lot.id = component.credit_lot_id
LEFT JOIN journal_entries journal
  ON journal.business_reference = 'expire:' || package.id::text
WHERE component.expired_microcredits > 0
  AND lot.remaining_microcredits > component.available_microcredits::numeric
        + component.active_grant_microcredits
        + component.suspended_grant_microcredits
        + component.reserved_microcredits
ORDER BY package.id, component.id;

SELECT package.id AS package_id, package.status AS package_status,
       quota.id AS grant_id, quota.available_microcredits,
       quota.reserved_microcredits
FROM member_quota_grants quota
JOIN entitlement_packages package ON package.id = quota.package_id
WHERE package.status IN ('EXPIRED', 'CONSUMED')
  AND quota.status = 'SUSPENDED'
  AND (quota.available_microcredits > 0 OR quota.reserved_microcredits > 0)
ORDER BY package.id, quota.id;
ROLLBACK;
```

Preserve the existing package, grant, lot and journal identities. Before any
separate data-repair decision, verify the unique component-to-lot relationship,
all recharge/usage/refund/expiry postings, live and review allocations, wallet
totals, ownership, and financial-review evidence. Existing partial expiry entries
must remain immutable. A positive diagnostic result remains a data-repair and
release limitation until those facts and an authorized correction are established;
test databases do not prove that deployed databases are clean.
