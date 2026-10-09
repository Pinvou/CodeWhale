-- Historical simulator rows have no recorded mobile/SKU facts; preserve them as unknown.
ALTER TABLE simulator_orders
 ADD COLUMN mobile TEXT NOT NULL DEFAULT '',
 ADD COLUMN provider_sku TEXT NOT NULL DEFAULT '';
