ALTER TABLE entitlement_packages ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE entitlement_packages ALTER COLUMN payment_order_id DROP NOT NULL;
ALTER TABLE entitlement_packages DROP CONSTRAINT entitlement_packages_amount_fen_check;
ALTER TABLE entitlement_packages ADD CONSTRAINT entitlement_packages_amount_fen_check CHECK (amount_fen >= 0);
ALTER TABLE entitlement_packages DROP CONSTRAINT entitlement_packages_check;
ALTER TABLE entitlement_packages
    ADD COLUMN source_type TEXT NOT NULL DEFAULT 'PAYMENT' CHECK (source_type IN ('PAYMENT', 'MANUAL_ADJUSTMENT', 'SYSTEM')),
    ADD COLUMN source_reference TEXT NOT NULL DEFAULT '',
    ADD COLUMN points_expired BIGINT NOT NULL DEFAULT 0 CHECK (points_expired >= 0),
    ADD COLUMN points_revoked BIGINT NOT NULL DEFAULT 0 CHECK (points_revoked >= 0),
    ADD CONSTRAINT entitlement_packages_composition_check CHECK (
        points_remaining + points_reserved + points_used + points_expired + points_revoked = points_total
    );

UPDATE entitlement_packages
SET source_reference = 'payment-order:' || payment_order_id::text
WHERE source_reference = '' AND payment_order_id IS NOT NULL;
ALTER TABLE entitlement_packages
    ADD CONSTRAINT entitlement_packages_source_reference_check CHECK (source_reference <> '');

CREATE TABLE entitlement_components (
    id UUID PRIMARY KEY,
    package_id UUID NOT NULL REFERENCES entitlement_packages(id),
    component_type TEXT NOT NULL CHECK (component_type IN ('BASE', 'BONUS')),
    credit_lot_id UUID NOT NULL UNIQUE REFERENCES credit_lots(id),
    original_microcredits BIGINT NOT NULL CHECK (original_microcredits > 0),
    available_microcredits BIGINT NOT NULL CHECK (available_microcredits >= 0),
    reserved_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (reserved_microcredits >= 0),
    used_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (used_microcredits >= 0),
    active_grant_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (active_grant_microcredits >= 0),
    suspended_grant_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (suspended_grant_microcredits >= 0),
    expired_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (expired_microcredits >= 0),
    revoked_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (revoked_microcredits >= 0),
    credited_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('ACTIVE', 'CONSUMED', 'EXPIRED', 'REVOKED', 'FROZEN')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (package_id, component_type),
    CHECK (expires_at > credited_at),
    CHECK (
        available_microcredits + reserved_microcredits + used_microcredits +
        active_grant_microcredits + suspended_grant_microcredits +
        expired_microcredits + revoked_microcredits = original_microcredits
    )
);

INSERT INTO entitlement_components(
    id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,
    reserved_microcredits,used_microcredits,expired_microcredits,credited_at,expires_at,status,created_at,updated_at
)
SELECT credit_lot_id,id,'BASE',credit_lot_id,points_total,
       CASE WHEN status='EXPIRED' THEN 0 ELSE points_remaining END,
       points_reserved,points_used,CASE WHEN status='EXPIRED' THEN points_remaining ELSE 0 END,
       credited_at,valid_until,
       CASE status
           WHEN 'EXPIRED' THEN 'EXPIRED'
           WHEN 'CONSUMED' THEN 'CONSUMED'
           WHEN 'FROZEN' THEN 'FROZEN'
           WHEN 'CREDIT_FAILED' THEN 'FROZEN'
           ELSE 'ACTIVE'
       END,
       created_at,updated_at
FROM entitlement_packages;

UPDATE credit_lots lot
SET expires_at = lot.created_at + INTERVAL '30 days'
WHERE lot.expires_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM entitlement_components component WHERE component.credit_lot_id=lot.id);

INSERT INTO entitlement_packages(
    id,user_id,wallet_id,payment_order_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,
    points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,
    source_type,source_reference
)
SELECT lot.id,CASE WHEN wallet.owner_type='PERSONAL' THEN wallet.owner_id ELSE NULL END,lot.wallet_id,NULL,lot.id,
       CASE WHEN lot.source_type='ADJUSTMENT' THEN 'MANUAL_BONUS' ELSE 'LEGACY_BASE' END,
       CASE WHEN lot.source_type='ADJUSTMENT' THEN 'Manual bonus' ELSE 'Legacy base entitlement' END,
       '',0,lot.original_microcredits,lot.remaining_microcredits,lot.created_at,lot.created_at,lot.expires_at,
       CASE WHEN lot.remaining_microcredits=0 THEN 'CONSUMED' ELSE 'ACTIVE' END,lot.created_at,lot.created_at,
       CASE WHEN lot.source_type='ADJUSTMENT' THEN 'MANUAL_ADJUSTMENT' ELSE 'SYSTEM' END,
       COALESCE(NULLIF(lot.source_reference,''),'legacy-lot:' || lot.id::text)
