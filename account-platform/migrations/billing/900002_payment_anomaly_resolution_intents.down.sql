DO $$
BEGIN
    RAISE EXCEPTION 'billing anomaly resolution intents are forward-only; preserve immutable decisions';
END $$;
