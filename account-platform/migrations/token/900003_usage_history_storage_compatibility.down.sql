DO $$ BEGIN RAISE EXCEPTION 'Token history storage hardening is forward-only'; END $$;
