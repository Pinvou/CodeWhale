CREATE TABLE public.sms_send_provider_admission_fences (
    provider_id TEXT PRIMARY KEY
);

CREATE FUNCTION public.fence_sms_send_provider_snapshot() RETURNS trigger
    LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
    -- Write a new row version even when the provider ID is unchanged. An
    -- older SERIALIZABLE snapshot must abort before its attempt commits.
    INSERT INTO public.sms_send_provider_admission_fences(provider_id)
    VALUES (NEW.provider_id)
    ON CONFLICT (provider_id) DO UPDATE SET provider_id=EXCLUDED.provider_id;
    RETURN NEW;
END;
$$;

CREATE TRIGGER sms_send_provider_snapshot_fence
    BEFORE INSERT ON public.sms_send_attempts
    FOR EACH ROW WHEN (NEW.result='ACCEPTED')
    EXECUTE FUNCTION public.fence_sms_send_provider_snapshot();
