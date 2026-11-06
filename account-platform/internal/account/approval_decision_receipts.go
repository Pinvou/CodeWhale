package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrApprovalDecisionUnconfirmed = errors.New("the original approval decision is not confirmed")

type approvalDecisionProtocolKey struct{}
type approvalDecisionCommand struct{ Decision, RawReason, ReasonDigest string }

// Only a decoded new HTTP command supplies these original wire facts. Its raw
// reason digest is separate from the existing domain's normalized reason.
func withApprovalDecisionProtocol(ctx context.Context, decision, reason string) context.Context {
	digest := sha256.Sum256([]byte(reason))
	return context.WithValue(ctx, approvalDecisionProtocolKey{}, approvalDecisionCommand{decision, reason, hex.EncodeToString(digest[:])})
}
func approvalDecisionProtocol(ctx context.Context) (approvalDecisionCommand, bool) {
	command, ok := ctx.Value(approvalDecisionProtocolKey{}).(approvalDecisionCommand)
	return command, ok
}

type ApprovalDecisionReceipt struct {
	ApprovalRequestID          uuid.UUID `json:"approval_request_id"`
	CommandProtocol            int       `json:"command_protocol"`
	Operation                  string    `json:"operation"`
	Decision                   string    `json:"decision"`
	ExpectedVersion            int64     `json:"expected_version"`
	ResultVersion              int64     `json:"result_version"`
	OriginActionControlVersion int64     `json:"origin_action_control_version"`
	ActionControlVersion       int64     `json:"action_control_version"`
	ReasonDigest               string    `json:"reason_digest"`
	ReceiptEventID             uuid.UUID `json:"receipt_event_id"`
	DecisionID                 uuid.UUID `json:"decision_id"`
	RecordedAt                 time.Time `json:"recorded_at"`
}

func approvalDecisionEventDetails(ctx context.Context, item ApprovalRequest, expected int64, decision, reason string, decisionID uuid.UUID) (map[string]any, error) {
	details := map[string]any{"reason": reason}
	command, protocol := approvalDecisionProtocol(ctx)
	if !protocol {
		return details, nil
	}
	control := approvalV2ControlVersion(ctx)
	if command.Decision != decision || strings.TrimSpace(command.RawReason) != reason || control < 1 || expected < 1 || expected == math.MaxInt64 || decisionID == uuid.Nil {
		return nil, ErrConflict
	}
	fingerprint, err := approvalRequestOriginalFingerprint(item)
	if err != nil {
		return nil, err
	}
	details["decision_command_protocol"] = 1
	details["decision_expected_version"] = strconv.FormatInt(expected, 10)
	details["decision_original_fingerprint"] = hex.EncodeToString(fingerprint[:])
	details["decision_wire_decision"] = decision
	details["decision_reason_digest"] = command.ReasonDigest
	details["decision_action_control_version"] = strconv.FormatInt(control, 10)
	details["decision_result_version"] = strconv.FormatInt(expected+1, 10)
	details["decision_id"] = decisionID.String()
	return details, nil
}

func validApprovalDecisionReceiptFacts(expected, origin int64, decision, reasonDigest string) bool {
	if expected < 1 || expected == math.MaxInt64 || origin < 1 || (decision != "APPROVED" && decision != "REJECTED") || len(reasonDigest) != 64 {
		return false
	}
	digest, err := hex.DecodeString(reasonDigest)
	return err == nil && len(digest) == sha256.Size && hex.EncodeToString(digest) == reasonDigest
}

