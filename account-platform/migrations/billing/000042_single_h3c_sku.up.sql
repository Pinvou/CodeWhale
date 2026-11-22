DO $$
DECLARE
    transition_at TIMESTAMPTZ := clock_timestamp();
BEGIN
    -- Keep historical mappings for frozen orders, but remove every current
    -- purchase mapping before publishing the single approved SKU.
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
        gen_random_uuid(), facts.provider_id, facts.provider_version, 'PLAN_TRIAL',
        COALESCE((SELECT max(existing.mapping_version)+1
                  FROM payment_product_mappings existing
                  WHERE existing.provider_id=facts.provider_id
                    AND existing.provider_version=facts.provider_version
                    AND existing.product_id='PLAN_TRIAL'), 1),
        facts.external_sku, 10, 'CNY', facts.channels, 'TEST_ONLY',
        transition_at, transition_at
    FROM (VALUES
        ('h3c_mall','h3c-mall-v1','0502AJ0Q',ARRAY['ALIPAY']::TEXT[]),
        ('development_simulator','development-simulator-v1','SIM_PLAN_TRIAL',ARRAY['UNIONPAY','ALIPAY','WECHAT']::TEXT[])
    ) AS facts(provider_id,provider_version,external_sku,channels);

    UPDATE recharge_products
    SET amount_fen=10,
        catalog_version=catalog_version+1,
        price_status='UAT_TENTATIVE'
    WHERE product_id='PLAN_TRIAL';
END $$;
