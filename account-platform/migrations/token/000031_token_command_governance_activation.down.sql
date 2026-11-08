DO $$
BEGIN
    IF session_user<>'token_migrator' AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=session_user AND rolsuper) THEN
        RAISE EXCEPTION '000031 down requires the dedicated token_migrator identity';
    END IF;
    IF EXISTS (SELECT 1 FROM public.token_command_governance_activation WHERE state<>'PRE_ACTIVATION' OR epoch<>0)
       OR EXISTS (SELECT 1 FROM public.token_command_governance_transitions)
       OR EXISTS (SELECT 1 FROM public.token_account_provisioning_reservations WHERE activation_epoch>0) THEN
        RAISE EXCEPTION 'activated Token governance history or references must be preserved' USING ERRCODE='55000';
    END IF;
END $$;
SET ROLE token_schema_owner;
DROP FUNCTION public.lock_token_command_governance_activation();
DROP TABLE public.token_command_governance_activation;
DROP TABLE public.token_command_governance_transitions;
DROP FUNCTION public.protect_token_governance_transition_history();
DROP FUNCTION public.protect_token_governance_activation();
DROP FUNCTION public.record_token_governance_transition();
SET ROLE token_gateway;
