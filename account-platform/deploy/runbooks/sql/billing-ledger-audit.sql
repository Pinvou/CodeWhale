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
  SELECT wallet_id,sum(amount) AS actual FROM (
    SELECT wallet_id,max_microcredits AS amount
    FROM usage_holds WHERE status IN ('ACTIVE','REVIEW')
    UNION ALL
    SELECT payment.wallet_id,refund.credit_microcredits AS amount
    FROM refunds refund JOIN payment_orders payment ON payment.id=refund.payment_order_id
    WHERE refund.status IN ('REQUESTED','MANUAL_REVIEW')
  ) reservations GROUP BY wallet_id
) holds ON holds.wallet_id=wallet.id
WHERE wallet.reserved_microcredits <> COALESCE(holds.actual,0);

SELECT 'expired_active_holds' AS check_name, count(*) AS violations
FROM usage_holds WHERE status='ACTIVE' AND expires_at <= now();

SELECT 'paid_orders_not_credited' AS check_name, count(*) AS violations
FROM payment_orders
WHERE status IN ('PAID','PAID_LATE') AND fulfillment_status <> 'CREDITED';

ROLLBACK;
