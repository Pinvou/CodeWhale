DO $$
DECLARE
    transition_at TIMESTAMPTZ := clock_timestamp();
BEGIN
    UPDATE payment_product_mappings
    SET status='RETIRED', effective_to=transition_at
    WHERE provider_id IN ('h3c_mall','development_simulator')
      AND status IN ('TEST_ONLY','FROZEN')
      AND effective_from<=transition_at
      AND (effective_to IS NULL OR effective_to>transition_at);

    INSERT INTO payment_product_mappings(
        id,provider_id,provider_version,product_id,mapping_version,external_sku,
        amount_fen,currency,channels,status,effective_from,created_at
    )
    SELECT
        gen_random_uuid(), facts.provider_id, facts.provider_version, facts.product_id,
        COALESCE((SELECT max(existing.mapping_version)+1
                  FROM payment_product_mappings existing
                  WHERE existing.provider_id=facts.provider_id
                    AND existing.provider_version=facts.provider_version
                    AND existing.product_id=facts.product_id), 1),
        facts.external_sku, facts.amount_fen, 'CNY', facts.channels, 'TEST_ONLY',
        transition_at, transition_at
    FROM (VALUES
        ('h3c_mall','h3c-mall-v1','PLAN_TRIAL','8814A1D2',20000::BIGINT,ARRAY['ALIPAY']::TEXT[]),
        ('h3c_mall','h3c-mall-v1','PLAN_STANDARD','0502AJ0Q',9900::BIGINT,ARRAY['ALIPAY']::TEXT[]),
        ('h3c_mall','h3c-mall-v1','PLAN_ADVANCED','0502AJOR',19900::BIGINT,ARRAY['ALIPAY']::TEXT[]),
        ('h3c_mall','h3c-mall-v1','PLAN_FLAGSHIP','0502AJOP',99900::BIGINT,ARRAY['ALIPAY']::TEXT[]),
        ('development_simulator','development-simulator-v1','PLAN_TRIAL','SIM_PLAN_TRIAL',20000::BIGINT,ARRAY['UNIONPAY','ALIPAY','WECHAT']::TEXT[]),
        ('development_simulator','development-simulator-v1','PLAN_STANDARD','SIM_PLAN_STANDARD',9900::BIGINT,ARRAY['UNIONPAY','ALIPAY','WECHAT']::TEXT[]),
        ('development_simulator','development-simulator-v1','PLAN_ADVANCED','SIM_PLAN_ADVANCED',19900::BIGINT,ARRAY['UNIONPAY','ALIPAY','WECHAT']::TEXT[]),
        ('development_simulator','development-simulator-v1','PLAN_FLAGSHIP','SIM_PLAN_FLAGSHIP',99900::BIGINT,ARRAY['UNIONPAY','ALIPAY','WECHAT']::TEXT[])
    ) AS facts(provider_id,provider_version,product_id,external_sku,amount_fen,channels);

    UPDATE recharge_products
    SET amount_fen=20000,
        catalog_version=catalog_version+1,
        price_status='UAT_TENTATIVE'
    WHERE product_id='PLAN_TRIAL';
END $$;
