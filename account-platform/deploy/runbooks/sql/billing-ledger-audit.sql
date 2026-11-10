\set ON_ERROR_STOP on
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
-- Explicit pg_temp position prevents a reused psql session shadowing public.
SET LOCAL search_path = pg_catalog, public, pg_temp;
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '5s';

\if :{?audit_database}
SET LOCAL TIME ZONE 'UTC';
SELECT 1 / CASE WHEN current_database() = :'audit_database'
  AND NOT row_security_active('public.schema_migrations'::regclass)
  AND EXISTS (SELECT FROM public.schema_migrations WHERE version = 2 AND name = 'ledger')
  THEN 1 ELSE 0 END AS audit_database_guard;
SELECT 'billing' AS audit_domain, current_database() AS audit_database, transaction_timestamp() AS captured_at;
\endif

-- Fail closed before counting when this session cannot see every audited row.
-- SELECT-only privileges do not bypass row security; hidden rows could fake zero.
SELECT 1 / CASE WHEN count(*) = 17 AND bool_and(
  has_table_privilege(current_user, relation, 'SELECT') AND NOT row_security_active(relation)
) THEN 1 ELSE 0 END AS audit_visibility_guard
FROM unnest(ARRAY['public.ledger_postings','public.ledger_accounts','public.wallets','public.usage_holds','public.refunds','public.payment_orders','public.receivables','public.receivable_allocations','public.chargeback_cases','public.payment_transactions','public.entitlement_packages','public.entitlement_components','public.credit_lots','public.usage_hold_allocations','public.usage_hold_lease_events','public.member_quota_grants','public.member_quota_hold_allocations']::regclass[]) AS required(relation);



SELECT 'unbalanced_journal_entries' AS check_name, count(*) AS violations
FROM (
  SELECT journal_entry_id FROM ledger_postings
  GROUP BY journal_entry_id HAVING sum(delta_microcredits) <> 0
) violations;

SELECT 'wallet_posted_snapshot_mismatch' AS check_name, count(*) AS violations
FROM wallets wallet
LEFT JOIN (
  SELECT account.wallet_id, sum(posting.delta_microcredits) AS actual
  FROM ledger_accounts account
  JOIN ledger_postings posting ON posting.ledger_account_id=account.id
  WHERE account.wallet_id IS NOT NULL
  GROUP BY account.wallet_id
) ledger ON ledger.wallet_id=wallet.id
WHERE wallet.posted_microcredits <> COALESCE(ledger.actual,0);

SELECT 'wallet_reserved_snapshot_mismatch' AS check_name, count(*) AS violations
FROM wallets wallet
LEFT JOIN (
  SELECT wallet_id,sum(amount) AS actual FROM (
    SELECT wallet_id,max_microcredits AS amount
    FROM usage_holds WHERE status IN ('ACTIVE','REVIEW')
    UNION ALL
    SELECT payment.wallet_id,refund.credit_microcredits AS amount
    FROM refunds refund JOIN payment_orders payment ON payment.id=refund.payment_order_id
    WHERE refund.status IN ('REQUESTED','MANUAL_REVIEW')
      -- Migration 27 preserves legacy wallet reservations. New targeted
      -- refunds freeze entitlement components without reserving the wallet.
      AND refund.entitlement_snapshot->>'migration_source'='LEGACY_ORDER_REFUND'
  ) reservations GROUP BY wallet_id
) holds ON holds.wallet_id=wallet.id
WHERE wallet.reserved_microcredits <> COALESCE(holds.actual,0);

SELECT 'wallet_receivable_snapshot_mismatch' AS check_name, count(*) AS violations
FROM wallets wallet
LEFT JOIN (
  SELECT wallet_id,sum(outstanding_microcredits) AS actual
  FROM receivables WHERE outstanding_microcredits>0 GROUP BY wallet_id
) receivable ON receivable.wallet_id=wallet.id
WHERE wallet.receivable_microcredits <> COALESCE(receivable.actual,0)
   OR (wallet.financial_status='RECEIVABLE_OPEN') <> (COALESCE(receivable.actual,0)>0);

SELECT 'receivable_amount_mismatch' AS check_name, count(*) AS violations
FROM receivables
WHERE original_microcredits <> settled_microcredits + outstanding_microcredits
   OR (status IN ('SETTLED','WRITTEN_OFF')) <> (outstanding_microcredits=0);

SELECT 'receivable_allocation_mismatch' AS check_name, count(*) AS violations
FROM receivables receivable
LEFT JOIN (
  SELECT receivable_id,sum(amount_microcredits) AS actual
  FROM receivable_allocations GROUP BY receivable_id
) allocation ON allocation.receivable_id=receivable.id
WHERE receivable.settled_microcredits <> COALESCE(allocation.actual,0);

SELECT 'chargeback_package_mismatch' AS check_name, count(*) AS violations
FROM chargeback_cases chargeback
JOIN payment_transactions transaction ON transaction.id=chargeback.payment_transaction_id
JOIN entitlement_packages package ON package.id=chargeback.entitlement_package_id
WHERE transaction.payment_order_id<>chargeback.payment_order_id
   OR package.source_payment_transaction_id<>chargeback.payment_transaction_id
   OR package.wallet_id<>chargeback.wallet_id;

SELECT 'entitlement_component_conservation_mismatch' AS check_name, count(*) AS violations
FROM entitlement_components
WHERE original_microcredits <>
  available_microcredits + reserved_microcredits + used_microcredits +
  active_grant_microcredits + suspended_grant_microcredits +
  expired_microcredits + revoked_microcredits + reclassified_out_microcredits;

