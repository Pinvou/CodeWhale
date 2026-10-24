package billing

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type RechargeProductTransitionCommand struct {
	CatalogItemID                              uuid.UUID
	ExpectedVersion                            int64
	Target                                     string
	EvidenceID                                 *uuid.UUID
	IdempotencyKey, RequestHash, Actor, Reason string
}

// ExecuteRechargeProductTransition commits lifecycle/admission effects with
// their original response and audit. It never repairs a previously committed
// state by inventing a receipt after the fact.
func (r *PostgresRepository) ExecuteRechargeProductTransition(ctx context.Context, command RechargeProductTransitionCommand) (RechargeProductCatalogItem, error) {
	command.Target = strings.ToUpper(strings.TrimSpace(command.Target))
	kind := map[string]string{"ON_SALE": "BILLING_RECHARGE_PRODUCT_PUBLISH", "OFF_SALE": "BILLING_RECHARGE_PRODUCT_UNPUBLISH", "DELETED": "BILLING_RECHARGE_PRODUCT_DELETE"}[command.Target]
	if kind == "" {
		return RechargeProductCatalogItem{}, ErrConflict
	}
	meta := rechargeProductAtomicCommand{CatalogItemID: command.CatalogItemID, ExpectedVersion: command.ExpectedVersion, Kind: kind, IdempotencyKey: command.IdempotencyKey, RequestHash: command.RequestHash, Actor: command.Actor, Reason: command.Reason, Status: 200}
	return executeRechargeProductAtomicCommand(ctx, r, meta, func(tx pgx.Tx) (RechargeProductCatalogItem, int64, error) {
		item, err := r.transitionRechargeProduct(ctx, tx, command.CatalogItemID, command.ExpectedVersion, command.Target, command.EvidenceID)
		return item, item.Version, err
	})
}
