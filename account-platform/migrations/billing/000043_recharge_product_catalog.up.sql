CREATE TABLE recharge_product_catalog_items (
    catalog_item_id UUID PRIMARY KEY,
    product_id TEXT NOT NULL UNIQUE REFERENCES recharge_products(product_id),
    display_name_zh_cn TEXT NOT NULL,
    display_name_en TEXT NOT NULL,
    display_name_ja TEXT NOT NULL,
    display_order INTEGER NOT NULL DEFAULT 0,
    replacement_product_id TEXT REFERENCES recharge_products(product_id),
    lifecycle_status TEXT NOT NULL CHECK (lifecycle_status IN ('DRAFT','ON_SALE','OFF_SALE','DELETED')),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    admission_version BIGINT NOT NULL DEFAULT 0 CHECK (admission_version >= 0),
    ever_published BOOLEAN NOT NULL DEFAULT FALSE,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CHECK ((lifecycle_status='DELETED') = (deleted_at IS NOT NULL))
);

CREATE TABLE recharge_product_mapping_drafts (
    catalog_item_id UUID PRIMARY KEY REFERENCES recharge_product_catalog_items(catalog_item_id),
    draft_version BIGINT NOT NULL DEFAULT 1 CHECK (draft_version > 0),
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    external_sku_bom TEXT NOT NULL,
    amount_fen BIGINT NOT NULL CHECK (amount_fen > 0),
    currency TEXT NOT NULL,
    channels TEXT[] NOT NULL CHECK (cardinality(channels) > 0),
    admission_class TEXT NOT NULL CHECK (admission_class IN ('FORMAL','TEST_ONLY')),
    released_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX recharge_product_draft_active_sku_idx
    ON recharge_product_mapping_drafts(provider_id,provider_version,external_sku_bom)
    WHERE released_at IS NULL;

CREATE TABLE product_admission_verifications (
    verification_id UUID PRIMARY KEY,
    catalog_item_id UUID NOT NULL REFERENCES recharge_product_catalog_items(catalog_item_id),
    retry_of UUID REFERENCES product_admission_verifications(verification_id),
    target_mapping_id UUID REFERENCES payment_product_mappings(id),
    target_draft_version BIGINT,
    publish_fingerprint TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING','VERIFIED','FAILED')),
    failure_code TEXT,
    requested_by UUID,
    requested_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    CHECK ((target_mapping_id IS NULL) <> (target_draft_version IS NULL))
);

CREATE UNIQUE INDEX product_admission_pending_target_idx
    ON product_admission_verifications(catalog_item_id,publish_fingerprint)
    WHERE status='PENDING';

CREATE TABLE product_admission_evidence (
    evidence_id UUID PRIMARY KEY,
    verification_id UUID NOT NULL UNIQUE REFERENCES product_admission_verifications(verification_id),
    catalog_item_id UUID NOT NULL REFERENCES recharge_product_catalog_items(catalog_item_id),
    publish_fingerprint TEXT NOT NULL,
    issuer_workload TEXT NOT NULL,
    verification_method TEXT NOT NULL,
    source_reference TEXT NOT NULL,
    issued_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL CHECK (expires_at > issued_at),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE product_admission_bindings (
    catalog_item_id UUID NOT NULL REFERENCES recharge_product_catalog_items(catalog_item_id),
    admission_version BIGINT NOT NULL CHECK (admission_version > 0),
    evidence_id UUID NOT NULL REFERENCES product_admission_evidence(evidence_id),
    mapping_id UUID REFERENCES payment_product_mappings(id),
    effective_from TIMESTAMPTZ NOT NULL,
    effective_to TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(catalog_item_id,admission_version),
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);

CREATE UNIQUE INDEX product_admission_current_binding_idx
    ON product_admission_bindings(catalog_item_id) WHERE effective_to IS NULL;

CREATE TABLE product_admission_revocations (
    revocation_id UUID PRIMARY KEY,
    evidence_id UUID NOT NULL REFERENCES product_admission_evidence(evidence_id),
    revocation_sequence BIGINT NOT NULL CHECK (revocation_sequence > 0),
    source_type TEXT NOT NULL CHECK (source_type IN ('HUMAN','PROVIDER')),
    actor_subject TEXT NOT NULL,
    reason TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(evidence_id,revocation_sequence)
);

ALTER TABLE payment_orders
    ADD COLUMN provider_call_status TEXT NOT NULL DEFAULT 'COMPLETED'
        CHECK (provider_call_status IN ('NOT_STARTED','IN_PROGRESS','COMPLETED','RESULT_UNKNOWN','FAILED'));

-- Import the environment's existing facts without embedding a product, SKU,
-- amount, entitlement, or catalog cardinality in this migration.
INSERT INTO recharge_product_catalog_items(
    catalog_item_id,product_id,display_name_zh_cn,display_name_en,display_name_ja,
    display_order,lifecycle_status,version,ever_published,created_at,updated_at
)
SELECT gen_random_uuid(), product_id,
       COALESCE(NULLIF(plan_name,''),product_id), COALESCE(NULLIF(plan_name,''),product_id),
       COALESCE(NULLIF(plan_name,''),product_id),
       row_number() OVER (ORDER BY amount_fen,product_id),
       CASE WHEN enabled THEN 'ON_SALE' ELSE 'OFF_SALE' END,
       GREATEST(catalog_version,1), enabled, created_at, now()
FROM recharge_products product
WHERE EXISTS (
    SELECT 1 FROM payment_product_mappings mapping
    WHERE mapping.product_id=product.product_id
      AND mapping.effective_from<=now()
      AND (mapping.effective_to IS NULL OR mapping.effective_to>now())
      AND mapping.status IN ('FROZEN','TEST_ONLY')
);

INSERT INTO recharge_product_mapping_drafts(
    catalog_item_id,draft_version,provider_id,provider_version,external_sku_bom,
    amount_fen,currency,channels,admission_class,created_at,updated_at
)
SELECT item.catalog_item_id, mapping.mapping_version, mapping.provider_id,
       mapping.provider_version,mapping.external_sku,mapping.amount_fen,
       mapping.currency,mapping.channels,
       CASE mapping.status WHEN 'FROZEN' THEN 'FORMAL' ELSE 'TEST_ONLY' END,
       mapping.created_at,now()
FROM recharge_product_catalog_items item
JOIN LATERAL (
    SELECT candidate.* FROM payment_product_mappings candidate
    WHERE candidate.product_id=item.product_id
      AND candidate.effective_from<=now()
      AND (candidate.effective_to IS NULL OR candidate.effective_to>now())
      AND candidate.status IN ('FROZEN','TEST_ONLY')
    ORDER BY candidate.mapping_version DESC LIMIT 1
) mapping ON TRUE;

INSERT INTO product_admission_verifications(
    verification_id,catalog_item_id,target_mapping_id,target_draft_version,
    publish_fingerprint,status,requested_at,completed_at
)
SELECT gen_random_uuid(),item.catalog_item_id,mapping.id,NULL,
       md5(jsonb_build_object(
           'product_id',product.product_id,'name',product.plan_name,
           'base_points',product.base_points,'bonus_points',product.bonus_points,
           'validity_days',product.validity_days,'invoice',product.invoice_product_name,
           'provider_id',mapping.provider_id,'provider_version',mapping.provider_version,
           'external_sku_bom',mapping.external_sku,'amount_fen',mapping.amount_fen,
           'currency',mapping.currency,'channels',mapping.channels,'status',mapping.status
       )::text),
       'VERIFIED',now(),now()
FROM recharge_product_catalog_items item
JOIN recharge_products product ON product.product_id=item.product_id
JOIN recharge_product_mapping_drafts draft USING(catalog_item_id)
JOIN payment_product_mappings mapping
  ON mapping.product_id=item.product_id
 AND mapping.provider_id=draft.provider_id
 AND mapping.provider_version=draft.provider_version
 AND mapping.external_sku=draft.external_sku_bom
 AND mapping.mapping_version=draft.draft_version
WHERE item.lifecycle_status='ON_SALE'
  AND mapping.effective_from<=now()
  AND (mapping.effective_to IS NULL OR mapping.effective_to>now())
  AND mapping.status IN ('FROZEN','TEST_ONLY');

INSERT INTO product_admission_evidence(
    evidence_id,verification_id,catalog_item_id,publish_fingerprint,
    issuer_workload,verification_method,source_reference,issued_at,expires_at,created_at
)
SELECT gen_random_uuid(),verification_id,catalog_item_id,publish_fingerprint,
       'billing-catalog-migration','IMPORT_EXISTING_RUNTIME_FACT',
       'payment_product_mappings:'||target_mapping_id::text,requested_at,'infinity'::timestamptz,now()
FROM product_admission_verifications;

INSERT INTO product_admission_bindings(
    catalog_item_id,admission_version,evidence_id,mapping_id,effective_from,created_at
)
SELECT evidence.catalog_item_id,1,evidence.evidence_id,verification.target_mapping_id,now(),now()
FROM product_admission_evidence evidence
JOIN product_admission_verifications verification USING(verification_id);

UPDATE recharge_product_catalog_items item
   SET admission_version=1
 WHERE EXISTS(SELECT 1 FROM product_admission_bindings binding
              WHERE binding.catalog_item_id=item.catalog_item_id);

CREATE OR REPLACE FUNCTION enforce_recharge_product_projection() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE projected BOOLEAN;
BEGIN
    SELECT lifecycle_status='ON_SALE' INTO projected
      FROM recharge_product_catalog_items WHERE product_id=NEW.product_id;
    IF projected IS NOT NULL AND NEW.enabled IS DISTINCT FROM projected THEN
        RAISE EXCEPTION 'recharge_products.enabled is a managed catalog projection';
    END IF;
    RETURN NEW;
END $$;

CREATE CONSTRAINT TRIGGER recharge_product_projection_guard
AFTER INSERT OR UPDATE OF enabled ON recharge_products DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION enforce_recharge_product_projection();

CREATE OR REPLACE FUNCTION set_recharge_product_lifecycle(
    requested_catalog_item_id UUID,
    requested_expected_version BIGINT,
    requested_status TEXT,
    transition_at TIMESTAMPTZ DEFAULT clock_timestamp()
) RETURNS recharge_product_catalog_items
LANGUAGE plpgsql SECURITY DEFINER SET search_path=public,pg_temp AS $$
DECLARE item recharge_product_catalog_items;
BEGIN
    SELECT * INTO item FROM recharge_product_catalog_items
     WHERE catalog_item_id=requested_catalog_item_id FOR UPDATE;
    IF NOT FOUND OR item.version<>requested_expected_version THEN
        RAISE EXCEPTION 'catalog version conflict';
    END IF;
    IF NOT ((item.lifecycle_status='DRAFT' AND requested_status IN ('ON_SALE','DELETED')) OR
            (item.lifecycle_status='ON_SALE' AND requested_status='OFF_SALE') OR
            (item.lifecycle_status='OFF_SALE' AND requested_status IN ('ON_SALE','DELETED'))) THEN
        RAISE EXCEPTION 'invalid catalog lifecycle transition';
    END IF;
    IF requested_status='DELETED' AND item.lifecycle_status='ON_SALE' THEN
        RAISE EXCEPTION 'on-sale products cannot be deleted';
    END IF;
    UPDATE recharge_product_catalog_items
       SET lifecycle_status=requested_status, version=version+1,
           ever_published=ever_published OR requested_status='ON_SALE',
           deleted_at=CASE WHEN requested_status='DELETED' THEN transition_at ELSE NULL END,
           updated_at=transition_at
     WHERE catalog_item_id=requested_catalog_item_id RETURNING * INTO item;
    UPDATE recharge_products SET enabled=(requested_status='ON_SALE'),
           catalog_version=item.version WHERE product_id=item.product_id;
    RETURN item;
END $$;

REVOKE ALL ON FUNCTION set_recharge_product_lifecycle(UUID,BIGINT,TEXT,TIMESTAMPTZ) FROM PUBLIC;
