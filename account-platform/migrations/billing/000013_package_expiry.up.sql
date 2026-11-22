ALTER TABLE journal_entries DROP CONSTRAINT journal_entries_entry_type_check;
ALTER TABLE journal_entries ADD CONSTRAINT journal_entries_entry_type_check
    CHECK (entry_type IN ('RECHARGE', 'USAGE', 'REFUND', 'REVERSAL', 'ADJUSTMENT', 'EXPIRE', 'REPAIR_CREDIT'));
