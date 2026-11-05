package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/usagewaiver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type usageWaiverStore interface {
	PrepareUsageWaiver(context.Context, usagewaiver.Command) (usagewaiver.Decision, error)
	PendingUsageWaiver(context.Context, uuid.UUID) (*usagewaiver.Command, error)
	FinishUsageWaiver(context.Context, usagewaiver.Command, usagewaiver.Result) error
}
type usageWaiverWriter interface {
	ExecuteUsageWaiver(context.Context, usagewaiver.Command) (usagewaiver.Result, error)
}

type usageWaiverEvidenceStore interface {
	PreserveUsageWaiverEvidence(context.Context, uuid.UUID, Usage) (bool, error)
}

// New provider evidence must not turn a persisted release-only intent back into
// an ordinary Extend/Commit retry, including after the original deadline.
func (r *PostgresRepository) PreserveUsageWaiverEvidence(ctx context.Context, usageID uuid.UUID, usage Usage) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID).Scan(&id); err != nil {
		return false, err
	}
	c, err := tokenWaiverCommandTx(ctx, tx, usageID)
	if err != nil {
		return false, err
	}
	if c == nil {
		return false, tx.Commit(ctx)
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 {
		return true, ErrForbidden
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usageID, "RESULT_OBSERVED", "GATEWAY", "REVIEW", "late-usage-after-waiver-intent", map[string]any{"usage": usage, "waiver_command_reference": c.Reference, "customer_debit_prohibited": true, "provider_outcome_inferred": false}, r.now()); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}

func (c *HTTPBillingClient) ExecuteUsageWaiver(ctx context.Context, command usagewaiver.Command) (usagewaiver.Result, error) {
	var result usagewaiver.Result
	client, err := usagewaiver.NewClient(c.baseURL.String(), c.serviceToken, c.client)
	if err != nil {
		return result, err
	}
	err = client.Call(ctx, "/internal/v1/holds/waiver", usagewaiver.ExecuteRequest{Reference: command.Reference, Digest: command.Digest}, &result)
	if err == nil && !result.Matches(command) {
		err = ErrForbidden
	}
	return result, err
}

