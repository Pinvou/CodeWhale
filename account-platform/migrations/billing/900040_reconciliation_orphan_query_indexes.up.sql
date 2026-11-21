-- Bound the new cross-date case lookup and per-case observation projection.
CREATE INDEX reconciliation_cases_payment_order_created_idx
 ON public.reconciliation_cases(payment_order_id,created_at,id);
CREATE INDEX reconciliation_observation_progress_case_idx
 ON public.reconciliation_observation_progress(case_id,observation_id);
CREATE INDEX reconciliation_observation_facts_order_idx
 ON public.reconciliation_observation_facts(observation_id,payment_order_id);
