package billing

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type RechargeProductAdmissionCommand struct {
	CatalogItemID, VerificationID              uuid.UUID
	ExpectedVersion                            int64
	RetryOf                                    *uuid.UUID
	IdempotencyKey, RequestHash, Actor, Reason string
}

type RechargeProductAdmissionRevokeResult struct {
	Status     string    `json:"status"`
	EvidenceID uuid.UUID `json:"evidence_id"`
}

// Admission and the catalogue version are locked in their existing order.
// The check runs only after original-key replay has been considered.
func lockProductAdmissionCommand(ctx context.Context, tx pgx.Tx, id uuid.UUID, expected int64) error {
	if err := lockTransactionKey(ctx, tx, "catalog.admission", id.String()); err != nil {
		return err
	}
	var version int64
	if err := tx.QueryRow(ctx, `SELECT version FROM recharge_product_catalog_items WHERE catalog_item_id=$1 FOR UPDATE`, id).Scan(&version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if version != expected {
		return ErrConflict
	}
	return nil
}

func (r *PostgresRepository) ExecuteProductAdmissionVerification(ctx context.Context, command RechargeProductAdmissionCommand) (ProductAdmissionVerification, error) {
	actor, err := uuid.Parse(strings.TrimSpace(command.Actor))
	if err != nil {
		return ProductAdmissionVerification{}, ErrConflict
	}
	meta := rechargeProductAtomicCommand{CatalogItemID: command.CatalogItemID, ExpectedVersion: command.ExpectedVersion, Kind: "BILLING_RECHARGE_PRODUCT_ADMISSION_VERIFY", IdempotencyKey: command.IdempotencyKey, RequestHash: command.RequestHash, Actor: command.Actor, Reason: command.Reason, Status: 202}
	return executeRechargeProductAtomicCommand(ctx, r, meta, func(tx pgx.Tx) (ProductAdmissionVerification, int64, error) {
		if err := lockProductAdmissionCommand(ctx, tx, command.CatalogItemID, command.ExpectedVersion); err != nil {
			return ProductAdmissionVerification{}, 0, err
		}
		item, _, err := r.startProductAdmissionVerification(ctx, tx, command.CatalogItemID, actor, command.RetryOf)
		return item, command.ExpectedVersion, err
	})
}

func (r *PostgresRepository) ExecuteProductAdmissionRevoke(ctx context.Context, command RechargeProductAdmissionCommand) (RechargeProductAdmissionRevokeResult, error) {
	if command.VerificationID == uuid.Nil {
		return RechargeProductAdmissionRevokeResult{}, ErrConflict
	}
	meta := rechargeProductAtomicCommand{CatalogItemID: command.CatalogItemID, ExpectedVersion: command.ExpectedVersion, Kind: "BILLING_RECHARGE_PRODUCT_ADMISSION_REVOKE", IdempotencyKey: command.IdempotencyKey, RequestHash: command.RequestHash, Actor: command.Actor, Reason: command.Reason, Status: 200}
	return executeRechargeProductAtomicCommand(ctx, r, meta, func(tx pgx.Tx) (RechargeProductAdmissionRevokeResult, int64, error) {
		if err := lockProductAdmissionCommand(ctx, tx, command.CatalogItemID, command.ExpectedVersion); err != nil {
			return RechargeProductAdmissionRevokeResult{}, 0, err
		}
		var evidenceID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT evidence_id FROM product_admission_evidence WHERE catalog_item_id=$1 AND verification_id=$2`, command.CatalogItemID, command.VerificationID).Scan(&evidenceID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return RechargeProductAdmissionRevokeResult{}, 0, ErrNotFound
			}
			return RechargeProductAdmissionRevokeResult{}, 0, err
		}
		if err := r.revokeProductAdmission(ctx, tx, evidenceID, "HUMAN", command.Actor, command.Reason); err != nil {
			return RechargeProductAdmissionRevokeResult{}, 0, err
		}
		return RechargeProductAdmissionRevokeResult{Status: "REVOKED", EvidenceID: evidenceID}, command.ExpectedVersion, nil
	})
}
