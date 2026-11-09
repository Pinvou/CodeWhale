-- Stop the new worker before rollback. Retain all completed metadata and the
-- original consumer terminal primitive; no domain effects are reversed.
DO $$ BEGIN
  IF session_user <> 'account_migrator' OR current_user <> 'account_service' THEN
    RAISE EXCEPTION 'terminal recovery rollback requires isolated migration identities' USING ERRCODE='42501';
  END IF;
END $$;
RESET ROLE;
SET ROLE account_approval_authorization_owner;
DROP FUNCTION public.recover_approval_authorization_terminals(INTEGER);
DROP INDEX account_approval_authorization.approval_delivery_result_terminal_recovery_idx;
RESET ROLE;
SET ROLE account_service;
