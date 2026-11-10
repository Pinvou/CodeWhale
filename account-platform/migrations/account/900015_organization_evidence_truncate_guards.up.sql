-- Preserve the original row guards, command facts and ownership contract.
CREATE TRIGGER organization_creation_requests_no_truncate
BEFORE TRUNCATE ON public.organization_creation_requests
FOR EACH STATEMENT EXECUTE FUNCTION public.protect_organization_creation_request();

CREATE TRIGGER organization_unit_commands_no_truncate
BEFORE TRUNCATE ON public.organization_unit_commands
FOR EACH STATEMENT EXECUTE FUNCTION public.protect_organization_unit_command();

CREATE TRIGGER membership_commands_no_truncate
BEFORE TRUNCATE ON public.membership_commands
FOR EACH STATEMENT EXECUTE FUNCTION public.protect_membership_governance_history();
