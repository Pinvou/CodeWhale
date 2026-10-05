package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

// The Account approval service is controlled over real HTTP; Billing uses its
// production gate, consumer and real PostgreSQL migrations/ledger. This does
// not claim live Account authorization or an actual NATS redelivery.
func TestPostgresApprovedExecutionLostACKAndConcurrentRedelivery(t *testing.T) {
	for _, persistBeforeLoss := range []bool{false, true} {
		label := "before-completion-persistence"
		if persistBeforeLoss {
			label = "after-completion-persistence"
		}
		t.Run(label, func(t *testing.T) {
			ctx := context.Background()
			repo, wallet := billingFixture(t)
			requester, approver, approvalID, eventID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			adjustment, err := repo.RequestAdjustmentWithIdempotency(ctx, wallet.ID, 300, "independently requested credit", requester, "approved-execution-request")
			if err != nil {
				t.Fatal(err)
			}
			normalized, _ := json.Marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": wallet.ID, "amount_microcredits": int64(300), "requested_by": requester})
			versions := json.RawMessage(`{"adjustment_version":1}`)
			command := ApprovedCommand{ApprovalRequestID: approvalID, CommandType: "WALLET_ADJUSTMENT", NormalizedCommand: normalized, TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(), CriticalResourceVersions: versions, RequesterUserID: requester, ApproverUserID: approver, DecisionReason: "independent financial authorization", ExecutionIdempotencyKey: "approval:" + approvalID.String() + ":stable", Status: "EXECUTING"}
			var mutex sync.Mutex
			var begins, completions int
			lost := false
			var completedResult, completedLedger json.RawMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mutex.Lock()
				defer mutex.Unlock()
				if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer approval-fixture-service-token" {
					w.WriteHeader(403)
					return
				}
				var body struct {
					Executor   string          `json:"executor_identity"`
					JobRunID   string          `json:"job_run_id"`
					Observed   json.RawMessage `json:"observed_versions"`
					Result     string          `json:"result"`
					Domain     json.RawMessage `json:"domain_result"`
					Ledger     json.RawMessage `json:"ledger_references"`
					ErrorClass string          `json:"error_class"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Executor != billingApprovalExecutorIdentity || body.JobRunID != eventID.String() {
					w.WriteHeader(409)
					return
				}
				prefix := "/internal/v1/approval-requests/" + approvalID.String() + "/executions"
				if r.URL.Path == prefix {
					begins++
					var version struct {
						Version int64 `json:"adjustment_version"`
					}
					if json.Unmarshal(body.Observed, &version) != nil || version.Version != 1 {
						w.WriteHeader(409)
						return
					}
					_ = json.NewEncoder(w).Encode(command)
					return
				}
				if r.URL.Path != prefix+"/complete" || body.Result != "EXECUTED" || body.ErrorClass != "" {
					w.WriteHeader(409)
					return
				}
				completions++
				if command.Status == "EXECUTED" && (!sameApprovalFixtureJSON(completedResult, body.Domain) || !sameApprovalFixtureJSON(completedLedger, body.Ledger)) {
					w.WriteHeader(409)
					return
				}
				persist := func() {
					command.Status = "EXECUTED"
					completedResult = append([]byte(nil), body.Domain...)
					completedLedger = append([]byte(nil), body.Ledger...)
				}
				if !lost {
					lost = true
					if persistBeforeLoss {
						persist()
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error("could not simulate lost completion response")
						return
					}
					_ = conn.Close()
					return
				}
				persist()
				w.WriteHeader(204)
			}))
			defer server.Close()
			gate, err := NewHTTPApprovalExecutionGate(server.URL, "approval-fixture-service-token")
			if err != nil {
				t.Fatal(err)
			}
			consumer := NewApprovalCommandConsumer(NewService(repo).WithApprovalExecutionGate(gate))
			payload, _ := json.Marshal(map[string]any{"approval_request_id": approvalID, "command_type": "WALLET_ADJUSTMENT", "target_resource_type": "MANUAL_ADJUSTMENT", "target_resource_id": adjustment.ID, "critical_resource_versions": map[string]any{"adjustment_version": 1}, "executor_identity": billingApprovalExecutorIdentity})
			event := events.Event{ID: eventID, Type: "account.approval-command.authorized.v1", AggregateID: approvalID, AggregateVersion: 2, TraceID: "approval-fixture", Payload: payload, OccurredAt: time.Now()}
			if err := consumer.Handle(ctx, event); !errors.Is(err, ErrApprovalCompletionPending) {
				t.Fatal("lost ACK was not retryable after financial posting")
			}
			first, err := repo.AdjustmentForExecution(ctx, adjustment.ID)
			if err != nil || first.Status != "POSTED" || first.Version != 2 || first.JournalEntryID == nil {
				t.Fatal("approved financial execution was not persisted")
			}
			start := make(chan struct{})
			out := make(chan error, 8)
			for range 8 {
				go func() { <-start; out <- consumer.Handle(ctx, event) }()
			}
			close(start)
			for range 8 {
				if err := <-out; err != nil {
					t.Fatal("same event replay did not converge", err)
				}
			}
			final, err := repo.AdjustmentForExecution(ctx, adjustment.ID)
			if err != nil || final.Version != 2 || final.JournalEntryID == nil || *final.JournalEntryID != *first.JournalEntryID {
				t.Fatal("redelivery changed authoritative receipt")
			}
			actual, err := repo.GetWallet(ctx, wallet.ID)
			if err != nil || actual.PostedMicrocredits != 300 || actual.ReservedMicrocredits != 0 {
				t.Fatal("redelivery applied financial effect more than once")
			}
			var count int
			if err = repo.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE business_reference=$1`, "adjustment:"+adjustment.ID.String()).Scan(&count); err != nil || count != 1 {
				t.Fatal("redelivery emitted additional financial journals")
			}
			mutex.Lock()
			status, b, c := command.Status, begins, completions
			mutex.Unlock()
			if status != "EXECUTED" || b != 9 || c != 9 {
				t.Fatal("replay skipped authoritative Begin/immutable completion")
			}
			assertBalanced(t, repo)
			for name, count := range runLedgerRestoreAudit(t, repo) {
				if count != 0 {
					t.Fatalf("execution audit %s=%d", name, count)
				}
			}
			// A different delivery identity cannot claim the established execution.
			alternate := event
			alternate.ID = uuid.New()
			if err := consumer.Handle(ctx, alternate); err != nil {
				t.Fatal("non-executable delivery should be acknowledged without effect", err)
			}
			latest, err := repo.GetWallet(ctx, wallet.ID)
			if err != nil || latest.PostedMicrocredits != 300 {
				t.Fatal("different job-run stole financial execution")
			}
		})
	}
}

