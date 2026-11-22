WITH pending AS (
 SELECT count(*) AS total,min(occurred_at) AS oldest FROM public.outbox_events
 WHERE published_at IS NULL AND dead_lettered_at IS NULL AND NOT(event_type=ANY($1::text[]))
), dead AS (
 SELECT count(*) AS total FROM public.outbox_events
 WHERE dead_lettered_at IS NOT NULL AND NOT(event_type=ANY($1::text[]))
)
SELECT pending.total,dead.total,COALESCE(EXTRACT(EPOCH FROM now()-pending.oldest),0),
       pg_total_relation_size('public.outbox_events'::regclass)
FROM pending CROSS JOIN dead
