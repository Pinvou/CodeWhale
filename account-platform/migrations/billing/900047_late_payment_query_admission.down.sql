DO $$ BEGIN
 RAISE EXCEPTION 'late payment query admission facts are forward-only';
END $$;