FROM credit_lots lot
JOIN wallets wallet ON wallet.id=lot.wallet_id
WHERE NOT EXISTS (SELECT 1 FROM entitlement_components component WHERE component.credit_lot_id=lot.id);

INSERT INTO entitlement_components(
    id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,
    credited_at,expires_at,status,created_at,updated_at
)
SELECT lot.id,lot.id,CASE WHEN lot.source_type='ADJUSTMENT' THEN 'BONUS' ELSE 'BASE' END,lot.id,
       lot.original_microcredits,lot.remaining_microcredits,lot.created_at,lot.expires_at,
       CASE WHEN lot.remaining_microcredits=0 THEN 'CONSUMED' ELSE 'ACTIVE' END,lot.created_at,lot.created_at
FROM credit_lots lot
WHERE NOT EXISTS (SELECT 1 FROM entitlement_components component WHERE component.credit_lot_id=lot.id);

ALTER TABLE usage_hold_allocations ADD COLUMN component_id UUID REFERENCES entitlement_components(id);
UPDATE usage_hold_allocations allocation
SET component_id = component.id
FROM entitlement_components component
WHERE component.package_id=allocation.package_id;
ALTER TABLE usage_hold_allocations ALTER COLUMN component_id SET NOT NULL;
ALTER TABLE usage_hold_allocations DROP CONSTRAINT usage_hold_allocations_pkey;
ALTER TABLE usage_hold_allocations ADD PRIMARY KEY (hold_id, component_id);
CREATE INDEX usage_hold_allocations_component_idx ON usage_hold_allocations(component_id,hold_id);

CREATE TABLE usage_loss_reviews (
    id UUID PRIMARY KEY,
    usage_request_id UUID NOT NULL,
    hold_id UUID NOT NULL UNIQUE REFERENCES usage_holds(id),
    wallet_id UUID NOT NULL REFERENCES wallets(id),
    reason_code TEXT NOT NULL CHECK (reason_code IN ('ENTITLEMENT_EXPIRED_UNCERTAIN')),
    claimed_actual_microcredits BIGINT CHECK (claimed_actual_microcredits IS NULL OR claimed_actual_microcredits >= 0),
    usage_snapshot JSONB,
    status TEXT NOT NULL CHECK (status IN ('REVIEW_REQUIRED', 'RESOLVED_PLATFORM_LOSS')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE OR REPLACE FUNCTION project_entitlement_package_components() RETURNS trigger AS $$
DECLARE
    target_package UUID;
BEGIN
    target_package := CASE WHEN TG_OP='DELETE' THEN OLD.package_id ELSE NEW.package_id END;
    UPDATE entitlement_packages package SET
        points_total = totals.original,
        points_remaining = totals.available + totals.active_grant + totals.suspended_grant,
        points_reserved = totals.reserved,
        points_used = totals.used,
        points_expired = totals.expired,
        points_revoked = totals.revoked,
        updated_at = GREATEST(package.updated_at, totals.updated_at)
    FROM (
        SELECT COALESCE(sum(original_microcredits),0) original,
               COALESCE(sum(available_microcredits),0) available,
               COALESCE(sum(reserved_microcredits),0) reserved,
               COALESCE(sum(used_microcredits),0) used,
               COALESCE(sum(active_grant_microcredits),0) active_grant,
               COALESCE(sum(suspended_grant_microcredits),0) suspended_grant,
               COALESCE(sum(expired_microcredits),0) expired,
               COALESCE(sum(revoked_microcredits),0) revoked,
               COALESCE(max(updated_at),CURRENT_TIMESTAMP) updated_at
        FROM entitlement_components WHERE package_id=target_package
    ) totals
    WHERE package.id=target_package AND totals.original>0;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER entitlement_components_project_package
AFTER INSERT OR UPDATE OR DELETE ON entitlement_components
FOR EACH ROW EXECUTE FUNCTION project_entitlement_package_components();

CREATE INDEX entitlement_components_consumption_idx
    ON entitlement_components(expires_at,component_type,credited_at,id)
    WHERE status='ACTIVE' AND available_microcredits>0;
