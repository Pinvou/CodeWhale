CREATE TABLE payment_product_mappings (
    id UUID PRIMARY KEY,
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    product_id TEXT NOT NULL REFERENCES recharge_products(product_id),
    mapping_version BIGINT NOT NULL CHECK (mapping_version > 0),
    external_sku TEXT NOT NULL,
    amount_fen BIGINT NOT NULL CHECK (amount_fen > 0),
    currency TEXT NOT NULL,
    channels TEXT[] NOT NULL CHECK (cardinality(channels) > 0),
    status TEXT NOT NULL CHECK (status IN ('TEST_ONLY','FROZEN','RETIRED')),
    effective_from TIMESTAMPTZ NOT NULL,
    effective_to TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (provider_id,provider_version,product_id,mapping_version),
    UNIQUE (provider_id,provider_version,external_sku,mapping_version),
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);

INSERT INTO payment_product_mappings(id,provider_id,provider_version,product_id,mapping_version,external_sku,amount_fen,currency,channels,status,effective_from,created_at) VALUES
('00000000-0000-7000-8300-000000000001','h3c_mall','h3c-mall-v1','PLAN_TRIAL',1,'0502AJON',10,'CNY',ARRAY['ALIPAY'],'TEST_ONLY',now(),now()),
('00000000-0000-7000-8300-000000000002','h3c_mall','h3c-mall-v1','PLAN_STANDARD',1,'0502AJ0Q',1000,'CNY',ARRAY['ALIPAY'],'TEST_ONLY',now(),now()),
('00000000-0000-7000-8300-000000000003','h3c_mall','h3c-mall-v1','PLAN_ADVANCED',1,'0502AJOR',3000,'CNY',ARRAY['ALIPAY'],'TEST_ONLY',now(),now()),
('00000000-0000-7000-8300-000000000004','h3c_mall','h3c-mall-v1','PLAN_FLAGSHIP',1,'0502AJOP',20000,'CNY',ARRAY['ALIPAY'],'TEST_ONLY',now(),now()),
('00000000-0000-7000-8300-000000000005','development_simulator','development-simulator-v1','PLAN_TRIAL',1,'SIM_PLAN_TRIAL',10,'CNY',ARRAY['UNIONPAY','ALIPAY','WECHAT'],'TEST_ONLY',now(),now()),
('00000000-0000-7000-8300-000000000006','development_simulator','development-simulator-v1','PLAN_STANDARD',1,'SIM_PLAN_STANDARD',1000,'CNY',ARRAY['UNIONPAY','ALIPAY','WECHAT'],'TEST_ONLY',now(),now()),
('00000000-0000-7000-8300-000000000007','development_simulator','development-simulator-v1','PLAN_ADVANCED',1,'SIM_PLAN_ADVANCED',3000,'CNY',ARRAY['UNIONPAY','ALIPAY','WECHAT'],'TEST_ONLY',now(),now()),
('00000000-0000-7000-8300-000000000008','development_simulator','development-simulator-v1','PLAN_FLAGSHIP',1,'SIM_PLAN_FLAGSHIP',20000,'CNY',ARRAY['UNIONPAY','ALIPAY','WECHAT'],'TEST_ONLY',now(),now());

-- External provider SKU ownership moves from the generic product catalog to
-- the versioned provider mapping above. The legacy column remains only for
-- rollback compatibility and is no longer populated or exposed.
DROP INDEX recharge_products_enabled_sku_idx;
UPDATE recharge_products SET sku_code='' WHERE enabled;

ALTER TABLE payment_orders
    ADD COLUMN provider_id TEXT NOT NULL DEFAULT 'legacy',
    ADD COLUMN provider_version TEXT NOT NULL DEFAULT 'legacy-v1',
    ADD COLUMN product_mapping_id UUID REFERENCES payment_product_mappings(id),
    ADD COLUMN product_mapping_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN provider_capability_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE OR REPLACE FUNCTION reject_payment_mapping_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment product mappings cannot be deleted';
    END IF;
    IF OLD.status IN ('TEST_ONLY','FROZEN') AND NEW.status='RETIRED'
       AND (to_jsonb(NEW) - 'status' - 'effective_to') = (to_jsonb(OLD) - 'status' - 'effective_to')
       AND NEW.effective_to IS NOT NULL THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'published payment product mapping facts are immutable';
END $$;

CREATE TRIGGER payment_product_mapping_immutable BEFORE UPDATE OR DELETE ON payment_product_mappings
FOR EACH ROW EXECUTE FUNCTION reject_payment_mapping_mutation();
