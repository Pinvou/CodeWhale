package account

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ApprovalActionControl reads the sole cluster gate. Action-bearing responses
// are versioned against this row, not against a process-local mode cache.
func (r *PostgresRepository) ApprovalActionControl(ctx context.Context) (AdminApprovalControl, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AdminApprovalControl{}, err
	}
	defer tx.Rollback(ctx)
	control, err := ReadAdminApprovalControl(ctx, tx)
	if err != nil {
		return AdminApprovalControl{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AdminApprovalControl{}, err
	}
	return control, nil
}

func approvalActionsForControl(actions []string, control AdminApprovalControl) ([]string, error) {
	mode, err := AdminApprovalActionsFor(control)
	if err != nil {
		return nil, err
	}
	filtered := make([]string, 0, len(actions))
	for _, action := range actions {
		switch action {
		case "EDIT", "SUBMIT":
			if mode.DraftWrites {
				filtered = append(filtered, action)
			}
		case "APPROVE":
			if mode.Approve {
				filtered = append(filtered, action)
			}
		case "REJECT", "WITHDRAW":
			if mode.SafeClose {
				filtered = append(filtered, action)
			}
		default:
			return nil, ErrAdminApprovalControlInvalid
		}
	}
	return filtered, nil
}

func (r *PostgresRepository) ApprovalRequestV2(ctx context.Context, principal identity.Principal, id uuid.UUID) (any, error) {
	// This current-actor visibility read precedes all presentation work.
	item, err := r.ApprovalRequest(ctx, principal, id)
	if err != nil {
		return nil, err
	}
	control, err := r.ApprovalActionControl(ctx)
	if err != nil {
		return nil, err
	}
	item.AllowedActions, err = approvalActionsForControl(item.AllowedActions, control)
	if err != nil {
		return nil, err
	}
	detail, err := approvalV2Detail(item, control.Version)
	if errors.Is(err, ErrApprovalDetailPresentationUnavailable) {
		summary := approvalV2Summary(item)
		summary.DetailAvailability = "DETAIL_PRESENTATION_UNAVAILABLE"
		return summary, nil
	}
	return detail, err
}
