DO $$
DECLARE
    transition_at TIMESTAMPTZ := clock_timestamp();
BEGIN
    UPDATE payment_product_mappings
    SET status='RETIRED', effective_to=transition_at
    WHERE provider_id IN ('h3c_mall','development_simulator')
      AND product_id IN ('PLAN_TRIAL','PLAN_STANDARD','PLAN_ADVANCED','PLAN_FLAGSHIP')
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
        ('h3c_mall','h3c-mall-v1','PLAN_TRIAL','0502AJON',10,ARRAY['ALIPAY']::TEXT[]),
        ('h3c_mall','h3c-mall-v1','PLAN_STANDARD','0502AJ0Q',1000,ARRAY['ALIPAY']::TEXT[]),
        ('h3c_mall','h3c-mall-v1','PLAN_ADVANCED','0502AJOR',3000,ARRAY['ALIPAY']::TEXT[]),
        ('h3c_mall','h3c-mall-v1','PLAN_FLAGSHIP','0502AJOP',20000,ARRAY['ALIPAY']::TEXT[]),
        ('development_simulator','development-simulator-v1','PLAN_TRIAL','SIM_PLAN_TRIAL',10,ARRAY['UNIONPAY','ALIPAY','WECHAT']::TEXT[]),
        ('development_simulator','development-simulator-v1','PLAN_STANDARD','SIM_PLAN_STANDARD',1000,ARRAY['UNIONPAY','ALIPAY','WECHAT']::TEXT[]),
        ('development_simulator','development-simulator-v1','PLAN_ADVANCED','SIM_PLAN_ADVANCED',3000,ARRAY['UNIONPAY','ALIPAY','WECHAT']::TEXT[]),
        ('development_simulator','development-simulator-v1','PLAN_FLAGSHIP','SIM_PLAN_FLAGSHIP',20000,ARRAY['UNIONPAY','ALIPAY','WECHAT']::TEXT[])
    ) AS facts(provider_id,provider_version,product_id,external_sku,amount_fen,channels);
END $$;

UPDATE recharge_products
SET amount_fen=pricing.amount_fen,
    credit_microcredits=pricing.total_points*10000,
    display_points=pricing.total_points,
    base_points=pricing.base_points,
    bonus_points=pricing.bonus_points,
    validity_days=30,
    catalog_version=catalog_version+1,
    price_status='UAT_TENTATIVE'
FROM (VALUES
    ('PLAN_TRIAL',10::BIGINT,1000::BIGINT,1000::BIGINT,2000::BIGINT),
    ('PLAN_STANDARD',1000::BIGINT,2000::BIGINT,2000::BIGINT,4000::BIGINT),
    ('PLAN_ADVANCED',3000::BIGINT,4000::BIGINT,5000::BIGINT,9000::BIGINT),
    ('PLAN_FLAGSHIP',20000::BIGINT,20000::BIGINT,30000::BIGINT,50000::BIGINT)
) AS pricing(product_id,amount_fen,base_points,bonus_points,total_points)
WHERE recharge_products.product_id=pricing.product_id;
