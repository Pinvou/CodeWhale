CREATE TABLE payment_product_controls (
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    product_id TEXT NOT NULL REFERENCES recharge_products(product_id),
    new_orders_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    reason_code TEXT NOT NULL DEFAULT 'AVAILABLE' CHECK (reason_code IN ('AVAILABLE','AUTOMATIC_STOP_LOSS','MANUAL_SAFETY_PAUSE')),
    reason TEXT NOT NULL DEFAULT '',
    source_case_id UUID REFERENCES manual_review_cases(id),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version>0),
    updated_by UUID,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(provider_id,provider_version,product_id)
);

INSERT INTO payment_product_controls(provider_id,provider_version,product_id,new_orders_enabled,reason_code,reason,source_case_id,updated_at)
SELECT DISTINCT ON (evaluation.provider_id,evaluation.provider_version,evaluation.payment_product_id)
       evaluation.provider_id,evaluation.provider_version,evaluation.payment_product_id,FALSE,
       'AUTOMATIC_STOP_LOSS','P0/P1 manual review mitigation SLA exceeded',evaluation.case_id,evaluation.evaluated_at
  FROM manual_review_stop_loss_evaluations evaluation
 WHERE evaluation.provider_id IS NOT NULL
   AND evaluation.provider_version IS NOT NULL
   AND evaluation.payment_product_id IS NOT NULL
   AND COALESCE((evaluation.action_snapshot->>'payment_product_paused')::boolean,FALSE)
 ORDER BY evaluation.provider_id,evaluation.provider_version,evaluation.payment_product_id,evaluation.evaluated_at DESC,evaluation.case_id
ON CONFLICT(provider_id,provider_version,product_id) DO NOTHING;

UPDATE recharge_products product
   SET enabled=TRUE
 WHERE NOT product.enabled
   AND EXISTS(
       SELECT 1
         FROM manual_review_stop_loss_evaluations evaluation
        WHERE evaluation.payment_product_id=product.product_id
          AND COALESCE((evaluation.action_snapshot->>'payment_product_paused')::boolean,FALSE)
   );

CREATE INDEX payment_product_controls_paused_idx
    ON payment_product_controls(provider_id,provider_version,updated_at,product_id)
    WHERE NOT new_orders_enabled;
