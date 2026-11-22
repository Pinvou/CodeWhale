-- Only live ciphertext participates in unsettled-content cleanup. Metadata
-- stubs retain their original identity but must not grow each periodic scan.
CREATE INDEX usage_stream_events_cleanup_expiry_idx
    ON usage_stream_events(expires_at) WHERE octet_length(encrypted_payload) > 0;
CREATE INDEX usage_stream_events_cleanup_created_idx
    ON usage_stream_events(created_at) WHERE octet_length(encrypted_payload) > 0;
CREATE INDEX usage_replay_results_cleanup_expiry_idx
    ON usage_replay_results(expires_at) WHERE octet_length(encrypted_payload) > 0;
CREATE INDEX usage_replay_results_cleanup_created_idx
    ON usage_replay_results(created_at) WHERE octet_length(encrypted_payload) > 0;
