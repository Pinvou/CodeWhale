package billingschema

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"strings"
	"testing"
	"time"
)

// Reuses the original public proof/approval/relay/grant/Begin chain unchanged.
// Only mutable review deadlines are made due. No financial executor runs here.
func TestF02ApprovalStopLossExecutionFence(t *testing.T) {
	for _, scenario := range []string{"pause and closed case", "missing target", "stale serializable begin"} {
		t.Run(scenario, func(t *testing.T) {
			requireF02StopLossOwnedRuntime(t)
			retainedRecoveryLeaseScenario(t, false, false, func(ctx context.Context, f *stageFixture, c, b *pgx.Conn, repo *account.PostgresRepository, candidate account.ApprovalRecoveryCandidate) {
				if err := repo.CompleteApprovalExecution(ctx, candidate.RequestID, candidate.Workload, candidate.OriginalEventID.String(), "FAILED", json.RawMessage(`{}`), json.RawMessage(`{}`), "DOMAIN_RESOURCE_NOT_FOUND"); err != nil {
					t.Fatal(err)
				}
				if _, err := repo.SyncManualReviewCases(ctx, 100); err != nil {
					t.Fatal(err)
				}
				var caseID uuid.UUID
				if err := c.QueryRow(ctx, `SELECT id FROM manual_review_cases WHERE domain='ACCOUNT' AND case_type='HIGH_RISK_COMMAND_REPAIR' AND resource_id=$1`, candidate.RequestID).Scan(&caseID); err != nil {
					t.Fatal(err)
				}
				if scenario == "missing target" {
					if _, err := c.Exec(ctx, `UPDATE manual_review_cases SET resource_id=$2 WHERE id=$1`, caseID, uuid.New()); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := c.Exec(ctx, `UPDATE manual_review_cases SET mitigation_due_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, caseID); err != nil {
					t.Fatal(err)
				}
				var original string
				if err := c.QueryRow(ctx, `SELECT to_jsonb(r)::text FROM approval_requests r WHERE id=$1`, candidate.RequestID).Scan(&original); err != nil {
					t.Fatal(err)
				}
				binding, err := repo.ResolveApprovalAuthorizationBinding(ctx, candidate.OriginalEventID, candidate.Durable, candidate.Workload)
				if err != nil {
					t.Fatal(err)
				}
				begin := func() error {
					_, err := repo.BeginApprovalExecutionWithAuthorization(ctx, candidate.RequestID, candidate.Workload, candidate.OriginalEventID.String(), json.RawMessage(`{"adjustment_version":1}`), json.RawMessage(`{"wallet_id":"0198f110-f538-7f30-a8b1-f3984fd459a1","delta_microcredits":100}`), binding)
					return err
				}
				escalate := func() error {
					_, e := repo.EscalateOverdueManualReviews(ctx, manualreview.OnCallSchedule{}, 100)
					return e
				}
				if scenario == "stale serializable begin" {
					gate, e := c.Begin(ctx)
					if e != nil {
						t.Fatal(e)
					}
					defer gate.Rollback(context.Background())
					if _, e = gate.Exec(ctx, `LOCK TABLE manual_review_stop_loss_evaluations IN SHARE MODE`); e != nil {
						t.Fatal(e)
					}
					evaluateDone := make(chan error, 1)
					go func() { evaluateDone <- escalate() }()
					wait := func(fragment string) {
						t.Helper()
						deadline := time.Now().Add(3 * time.Second)
						for time.Now().Before(deadline) {
							var blocked bool
							e := f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND usename='account_service' AND wait_event_type='Lock' AND query LIKE $2)`, c.Config().Database, "%"+fragment+"%").Scan(&blocked)
							if e != nil {
								t.Fatal(e)
							}
							if blocked {
								return
							}
							time.Sleep(10 * time.Millisecond)
						}
						t.Fatalf("actual blocked caller not observed: %s", fragment)
					}
					wait("INSERT INTO manual_review_stop_loss_evaluations")
					beginDone := make(chan error, 1)
					go func() { beginDone <- begin() }()
					wait("validate_approval_authorization_begin")
					if e = gate.Commit(ctx); e != nil {
						t.Fatal(e)
					}
					if e = <-evaluateDone; e != nil {
						t.Fatal(e)
					}
					e = <-beginDone
					var pgErr *pgconn.PgError
					if !errors.As(e, &pgErr) || pgErr.Code != "40001" {
						t.Fatalf("stale Begin must fail serialization without effects: %v", e)
					}
				} else {
					if err = escalate(); err != nil {
						t.Fatal(err)
					}
				}
				var unchanged bool
				if err = c.QueryRow(ctx, `SELECT to_jsonb(r)::text=$2 FROM approval_requests r WHERE id=$1`, candidate.RequestID, original).Scan(&unchanged); err != nil || !unchanged {
					t.Fatalf("stop-loss changed immutable request/status/version/time: %v", err)
				}
				var evaluations, events int
				var mitigation *time.Time
				if err = c.QueryRow(ctx, `SELECT (SELECT count(*) FROM manual_review_stop_loss_evaluations WHERE case_id=$1),(SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type='STOP_LOSS_EVALUATED'),mitigation_at FROM manual_review_cases WHERE id=$1`, caseID).Scan(&evaluations, &events, &mitigation); err != nil {
					t.Fatal(err)
				}
				if scenario == "missing target" {
					if evaluations != 0 || events != 0 || mitigation != nil {
						t.Fatal("missing target produced false pause/mitigation evidence")
					}
					return
				}
				if evaluations != 1 || events != 1 || mitigation == nil {
					t.Fatal("durable per-request evaluation missing")
				}
				if err = begin(); !errors.Is(err, account.ErrConflict) {
					t.Fatalf("paused request fresh retry: %v", err)
				}
				if _, err = c.Exec(ctx, `UPDATE manual_review_cases SET status='RESOLVED',resolved_at=clock_timestamp() WHERE id=$1`, caseID); err != nil {
					t.Fatal(err)
				}
				if err = begin(); !errors.Is(err, account.ErrConflict) {
					t.Fatalf("closing review cleared immutable pause: %v", err)
				}
				var executions int
				var status string
				if err = c.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM approval_executions WHERE approval_request_id=$1) FROM approval_requests WHERE id=$1`, candidate.RequestID).Scan(&status, &executions); err != nil || status != "EXECUTION_FAILED" || executions != 1 {
					t.Fatalf("paused retry changed execution: %v", err)
				}
				if scenario == "pause and closed case" {
					plan, e := repo.PlanProofApprovalRecovery(ctx, 100)
					if e != nil {
						t.Fatal(e)
					}
					var failed account.ApprovalRecoveryCandidate
					found := false
					for _, v := range plan.Candidates {
						if v.RequestID == candidate.RequestID {
							failed = v
							found = true
						}
					}
					if !found {
						t.Fatal("original failed receipt candidate missing")
					}
					config := f.config.Copy()
					config.Database = c.Config().Database
					recovery, e := pgx.ConnectConfig(ctx, config)
					if e != nil {
						t.Fatal(e)
					}
					defer recovery.Close(context.Background())
					if _, e = recovery.Exec(ctx, `SET SESSION AUTHORIZATION account_approval_recovery`); e != nil {
						t.Fatal(e)
					}
					leaser, e := account.NewRecoveryQueryLeaseStore(recovery)
					if e != nil {
						t.Fatal(e)
					}
					lease, e := leaser.Claim(ctx, failed)
					if e != nil {
						t.Fatal(e)
					}
					result := account.RecoveryResultObservation{State: "SUCCEEDED", ObservedAt: time.Now().UTC(), DomainResult: json.RawMessage(`{"adjustment_id":"0198f110-f538-7f30-a8b1-f3984fd459a0","wallet_id":"0198f110-f538-7f30-a8b1-f3984fd459a1","status":"POSTED","version":1}`), LedgerReferences: json.RawMessage(`{"journal_entry_id":"0198f110-f538-7f30-a8b1-f3984fd459a2"}`)}
					if e = repo.ConfirmOriginalApprovalSuccess(ctx, failed, lease, result); e != nil {
						t.Fatalf("existing SUCCEEDED receipt must converge despite pause: %v", e)
					}
					if e = c.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM approval_executions WHERE approval_request_id=$1) FROM approval_requests WHERE id=$1`, candidate.RequestID).Scan(&status, &executions); e != nil || status != "EXECUTED" || executions != 1 {
						t.Fatalf("receipt confirmation created/replaced an execution: %v", e)
					}
					if e = begin(); e != nil {
						t.Fatalf("executed original replay must stay allowed: %v", e)
					}
				}

			})
		})
	}
}

