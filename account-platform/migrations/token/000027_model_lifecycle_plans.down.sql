DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM model_lifecycle_plans)
       OR EXISTS (SELECT 1 FROM model_lifecycle_plan_events) THEN
        RAISE EXCEPTION 'APT-134 rollback refused: lifecycle plan history must be retained';
    END IF;
END $$;

DROP TRIGGER model_lifecycle_plan_event_immutable ON model_lifecycle_plan_events;
DROP FUNCTION reject_model_lifecycle_plan_event_mutation();
DROP TABLE model_lifecycle_plan_events;
DROP TRIGGER model_lifecycle_plan_definition_immutable ON model_lifecycle_plans;
DROP FUNCTION preserve_model_lifecycle_plan_definition();
DROP TABLE model_lifecycle_plans;
