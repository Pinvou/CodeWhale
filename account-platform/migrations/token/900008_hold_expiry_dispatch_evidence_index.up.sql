CREATE INDEX usage_cancellation_dispatch_fence_idx
    ON public.usage_cancellation_evidence(usage_request_id)
    WHERE adapter_version='hold-expiry-v1' AND evidence_type='CANCEL_REQUESTED';
