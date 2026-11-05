-- An existing mobile number is not evidence of a requester UUID. Historical
-- orders retain unknown identity; new mapped orders store the actual UUID fact.
ALTER TABLE simulator_orders
    ADD COLUMN merchant_customer_key TEXT NOT NULL DEFAULT ''
        CHECK (merchant_customer_key = '' OR (
            merchant_customer_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            AND merchant_customer_key <> '00000000-0000-0000-0000-000000000000'
        ));
