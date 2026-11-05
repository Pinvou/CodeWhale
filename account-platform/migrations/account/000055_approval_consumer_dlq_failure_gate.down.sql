DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000055 down requires account_migrator with MIGRATION_ROLE account_service' USING ERRCODE='42501';
  END IF;

  RAISE EXCEPTION '000055 approval consumer DLQ failure gate is forward-only; database down is forbidden'
    USING ERRCODE='55000';
END $$;
