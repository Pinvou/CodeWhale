package billing

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestRegistrationBonusPolicyConflictPostgresIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("actual PostgreSQL policy conflicts run in the dedicated integration step")
	}
	repository, ctx := retainedFinanceRepository(t)
	initial, err := repository.ActiveRegistrationBonusPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	const reason = "Validate immutable registration policy conflicts"
	const key = "registration-bonus-policy-conflicts-original"
	frozen, replayed, err := repository.UpdateRegistrationBonusPolicy(ctx, actor, true, 250000, 90, initial.Revision, reason, key)
	if err != nil || replayed || frozen.Revision != initial.Revision+1 {
		t.Fatalf("initial policy write: revision=%d replayed=%v err=%v", frozen.Revision, replayed, err)
	}
	// Move the active policy forward; a matching retry still belongs to the
	// original immutable command, rather than adopting this newer policy.
	if _, replayed, err := repository.UpdateRegistrationBonusPolicy(ctx, actor, true, 300000, 90, frozen.Revision, reason, key+"-next"); err != nil || replayed {
		t.Fatalf("advance active policy: replayed=%v err=%v", replayed, err)
	}
	snapshot := func(t *testing.T) []byte {
		t.Helper()
		facts := make(map[string]string)
		for _, table := range []string{
			"registration_bonus_policy_state", "registration_bonus_policy_versions",
			"registration_bonus_policy_commands", "registration_bonus_policy_audit",
			"registration_bonus_claims", "registration_bonus_receipts", "wallets",
			"entitlement_packages", "entitlement_components", "journal_entries",
			"ledger_postings", "outbox_events",
		} {
			var rows string
			// The table names are a fixed test-only list. Compare complete rows,
			// including request hashes and audit facts, rather than only counts.
			if err := repository.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM `+table+` t`).Scan(&rows); err != nil {
				t.Fatalf("snapshot %s: %v", table, err)
			}
			facts[table] = rows
		}
		raw, err := json.Marshal(facts)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	baseline := snapshot(t)
	for _, control := range []struct {
		name     string
		enabled  bool
		amount   int64
		days     int
		expected int64
		reason   string
		key      string
		want     error
		wantCode string
	}{
		{"changed-enabled", false, 250000, 90, initial.Revision, reason, key, ErrRegistrationBonusKeyReused, "IDEMPOTENCY_KEY_REUSED"},
		{"changed-amount", true, 260000, 90, initial.Revision, reason, key, ErrRegistrationBonusKeyReused, "IDEMPOTENCY_KEY_REUSED"},
		{"changed-validity", true, 250000, 91, initial.Revision, reason, key, ErrRegistrationBonusKeyReused, "IDEMPOTENCY_KEY_REUSED"},
		{"changed-expected-revision", true, 250000, 90, frozen.Revision, reason, key, ErrRegistrationBonusKeyReused, "IDEMPOTENCY_KEY_REUSED"},
		{"changed-reason", true, 250000, 90, initial.Revision, reason + " changed", key, ErrRegistrationBonusKeyReused, "IDEMPOTENCY_KEY_REUSED"},
		{"fresh-key-stale-revision", true, 250000, 90, initial.Revision, reason, key + "-stale", ErrRegistrationBonusConflict, "REGISTRATION_BONUS_CONFLICT"},
	} {
		t.Run(control.name, func(t *testing.T) {
			_, replayed, err := repository.UpdateRegistrationBonusPolicy(ctx, actor, control.enabled, control.amount, control.days, control.expected, control.reason, control.key)
			if !errors.Is(err, control.want) || replayed {
				t.Fatalf("rejected policy: replayed=%v err=%v", replayed, err)
			}
			// This exercises only the actual error mapper, not an authenticated
			// end-to-end HTTP request or a real issuer's recent-authentication gate.
			response := httptest.NewRecorder()
			(&Service{}).writeRegistrationBonusError(response, httptest.NewRequest(http.MethodPut, "/admin/v1/registration-bonus-policy", nil), err)
			var body struct {
				Code string `json:"code"`
			}
			if decodeErr := json.Unmarshal(response.Body.Bytes(), &body); decodeErr != nil || response.Code != http.StatusConflict || body.Code != control.wantCode {
				t.Fatalf("typed conflict response: status=%d code=%q err=%v", response.Code, body.Code, decodeErr)
			}
			_, found, replayErr := repository.ReplayRegistrationBonusPolicy(ctx, actor, control.enabled, control.amount, control.days, control.expected, control.reason, control.key)
			if control.want == ErrRegistrationBonusKeyReused {
				if !errors.Is(replayErr, control.want) || found {
					t.Fatalf("changed read replay: found=%v err=%v", found, replayErr)
				}
			} else if replayErr != nil || found {
				t.Fatalf("failed stale write must not create a command: found=%v err=%v", found, replayErr)
			}
			if !bytes.Equal(baseline, snapshot(t)) {
				t.Fatal("rejected command changed persisted policy or financial facts")
			}
		})
	}
	t.Run("matching-replay-keeps-frozen-policy", func(t *testing.T) {
		for _, call := range []func() (RegistrationBonusPolicy, bool, error){
			func() (RegistrationBonusPolicy, bool, error) {
				return repository.UpdateRegistrationBonusPolicy(ctx, actor, true, 250000, 90, initial.Revision, reason, key)
			},
			func() (RegistrationBonusPolicy, bool, error) {
				return repository.ReplayRegistrationBonusPolicy(ctx, actor, true, 250000, 90, initial.Revision, reason, key)
			},
		} {
			result, replayed, err := call()
			if err != nil || !replayed || result.Revision != frozen.Revision || result.Enabled != frozen.Enabled || result.AmountMicrocredits != frozen.AmountMicrocredits || result.ValidityDays != frozen.ValidityDays {
				t.Fatalf("matching original command: policy=%+v replayed=%v err=%v", result, replayed, err)
			}
		}
		if !bytes.Equal(baseline, snapshot(t)) {
			t.Fatal("matching replay changed persisted policy or financial facts")
		}
	})
}
