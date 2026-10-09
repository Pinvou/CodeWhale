-- The retained relay fences completion by generation and lease identity.
-- Existing events retain generation zero; every completion also matches the lease.
ALTER TABLE outbox_events
    ADD COLUMN publication_generation BIGINT NOT NULL DEFAULT 0
        CHECK (publication_generation >= 0);
