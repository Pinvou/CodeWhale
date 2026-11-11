package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrInvalidGrantRecovery = errors.New("invalid manual grant recovery evidence")
var ErrGrantRecoveryUnavailable = errors.New("manual grant recovery unavailable")
var grantEvidenceReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var grantEvidenceDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

type GrantRecoveryCase struct {
	ID                           uuid.UUID  `json:"id"`
	OriginalEventID              uuid.UUID  `json:"original_event_id"`
	Durable                      string     `json:"durable"`
	RequestID                    uuid.UUID  `json:"approval_request_id"`
	CommandType                  string     `json:"command_type"`
	PolicyVersion                int64      `json:"policy_version"`
	ApprovedRequestVersion       int64      `json:"approved_request_version"`
	ApprovalEpoch                int64      `json:"approval_epoch"`
	EventDigest                  string     `json:"event_digest"`
	CommandFingerprint           string     `json:"command_fingerprint"`
	Status                       string     `json:"status"`
	Version                      int64      `json:"version"`
	ProposalID                   *uuid.UUID `json:"proposal_id"`
	ProposerID                   *uuid.UUID `json:"proposer_id"`
	ProposerBindingID            *uuid.UUID `json:"proposer_binding_id"`
	ProposerAuthorizationVersion *int64     `json:"proposer_authorization_version"`
	ReviewerID                   *uuid.UUID `json:"reviewer_id"`
	LastCompletionID             *uuid.UUID `json:"last_completion_id"`
	CreatedAt                    time.Time  `json:"created_at"`
	UpdatedAt                    time.Time  `json:"updated_at"`
}

type GrantRecoveryObservation struct {
	State     string `json:"state"`
	Reference string `json:"reference"`
	SHA256    string `json:"sha256"`
}
type GrantRecoveryProposal struct {
	Authority                string    `json:"authority"`
	OriginalEventID          uuid.UUID `json:"original_event_id"`
	RequestID                uuid.UUID `json:"approval_request_id"`
	CommandFingerprint       string    `json:"command_fingerprint"`
	ApprovedRequestVersion   int64     `json:"approved_request_version"`
	ApprovalEpoch            int64     `json:"approval_epoch"`
	Kind                     string    `json:"kind"`
	Outcome                  string    `json:"outcome"`
	Reference                string    `json:"reference"`
	SHA256                   string    `json:"sha256"`
	CompletionFenceReference string    `json:"completion_fence_reference"`
	AllObservationsResolved  bool      `json:"all_observations_resolved"`
}
type GrantRecoveryReview struct {
	ProposalID            uuid.UUID `json:"proposal_id"`
	SHA256                string    `json:"sha256"`
	IndependentlyVerified bool      `json:"independently_verified"`
}
type GrantRecoveryMutation struct {
	OperationID     uuid.UUID       `json:"operation_id"`
	ExpectedVersion int64           `json:"expected_version"`
	Evidence        json.RawMessage `json:"evidence"`
}
type GrantRecoveryPage struct {
	Items []GrantRecoveryCase `json:"items"`
	Next  *uuid.UUID          `json:"next,omitempty"`
}
type GrantRecoveryDetail struct {
	Case        GrantRecoveryCase `json:"case"`
	Operations  []json.RawMessage `json:"operations"`
	NextVersion *int64            `json:"next_version,omitempty"`
}

func grantRecoveryPayload(action string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > 4096 || !uniqueRecoveryJSONKeys(raw) {
		return nil, ErrInvalidGrantRecovery
	}
	decode := func(value any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
			return ErrInvalidGrantRecovery
		}
		return nil
	}
	var value any
	switch action {
	case "OBSERVE":
		var e GrantRecoveryObservation
		if decode(&e) != nil || !grantEvidenceReference.MatchString(e.Reference) || !grantEvidenceDigest.MatchString(e.SHA256) {
			return nil, ErrInvalidGrantRecovery
		}
		switch e.State {
		case "ABSENT", "OBSERVED", "UNAVAILABLE", "EVIDENCE_CONFLICT":
		default:
			return nil, ErrInvalidGrantRecovery
		}
		value = e
	case "PROPOSE":
		var e GrantRecoveryProposal
		if decode(&e) != nil || e.Authority != "BILLING" || e.OriginalEventID == uuid.Nil || e.RequestID == uuid.Nil || !grantEvidenceDigest.MatchString(e.CommandFingerprint) || e.ApprovedRequestVersion < 1 || e.ApprovalEpoch < 1 || !grantEvidenceReference.MatchString(e.Reference) || !grantEvidenceReference.MatchString(e.CompletionFenceReference) || !grantEvidenceDigest.MatchString(e.SHA256) || !e.AllObservationsResolved {
			return nil, ErrInvalidGrantRecovery
		}
		if !(e.Kind == "AUTHORITATIVE_FINAL_RESULT" && (e.Outcome == "SUCCEEDED" || e.Outcome == "FAILED")) && !(e.Kind == "ACTION_ENDED_NO_EFFECT" && e.Outcome == "NO_EFFECT") {
			return nil, ErrInvalidGrantRecovery
		}
		value = e
	case "REVIEW":
		var e GrantRecoveryReview
		if decode(&e) != nil || e.ProposalID == uuid.Nil || !grantEvidenceDigest.MatchString(e.SHA256) || !e.IndependentlyVerified {
			return nil, ErrInvalidGrantRecovery
		}
		value = e
	default:
		return nil, ErrInvalidGrantRecovery
	}
	return json.Marshal(value)
}