// Validate the existing disposable runtime before the reused fixture can
// create any role/database. This is not an external/deployment test profile.
func requireF02StopLossOwnedRuntime(t *testing.T) {
	t.Helper()
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("owned local schema database required")
	}
	config, err := pgx.ParseConfig(raw)
	if err != nil || config.Host != "127.0.0.1" || config.Port != 55504 || config.Database != "postgres" || config.User != "pinvou_wave_test" || len(config.Fallbacks) != 0 {
		t.Fatal("local fixture endpoint identity mismatch")
	}
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("owned guard connection failed")
	}
	defer conn.Close(context.Background())
	var actor, directory string
	var roles, databases, sessions int
	err = conn.QueryRow(ctx, `SELECT session_user,current_setting('data_directory'),
 (SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)),
 (SELECT count(*) FROM pg_database WHERE datname NOT IN ('postgres','template0','template1')),
 (SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid())`, []string{"account_service", "account_migrator", "account_approval_control_owner", "account_approval_authorization_owner", "account_approval_writer", "account_approval_backfill", "account_approval_relay", "account_approval_recovery", "billing_service", "billing_schema_owner", "billing_migrator"}).Scan(&actor, &directory, &roles, &databases, &sessions)
	if err != nil || actor != "pinvou_wave_test" || !strings.EqualFold(strings.ReplaceAll(directory, "\\", "/"), "D:/Projects/pinvou3-account-functional/.cache/parallel-runtimes/f04/data") || roles != 0 || databases != 0 || sessions != 0 {
		t.Fatal("owned runtime/actors/databases/sessions guard failed before writes")
	}
}
