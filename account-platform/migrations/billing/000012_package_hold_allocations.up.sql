CREATE TABLE usage_hold_allocations (
    hold_id UUID NOT NULL REFERENCES usage_holds(id),
    package_id UUID NOT NULL REFERENCES entitlement_packages(id),
    reserved_microcredits BIGINT NOT NULL CHECK (reserved_microcredits > 0),
    settled_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (settled_microcredits >= 0),
    released_microcredits BIGINT NOT NULL DEFAULT 0 CHECK (released_microcredits >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (hold_id, package_id),
    CHECK (settled_microcredits + released_microcredits <= reserved_microcredits)
);

CREATE INDEX usage_hold_allocations_package_idx
    ON usage_hold_allocations (package_id, hold_id);
