\set ON_ERROR_STOP on
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;

SELECT event_type,count(*) AS pending,min(occurred_at) AS oldest, max(attempts) AS maximum_attempts
FROM outbox_events
WHERE published_at IS NULL AND dead_lettered_at IS NULL
GROUP BY event_type ORDER BY oldest;

SELECT event_id,event_type,aggregate_id,aggregate_version,attempts,last_error,dead_lettered_at
FROM outbox_events WHERE dead_lettered_at IS NOT NULL
ORDER BY dead_lettered_at,event_id;

ROLLBACK;
