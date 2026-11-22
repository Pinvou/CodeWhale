DO $$
BEGIN
  IF session_user<>'account_migrator' OR current_user<>'account_service' THEN
    RAISE EXCEPTION '000062 requires the isolated account migration identities' USING ERRCODE='42501';
  END IF;
  IF NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=61) THEN
    RAISE EXCEPTION '000062 requires approval pagination snapshots' USING ERRCODE='55000';
  END IF;
END $$;

-- A retired or replaced HMAC key must never be silently reintroduced under
-- the same kid. Pre-62 rows remain NULL and fail closed until their complete
-- 35-minute cursor/tombstone retention window drains.
ALTER TABLE approval_pagination_signing_keys
  ADD COLUMN key_digest TEXT CHECK(key_digest~'^[0-9a-f]{64}$');
