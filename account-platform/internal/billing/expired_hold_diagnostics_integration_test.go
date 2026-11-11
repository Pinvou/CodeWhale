package billing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
)

type holdProofProbe func(context.Context, ExpiredHoldIdentity) (ExpiredHoldProof, error)

func (probe holdProofProbe) FenceExpiredHold(ctx context.Context, identity ExpiredHoldIdentity) (ExpiredHoldProof, error) {
	return probe(ctx, identity)
}

type stopOnHoldDiagnostic struct {
	match string
	stop  context.CancelFunc
}

func (observer stopOnHoldDiagnostic) Write(record []byte) (int, error) {
	if bytes.Contains(record, []byte(observer.match)) {
		observer.stop()
	}
	return len(record), nil
}

func TestExpiredHoldProofRetryDiagnosticsPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	const privateError = "private diagnostic test value: SELECT password"
	for _, scenario := range []struct {
		name string
		code string
	}{
		{"quick-failure", "HOLD_PROOF_REQUEST_FAILED"},
		{"timeout", "HOLD_PROOF_TIMEOUT"},
		{"cancel", "HOLD_PROOF_CANCELED"},
		{"unavailable", "HOLD_PROOF_UNAVAILABLE"},
		{"invalid-proof", ""},
		{"valid-proof", ""},
		{"persistence-failure", ""},
		{"operator", ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			hold, err := r.Reserve(ctx, uuid.New(), order.WalletID, 100, nil, time.Now().UTC().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.pool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, hold.ID); err != nil {
				t.Fatal(err)
			}

			runCtx, stop := context.WithCancel(ctx)
			defer stop()
			unlock := func() {}
			defer func() { unlock() }()
			var client ExpiredHoldProofClient = holdProofProbe(func(proofCtx context.Context, i ExpiredHoldIdentity) (ExpiredHoldProof, error) {
				switch scenario.name {
				case "timeout":
					return ExpiredHoldProof{}, context.DeadlineExceeded
				case "cancel":
					stop()
					return ExpiredHoldProof{}, proofCtx.Err()
				case "invalid-proof":
					return ExpiredHoldProof{}, nil
				case "valid-proof":
					if _, err := r.Release(proofCtx, i.UsageRequestID, i.HoldID, "controlled proof"); err != nil {
						return ExpiredHoldProof{}, err
					}
					return ExpiredHoldProof{Identity: i, ProofID: uuid.New(), Status: "LOCAL_CONFIRMED"}, nil
				case "persistence-failure":
					tx, err := r.pool.Begin(proofCtx)
					if err != nil {
						return ExpiredHoldProof{}, err
					}
					unlock = func() { _ = tx.Rollback(ctx) }
					_, err = tx.Exec(proofCtx, `SELECT hold_id FROM usage_hold_expiry_recovery WHERE hold_id=$1 FOR UPDATE`, i.HoldID)
					if err != nil {
						return ExpiredHoldProof{}, err
					}
				}
				return ExpiredHoldProof{}, errors.New(privateError)
			})
			if scenario.name == "unavailable" {
				client = nil
			}
			var logs bytes.Buffer
			previous := slog.Default()
			var outputWriter io.Writer = &logs
			if scenario.name == "quick-failure" {
				outputWriter = io.MultiWriter(&logs, stopOnHoldDiagnostic{match: "expired_hold_proof_retry_scheduled", stop: stop})
			}
			if scenario.name == "persistence-failure" {
				outputWriter = io.MultiWriter(&logs, stopOnHoldDiagnostic{match: "expired_hold_recovery_failed", stop: stop})
			}
			slog.SetDefault(observability.NewLogger(outputWriter, observability.Config{Service: "billing-test"}))
			defer slog.SetDefault(previous)
			worker := NewExpiredHoldRecoveryWorker(r, client, time.Minute)
			switch scenario.name {
			case "operator":
				err = worker.RunOnceLimit(runCtx, 1)
			case "quick-failure", "persistence-failure":
				bounded, cancel := context.WithTimeout(runCtx, 10*time.Second)
				defer cancel()
				worker.Run(bounded)
			default:
				err = worker.runOnceLimit(runCtx, 1, true)
			}
			unlock()
			switch scenario.name {
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected cancellation, got %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			output := logs.String()
			if scenario.name == "persistence-failure" {
				if strings.Contains(output, "expired_hold_proof_retry_scheduled") ||
					!strings.Contains(output, `"event":"expired_hold_recovery_failed"`) {
					t.Fatalf("persistence failure was reported as a scheduled retry: %s", output)
				}
			} else if scenario.code == "" {
				if output != "" {
					t.Fatalf("unexpected diagnostics: %s", output)
				}
			} else if strings.Count(output, `"event":"expired_hold_proof_retry_scheduled"`) != 1 ||
				!strings.Contains(output, `"error_code":"`+scenario.code+`"`) {
				t.Fatalf("missing %s diagnostic: %s", scenario.code, output)
			}
			if strings.Contains(output, privateError) || strings.Contains(output, hold.ID.String()) {
				t.Fatal("diagnostics exposed private data")
			}
			var state, status string
			var scheduled bool
			var evidence int
			if err := r.pool.QueryRow(ctx, `SELECT r.state,h.status,r.next_attempt_at>r.updated_at,
				(SELECT count(*) FROM usage_hold_expiry_evidence WHERE hold_id=h.id AND decision='DISPATCH_UNPROVEN')
				FROM usage_holds h JOIN usage_hold_expiry_recovery r ON r.hold_id=h.id WHERE h.id=$1`, hold.ID).Scan(&state, &status, &scheduled, &evidence); err != nil {
				t.Fatal(err)
			}
			wantState, wantStatus := "MANUAL_REVIEW", "ACTIVE"
			if scenario.name == "valid-proof" {
				wantState, wantStatus = "COMPLETED", "RELEASED"
			}
			if state != wantState || status != wantStatus || scheduled != (scenario.name != "persistence-failure") || evidence != 1 {
				t.Fatalf("unexpected recovery facts: %s %s retry=%v evidence=%d", state, status, scheduled, evidence)
			}
			// Keep this candidate outside the following subtest's one-item pass.
			if _, err := r.pool.Exec(ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp()+interval '1 hour' WHERE hold_id=$1`, hold.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}
