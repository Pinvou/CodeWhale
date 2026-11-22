DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000063 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=62) THEN
    RAISE EXCEPTION '000063 requires approval pagination key binding' USING ERRCODE='55000';
  END IF;
END $$;

-- This ledger-only forward migration fences the rollback path of 000062.
-- Older 000062 down must not remove key binding while live snapshot members
-- still exist, even if the first page did not issue a cursor/signing-key row.
