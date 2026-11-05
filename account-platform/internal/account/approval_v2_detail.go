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
	if err != nil {
		return nil, err
	}
	detail.Execution, err = r.approvalV2Execution(ctx, item)
	if err != nil {
		return nil, err
	}
	return detail, err
}

func (r *PostgresRepository) approvalV2Execution(ctx context.Context, item ApprovalRequest) (*ApprovalV2Execution, error) {
	var version int64
	var status, failure *string
	err := r.pool.QueryRow(ctx, `SELECT ar.version,e.status,e.error_class
FROM approval_requests ar LEFT JOIN approval_executions e
  ON e.approval_request_id=ar.id AND e.command_type=ar.command_type
WHERE ar.id=$1`, item.ID).Scan(&version, &status, &failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrApprovalRequestNotFound
	}
	if err != nil {
		return nil, err
	}
	if version != item.Version {
		return nil, ErrConflict
	}
	if status == nil {
		return nil, nil
	}
	// No automatic retry is exposed by GSA-16: it delivers read-only
	// investigation plus the governed positive-result and proven-zero-effect
	// terminal paths, never a second domain attempt scheduled from an error.
	projection := &ApprovalV2Execution{AttemptStatus: *status, Retryable: false,
		ExecutionDeadline: item.ExecutionDeadline}
	if *status == "FAILED" {
		code := "EXECUTION_FAILED"
		projection.ErrorClass = &code
	}
	return projection, nil
}
