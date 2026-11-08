-- Price lifecycle reads require one authoritative ETag row for every pricing
-- space. Restore rows that may be absent when legacy price facts were imported
-- after the versioned-pricing migration had already run.
INSERT INTO pricing_space_versions(
    scope_type,
    scope_key,
    protocol_code,
    formula_version_id,
    credit_unit,
    etag,
    updated_at
)
SELECT
    scope_type,
    CASE WHEN scope_type='GLOBAL' THEN '*' ELSE scope_id END,
    protocol_code,
    formula_version_id,
    credit_unit,
    GREATEST(MAX(space_etag),1),
    MAX(created_at)
FROM pricing_versions
GROUP BY scope_type,scope_id,protocol_code,formula_version_id,credit_unit
ON CONFLICT (scope_type,scope_key,protocol_code,formula_version_id,credit_unit)
DO UPDATE SET
    etag=GREATEST(pricing_space_versions.etag,EXCLUDED.etag),
    updated_at=GREATEST(pricing_space_versions.updated_at,EXCLUDED.updated_at);

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pricing_versions price
        LEFT JOIN pricing_space_versions space
          ON space.scope_type=price.scope_type
         AND space.scope_key=CASE WHEN price.scope_type='GLOBAL' THEN '*' ELSE price.scope_id END
         AND space.protocol_code=price.protocol_code
         AND space.formula_version_id=price.formula_version_id
         AND space.credit_unit=price.credit_unit
        WHERE space.etag IS NULL OR space.etag<price.space_etag
    ) THEN
        RAISE EXCEPTION 'pricing space version invariant failed';
    END IF;
END $$;
