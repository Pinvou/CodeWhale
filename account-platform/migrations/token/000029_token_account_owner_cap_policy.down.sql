SET ROLE token_gateway;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM public.token_account_owner_cap_policy_revisions) OR EXISTS(SELECT 1 FROM public.token_account_owner_cap_policy_active WHERE active_policy_version IS NOT NULL) OR
    EXISTS(SELECT 1 FROM public.outbox_events WHERE event_type IN ('token.account-owner-cap-policy.changed.v1','token.account-provisioning.requested.v1')) OR
    EXISTS(SELECT 1 FROM public.token_command_results WHERE command_type='TOKEN_ACCOUNT_CREATE') OR EXISTS(SELECT 1 FROM public.token_account_provisioning_reservations) OR EXISTS(SELECT 1 FROM public.token_account_provisioning_recovery_actions)
 THEN RAISE EXCEPTION 'owner-cap/provisioning history exists; keep migration 000029' USING ERRCODE='55000'; END IF;
END $$;
RESET ROLE;
SET ROLE token_cap_policy_deployer;
DROP FUNCTION public.apply_token_account_owner_cap_policy(INTEGER,BIGINT,TEXT,TEXT,UUID);
DROP TABLE public.token_account_provisioning_recovery_actions;
DROP TABLE public.token_account_provisioning_reservations;
DROP TRIGGER token_account_owner_cap_policy_revision_immutable ON public.token_account_owner_cap_policy_revisions;
DROP FUNCTION public.reject_token_account_owner_cap_policy_revision_mutation();
DROP TABLE public.token_account_owner_cap_policy_active;
DROP TABLE public.token_account_owner_cap_policy_revisions;
SET ROLE token_gateway;