func (s *Service) internalUsageWaiver(w http.ResponseWriter, r *http.Request) {
	store, ok := s.repository.(usageWaiverStore)
	writer, writerOK := s.billing.(usageWaiverWriter)
	if !ok || !writerOK {
		api.WriteError(w, 503, "USAGE_WAIVER_UNAVAILABLE", "usage waiver is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var command usagewaiver.Command
	if api.DecodeJSON(w, r, &command) != nil {
		api.WriteError(w, 400, "INVALID_JSON", "request body is invalid", httpserver.RequestID(r.Context()))
		return
	}
	if !command.Valid() {
		api.WriteError(w, 422, "INVALID_USAGE_WAIVER", "usage waiver command is invalid", httpserver.RequestID(r.Context()))
		return
	}
	decision, err := store.PrepareUsageWaiver(r.Context(), command)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if decision.Admission == usagewaiver.Rejected {
		api.WriteJSON(w, 200, decision)
		return
	}
	result, err := writer.ExecuteUsageWaiver(r.Context(), command)
	if err == nil {
		err = store.FinishUsageWaiver(r.Context(), command, result)
	}
	if err != nil {
		api.WriteError(w, 503, "USAGE_WAIVER_RESULT_UNKNOWN", "the original waiver requires recovery", httpserver.RequestID(r.Context()))
		return
	}
	decision.Result = &result
	api.WriteJSON(w, 200, decision)
}

func tokenWaiverCommandTx(ctx context.Context, tx pgx.Tx, usageID uuid.UUID) (*usagewaiver.Command, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT command_snapshot FROM usage_waiver_intents WHERE usage_request_id=$1`, usageID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var command usagewaiver.Command
	if json.Unmarshal(raw, &command) != nil || !command.Valid() {
		return nil, ErrForbidden
	}
	return &command, nil
}

func (r *PostgresRepository) PendingUsageWaiver(ctx context.Context, usageID uuid.UUID) (*usagewaiver.Command, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	c, err := tokenWaiverCommandTx(ctx, tx, usageID)
	if err != nil {
		return nil, err
	}
	return c, tx.Commit(ctx)
}

func (r *PostgresRepository) PrepareUsageWaiver(ctx context.Context, c usagewaiver.Command) (usagewaiver.Decision, error) {
	var zero usagewaiver.Decision
	if !c.Valid() {
		return zero, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "token.usage-waiver:"+c.Reference.String()); err != nil {
		return zero, err
	}
	var priorRaw []byte
	err = tx.QueryRow(ctx, `SELECT decision_snapshot FROM usage_waiver_decisions WHERE command_reference=$1`, c.Reference).Scan(&priorRaw)
	if err == nil {
		var decision usagewaiver.Decision
		if json.Unmarshal(priorRaw, &decision) != nil || !decision.ValidFor(c) {
			return zero, ErrForbidden
		}
		return decision, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return zero, err
	}
	record := func(reason string) (usagewaiver.Decision, error) {
		admission := usagewaiver.Admitted
		if reason != "" {
			admission = usagewaiver.Rejected
		}
		decision := usagewaiver.Decision{CommandReference: c.Reference, RequestDigest: c.Digest, Admission: admission, ReasonCode: reason}
		if !decision.ValidFor(c) {
			return zero, ErrForbidden
		}
		commandRaw, _ := json.Marshal(c)
		decisionRaw, _ := json.Marshal(decision)
		if _, err := tx.Exec(ctx, `INSERT INTO usage_waiver_decisions(command_reference,request_digest,command_snapshot,admission,decision_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6)`, c.Reference, c.Digest, commandRaw, admission, decisionRaw, r.now()); err != nil {
			return zero, err
		}
		return decision, nil
	}
	reject := func(reason string) (usagewaiver.Decision, error) {
		decision, err := record(reason)
		if err != nil {
			return zero, err
		}
		return decision, tx.Commit(ctx)
	}
	var usage UsageRequest
	if err := scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1 FOR UPDATE`, c.UsageID), &usage); errors.Is(err, pgx.ErrNoRows) {
		return reject("USAGE_NOT_FOUND")
	} else if err != nil {
		return zero, err
	}
	if usage.HoldID == nil || *usage.HoldID != c.HoldID || usage.WalletID != c.WalletID || usage.MaximumMicrocredits != c.Maximum {
		return reject("USAGE_IDENTITY_MISMATCH")
	}
	prior, err := tokenWaiverCommandTx(ctx, tx, c.UsageID)
	if err != nil {
		return zero, err
	}
	if prior != nil {
		return reject("ANOTHER_WAIVER_INTENT")
	}
	if usage.Status == "UPSTREAM_ACTIVE" || usage.ExecutionStatus == "STREAMING" || usage.ExecutionStatus == "UPSTREAM_DISPATCHING" {
		return reject("EXECUTION_ACTIVE")
	}
	if usage.Status != "REVIEW" && usage.Status != "SETTLEMENT_PENDING" && usage.Status != "COMMITTED" && usage.Status != "RELEASED" {
		return reject("USAGE_NOT_ELIGIBLE")
	}
	if usage.Status != "COMMITTED" && usage.Status != "RELEASED" && usage.ExecutionStatus != "REVIEW" && usage.ExecutionStatus != "SETTLEMENT_PENDING" {
		return reject("USAGE_NOT_ELIGIBLE")
	}
	pending, err := missingReferenceReleasePendingTx(ctx, tx, c.UsageID)
	if err != nil {
		return zero, err
	}
	if pending && usage.Status != "RELEASED" {
		return reject("DEADLINE_RELEASE_OWNS_RECOVERY")
	}
	decision, err := record("")
	if err != nil {
		return zero, err
	}
	raw, _ := json.Marshal(c)
	if _, err := tx.Exec(ctx, `INSERT INTO usage_waiver_intents(usage_request_id,command_reference,request_digest,command_snapshot,created_at) VALUES($1,$2,$3,$4,$5)`, c.UsageID, c.Reference, c.Digest, raw, r.now()); err != nil {
		return zero, err
	}
	if usage.Status == "REVIEW" || usage.Status == "SETTLEMENT_PENDING" {
		if _, err := tx.Exec(ctx, `UPDATE usage_requests SET reconciliation_next_at=$2,reconciliation_lease_until=NULL,reconciliation_stopped_at=NULL,updated_at=$2 WHERE id=$1`, c.UsageID, r.now()); err != nil {
			return zero, err
		}
	}
	return decision, tx.Commit(ctx)
}

