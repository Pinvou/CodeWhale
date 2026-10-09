CREATE TABLE billing_financial_fact_revisions (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    revision BIGINT NOT NULL CHECK (revision > 0),
    updated_at TIMESTAMPTZ NOT NULL
);

INSERT INTO billing_financial_fact_revisions(singleton,revision,updated_at)
VALUES(TRUE,1,transaction_timestamp());

CREATE OR REPLACE FUNCTION bump_billing_financial_fact_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE billing_financial_fact_revisions
       SET revision=revision+1,updated_at=transaction_timestamp()
     WHERE singleton;
    RETURN COALESCE(NEW,OLD);
END $$;

CREATE TRIGGER billing_financial_fact_revision_prices
AFTER INSERT OR UPDATE OR DELETE ON pricing_versions
FOR EACH ROW EXECUTE FUNCTION bump_billing_financial_fact_revision();

CREATE TRIGGER billing_financial_fact_revision_configs
AFTER INSERT OR UPDATE OR DELETE ON pricing_version_unit_configs
FOR EACH ROW EXECUTE FUNCTION bump_billing_financial_fact_revision();

CREATE TRIGGER billing_financial_fact_revision_bindings
AFTER INSERT OR UPDATE OR DELETE ON billing_protocol_formula_bindings
FOR EACH ROW EXECUTE FUNCTION bump_billing_financial_fact_revision();

CREATE TRIGGER billing_financial_fact_revision_units
AFTER INSERT OR UPDATE OR DELETE ON metering_unit_definitions
FOR EACH ROW EXECUTE FUNCTION bump_billing_financial_fact_revision();

CREATE TRIGGER billing_financial_fact_revision_blocks
AFTER INSERT OR UPDATE OR DELETE ON pricing_blocks
FOR EACH ROW EXECUTE FUNCTION bump_billing_financial_fact_revision();

CREATE TRIGGER billing_financial_fact_revision_spaces
AFTER INSERT OR UPDATE OR DELETE ON pricing_space_versions
FOR EACH ROW EXECUTE FUNCTION bump_billing_financial_fact_revision();
