DO $$
BEGIN
    IF EXISTS(SELECT 1 FROM billing_approval_command_activations)
       OR EXISTS(SELECT 1 FROM billing_approval_command_activation_history) THEN
        RAISE EXCEPTION 'approval activation history exists; keep migration 000054' USING ERRCODE='55000';
    END IF;
END $$;
RESET ROLE;
SET ROLE billing_schema_owner;
DROP TRIGGER billing_approval_activation_history_immutable ON billing_approval_command_activation_history;
DROP TRIGGER billing_approval_activation_history_update ON billing_approval_command_activations;
DROP TRIGGER billing_approval_activation_history_insert ON billing_approval_command_activations;
DROP TRIGGER billing_approval_activation_truncate_guard ON billing_approval_command_activations;
DROP TRIGGER billing_approval_activation_update_guard ON billing_approval_command_activations;
DROP FUNCTION enforce_billing_approval_activation_history_immutable();
DROP FUNCTION append_billing_approval_activation_history();
DROP FUNCTION enforce_billing_approval_activation_update();
DROP TABLE billing_approval_command_activation_history;
DROP TABLE billing_approval_command_activations;
SET ROLE billing_service;
