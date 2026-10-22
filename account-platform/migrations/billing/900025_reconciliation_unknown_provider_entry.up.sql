-- Preserve bounded original API bill kinds for UNSUPPORTED_PROVIDER_ENTRY.
-- No payment/refund/chargeback interpretation or file-import relaxation.
ALTER TABLE reconciliation_provider_rows
 DROP CONSTRAINT reconciliation_provider_rows_entry_type_check;
ALTER TABLE reconciliation_provider_rows
 ADD CONSTRAINT reconciliation_provider_rows_entry_type_check
 CHECK (octet_length(entry_type) BETWEEN 1 AND 64);
