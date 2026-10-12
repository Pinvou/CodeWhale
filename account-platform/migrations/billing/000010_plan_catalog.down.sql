DROP INDEX recharge_products_enabled_sku_idx;

DELETE FROM recharge_products WHERE product_id IN ('PLAN_TRIAL', 'PLAN_STANDARD', 'PLAN_ADVANCED', 'PLAN_FLAGSHIP');
UPDATE recharge_products SET enabled = true WHERE product_id IN ('CNY_1', 'CNY_10', 'CNY_100', 'CNY_200');

ALTER TABLE recharge_products
    DROP COLUMN invoice_product_name,
    DROP COLUMN price_status,
    DROP COLUMN validity_days,
    DROP COLUMN bonus_points,
    DROP COLUMN base_points,
    DROP COLUMN sku_code,
    DROP COLUMN plan_name;
