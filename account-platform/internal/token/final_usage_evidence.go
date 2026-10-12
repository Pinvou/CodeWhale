package token

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"
)

// RecordFinalUsage retains only numeric financial evidence after a terminal call.
// The usage-row lock serializes writes; the shared per-user replay namespace
// does not serialize writers or imply an installed exclusive cleanup actor.
// This method does not recreate a response body or an idempotency key.
func (r *PostgresRepository) RecordFinalUsage(ctx context.Context, usageID uuid.UUID, provider, adapter string, usage Usage) error {
	if usageID == uuid.Nil || usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 {
		return ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT user_id FROM usage_requests WHERE id=$1`, usageID).Scan(&userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))`, "token.replay.user:"+userID.String()); err != nil {
		return err
	}
	var status string
	var snapshot []byte
	if err := tx.QueryRow(ctx, `SELECT status,request_snapshot FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID).Scan(&status, &snapshot); err != nil {
		return err
	}
	var frozen frozenUsageSnapshot
	if (status != "RELEASED" && status != "COMMITTED") || json.Unmarshal(snapshot, &frozen) != nil || frozen.ProviderID == "" || !strings.EqualFold(frozen.ProviderID, provider) || frozen.AdapterVersion == "" || frozen.AdapterVersion != adapter {
		return ErrForbidden
	}
	conflict, err := recordFinalUsageTx(ctx, tx, usageID, usage, r.now())
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if conflict {
		return ErrForbidden
	}
	return nil
}

// The usage row is already locked. Conflicting terminal evidence is retained for
// review without replacing the first confirmed numbers or any terminal state.
func confirmedUsageNumbers(raw []byte) (Usage, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return Usage{}, false
	}
	if nested, exists := fields["usage"]; exists {
		fields = nil
		if json.Unmarshal(nested, &fields) != nil {
			return Usage{}, false
		}
	}
	if len(fields) != 3 || fields["input_tokens"] == nil || fields["output_tokens"] == nil || fields["cached_input_tokens"] == nil {
		return Usage{}, false
	}
	for _, name := range []string{"input_tokens", "output_tokens", "cached_input_tokens"} {
		var count *int64
		if json.Unmarshal(fields[name], &count) != nil || count == nil {
			return Usage{}, false
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return Usage{}, false
	}
	var usage Usage
	if json.Unmarshal(encoded, &usage) != nil || usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 {
		return Usage{}, false
	}
	return usage, true
}

func recordFinalUsageTx(ctx context.Context, tx pgx.Tx, usageID uuid.UUID, usage Usage, now time.Time) (bool, error) {
	encoded, err := json.Marshal(usage)
	if err != nil {
		return false, err
	}
	var status string
	var prior []byte
	if err := tx.QueryRow(ctx, `SELECT status,trusted_usage_snapshot FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID).Scan(&status, &prior); err != nil {
		return false, err
	}
	if (status == "COMMITTED" || status == "RELEASED") && len(prior) > 0 && string(prior) != "null" {
		confirmed, valid := confirmedUsageNumbers(prior)
		if !valid || confirmed != usage {
			_, err := tx.Exec(ctx, `UPDATE usage_requests SET usage_snapshot=COALESCE(usage_snapshot,'{}'::jsonb)||jsonb_build_object('conflicting_provider_final_usage',$2::jsonb) WHERE id=$1`, usageID, encoded)
			return true, err
		}
		return false, nil
	}
	_, err = tx.Exec(ctx, `UPDATE usage_requests SET trusted_usage_snapshot=$2,trusted_usage_source='PROVIDER_FINAL_USAGE',final_usage_observed_at=COALESCE(final_usage_observed_at,$3),provider_terminal_at=COALESCE(provider_terminal_at,$3),settlement_status=CASE WHEN status IN ('COMMITTED','RELEASED') THEN settlement_status ELSE 'USAGE_PENDING' END,updated_at=CASE WHEN status IN ('COMMITTED','RELEASED') THEN updated_at ELSE $3 END WHERE id=$1`, usageID, encoded, now)
	return false, err
}

// A late provider result is evidence only once its original financial outcome is
// terminal. It cannot extend a hold, reopen settlement, or redeliver a body.
func (s *Service) recordTerminalFinalUsage(ctx context.Context, usageID uuid.UUID, usage Usage) (bool, error) {
	current, err := s.repository.GetUsage(ctx, usageID)
	if err != nil {
		return false, err
	}
	if current.Status != "COMMITTED" && current.Status != "RELEASED" {
		return false, nil
	}
	var frozen frozenUsageSnapshot
	if json.Unmarshal(current.RequestSnapshot, &frozen) != nil || frozen.ProviderID == "" || frozen.AdapterVersion == "" {
		return true, ErrForbidden
	}
	return true, s.repository.RecordFinalUsage(ctx, usageID, frozen.ProviderID, frozen.AdapterVersion, usage)
}