func classifyGrantRecoveryError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "55000", "23505":
			return ErrConflict
		case "42501":
			return ErrForbidden
		case "22023":
			return ErrInvalidGrantRecovery
		case "P0002":
			return ErrNotFound
		case "42883", "42P01":
			return ErrGrantRecoveryUnavailable
		}
	}
	return err
}

func readGrantRecoveryCase(ctx context.Context, tx pgx.Tx, id uuid.UUID) (GrantRecoveryCase, error) {
	var raw []byte
	var item GrantRecoveryCase
	if err := tx.QueryRow(ctx, `SELECT public.read_approval_grant_recovery($1)`, id).Scan(&raw); err != nil {
		return item, classifyGrantRecoveryError(err)
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return item, err
	}
	return item, nil
}

func (r *PostgresRepository) GrantRecoveryCases(ctx context.Context, p identity.Principal, after *uuid.UUID, limit int) (GrantRecoveryPage, error) {
	if !humanApprovalPrincipal(p) {
		return GrantRecoveryPage{}, ErrForbidden
	}
	if limit < 1 || limit > 100 || (after != nil && *after == uuid.Nil) {
		return GrantRecoveryPage{}, ErrInvalidGrantRecovery
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := r.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return GrantRecoveryPage{}, err
	}
	defer tx.Rollback(bounded)
	actor, err := adminActorID(bounded, tx, p)
	if err != nil || actor == nil {
		return GrantRecoveryPage{}, ErrForbidden
	}
	if _, _, _, err = strongPlatformAuthorization(bounded, tx, *actor, "platform.billing.ledger.read"); err != nil {
		return GrantRecoveryPage{}, err
	}
	var cursor any
	if after != nil {
		cursor = *after
	}
	rows, err := tx.Query(bounded, `SELECT * FROM public.list_approval_grant_recovery($1,$2)`, cursor, limit+1)
	if err != nil {
		return GrantRecoveryPage{}, classifyGrantRecoveryError(err)
	}
	page := GrantRecoveryPage{Items: make([]GrantRecoveryCase, 0)}
	for rows.Next() {
		var raw []byte
		var item GrantRecoveryCase
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return GrantRecoveryPage{}, err
		}
		if err = json.Unmarshal(raw, &item); err != nil {
			rows.Close()
			return GrantRecoveryPage{}, err
		}
		if len(page.Items) < limit {
			page.Items = append(page.Items, item)
		} else {
			last := page.Items[len(page.Items)-1].ID
			page.Next = &last
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return GrantRecoveryPage{}, err
	}
	if err = tx.Commit(bounded); err != nil {
		return GrantRecoveryPage{}, err
	}
	return page, nil
}

