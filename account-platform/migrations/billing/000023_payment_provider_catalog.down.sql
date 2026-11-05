DROP TRIGGER payment_product_mapping_immutable ON payment_product_mappings;
DROP FUNCTION reject_payment_mapping_mutation();

ALTER TABLE payment_orders
    DROP COLUMN provider_capability_snapshot,
    DROP COLUMN product_mapping_snapshot,
    DROP COLUMN product_mapping_id,
    DROP COLUMN provider_version,
    DROP COLUMN provider_id;

UPDATE recharge_products product
SET sku_code=mapping.external_sku
FROM payment_product_mappings mapping
WHERE mapping.provider_id='h3c_mall' AND mapping.provider_version='h3c-mall-v1'
  AND mapping.mapping_version=1 AND mapping.product_id=product.product_id;
CREATE UNIQUE INDEX recharge_products_enabled_sku_idx
    ON recharge_products (sku_code)
    WHERE enabled;

DROP TABLE payment_product_mappings;