SELECT 'entitlement_component_lot_mismatch' AS check_name, count(*) AS violations
FROM entitlement_components component
JOIN credit_lots lot ON lot.id=component.credit_lot_id
WHERE lot.remaining_microcredits <>
  component.available_microcredits + component.reserved_microcredits +
  component.active_grant_microcredits + component.suspended_grant_microcredits;

SELECT 'entitlement_package_projection_mismatch' AS check_name, count(*) AS violations
FROM entitlement_packages package
LEFT JOIN (
  SELECT package_id,sum(original_microcredits-reclassified_out_microcredits) total,
    sum(available_microcredits+active_grant_microcredits+suspended_grant_microcredits) remaining,
    sum(reserved_microcredits) reserved,sum(used_microcredits) used,
    sum(expired_microcredits) expired,sum(revoked_microcredits) revoked
  FROM entitlement_components GROUP BY package_id
) component ON component.package_id=package.id
WHERE (package.points_total,package.points_remaining,package.points_reserved,package.points_used,
       package.points_expired,package.points_revoked) <>
      (COALESCE(component.total,0),COALESCE(component.remaining,0),
       COALESCE(component.reserved,0),COALESCE(component.used,0),
       COALESCE(component.expired,0),COALESCE(component.revoked,0));

SELECT 'entitlement_hold_allocation_mismatch' AS check_name, count(*) AS violations
FROM entitlement_components component
LEFT JOIN (
  SELECT allocation.component_id,
    sum(allocation.reserved_microcredits-allocation.settled_microcredits-allocation.released_microcredits) reserved
  FROM usage_hold_allocations allocation
  JOIN usage_holds hold ON hold.id=allocation.hold_id
  WHERE hold.status IN ('ACTIVE','REVIEW')
  GROUP BY allocation.component_id
) allocation ON allocation.component_id=component.id
WHERE component.reserved_microcredits <> COALESCE(allocation.reserved,0);

SELECT 'expired_active_holds' AS check_name, count(*) AS violations
FROM usage_holds WHERE status='ACTIVE' AND expires_at <= now();

SELECT 'usage_hold_allocation_mismatch' AS check_name, count(*) AS violations
FROM usage_holds hold
LEFT JOIN (
  SELECT hold_id,sum(reserved_microcredits-settled_microcredits-released_microcredits) outstanding
  FROM usage_hold_allocations GROUP BY hold_id
) allocation ON allocation.hold_id=hold.id
WHERE (hold.status IN ('ACTIVE','REVIEW') AND COALESCE(allocation.outstanding,0)<>hold.max_microcredits)
   OR (hold.status IN ('COMMITTED','RELEASED','EXPIRED') AND COALESCE(allocation.outstanding,0)<>0);

SELECT 'usage_hold_latest_lease_mismatch' AS check_name, count(*) AS violations
FROM usage_holds hold
LEFT JOIN LATERAL (
  SELECT expires_at,max_microcredits FROM usage_hold_lease_events event
  WHERE event.hold_id=hold.id ORDER BY event.created_at DESC,event.id DESC LIMIT 1
) latest ON true
WHERE latest.expires_at IS NULL
   OR (latest.expires_at,latest.max_microcredits)<>(hold.expires_at,hold.max_microcredits);

SELECT 'paid_orders_not_credited' AS check_name, count(*) AS violations
FROM payment_orders
WHERE status IN ('PAID','PAID_LATE') AND fulfillment_status <> 'CREDITED';

-- Only unreserved member availability lives in the component grant buckets.
SELECT 'member_grant_component_projection_mismatch' AS check_name, count(*) AS violations
FROM entitlement_components component
LEFT JOIN (
 SELECT component_id,
   COALESCE(sum(available_microcredits) FILTER (WHERE status='ACTIVE'),0) active,
   COALESCE(sum(available_microcredits) FILTER (WHERE status='SUSPENDED'),0) suspended
 FROM member_quota_grants GROUP BY component_id
) grants ON grants.component_id=component.id
WHERE component.active_grant_microcredits <> COALESCE(grants.active,0)
   OR component.suspended_grant_microcredits <> COALESCE(grants.suspended,0);

SELECT 'member_grant_reserved_projection_mismatch' AS check_name, count(*) AS violations
FROM member_quota_grants grant_row
LEFT JOIN (
 SELECT grant_id,sum(reserved_microcredits-settled_microcredits-released_microcredits) reserved
 FROM member_quota_hold_allocations GROUP BY grant_id
) allocation ON allocation.grant_id=grant_row.id
WHERE grant_row.reserved_microcredits <> COALESCE(allocation.reserved,0);

SELECT 'member_hold_source_identity_mismatch' AS check_name, count(*) AS violations
FROM member_quota_hold_allocations member_allocation
JOIN member_quota_grants grant_row ON grant_row.id=member_allocation.grant_id
JOIN usage_holds hold ON hold.id=member_allocation.hold_id
JOIN entitlement_components component ON component.id=member_allocation.component_id
JOIN entitlement_packages package ON package.id=component.package_id
LEFT JOIN usage_hold_allocations allocation ON allocation.hold_id=hold.id
 AND allocation.component_id=component.id
WHERE grant_row.component_id<>component.id OR grant_row.package_id<>package.id
 OR grant_row.component_type<>component.component_type
 OR hold.wallet_id<>package.wallet_id
 OR hold.organization_id IS DISTINCT FROM grant_row.organization_id
 OR hold.member_user_id IS DISTINCT FROM grant_row.member_user_id
 OR allocation.component_id IS NULL
 OR (SELECT COALESCE(sum(m.reserved_microcredits-m.settled_microcredits-m.released_microcredits),0)
     FROM member_quota_hold_allocations m WHERE m.hold_id=hold.id AND m.component_id=component.id)
    <> allocation.reserved_microcredits-allocation.settled_microcredits-allocation.released_microcredits;

ROLLBACK;