func (r *PostgresRepository) GrantRecoveryCase(ctx context.Context, p identity.Principal, id uuid.UUID, after int64, limit int) (GrantRecoveryDetail, error) {
	if !humanApprovalPrincipal(p) {
		return GrantRecoveryDetail{}, ErrForbidden
	}
	if id == uuid.Nil || after < 0 || limit < 1 || limit > 100 {
		return GrantRecoveryDetail{}, ErrInvalidGrantRecovery
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := r.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return GrantRecoveryDetail{}, err
	}
	defer tx.Rollback(bounded)
	item, err := readGrantRecoveryCase(bounded, tx, id)
	if err != nil {
		return GrantRecoveryDetail{}, err
	}
	original, err := approvalRequestByID(bounded, tx, item.RequestID)
	if err != nil {
		return GrantRecoveryDetail{}, err
	}
	policy, err := approvalPolicyForItem(original)
	if err != nil {
		return GrantRecoveryDetail{}, err
	}
	actor, err := adminActorID(bounded, tx, p)
	if err != nil || actor == nil {
		return GrantRecoveryDetail{}, ErrForbidden
	}
	allowed := false
	for _, permission := range []string{"platform.billing.ledger.read", policy.RequesterPermission, policy.ApproverPermission} {
		if _, _, _, e := strongPlatformAuthorization(bounded, tx, *actor, permission); e == nil {
			allowed = true
			break
		} else if !errors.Is(e, ErrForbidden) {
			return GrantRecoveryDetail{}, e
		}
	}
	if !allowed {
		return GrantRecoveryDetail{}, ErrForbidden
	}
	rows, err := tx.Query(bounded, `SELECT * FROM public.read_approval_grant_recovery_operations($1,$2,$3)`, id, after, limit+1)
	if err != nil {
		return GrantRecoveryDetail{}, classifyGrantRecoveryError(err)
	}
	detail := GrantRecoveryDetail{Case: item, Operations: make([]json.RawMessage, 0)}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return GrantRecoveryDetail{}, err
		}
		if len(detail.Operations) < limit {
			detail.Operations = append(detail.Operations, json.RawMessage(raw))
		} else {
			var op struct {
				Version int64 `json:"case_version"`
			}
			if err = json.Unmarshal(detail.Operations[len(detail.Operations)-1], &op); err != nil {
				rows.Close()
				return GrantRecoveryDetail{}, err
			}
			detail.NextVersion = &op.Version
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return GrantRecoveryDetail{}, err
	}
	if err = tx.Commit(bounded); err != nil {
		return GrantRecoveryDetail{}, err
	}
	return detail, nil
}

