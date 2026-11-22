package main

// Storage hardening transfers ordinary domain tables to the NOLOGIN schema
// owner while retaining business DML. That ownership alone is not evidence of
// control-plane authority. This allowance follows the published 900001/900003
// ACL contract and the forward wrapper's append-only restrictions; it never
// permits TRUNCATE or control/projection writes. Retained replay history also
// has its original immutable trigger verified by installationSnapshot before
// this query; its published ACL need not match newer append-only table ACLs.
// Catalog aliases r,c,owner,rights belong to the enclosing capability query.
const hardenedRuntimeDMLAllowanceSQL = `(
 owner.rolname='token_schema_owner'
 AND EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=900001)
 AND c.relname NOT IN ('schema_migrations','token_runtime_hardening',
 'token_command_governance_activation','token_command_governance_transitions',
 'usage_history_projection_state','usage_history_members','usage_history_groups')
 AND rights.privilege IN ('INSERT','UPDATE','DELETE')
 AND (rights.privilege='INSERT' OR (
 c.relname NOT IN ('token_audit_logs','client_readiness_attestations',
 'usage_replay_anonymized_users','usage_waiver_intents','usage_waiver_receipts','usage_waiver_decisions')
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_trigger trigger
 JOIN pg_catalog.pg_proc function ON function.oid=trigger.tgfoid
 WHERE trigger.tgrelid=c.oid AND NOT trigger.tgisinternal AND function.proname IN (
 'reject_model_catalog_history_mutation','reject_usage_execution_history_mutation',
 'reject_usage_command_or_evidence_mutation','reject_token_manual_review_evidence_mutation',
 'reject_route_health_history_mutation','reject_model_lifecycle_plan_event_mutation','reject_token_audit_mutation'))
 )))`
