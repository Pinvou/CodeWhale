-- Policy facts are immutable, including when no approval currently references them.
-- Operational rollback disables the consumer and activation; it never erases policy history.
DO $$ BEGIN
    RAISE EXCEPTION 'entitlement component adjustment policy v2 is forward-only' USING ERRCODE = '55000';
END $$;
