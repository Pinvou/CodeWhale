DROP TRIGGER usage_platform_loss_reviews_immutable ON usage_platform_loss_reviews;
DROP TRIGGER usage_reconciliation_commands_immutable ON usage_reconciliation_commands;
DROP TRIGGER usage_cancellation_evidence_immutable ON usage_cancellation_evidence;
DROP TRIGGER usage_cancel_commands_immutable ON usage_cancel_commands;
DROP FUNCTION reject_usage_command_or_evidence_mutation();
DROP INDEX usage_cancel_reconciliation_due_idx;
DROP TABLE usage_platform_loss_reviews;
DROP TABLE usage_reconciliation_commands;
DROP TABLE usage_delivery_confirmations;
DROP TABLE usage_cancellation_evidence;
DROP TABLE usage_cancel_commands;
ALTER TABLE usage_requests
    DROP COLUMN cancel_reconciliation_seconds,
    DROP COLUMN final_usage_delay_seconds,
    DROP COLUMN maximum_execution_seconds,
    DROP COLUMN convergence_policy_version,
    DROP COLUMN cancel_reconciliation_deadline,
    DROP COLUMN trusted_usage_source,
    DROP COLUMN trusted_usage_snapshot,
    DROP COLUMN last_client_confirmed_sequence,
    DROP COLUMN last_persisted_sequence,
    DROP COLUMN user_stopped_at,
    DROP COLUMN final_usage_observed_at,
    DROP COLUMN provider_terminal_at,
    DROP COLUMN provider_cancel_confirmed_at,
    DROP COLUMN provider_cancel_sent_at,
    DROP COLUMN cancel_requested_at,
    DROP COLUMN settlement_status,
    DROP COLUMN cancel_status;
ALTER TABLE usage_requests DROP CONSTRAINT usage_requests_execution_status_check;
ALTER TABLE usage_requests ADD CONSTRAINT usage_requests_execution_status_check CHECK (execution_status IN (
    'RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING',
    'STREAMING','SETTLEMENT_PENDING','SUCCEEDED','FAILED','REVIEW'
));
