-- System ledger accounts are platform facts, not user-owned debug data. Restore
-- any rows removed by an out-of-band cleanup before billing workers start.
INSERT INTO ledger_accounts(id,code,account_type,created_at) VALUES
('00000000-0000-7000-8000-000000000001','platform:payment-clearing','PAYMENT_CLEARING',now()),
('00000000-0000-7000-8000-000000000002','platform:usage-revenue','USAGE_REVENUE',now()),
('00000000-0000-7000-8000-000000000003','platform:refund-clearing','REFUND_CLEARING',now()),
('00000000-0000-7000-8000-000000000004','platform:adjustment','ADJUSTMENT',now()),
('00000000-0000-7000-8000-000000000005','platform:receivables','RECEIVABLE_ASSET',now()),
('00000000-0000-7000-8000-000000000006','platform:chargeback-clearing','CHARGEBACK_CLEARING',now())
ON CONFLICT (id) DO NOTHING;

DO $$
BEGIN
    IF (SELECT count(*) FROM ledger_accounts WHERE
        (id='00000000-0000-7000-8000-000000000001' AND wallet_id IS NULL AND code='platform:payment-clearing' AND account_type='PAYMENT_CLEARING') OR
        (id='00000000-0000-7000-8000-000000000002' AND wallet_id IS NULL AND code='platform:usage-revenue' AND account_type='USAGE_REVENUE') OR
        (id='00000000-0000-7000-8000-000000000003' AND wallet_id IS NULL AND code='platform:refund-clearing' AND account_type='REFUND_CLEARING') OR
        (id='00000000-0000-7000-8000-000000000004' AND wallet_id IS NULL AND code='platform:adjustment' AND account_type='ADJUSTMENT') OR
        (id='00000000-0000-7000-8000-000000000005' AND wallet_id IS NULL AND code='platform:receivables' AND account_type='RECEIVABLE_ASSET') OR
        (id='00000000-0000-7000-8000-000000000006' AND wallet_id IS NULL AND code='platform:chargeback-clearing' AND account_type='CHARGEBACK_CLEARING')) <> 6 THEN
        RAISE EXCEPTION 'billing system ledger account invariant failed';
    END IF;
END $$;

ALTER TABLE payment_notifications
    ADD COLUMN processing_attempts INTEGER NOT NULL DEFAULT 0 CHECK (processing_attempts >= 0),
    ADD COLUMN last_attempt_at TIMESTAMPTZ,
    ADD COLUMN next_attempt_at TIMESTAMPTZ,
    ADD COLUMN last_error_code TEXT CHECK (last_error_code IS NULL OR length(last_error_code) BETWEEN 1 AND 160);

CREATE INDEX payment_notifications_retry_idx
    ON payment_notifications(next_attempt_at,received_at,notification_id)
    WHERE result='RECEIVED';
