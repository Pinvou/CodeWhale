\set ON_ERROR_STOP on
-- Controlled operator connection only. This closes a case, never its task,
-- supplier cancellation, usage, hold, settlement or funds outcome.
BEGIN;
SET LOCAL ROLE token_gateway;
SET LOCAL search_path=pg_catalog,public,pg_temp;
SELECT set_config('pinvou.ops.expected_database', :'expected_database', true) AS expected_database,
       set_config('pinvou.ops.case_id', :'case_id', true) AS case_id,
       set_config('pinvou.ops.task_id', :'task_id', true) AS task_id,
       set_config('pinvou.ops.resource_id', :'resource_id', true) AS resource_id,
       set_config('pinvou.ops.evidence_id', :'evidence_id', true) AS evidence_id,
       set_config('pinvou.ops.actor_id', :'actor_id', true) AS actor_id,
       set_config('pinvou.ops.reason', :'reason', true) AS reason
\gset closed_case_
-- BEGIN CASE-ONLY BODY
-- Resolve row types and all relation/function names before DO compilation.
SET LOCAL search_path=pg_catalog,public,pg_temp;
DO $case_only$
DECLARE
 task model_emergency_cancellations%ROWTYPE;
 review manual_review_cases%ROWTYPE;
 evidence model_emergency_cancellation_evidence%ROWTYPE;
 actor uuid := current_setting('pinvou.ops.actor_id')::uuid;
 reason text := btrim(current_setting('pinvou.ops.reason'));
 at timestamptz;
BEGIN
 IF current_database() <> current_setting('pinvou.ops.expected_database')
    OR session_user <> 'token_migrator' OR current_user <> 'token_gateway'
    OR actor='00000000-0000-0000-0000-000000000000'::uuid
    OR length(reason) NOT BETWEEN 1 AND 1000
    OR reason !~ '[^[:space:]]' OR reason ~ '[[:cntrl:]]' THEN
  RAISE EXCEPTION 'CASE_ONLY_OPERATOR_OR_REASON_INVALID' USING ERRCODE='42501';
 END IF;
 -- Keep the worker's task-first lock order. Its original ownership is intact.
 SELECT * INTO STRICT task FROM model_emergency_cancellations
 WHERE id=current_setting('pinvou.ops.task_id')::uuid FOR UPDATE;
 SELECT * INTO STRICT review FROM manual_review_cases
 WHERE id=current_setting('pinvou.ops.case_id')::uuid FOR UPDATE;
 SELECT * INTO STRICT evidence FROM model_emergency_cancellation_evidence
 WHERE id=current_setting('pinvou.ops.evidence_id')::uuid;
 IF review.domain<>'TOKEN' OR review.case_type<>'MODEL_EMERGENCY_CANCELLATION_REVIEW'
    OR review.resource_type<>'USAGE_REQUEST' OR review.status<>'OPEN'
    OR review.resource_id<>current_setting('pinvou.ops.resource_id')::uuid
    OR review.resource_id<>task.usage_request_id
    OR task.status<>'MANUAL_REVIEW' OR task.retry_attempts<8 OR task.attempts<1
    OR task.lease_until IS NOT NULL OR task.next_attempt_at IS NOT NULL
    OR task.provider_cancel_result IS NULL OR task.provider_cancel_result NOT IN
       ('ATTEMPT_ERROR:UNKNOWN','ATTEMPT_ERROR:DEADLINE_EXCEEDED','ATTEMPT_ERROR:CONTEXT_CANCELLED',
        'ATTEMPT_ERROR:DEPENDENCY_ERROR','ATTEMPT_ERROR:LEASE_EXPIRED')
    OR evidence.emergency_cancellation_id<>task.id OR evidence.evidence_type<>'MANUAL_REVIEW'
    OR evidence.created_at IS DISTINCT FROM task.updated_at
    OR evidence.snapshot IS DISTINCT FROM jsonb_build_object(
       'retry_attempts',task.retry_attempts,'diagnostic',task.provider_cancel_result) THEN
  RAISE EXCEPTION 'CASE_ONLY_EVIDENCE_MISMATCH' USING ERRCODE='55000';
 END IF;
 at := clock_timestamp();
 UPDATE manual_review_cases SET status='RESOLVED',resolved_at=at,updated_at=at WHERE id=review.id;
 INSERT INTO manual_review_case_events(id,case_id,event_type,actor_id,details,created_at)
 VALUES(gen_random_uuid(),review.id,'EMERGENCY_CANCELLATION_CASE_CLOSED',actor,
        jsonb_build_object('task_id',task.id,'evidence_id',evidence.id,'reason',reason,'case_only',true),at);
END
$case_only$;
-- END CASE-ONLY BODY
COMMIT;
