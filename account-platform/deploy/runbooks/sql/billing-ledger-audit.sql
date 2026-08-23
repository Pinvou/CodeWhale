\set ON_ERROR_STOP on
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;

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
  SELECT wallet_id,sum(max_microcredits) AS actual
  FROM usage_holds WHERE status='ACTIVE' GROUP BY wallet_id
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
   OR (status='SETTLED') <> (outstanding_microcredits=0);

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
   OR package.payment_transaction_id<>chargeback.payment_transaction_id
   OR package.wallet_id<>chargeback.wallet_id;

SELECT 'entitlement_component_conservation_mismatch' AS check_name, count(*) AS violations
FROM entitlement_components
WHERE original_microcredits <>
  available_microcredits + reserved_microcredits + used_microcredits +
  active_grant_microcredits + suspended_grant_microcredits +
  expired_microcredits + revoked_microcredits;

SELECT 'entitlement_component_lot_mismatch' AS check_name, count(*) AS violations
FROM entitlement_components component
JOIN credit_lots lot ON lot.id=component.credit_lot_id
WHERE lot.remaining_microcredits <>
  component.available_microcredits + component.reserved_microcredits +
  component.active_grant_microcredits + component.suspended_grant_microcredits;

SELECT 'entitlement_package_projection_mismatch' AS check_name, count(*) AS violations
FROM entitlement_packages package
JOIN (
  SELECT package_id,sum(original_microcredits) total,
    sum(available_microcredits+active_grant_microcredits+suspended_grant_microcredits) remaining,
    sum(reserved_microcredits) reserved,sum(used_microcredits) used,
    sum(expired_microcredits) expired,sum(revoked_microcredits) revoked
  FROM entitlement_components GROUP BY package_id
) component ON component.package_id=package.id
WHERE (package.points_total,package.points_remaining,package.points_reserved,package.points_used,
       package.points_expired,package.points_revoked) <>
      (component.total,component.remaining,component.reserved,component.used,
       component.expired,component.revoked);

SELECT 'entitlement_hold_allocation_mismatch' AS check_name, count(*) AS violations
FROM entitlement_components component
LEFT JOIN (
  SELECT allocation.component_id,
    sum(allocation.reserved_microcredits-allocation.settled_microcredits-allocation.released_microcredits) reserved
  FROM usage_hold_allocations allocation
  JOIN usage_holds hold ON hold.id=allocation.hold_id
  WHERE hold.status='ACTIVE'
  GROUP BY allocation.component_id
) allocation ON allocation.component_id=component.id
WHERE component.reserved_microcredits <> COALESCE(allocation.reserved,0);

SELECT 'expired_active_holds' AS check_name, count(*) AS violations
FROM usage_holds WHERE status='ACTIVE' AND expires_at <= now();

SELECT 'paid_orders_not_credited' AS check_name, count(*) AS violations
FROM payment_orders
WHERE status IN ('PAID','PAID_LATE') AND fulfillment_status <> 'CREDITED';

ROLLBACK;
