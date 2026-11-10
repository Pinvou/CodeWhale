-- Index-only rollback retains all original financial and investigation facts.
DROP INDEX public.reconciliation_observation_facts_order_idx;
DROP INDEX public.reconciliation_observation_progress_case_idx;
DROP INDEX public.reconciliation_cases_payment_order_created_idx;
