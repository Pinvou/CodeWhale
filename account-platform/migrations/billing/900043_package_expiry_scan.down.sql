-- Keep the installed identity and financial history; operational recovery uses
-- the existing forward-repair contract rather than downgrading the schema.
DO $$ BEGIN
 RAISE EXCEPTION 'package expiry scan index is forward-only' USING ERRCODE='55000';
END $$;
