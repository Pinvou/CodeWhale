DO $$
DECLARE
    transition_at TIMESTAMPTZ := clock_timestamp();
    next_version BIGINT;
BEGIN
    UPDATE payment_product_mappings
    SET status='RETIRED', effective_to=transition_at
    WHERE provider_id='h3c_mall'
      AND provider_version='h3c-mall-v1'
      AND product_id='PLAN_TRIAL'
      AND status IN ('TEST_ONLY','FROZEN')
      AND effective_from<=transition_at
      AND (effective_to IS NULL OR effective_to>transition_at);

    SELECT COALESCE(max(mapping_version)+1,1)
    INTO next_version
    FROM payment_product_mappings
    WHERE provider_id='h3c_mall'
      AND provider_version='h3c-mall-v1'
      AND product_id='PLAN_TRIAL';

    INSERT INTO payment_product_mappings(
        id,provider_id,provider_version,product_id,mapping_version,external_sku,
        amount_fen,currency,channels,status,effective_from,created_at
    ) VALUES (
        gen_random_uuid(),'h3c_mall','h3c-mall-v1','PLAN_TRIAL',next_version,
        '0502AJON',4900,'CNY',ARRAY['ALIPAY'],'TEST_ONLY',transition_at,transition_at
    );
END $$;
