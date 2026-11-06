-- Bridge the original published 003 without changing its SQL or ledger row.
SET ROLE account_approval_authorization_owner;
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid='public.outbox_events'::regclass
                AND attname='publication_generation' AND NOT attisdropped) THEN
  ALTER TABLE public.outbox_events ADD COLUMN publication_generation BIGINT NOT NULL DEFAULT 0 CHECK (publication_generation >= 0);
 END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_attribute a JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
                WHERE a.attrelid='public.outbox_events'::regclass AND a.attname='publication_generation'
                AND a.atttypid='bigint'::regtype AND a.attnotnull AND NOT a.attisdropped
                AND pg_get_expr(d.adbin,d.adrelid) IN ('0','0::bigint')) THEN
  RAISE EXCEPTION 'Account outbox generation has an unknown storage definition';
 END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='public.outbox_events'::regclass
                AND contype='c' AND convalidated AND pg_get_constraintdef(oid)='CHECK ((publication_generation >= 0))') THEN
  RAISE EXCEPTION 'Account outbox generation has no validated nonnegative guard';
 END IF;
END $$;
SET ROLE account_service;
