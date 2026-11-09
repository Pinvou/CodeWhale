-- Stop and drain Account profile workers and Identity profile calls first.
-- Receipts contain no copied subject, mobile number or provider payload.
CREATE TABLE public.profile_session_revocation_receipts (
    operation_id uuid PRIMARY KEY REFERENCES public.profile_change_requests(id),
    state text NOT NULL CHECK (state IN ('RESERVED','DISPATCHED','CONFIRMED','UNKNOWN')),
    dispatch_token_hash bytea,
    created_at timestamptz NOT NULL,
    dispatched_at timestamptz,
    confirmed_at timestamptz,
    CHECK ((state='RESERVED' AND dispatch_token_hash IS NULL AND dispatched_at IS NULL AND confirmed_at IS NULL)
        OR (state='DISPATCHED' AND dispatch_token_hash IS NOT NULL AND octet_length(dispatch_token_hash)=32 AND dispatched_at IS NOT NULL AND confirmed_at IS NULL)
        OR (state='CONFIRMED' AND confirmed_at IS NOT NULL)
        OR (state='UNKNOWN' AND dispatch_token_hash IS NULL AND confirmed_at IS NULL))
);
CREATE INDEX profile_session_revocation_pending_idx
    ON public.profile_session_revocation_receipts(operation_id) WHERE state<>'CONFIRMED';

-- Existing grants may have reached the provider. Only an existing Account ACK
-- proves confirmation. Neither a missing adapter record nor age proves no effect.
INSERT INTO public.profile_session_revocation_receipts(operation_id,state,created_at,confirmed_at)
SELECT id,CASE WHEN sessions_revoked_at IS NULL THEN 'UNKNOWN' ELSE 'CONFIRMED' END,
       sessions_revocation_started_at,sessions_revoked_at
FROM public.profile_change_requests WHERE sessions_revocation_started_at IS NOT NULL;