// A decision's request can already be executing or executed. Only its exact
// immutable original event/decision tuple proves this operation, never the
// request's current terminal label or an assumed N+1 current version.
func readApprovalDecisionReceipt(ctx context.Context, tx pgx.Tx, item ApprovalRequest, actor uuid.UUID, expected, origin, current int64, decision, reasonDigest string) (ApprovalDecisionReceipt, error) {
	if !validApprovalDecisionReceiptFacts(expected, origin, decision, reasonDigest) || current < 1 || item.Version < expected+1 {
		return ApprovalDecisionReceipt{}, ErrApprovalDecisionUnconfirmed
	}
	fingerprint, err := approvalRequestOriginalFingerprint(item)
	if err != nil {
		return ApprovalDecisionReceipt{}, err
	}
	var eventID, decisionID uuid.UUID
	var at time.Time
	err = tx.QueryRow(ctx, `SELECT e.id,d.id,e.occurred_at
FROM approval_state_events e JOIN approval_decisions d
ON d.id::text=e.details->>'decision_id' AND d.approval_request_id=e.approval_request_id
WHERE e.approval_request_id=$1 AND e.actor_type='USER' AND e.actor_id=$2
AND e.from_status='SUBMITTED' AND e.to_status=$3 AND e.reason_code='INDEPENDENT_DECISION'
AND d.approver_user_id=$4 AND d.decision=$3 AND e.details->>'reason'=d.reason
AND e.details->>'decision_command_protocol'='1'
AND e.details->>'decision_expected_version'=$5 AND e.details->>'decision_original_fingerprint'=$6
AND e.details->>'decision_wire_decision'=$3 AND e.details->>'decision_reason_digest'=$7
AND e.details->>'decision_action_control_version'=$8 AND e.details->>'decision_result_version'=$9
ORDER BY e.occurred_at,e.id LIMIT 1`, item.ID, actor.String(), decision, actor, strconv.FormatInt(expected, 10), hex.EncodeToString(fingerprint[:]), reasonDigest, strconv.FormatInt(origin, 10), strconv.FormatInt(expected+1, 10)).Scan(&eventID, &decisionID, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApprovalDecisionReceipt{}, ErrApprovalDecisionUnconfirmed
	}
	if err != nil {
		return ApprovalDecisionReceipt{}, err
	}
	return ApprovalDecisionReceipt{item.ID, 1, "DECIDE", decision, expected, expected + 1, origin, current, reasonDigest, eventID, decisionID, at}, nil
}

func (r *PostgresRepository) approvalDecisionResult(ctx context.Context, principal identity.Principal, requestID uuid.UUID, expected, origin int64, decision, reasonDigest string) (ApprovalDecisionReceipt, ApprovalRequest, error) {
	empty := ApprovalDecisionReceipt{}
	noItem := ApprovalRequest{}
	if !humanApprovalPrincipal(principal) {
		return empty, noItem, ErrForbidden
	}
	if approvalV2ControlVersion(ctx) < 1 || !validApprovalDecisionReceiptFacts(expected, origin, decision, reasonDigest) {
		return empty, noItem, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return empty, noItem, err
	}
	defer tx.Rollback(ctx)
	actor, err := adminActorID(ctx, tx, principal)
	if err != nil || actor == nil {
		return empty, noItem, ErrForbidden
	}
	if _, err = approvalRequestVisibilityForActor(ctx, tx, *actor, requestID); err != nil {
		return empty, noItem, err
	}
	// This shared current-control lock authorizes a read of an old fact. It
	// never grants the capability to advance an approval under that old epoch.
	if err = requireApprovalDraftCloseControl(ctx, tx); err != nil {
		return empty, noItem, err
	}
	item, err := approvalRequestByIDForUpdate(ctx, tx, requestID)
	if err != nil {
		return empty, noItem, approvalVisibleRequestReadError(err)
	}
	if item.RequesterUserID == *actor {
		return empty, noItem, ErrApprovalSelfDecision
	}
	policy, err := approvalPolicyForItem(item)
	if err != nil {
		return empty, noItem, ErrApprovalInvalidated
	}
	if _, _, _, err = strongPlatformAuthorization(ctx, tx, *actor, policy.ApproverPermission); err != nil {
		return empty, noItem, err
	}
	control, err := ReadAdminApprovalControl(ctx, tx)
	if err != nil {
		return empty, noItem, err
	}
	receipt, err := readApprovalDecisionReceipt(ctx, tx, item, *actor, expected, origin, control.Version, decision, reasonDigest)
	if err != nil {
		return empty, noItem, err
	}
	return receipt, item, tx.Commit(ctx)
}

func (r *PostgresRepository) ApprovalDecisionResult(ctx context.Context, principal identity.Principal, requestID uuid.UUID, expected, origin int64, decision, reasonDigest string) (ApprovalDecisionReceipt, error) {
	receipt, _, err := r.approvalDecisionResult(ctx, principal, requestID, expected, origin, decision, reasonDigest)
	return receipt, err
}
