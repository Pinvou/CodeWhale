package billing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The caller holds catalog.admission. A historical evidence fact cannot retire
// a newer binding or affect its control/version. Lock the catalogue before the
// binding, matching payment creation and renewal. Provider identity comes from
// the affected immutable mapping, never a possibly newer editable draft.
func (r *PostgresRepository) retireCurrentProductAdmission(ctx context.Context, tx pgx.Tx, catalogID, evidenceID uuid.UUID, reasonCode, reason string, now time.Time) (bool, error) {
	var productID string
	if err := tx.QueryRow(ctx, `SELECT product_id FROM recharge_product_catalog_items WHERE catalog_item_id=$1 FOR UPDATE`, catalogID).Scan(&productID); err != nil {
		return false, err
	}
	var mappingID *uuid.UUID
	err := tx.QueryRow(ctx, `UPDATE product_admission_bindings SET effective_to=$3 WHERE catalog_item_id=$1 AND evidence_id=$2 AND effective_to IS NULL RETURNING mapping_id`, catalogID, evidenceID, now).Scan(&mappingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if mappingID != nil {
		if _, err = tx.Exec(ctx, `INSERT INTO payment_product_controls(provider_id,provider_version,product_id,new_orders_enabled,reason_code,reason,version,updated_at)
SELECT provider_id,provider_version,product_id,FALSE,$3,$4,1,$5 FROM payment_product_mappings WHERE id=$1 AND product_id=$2
ON CONFLICT(provider_id,provider_version,product_id) DO UPDATE SET new_orders_enabled=FALSE,reason_code=EXCLUDED.reason_code,reason=EXCLUDED.reason,version=payment_product_controls.version+1,updated_at=EXCLUDED.updated_at`, *mappingID, productID, reasonCode, reason, now); err != nil {
			return false, err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE recharge_product_catalog_items SET admission_version=admission_version+1,updated_at=$2 WHERE catalog_item_id=$1`, catalogID, now)
	return err == nil, err
}

// A worker may not have observed an invalid current binding yet. Renewing it
// must not silently restore sale before PAY-46 review. The lock is already held;
// use the database wall clock after lock acquisition, not a transaction's earlier
// timestamp. An unexpired, unrevoked binding still renews without interruption.
func (r *PostgresRepository) retireInvalidProductAdmissionBeforeRenewal(ctx context.Context, tx pgx.Tx, catalogID uuid.UUID, now time.Time) error {
	var evidenceID uuid.UUID
	var revoked bool
	err := tx.QueryRow(ctx, `SELECT evidence.evidence_id,EXISTS(SELECT 1 FROM product_admission_revocations revocation WHERE revocation.evidence_id=evidence.evidence_id)
FROM product_admission_bindings binding JOIN product_admission_evidence evidence USING(evidence_id)
WHERE binding.catalog_item_id=$1 AND binding.effective_to IS NULL
 AND (evidence.expires_at<=clock_timestamp() OR EXISTS(SELECT 1 FROM product_admission_revocations revocation WHERE revocation.evidence_id=evidence.evidence_id))`, catalogID).Scan(&evidenceID, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	reasonCode, reason := "AUTOMATIC_STOP_LOSS", "PRODUCT_ADMISSION_EXPIRED"
	if revoked {
		reasonCode, reason = "MANUAL_SAFETY_PAUSE", "PRODUCT_ADMISSION_REVOKED"
	}
	_, err = r.retireCurrentProductAdmission(ctx, tx, catalogID, evidenceID, reasonCode, reason, now)
	return err
}
