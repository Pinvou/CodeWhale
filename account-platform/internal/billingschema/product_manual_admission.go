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
func prepareManualProductAdmissionMigration(ctx context.Context, tx pgx.Tx, m migrations.Migration) error {
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
