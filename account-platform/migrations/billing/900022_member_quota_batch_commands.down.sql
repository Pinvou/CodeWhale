DO $$ BEGIN
 RAISE EXCEPTION 'member quota batch facts are forward-only; preserve original retry identities' USING ERRCODE = '55000';
END $$;
