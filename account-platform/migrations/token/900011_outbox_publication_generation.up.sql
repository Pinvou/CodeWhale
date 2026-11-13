-- Ordinary Token publication uses the shared generation-and-lease fence.
-- Retained domain events keep their original envelope and generation zero.
ALTER TABLE public.outbox_events
    ADD COLUMN publication_generation BIGINT NOT NULL DEFAULT 0
        CHECK (publication_generation >= 0);