// ApplyGrantRecovery changes only manual evidence, the original authorization
// gate and delivery metadata. Independent human review is the authority here;
// an ABSENT domain query is never a terminal result or a no-effect proof.
func (r *PostgresRepository) ApplyGrantRecovery(ctx context.Context, p identity.Principal, id uuid.UUID, action string, command GrantRecoveryMutation) (GrantRecoveryCase, error) {
	if !humanApprovalPrincipal(p) {
		return GrantRecoveryCase{}, ErrForbidden
	}
	if id == uuid.Nil || command.OperationID == uuid.Nil || command.ExpectedVersion < 1 {
		return GrantRecoveryCase{}, ErrInvalidGrantRecovery
	}
	payload, err := grantRecoveryPayload(action, command.Evidence)
	if err != nil {
		return GrantRecoveryCase{}, err
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := r.pool.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return GrantRecoveryCase{}, err
	}
	defer tx.Rollback(bounded)
	current, err := readGrantRecoveryCase(bounded, tx, id)
	if err != nil {
		return GrantRecoveryCase{}, err
	}
	original, err := approvalRequestByIDForUpdate(bounded, tx, current.RequestID)
	if err != nil {
		return GrantRecoveryCase{}, err
	}
	var locked []byte
	if err = tx.QueryRow(bounded, `SELECT public.lock_approval_grant_recovery($1)`, id).Scan(&locked); err != nil {
		return GrantRecoveryCase{}, classifyGrantRecoveryError(err)
	}
	if err = json.Unmarshal(locked, &current); err != nil {
		return GrantRecoveryCase{}, err
	}
	var publicCase uuid.UUID
	if err = tx.QueryRow(bounded, `SELECT id FROM manual_review_cases WHERE id=$1 FOR UPDATE`, id).Scan(&publicCase); err != nil {
		return GrantRecoveryCase{}, err
	}
	if original.CommandFingerprint != current.CommandFingerprint || original.CommandType != current.CommandType || original.PolicyVersion != current.PolicyVersion {
		return GrantRecoveryCase{}, ErrConflict
	}
	policy, err := approvalPolicyForItem(original)
	if err != nil {
		return GrantRecoveryCase{}, err
	}
	actor, err := adminActorID(bounded, tx, p)
	if err != nil || actor == nil {
		return GrantRecoveryCase{}, ErrForbidden
	}
	var previous []byte
	if err = tx.QueryRow(bounded, `SELECT public.read_approval_grant_recovery_operation($1,$2)`, id, command.OperationID).Scan(&previous); err != nil {
		return GrantRecoveryCase{}, classifyGrantRecoveryError(err)
	}
	replay := len(previous) > 0
	permission := policy.RequesterPermission
	if action == "REVIEW" {
		permission = policy.ApproverPermission
	}
	requirements := []platformAuthorizationRequirement{{UserID: *actor, Permission: permission}}
	if action == "OBSERVE" {
		requirements = append(requirements, platformAuthorizationRequirement{UserID: *actor, Permission: policy.ApproverPermission})
	}
	if action == "REVIEW" && !replay {
		if current.Status != "PROPOSED" || current.ProposerID == nil || current.ProposerBindingID == nil || current.ProposerAuthorizationVersion == nil || *current.ProposerID == *actor {
			return GrantRecoveryCase{}, ErrConflict
		}
		requirements = append(requirements, platformAuthorizationRequirement{UserID: *current.ProposerID, Permission: policy.RequesterPermission})
	}
	facts, err := lockPlatformAuthorizationFacts(bounded, tx, requirements...)
	if err != nil {
		return GrantRecoveryCase{}, err
	}
	now, err := approvalDatabaseTime(bounded, tx)
	if err != nil {
		return GrantRecoveryCase{}, err
	}
	if !recentlyAuthenticated(p.AuthenticatedAt, now) {
		return GrantRecoveryCase{}, ErrRecentAuthenticationRequired
	}
	binding, version, _, err := platformAuthorizationAt(bounded, tx, facts[0], now)
	if action == "OBSERVE" && errors.Is(err, ErrForbidden) {
		binding, version, _, err = platformAuthorizationAt(bounded, tx, facts[1], now)
	}
	if err != nil {
		return GrantRecoveryCase{}, err
	}
	if action == "REVIEW" && !replay {
		proposerBinding, proposerVersion, _, e := platformAuthorizationAt(bounded, tx, facts[1], now)
		if e != nil {
			return GrantRecoveryCase{}, e
		}
		if proposerBinding != *current.ProposerBindingID || proposerVersion != *current.ProposerAuthorizationVersion {
			return GrantRecoveryCase{}, ErrConflict
		}
	}
	var raw []byte
	var result GrantRecoveryCase
	err = tx.QueryRow(bounded, `SELECT public.apply_approval_grant_recovery($1,$2,$3,$4,$5,$6::jsonb,$7,$8)`, id, command.OperationID, action, *actor, command.ExpectedVersion, string(payload), binding, version).Scan(&raw)
	if err != nil {
		return GrantRecoveryCase{}, classifyGrantRecoveryError(err)
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return GrantRecoveryCase{}, err
	}
	// Replays return the immutable original operation response, not a second
	// audit/state transition. GET returns the current case if it has advanced.
	if result.Version == current.Version+1 {
		if result.Status == "COMPLETED" {
			if original.Status == "APPROVED" {
				if _, err = tx.Exec(bounded, `UPDATE approval_requests SET status='INVALIDATED',invalidation_code='MANUAL_RECOVERY_CONFIRMED',invalidation_details=$2,version=version+1,updated_at=$3 WHERE id=$1`, original.ID, json.RawMessage(`{"manual_case_id":"`+id.String()+`","completion_operation_id":"`+command.OperationID.String()+`"}`), now); err != nil {
					return GrantRecoveryCase{}, err
				}
				if err = appendApprovalStateEvent(bounded, tx, original.ID, "APPROVED", "INVALIDATED", "USER", actor.String(), "MANUAL_RECOVERY_CONFIRMED", map[string]any{"case_id": id, "operation_id": command.OperationID}, now); err != nil {
					return GrantRecoveryCase{}, err
				}
			}
			_, err = tx.Exec(bounded, `UPDATE manual_review_cases SET status='RESOLVED',resolved_at=$2,updated_at=$2 WHERE id=$1`, id, now)
		} else {
			_, err = tx.Exec(bounded, `UPDATE manual_review_cases SET status='OPEN',resolved_at=NULL,updated_at=$2 WHERE id=$1`, id, now)
		}
		if err != nil {
			return GrantRecoveryCase{}, err
		}
		if _, err = tx.Exec(bounded, `INSERT INTO manual_review_case_events(id,case_id,event_type,details,created_at) VALUES($1,$2,$3,$4,$5)`, uuid.Must(uuid.NewV7()), id, "GRANT_RECOVERY_"+action, map[string]any{"operation_id": command.OperationID, "version": result.Version}, now); err != nil {
			return GrantRecoveryCase{}, err
		}
		if err = appendAdminAudit(bounded, tx, actor, p, nil, "approval.grant.recovery."+action, "APPROVAL_GRANT_RECOVERY", id.String(), map[string]any{"operation_id": command.OperationID, "original_event_id": result.OriginalEventID, "case_version": result.Version}); err != nil {
			return GrantRecoveryCase{}, err
		}
	}
	if err = tx.Commit(bounded); err != nil {
		return GrantRecoveryCase{}, err
	}
	return result, nil
}
