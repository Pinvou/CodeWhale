ALTER TABLE recharge_products
    ADD COLUMN plan_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN sku_code TEXT NOT NULL DEFAULT '',
    ADD COLUMN base_points BIGINT NOT NULL DEFAULT 0 CHECK (base_points >= 0),
    ADD COLUMN bonus_points BIGINT NOT NULL DEFAULT 0 CHECK (bonus_points >= 0),
    ADD COLUMN validity_days INTEGER NOT NULL DEFAULT 0 CHECK (validity_days >= 0),
    ADD COLUMN price_status TEXT NOT NULL DEFAULT 'LEGACY' CHECK (price_status IN ('LEGACY', 'UAT_TENTATIVE', 'FROZEN')),
    ADD COLUMN invoice_product_name TEXT NOT NULL DEFAULT '';

UPDATE recharge_products SET enabled = false WHERE product_id IN ('CNY_1', 'CNY_10', 'CNY_100', 'CNY_200');

INSERT INTO recharge_products(
    product_id, amount_fen, credit_microcredits, display_points, currency, channels,
    catalog_version, enabled, created_at, plan_name, sku_code, base_points,
    bonus_points, validity_days, price_status, invoice_product_name
) VALUES
('PLAN_TRIAL', 10, 20000000, 2000, 'CNY', ARRAY['ALIPAY'], 2, true, now(), '体验版', '0502AJON', 1000, 1000, 30, 'UAT_TENTATIVE', '品悟体验版积分授权'),
('PLAN_STANDARD', 1000, 40000000, 4000, 'CNY', ARRAY['ALIPAY'], 2, true, now(), '标准版', '0502AJ0Q', 2000, 2000, 30, 'UAT_TENTATIVE', '品悟标准版积分授权'),
('PLAN_ADVANCED', 3000, 90000000, 9000, 'CNY', ARRAY['ALIPAY'], 2, true, now(), '高级版', '0502AJOR', 4000, 5000, 30, 'UAT_TENTATIVE', '品悟高级版积分授权'),
('PLAN_FLAGSHIP', 20000, 500000000, 50000, 'CNY', ARRAY['ALIPAY'], 2, true, now(), '旗舰版', '0502AJOP', 20000, 30000, 30, 'UAT_TENTATIVE', '品悟旗舰版积分授权');

CREATE UNIQUE INDEX recharge_products_enabled_sku_idx
    ON recharge_products (sku_code)
    WHERE enabled AND sku_code <> '';
