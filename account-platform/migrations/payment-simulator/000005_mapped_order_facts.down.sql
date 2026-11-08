-- Never discard the immutable request facts of an existing mapped order.
LOCK TABLE simulator_orders IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM simulator_orders WHERE mobile<>'' OR provider_sku<>'') THEN
  RAISE EXCEPTION 'mapped simulator order facts prevent rollback';
 END IF;
END $$;
ALTER TABLE simulator_orders DROP COLUMN provider_sku, DROP COLUMN mobile;
