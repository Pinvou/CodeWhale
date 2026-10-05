package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Both domain repositories, Account authorization and HTTP gate are production
// implementations. The transport fixture owns a unique NATS stream and invokes
// the real Billing consumer; it does not replace the shared deployment stream.
func TestAccountBillingPostgresNATSLostCompletionACKConverges(t *testing.T) {
	for _, persistBeforeLoss := range []bool{false, true} {
		name := "before-account-completion"
		if persistBeforeLoss {
			name = "after-account-completion"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			billingRepo, wallet := billingFixture(t)
			accountPool := testdb.Open(t, "account")
			// The helper exports its owned database and schema in ConnString too,
			// so this second production pool remains within the same fixture.
			accountRepo, err := account.NewPostgresRepository(ctx, accountPool.Config().ConnString())
			if err != nil {
				t.Fatal("open isolated Account repository failed")
			}
			defer accountRepo.Close()
			makeActor := func(role string) (identity.Principal, uuid.UUID) {
				p := identity.Principal{Issuer: "https://approval.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), AuthenticatedAt: time.Now()}
				me, _, err := accountRepo.Bootstrap(ctx, p)
				if err != nil {
					t.Fatal("bootstrap approval actor failed")
				}
				if err = accountRepo.ActivateUser(ctx, me.UserID); err != nil {
					t.Fatal("activate approval actor failed")
				}
				if _, err = accountPool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,created_at) VALUES($1,$2,$3,'TEST',statement_timestamp())`, uuid.New(), me.UserID, role); err != nil {
					t.Fatal("install exact approval actor authority failed")
				}
				return p, me.UserID
			}
			requester, requesterID := makeActor("PLATFORM_BILLING_OPERATOR")
			approver, _ := makeActor("PLATFORM_BILLING_APPROVER")
			adjustment, err := billingRepo.RequestAdjustmentWithIdempotency(ctx, wallet.ID, 300, "approved joint domain repair", requesterID, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			normalized, _ := json.Marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": wallet.ID, "amount_microcredits": int64(300), "requested_by": requesterID})
			versions := json.RawMessage(`{"adjustment_version":1}`)
			draft, err := accountRepo.CreateApprovalDraft(ctx, requester, account.ApprovalDraft{CommandType: "WALLET_ADJUSTMENT", IdempotencyKey: uuid.NewString(), NormalizedCommand: normalized, TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(), Reason: "joint domain independent repair", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{"delta":300}`), FinancialSnapshot: json.RawMessage(`{"available":0}`), CriticalResourceVersions: versions})
			if err != nil {
				t.Fatal("create actual approval draft failed", err)
			}
			submitted, err := accountRepo.SubmitApprovalRequest(ctx, requester, draft.ID, draft.Version)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = accountRepo.DecideApprovalRequest(ctx, requester, draft.ID, submitted.Version, "APPROVED", "independent authorization", versions); !errors.Is(err, account.ErrApprovalSelfDecision) {
				t.Fatal("requester self-approved the financial command")
			}
			approved, err := accountRepo.DecideApprovalRequest(ctx, approver, draft.ID, submitted.Version, "APPROVED", "independent authorization", versions)
			if err != nil {
				t.Fatal("independent actual approval failed", err)
			}
			var event events.Event
			if err = accountPool.QueryRow(ctx, `SELECT event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.approval-command.authorized.v1'`, approved.ID).Scan(&event.ID, &event.Type, &event.AggregateID, &event.AggregateVersion, &event.Payload, &event.TraceID, &event.OccurredAt); err != nil {
				t.Fatal("read actual atomic authorization event failed", err)
			}
			handler := account.NewService(accountRepo, nil).WithServiceToken("joint-approval-fixture-token").Handler(nil)
			var mutex sync.Mutex
			lost := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/executions/complete") {
					mutex.Lock()
					first := !lost
					lost = true
					mutex.Unlock()
					if first {
						if persistBeforeLoss {
							response := httptest.NewRecorder()
							handler.ServeHTTP(response, r)
							if response.Code != 204 {
								t.Error("actual completion did not persist before lost ACK")
							}
						}
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error("could not inject lost completion acknowledgement")
							return
						}
						_ = conn.Close()
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			gate, err := NewHTTPApprovalExecutionGate(server.URL, "joint-approval-fixture-token")
			if err != nil {
				t.Fatal(err)
			}
			consumer := NewApprovalCommandConsumer(NewService(billingRepo).WithApprovalExecutionGate(gate))
			js, natsConsumer, subject := jointApprovalNATS(t, ctx)
			bytes, err := event.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = js.Publish(ctx, subject, bytes, jetstream.WithMsgID(event.ID.String())); err != nil {
				t.Fatal("publish actual authorization event failed")
			}
			message, err := natsConsumer.Next(jetstream.FetchMaxWait(4 * time.Second))
			if err != nil {
				t.Fatal("fetch first NATS authorization failed")
			}
			firstEvent, err := events.Unmarshal(message.Data())
			if err != nil || firstEvent.ID != event.ID {
				t.Fatal("NATS changed original job-run identity")
			}
			if err = consumer.Handle(ctx, firstEvent); !errors.Is(err, ErrApprovalCompletionPending) {
				t.Fatal("unknown completion acknowledgement was not retryable", err)
			}
			if err = message.NakWithDelay(10 * time.Millisecond); err != nil {
				t.Fatal(err)
			}
			retry, err := natsConsumer.Next(jetstream.FetchMaxWait(4 * time.Second))
			if err != nil {
				t.Fatal("real NATS redelivery failed")
			}
			metadata, err := retry.Metadata()
			if err != nil || metadata.NumDelivered != 2 {
				t.Fatal("did not receive actual broker redelivery")
			}
			replay, err := events.Unmarshal(retry.Data())
			if err != nil || replay.ID != event.ID {
				t.Fatal("redelivery changed stable event identity")
			}
			out := make(chan error, 8)
			for range 8 {
				go func() {
					for {
						err := consumer.Handle(ctx, replay)
						if !errors.Is(err, ErrApprovalExecutionUnavailable) && !errors.Is(err, ErrApprovalCompletionPending) {
							out <- err
							return
						}
						// Serializable first completion can reject a racing reader.
						// Only the existing explicit retryable outcomes are retried.
						select {
						case <-ctx.Done():
							out <- ctx.Err()
							return
						case <-time.After(10 * time.Millisecond):
						}
					}
				}()
			}
			for range 8 {
				if err = <-out; err != nil {
					t.Fatal("cross-domain concurrent retry did not converge", err)
				}
			}
			if err = retry.DoubleAck(ctx); err != nil {
				t.Fatal(err)
			}
			final, err := billingRepo.AdjustmentForExecution(ctx, adjustment.ID)
			if err != nil || final.Status != "POSTED" || final.Version != 2 || final.JournalEntryID == nil {
				t.Fatal("Billing did not retain exact posted receipt")
			}
			actual, err := billingRepo.GetWallet(ctx, wallet.ID)
			if err != nil || actual.PostedMicrocredits != 300 || actual.ReservedMicrocredits != 0 {
				t.Fatal("joint redelivery changed money more than once")
			}
			var status, jobRun string
			var completionCount, journals int
			if err = accountPool.QueryRow(ctx, `SELECT status,job_run_id FROM approval_executions WHERE approval_request_id=$1`, approved.ID).Scan(&status, &jobRun); err != nil || status != "EXECUTED" || jobRun != event.ID.String() {
				t.Fatal("Account did not retain the actual stable executed winner")
			}
			if err = accountPool.QueryRow(ctx, `SELECT count(*) FROM approval_state_events WHERE approval_request_id=$1 AND reason_code='EXECUTED'`, approved.ID).Scan(&completionCount); err != nil || completionCount != 1 {
				t.Fatal("duplicate Account completion audit")
			}
			if err = billingRepo.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE business_reference=$1`, "adjustment:"+adjustment.ID.String()).Scan(&journals); err != nil || journals != 1 {
				t.Fatal("duplicate Billing financial journal")
			}
			assertBalanced(t, billingRepo)
			for name, count := range runLedgerRestoreAudit(t, billingRepo) {
				if count != 0 {
					t.Fatalf("joint financial audit %s has %d violations", name, count)
				}
			}
		})
	}
}

func jointApprovalNATS(t *testing.T, ctx context.Context) (jetstream.JetStream, jetstream.Consumer, string) {
	t.Helper()
	raw := os.Getenv("PINVOU_ACCOUNT_TEST_NATS_URL")
	if raw == "" {
		t.Skip("PINVOU_ACCOUNT_TEST_NATS_URL not configured; actual NATS not tested")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "nats" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Fatal("NATS fixture requires literal loopback without credentials")
	}
	connection, err := nats.Connect(raw, nats.Timeout(3*time.Second), nats.NoReconnect())
	if err != nil {
		t.Fatal("connect owned test NATS failed")
	}
	js, err := jetstream.New(connection)
	if err != nil {
		connection.Close()
		t.Fatal(err)
	}
	name := "A08_APPROVAL_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	subject := "test." + name + ".authorized"
	if _, err = js.CreateStream(ctx, jetstream.StreamConfig{Name: name, Subjects: []string{subject}, Storage: jetstream.FileStorage}); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := js.DeleteStream(cleanup, name); err != nil {
			t.Error("cleanup only owned approval stream failed")
		}
		connection.Close()
	})
	consumer, err := js.CreateConsumer(ctx, name, jetstream.ConsumerConfig{Name: "joint", Durable: "joint", FilterSubject: subject, AckPolicy: jetstream.AckExplicitPolicy, AckWait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return js, consumer, subject
}
