DO $$ BEGIN
    IF session_user <> 'account_migrator' OR current_user <> 'account_service' THEN
        RAISE EXCEPTION 'ordinary replay rollback requires the dedicated Account migrator' USING ERRCODE='42501';
    END IF;
END $$;
RESET ROLE;
SET ROLE account_approval_authorization_owner;
LOCK TABLE account_approval_authorization.ordinary_replay_generations IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
    IF EXISTS(SELECT 1 FROM account_approval_authorization.ordinary_replay_generations) THEN
        RAISE EXCEPTION 'ordinary replay generation history prevents rollback' USING ERRCODE='55000';
    END IF;
END $$;
DROP FUNCTION public.enqueue_ordinary_dead_letter_replay(UUID,UUID,BIGINT,TEXT,TEXT,TEXT,TEXT,TEXT,TEXT,TEXT,BYTEA);
DROP TABLE account_approval_authorization.ordinary_replay_generations;
DROP FUNCTION account_approval_authorization.reject_ordinary_replay_generation_mutation();
RESET ROLE;
SET ROLE account_service;
