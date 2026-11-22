DO $$ BEGIN
 RAISE EXCEPTION 'late payment query generation bindings are forward-only';
END $$;