// FinishUsageWaiver closes settlement bookkeeping only. It retains the attempt
// status, provider observation, token quantities and original trusted evidence.
func (r *PostgresRepository) FinishUsageWaiver(ctx context.Context, c usagewaiver.Command, result usagewaiver.Result) error {
	if !result.Matches(c) {
		return ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var usage UsageRequest
	if err := scanUsage(tx.QueryRow(ctx, `SELECT `+usageSelectColumns+` FROM usage_requests WHERE id=$1 FOR UPDATE`, c.UsageID), &usage); err != nil {
		return err
	}
	prior, err := tokenWaiverCommandTx(ctx, tx, c.UsageID)
	if err != nil {
		return err
	}
	if prior == nil || prior.Reference != c.Reference || prior.Digest != c.Digest {
		return ErrForbidden
	}
	raw, _ := json.Marshal(result)
	var priorResult []byte
	err = tx.QueryRow(ctx, `SELECT result_snapshot FROM usage_waiver_receipts WHERE command_reference=$1`, c.Reference).Scan(&priorResult)
	if err == nil {
		var equal bool
		if err := tx.QueryRow(ctx, `SELECT $1::jsonb=$2::jsonb`, priorResult, raw).Scan(&equal); err != nil {
			return err
		}
		if !equal {
			return ErrForbidden
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if usage.HoldID == nil || *usage.HoldID != c.HoldID || usage.WalletID != c.WalletID || usage.MaximumMicrocredits != c.Maximum {
		return ErrForbidden
	}
	var loss bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_platform_loss_reviews WHERE usage_request_id=$1 AND customer_debit_prohibited)`, c.UsageID).Scan(&loss); err != nil {
		return err
	}
	pending, err := missingReferenceReleasePendingTx(ctx, tx, c.UsageID)
	if err != nil {
		return err
	}
	now := r.now()
	if result.Outcome == usagewaiver.Committed {
		if loss || pending || usage.Status == "RELEASED" {
			return ErrForbidden
		}
		if usage.Status == "COMMITTED" && (usage.ActualMicrocredits == nil || *usage.ActualMicrocredits != *result.Actual) {
			return ErrForbidden
		}
	} else {
		if usage.Status == "COMMITTED" {
			return ErrForbidden
		}
		// The immutable missing-reference decision is owned by its existing
		// finisher; an admin retry may only observe its completed release.
		if pending && usage.Status != "RELEASED" {
			return ErrForbidden
		}
	}
	if usage.Status != "COMMITTED" && usage.Status != "RELEASED" {
		if usage.Status != "REVIEW" && usage.Status != "SETTLEMENT_PENDING" {
			return ErrForbidden
		}
		target, settlement := "RELEASED", "RELEASED"
		if result.Outcome == usagewaiver.Committed {
			target, settlement = "COMMITTED", "SETTLED"
		}
		authority, _ := json.Marshal(map[string]any{"waiver_settlement_authority": result})
		if _, err := tx.Exec(ctx, `UPDATE usage_requests SET status=$2,settlement_status=$3,actual_microcredits=$4,usage_snapshot=COALESCE(usage_snapshot,'{}'::jsonb)||$5::jsonb,terminal_at=$6,updated_at=$6,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,reconciliation_stopped_at=$6 WHERE id=$1`, c.UsageID, target, settlement, result.Actual, authority, now); err != nil {
			return err
		}
		if result.Outcome != usagewaiver.Committed && !usage.StartedAt.Add(24*time.Hour).After(now) && !loss {
			if _, err := tx.Exec(ctx, `INSERT INTO usage_platform_loss_reviews(id,usage_request_id,cause_code,provider_cost_evidence,created_at) VALUES($1,$2,'TRUSTED_USAGE_DEADLINE_EXCEEDED',jsonb_build_object('customer_hold_released',true,'late_usage_customer_debit_prohibited',true,'waiver_command_reference',$3::text),$4) ON CONFLICT(usage_request_id) DO NOTHING`, uuid.Must(uuid.NewV7()), c.UsageID, c.Reference, now); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_attempts SET ended_at=COALESCE(ended_at,$2) WHERE usage_request_id=$1`, c.UsageID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_waiver_receipts(command_reference,result_snapshot,created_at) VALUES($1,$2,$3)`, c.Reference, raw, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *UsageReconciler) reconcileUsageWaiver(ctx context.Context, item ReconciliationItem) (bool, error) {
	store, ok := r.store.(usageWaiverStore)
	if !ok {
		return false, nil
	}
	command, err := store.PendingUsageWaiver(ctx, item.ID)
	if err != nil {
		return true, err
	}
	if command == nil {
		return false, nil
	}
	writer, ok := r.billing.(usageWaiverWriter)
	if !ok {
		return true, errors.New("usage waiver writer unavailable")
	}
	result, err := writer.ExecuteUsageWaiver(ctx, *command)
	if err != nil {
		return true, err
	}
	return true, store.FinishUsageWaiver(ctx, *command, result)
}
