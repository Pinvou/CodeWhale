-- Durable Token cleanup acknowledgement is required before anonymization.
ALTER TABLE account_closure_requests
    ADD COLUMN token_replays_cleared_at TIMESTAMPTZ;
