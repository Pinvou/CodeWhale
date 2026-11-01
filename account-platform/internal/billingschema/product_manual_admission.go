package billingschema

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

const manualProductAdmissionVersion int64 = 900038
const manualProductAdmissionChecksum = "458f58680bc1ff48ceb59ec274329e429d5806a07000e0aae3c9551136eaaa70"
const manualProductAdmissionDownChecksum = "42f86b8e215847c398055a90a8f89d1a02c91fc93d95b2f86f3f03f0aa16cafe"

func validateManualProductAdmission(input []migrations.Migration) error {
	for _, m := range input {
		if (m.Name == "product_manual_admission" || m.Checksum == manualProductAdmissionChecksum) && m.Version != manualProductAdmissionVersion {
			return errors.New("manual product admission migration version mismatch")
		}
		if m.Version == manualProductAdmissionVersion && (m.Name != "product_manual_admission" || m.Checksum != manualProductAdmissionChecksum || sqlChecksum(m.UpSQL) != manualProductAdmissionChecksum || sqlChecksum(m.DownSQL) != manualProductAdmissionDownChecksum) {
			return errors.New("manual product admission migration identity mismatch")
		}
	}
	return nil
}

// The frozen historical hardener rejects every unrecognized SECURITY DEFINER.
// Later delivery of an older migration still runs that hardener. Temporarily
// restore only the exact installed retention trigger to its original invoker
// mode; finishBillingMigration restores definer mode before this transaction
// commits. Other sessions never observe the temporary mode or DDL authority.
const manualProductAdmissionLegacyChecksum = "0b9d6e695cdbf06c9d744b29c49fd24d41f06b371af1b42e00c1398c3952ffac"

const manualRetentionBodyChecksum = "c30ecde57219aabdba9fd06758e28947926c194bdbb40a7cd523330a7c473b51"

func prepareManualRetentionHardening(ctx context.Context, tx pgx.Tx) error {
	var installed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=$1 AND checksum IN ($2,$3))`, manualProductAdmissionVersion, manualProductAdmissionChecksum, manualProductAdmissionLegacyChecksum).Scan(&installed); err != nil {
		return err
	}
	if !installed {
		return nil
	}
	var body, owner string
	var trigger, definer bool
	if err := tx.QueryRow(ctx, `SELECT p.prosrc,pg_get_userbyid(p.proowner),p.prorettype='pg_catalog.trigger'::regtype AND p.prokind='f' AND p.pronargs=0,p.prosecdef FROM pg_catalog.pg_proc p WHERE p.oid=to_regprocedure('public.purge_product_manual_admission_facts()')`).Scan(&body, &owner, &trigger, &definer); err != nil {
		return err
	}
	if owner != "billing_schema_owner" || !trigger || !definer || sqlChecksum(body) != manualRetentionBodyChecksum {
		return errors.New("manual retention hardening requires the exact installed owner trigger")
	}
	_, err := tx.Exec(ctx, `SET ROLE billing_schema_owner; ALTER FUNCTION public.purge_product_manual_admission_facts() SECURITY INVOKER; SET ROLE billing_service`)
	return err
}

func prepareManualProductAdmissionMigration(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if err := prepareManualRetentionHardening(ctx, tx); err != nil {
		return err
	}
	if m.Version != manualProductAdmissionVersion {
		return nil
	}
	if err := validateManualProductAdmission([]migrations.Migration{m}); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SET ROLE billing_schema_owner; GRANT REFERENCES(material_id),TRIGGER ON public.product_admission_materials TO billing_service; SET ROLE billing_service`)
	return err
}
func finishManualProductAdmissionMigration(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
	if m.Version != manualProductAdmissionVersion {
		return nil
	}
	_, err := tx.Exec(ctx, `SET ROLE billing_schema_owner; REVOKE REFERENCES(material_id),TRIGGER ON public.product_admission_materials FROM billing_service; SET ROLE billing_service`)
	return err
}
