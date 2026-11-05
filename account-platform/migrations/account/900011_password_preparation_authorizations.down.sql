DO $$ BEGIN RAISE EXCEPTION USING ERRCODE='55000', MESSAGE='FORWARD_ONLY: password preparation authorization facts must be retained'; END $$;
