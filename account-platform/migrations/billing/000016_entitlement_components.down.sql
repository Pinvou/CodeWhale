DROP INDEX entitlement_components_consumption_idx;
DROP TRIGGER entitlement_components_project_package ON entitlement_components;
DROP FUNCTION project_entitlement_package_components();
DROP TABLE usage_loss_reviews;
DROP INDEX usage_hold_allocations_component_idx;

WITH grouped AS (
    SELECT hold_id,package_id,min(component_id::text)::uuid keep_component,
           sum(reserved_microcredits) reserved,
           sum(settled_microcredits) settled,
           sum(released_microcredits) released,
           min(created_at) created_at,max(updated_at) updated_at
    FROM usage_hold_allocations GROUP BY hold_id,package_id
), deleted AS (
    DELETE FROM usage_hold_allocations allocation
    USING grouped
    WHERE allocation.hold_id=grouped.hold_id AND allocation.package_id=grouped.package_id
      AND allocation.component_id<>grouped.keep_component
)
UPDATE usage_hold_allocations allocation SET
    reserved_microcredits=grouped.reserved,
    settled_microcredits=grouped.settled,
    released_microcredits=grouped.released,
    created_at=grouped.created_at,updated_at=grouped.updated_at
FROM grouped
WHERE allocation.hold_id=grouped.hold_id AND allocation.package_id=grouped.package_id
  AND allocation.component_id=grouped.keep_component;

ALTER TABLE usage_hold_allocations DROP CONSTRAINT usage_hold_allocations_pkey;
ALTER TABLE usage_hold_allocations ADD PRIMARY KEY (hold_id,package_id);
ALTER TABLE usage_hold_allocations DROP COLUMN component_id;

UPDATE credit_lots lot SET
    original_microcredits=package.points_total,
    remaining_microcredits=package.points_remaining+package.points_reserved
FROM entitlement_packages package
WHERE package.payment_order_id IS NOT NULL AND package.credit_lot_id=lot.id;

UPDATE credit_lots lot SET remaining_microcredits=0
FROM entitlement_components component
JOIN entitlement_packages package ON package.id=component.package_id
WHERE package.payment_order_id IS NOT NULL
  AND component.credit_lot_id=lot.id AND lot.id<>package.credit_lot_id;

DROP TABLE entitlement_components;
DELETE FROM entitlement_packages WHERE payment_order_id IS NULL;

ALTER TABLE entitlement_packages DROP CONSTRAINT entitlement_packages_composition_check;
ALTER TABLE entitlement_packages
    DROP COLUMN source_type,
    DROP COLUMN source_reference,
    DROP COLUMN points_expired,
    DROP COLUMN points_revoked;
ALTER TABLE entitlement_packages DROP CONSTRAINT entitlement_packages_amount_fen_check;
ALTER TABLE entitlement_packages ADD CONSTRAINT entitlement_packages_amount_fen_check CHECK (amount_fen > 0);
ALTER TABLE entitlement_packages ADD CONSTRAINT entitlement_packages_check CHECK (
    points_reserved + points_used + points_remaining = points_total
);
ALTER TABLE entitlement_packages ALTER COLUMN payment_order_id SET NOT NULL;
ALTER TABLE entitlement_packages ALTER COLUMN user_id SET NOT NULL;
