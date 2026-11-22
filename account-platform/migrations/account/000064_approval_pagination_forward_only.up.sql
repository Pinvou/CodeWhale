DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000064 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=63) THEN
    RAISE EXCEPTION '000064 requires the approval pagination rollback fence' USING ERRCODE='55000';
  END IF;
END $$;

-- Retain the pagination schema across application N/N-1 rollback. No
-- multi-transaction DownOne sequence can atomically protect the gate and
-- all live cursors after the previous down transaction releases its locks.
