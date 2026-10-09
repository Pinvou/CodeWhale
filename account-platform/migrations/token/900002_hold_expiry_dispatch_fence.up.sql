-- New dispatch intents must share the permanent cancellation gate.
CREATE FUNCTION public.guard_usage_dispatch_cancellation() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE cancellation TEXT; execution TEXT;
BEGIN
 SELECT cancel_status,execution_status INTO cancellation,execution FROM public.usage_requests WHERE id=NEW.usage_request_id FOR UPDATE;
 IF cancellation IS NULL OR cancellation<>'NOT_REQUESTED' OR execution IN ('CANCELLED_BEFORE_DISPATCH','REVIEW','FAILED','SUCCEEDED')
 OR EXISTS(SELECT 1 FROM public.usage_cancellation_evidence WHERE usage_request_id=NEW.usage_request_id AND adapter_version='hold-expiry-v1' AND evidence_type='CANCEL_REQUESTED') THEN
  RAISE EXCEPTION 'usage dispatch is cancelled or unresolved' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER usage_dispatch_cancellation_fence BEFORE INSERT ON usage_dispatch_intents FOR EACH ROW EXECUTE FUNCTION public.guard_usage_dispatch_cancellation();
