package account

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Only the explicitly validated new HTTP command carries this context. Old
// events and old pending browser facts cannot be promoted into this protocol.
type approvalWithdrawProtocolKey struct{}

func withApprovalWithdrawProtocol(ctx context.Context) context.Context {
	return context.WithValue(ctx, approvalWithdrawProtocolKey{}, true)
}

func approvalWithdrawProtocol(ctx context.Context) bool {
	value, _ := ctx.Value(approvalWithdrawProtocolKey{}).(bool)
	return value
}

// This receipt identifies a committed safe-close, not permission for a new
// action. The full intent fingerprint remains private in the immutable event.
type ApprovalWithdrawalReceipt struct {
	ApprovalRequestID          uuid.UUID `json:"approval_request_id"`
	CommandProtocol            int       `json:"command_protocol"`
	Operation                  string    `json:"operation"`
	ExpectedVersion            int64     `json:"expected_version"`
	ResultVersion              int64     `json:"result_version"`
	OriginActionControlVersion int64     `json:"origin_action_control_version"`
	ActionControlVersion       int64     `json:"action_control_version"`
	ReceiptEventID             uuid.UUID `json:"receipt_event_id"`
	Status                     string    `json:"status"`
	RecordedAt                 time.Time `json:"recorded_at"`
}

func approvalWithdrawalEventDetails(item ApprovalRequest, expectedVersion, controlVersion int64) (map[string]any, error) {
	if expectedVersion < 1 || expectedVersion == math.MaxInt64 || controlVersion < 1 {
		return nil, ErrConflict
	}
	fingerprint, err := approvalRequestOriginalFingerprint(item)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"withdraw_command_protocol":       1,
		"withdraw_expected_version":       strconv.FormatInt(expectedVersion, 10),
		"withdraw_original_fingerprint":   hex.EncodeToString(fingerprint[:]),
		"withdraw_action_control_version": strconv.FormatInt(controlVersion, 10),
		"withdraw_result_version":         strconv.FormatInt(expectedVersion+1, 10),
	}, nil
}

func readApprovalWithdrawalReceipt(ctx context.Context, tx pgx.Tx, item ApprovalRequest, actorID uuid.UUID, expectedVersion, originControl, currentControl int64) (ApprovalWithdrawalReceipt, error) {
	if expectedVersion < 1 || expectedVersion == math.MaxInt64 || originControl < 1 || currentControl < 1 ||
		item.RequesterUserID != actorID || item.Status != "WITHDRAWN" || item.Version != expectedVersion+1 {
		return ApprovalWithdrawalReceipt{}, ErrConflict
	}
	fingerprint, err := approvalRequestOriginalFingerprint(item)
	if err != nil {
		return ApprovalWithdrawalReceipt{}, err
	}
	var eventID uuid.UUID
	var at time.Time
	err = tx.QueryRow(ctx, `SELECT id,occurred_at FROM approval_state_events
WHERE approval_request_id=$1 AND actor_type='USER' AND actor_id=$2
AND from_status='SUBMITTED' AND to_status='WITHDRAWN' AND reason_code='WITHDRAWN_BY_REQUESTER'
AND details->>'withdraw_command_protocol'='1'
AND details->>'withdraw_expected_version'=$3 AND details->>'withdraw_original_fingerprint'=$4
AND details->>'withdraw_action_control_version'=$5 AND details->>'withdraw_result_version'=$6
ORDER BY occurred_at,id LIMIT 1`, item.ID, actorID.String(), strconv.FormatInt(expectedVersion, 10), hex.EncodeToString(fingerprint[:]), strconv.FormatInt(originControl, 10), strconv.FormatInt(expectedVersion+1, 10)).Scan(&eventID, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApprovalWithdrawalReceipt{}, ErrConflict
	}
	if err != nil {
		return ApprovalWithdrawalReceipt{}, err
	}
	return ApprovalWithdrawalReceipt{
		ApprovalRequestID: item.ID, CommandProtocol: 1, Operation: "WITHDRAW",
		ExpectedVersion: expectedVersion, ResultVersion: expectedVersion + 1,
		OriginActionControlVersion: originControl, ActionControlVersion: currentControl,
		ReceiptEventID: eventID, Status: "WITHDRAWN", RecordedAt: at,
	}, nil
}

// Confirmation never calls the transition method or substitutes a new write
// version. It uses current authority/control to read the exact original event.
func (r *PostgresRepository) ApprovalWithdrawalResult(ctx context.Context, principal identity.Principal, requestID uuid.UUID, expectedVersion, originControl int64) (ApprovalWithdrawalReceipt, error) {
	if !humanApprovalPrincipal(principal) {
		return ApprovalWithdrawalReceipt{}, ErrForbidden
	}
	if approvalV2ControlVersion(ctx) < 1 {
		return ApprovalWithdrawalReceipt{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return ApprovalWithdrawalReceipt{}, err
	}
	defer tx.Rollback(ctx)
	actor, err := adminActorID(ctx, tx, principal)
	if err != nil || actor == nil {
		return ApprovalWithdrawalReceipt{}, ErrForbidden
	}
	if _, err := approvalRequestVisibilityForActor(ctx, tx, *actor, requestID); err != nil {
		return ApprovalWithdrawalReceipt{}, err
	}
	if err := requireApprovalDraftCloseControl(ctx, tx); err != nil {
		return ApprovalWithdrawalReceipt{}, err
	}
	item, err := approvalRequestByIDForUpdate(ctx, tx, requestID)
	if err != nil {
		return ApprovalWithdrawalReceipt{}, approvalVisibleRequestReadError(err)
	}
	if item.RequesterUserID != *actor {
		return ApprovalWithdrawalReceipt{}, ErrForbidden
	}
	if _, _, _, err := strongPlatformAuthorization(ctx, tx, *actor, item.RequesterPermission); err != nil {
		return ApprovalWithdrawalReceipt{}, err
	}
	control, err := ReadAdminApprovalControl(ctx, tx)
	if err != nil {
		return ApprovalWithdrawalReceipt{}, err
	}
	receipt, err := readApprovalWithdrawalReceipt(ctx, tx, item, *actor, expectedVersion, originControl, control.Version)
	if err != nil {
		return ApprovalWithdrawalReceipt{}, err
	}
	return receipt, tx.Commit(ctx)
}