func sameApprovalFixtureJSON(left, right json.RawMessage) bool {
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return false
	}
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(aa) == string(bb)
}

func TestPostgresApprovedExecutionRejectsResourceVersionDriftBeforeFunds(t *testing.T) {
	ctx := context.Background()
	repo, wallet := billingFixture(t)
	requester, approver := uuid.New(), uuid.New()
	adjustment, err := repo.RequestAdjustment(ctx, wallet.ID, 100, "requested credit", requester)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.pool.Exec(ctx, `UPDATE manual_adjustments SET version=2 WHERE id=$1`, adjustment.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ExecuteApprovedAdjustment(ctx, adjustment.ID, uuid.New(), "approval-version-bound", approver, "independent approval", 1); !errors.Is(err, ErrConflict) {
		t.Fatal("stale financial resource version was applied")
	}
	actual, err := repo.GetWallet(ctx, wallet.ID)
	if err != nil || actual.PostedMicrocredits != 0 {
		t.Fatal("stale approval affected wallet")
	}
	var count int
	if err = repo.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries`).Scan(&count); err != nil || count != 0 {
		t.Fatal("stale approval emitted journal")
	}
}

func TestApprovalConsumerRejectsEnvelopeIdentityBeforeGate(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"approval_request_id": uuid.New(), "command_type": "WALLET_ADJUSTMENT", "target_resource_type": "MANUAL_ADJUSTMENT", "target_resource_id": uuid.New(), "critical_resource_versions": map[string]int{"adjustment_version": 1}, "executor_identity": billingApprovalExecutorIdentity})
	for _, event := range []events.Event{
		{Type: "account.approval-command.authorized.v1", Payload: payload},
		{ID: uuid.New(), Type: "account.approval-command.authorized.v1", AggregateID: uuid.New(), AggregateVersion: 2, TraceID: "valid-envelope-wrong-aggregate", Payload: payload},
	} {
		if err := NewApprovalCommandConsumer(&Service{}).Handle(context.Background(), event); err == nil {
			t.Fatal("invalid execution identity reached approval gate")
		}
	}
}

type rejectedCommandGate struct {
	command     ApprovedCommand
	unavailable bool
	completions int
}

func (g *rejectedCommandGate) Begin(context.Context, uuid.UUID, string, json.RawMessage) (ApprovedCommand, error) {
	return g.command, nil
}
func (g *rejectedCommandGate) Complete(_ context.Context, _ uuid.UUID, _ string, result string, _ json.RawMessage, _ json.RawMessage, errorClass string) error {
	g.completions++
	if result != "FAILED" || errorClass != "COMMAND_SNAPSHOT_MISMATCH" {
		return errors.New("unexpected failure completion facts")
	}
	if g.unavailable {
		return errors.New("completion unavailable")
	}
	return nil
}
func TestPostgresRejectedApprovedCommandRetainsCompletionRetry(t *testing.T) {
	ctx := context.Background()
	repo, wallet := billingFixture(t)
	requester, approval := uuid.New(), uuid.New()
	adjustment, err := repo.RequestAdjustment(ctx, wallet.ID, 100, "requested credit", requester)
	if err != nil {
		t.Fatal(err)
	}
	normalized, _ := json.Marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": wallet.ID, "amount_microcredits": int64(101), "requested_by": requester})
	versions := json.RawMessage(`{"adjustment_version":1}`)
	gate := &rejectedCommandGate{unavailable: true, command: ApprovedCommand{ApprovalRequestID: approval, CommandType: "WALLET_ADJUSTMENT", TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(), Status: "EXECUTING", CriticalResourceVersions: versions, NormalizedCommand: normalized, RequesterUserID: requester, ApproverUserID: uuid.New(), DecisionReason: "independent approval", ExecutionIdempotencyKey: "rejected-command-stable"}}
	service := NewService(repo).WithApprovalExecutionGate(gate)
	if _, err = service.executeApprovedAdjustmentCommand(ctx, adjustment.ID, approval, "original-event", versions); !errors.Is(err, ErrApprovalCompletionPending) {
		t.Fatal("lost rejection completion was incorrectly terminally acknowledged")
	}
	gate.unavailable = false
	if _, err = service.executeApprovedAdjustmentCommand(ctx, adjustment.ID, approval, "original-event", versions); !errors.Is(err, ErrApprovalSnapshotMismatch) || gate.completions != 2 {
		t.Fatal("rejected command did not converge exact failed completion")
	}
	w, err := repo.GetWallet(ctx, wallet.ID)
	if err != nil || w.PostedMicrocredits != 0 {
		t.Fatal("rejected approved command affected funds")
	}
}
