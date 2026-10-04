\set ON_ERROR_STOP on
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;

SELECT 'active_credential_count_mismatch' AS check_name,count(*) AS violations
FROM (
  SELECT token_account_id FROM provider_credentials
  WHERE status='ACTIVE' GROUP BY token_account_id HAVING count(*) > 1
) violations;

SELECT 'committed_usage_missing_facts' AS check_name,count(*) AS violations
FROM usage_requests
WHERE status='COMMITTED' AND (actual_microcredits IS NULL OR terminal_at IS NULL OR hold_id IS NULL OR price_version_id IS NULL);

SELECT 'terminal_usage_without_terminal_time' AS check_name,count(*) AS violations
FROM usage_requests
WHERE status IN ('COMMITTED','RELEASED') AND terminal_at IS NULL;

SELECT 'active_token_without_active_credential' AS check_name,count(*) AS violations
FROM token_accounts account
WHERE account.status='ACTIVE' AND NOT EXISTS (
  SELECT 1 FROM provider_credentials credential
  WHERE credential.token_account_id=account.id AND credential.status='ACTIVE'
);

ROLLBACK;
