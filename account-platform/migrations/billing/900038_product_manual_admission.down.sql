DO $$ BEGIN RAISE EXCEPTION 'manual admission facts cannot be rolled back; restore a compatible application version'; END $$;
