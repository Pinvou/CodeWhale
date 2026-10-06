package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func anomalyResolutionFixture(t *testing.T) (*PostgresRepository, Wallet, ForcedRefundCase, uuid.UUID, func(string, string, string) (int, string)) {
	t.Helper()
	r, wallet, transaction := forcedRefundPaidSource(t)
	item, err := r.RecordAndFreezeForcedRefund(context.Background(), transaction, "controlled-intent", 100, json.RawMessage(`{"confirmed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{Issuer: "https://fixture-authority.invalid", Subject: "fixture-platform-operator"}
	server := httptest.NewServer(NewService(r).WithAuthorizer(billingTestAuthorizer(t)).Handler("controlled-service-token", func(context.Context, string) (identity.Principal, error) { return principal, nil }))
	t.Cleanup(server.Close)
	call := func(key, resolution, reason string) (int, string) {
		body, _ := json.Marshal(map[string]string{"resolution": resolution, "reason": reason})
		request, err := http.NewRequest(http.MethodPost, server.URL+"/admin/v1/payment-anomalies/"+item.ManualReviewCaseID.String()+"/resolve", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer controlled-fixture-user")
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		payload, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(payload)
	}
	return r, wallet, item, adminActor(principal), call
}

func assertAnomalyFinancialFacts(t *testing.T, r *PostgresRepository, walletID uuid.UUID, posted int64, journals int) {
	t.Helper()
	current, err := r.GetWallet(context.Background(), walletID)
	if err != nil || current.PostedMicrocredits != posted || current.ReservedMicrocredits != 0 {
		t.Fatal("wrong financial result", err)
	}
	var actual int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM journal_entries WHERE entry_type='FORCED_REFUND'`).Scan(&actual); err != nil || actual != journals {
		t.Fatal("wrong financial winner count", actual, err)
	}
}

func TestPostgresAnomalyConflictPrecedesFinancialEffect(t *testing.T) {
	for _, stage := range []string{"claimed-key", "reserved-key", "already-resolved"} {
		t.Run(stage, func(t *testing.T) {
			r, wallet, item, actor, call := anomalyResolutionFixture(t)
			ctx, key, reason := context.Background(), uuid.NewString(), "controlled financial resolution"
			switch stage {
			case "claimed-key":
				if _, err := r.ClaimPaymentAnomaly(ctx, *item.ManualReviewCaseID, actor, key); err != nil {
					t.Fatal(err)
				}
			case "reserved-key":
				if _, complete, err := r.PreparePaymentAnomalyResolution(ctx, *item.ManualReviewCaseID, actor, key, "CONFIRM_FORCED_REFUND", reason); err != nil || complete {
					t.Fatal(err)
				}
				if _, err := r.ClaimPaymentAnomaly(ctx, *item.ManualReviewCaseID, actor, key); !errors.Is(err, ErrConflict) {
					t.Fatal("claim stole reserved financial command", err)
				}
				if status, _ := call(key, "CONFIRM_FORCED_REFUND", reason); status != 200 {
					t.Fatal(status)
				}
				assertAnomalyFinancialFacts(t, r, wallet.ID, 0, 1)
				return
			case "already-resolved":
				if status, _ := call(key, "ESCALATE_FINANCIAL_REPAIR", reason); status != 200 {
					t.Fatal(status)
				}
				key = uuid.NewString()
			}
			if status, _ := call(key, "CONFIRM_FORCED_REFUND", reason); status != 409 {
				t.Fatal("conflicting decision accepted", status)
			}
			assertAnomalyFinancialFacts(t, r, wallet.ID, wallet.PostedMicrocredits, 0)
		})
	}
}

// PostgreSQL faults occur at the actual financial journal or local receipt
// write. Retrying through the production Handler must reuse one durable intent.
func TestPostgresAnomalyIntentRecoversFinancialAndReceiptFailure(t *testing.T) {
	for _, stage := range []string{"financial-journal", "command-receipt"} {
		t.Run(stage, func(t *testing.T) {
			r, wallet, item, _, call := anomalyResolutionFixture(t)
			ctx, key, reason := context.Background(), uuid.NewString(), "controlled financial resolution"
			table, condition := "journal_entries", "NEW.entry_type='FORCED_REFUND'"
			if stage == "command-receipt" {
				table, condition = "payment_anomaly_commands", "true"
			}
			if _, err := r.pool.Exec(ctx, `CREATE FUNCTION controlled_intent_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF `+condition+` THEN RAISE EXCEPTION 'controlled receipt fault'; END IF; RETURN NEW; END $$; CREATE TRIGGER controlled_intent_fault BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION controlled_intent_fault()`); err != nil {
				t.Fatal(err)
			}
			if status, _ := call(key, "CONFIRM_FORCED_REFUND", reason); status < 500 {
				t.Fatal("fault was not exercised", status)
			}
			posted, journals := wallet.PostedMicrocredits, 0
			if stage == "command-receipt" {
				posted, journals = 0, 1
			}
			assertAnomalyFinancialFacts(t, r, wallet.ID, posted, journals)
			if status, _ := call(uuid.NewString(), "DISMISS", reason); status != 409 {
				t.Fatal("uncertain intent was replaced", status)
			}
			if _, _, err := r.PreparePaymentAnomalyResolution(ctx, *item.ManualReviewCaseID, uuid.New(), key, "CONFIRM_FORCED_REFUND", reason); !errors.Is(err, ErrConflict) {
				t.Fatal("intent actor changed", err)
			}
			if _, err := r.pool.Exec(ctx, `DROP TRIGGER controlled_intent_fault ON `+table+`; DROP FUNCTION controlled_intent_fault()`); err != nil {
				t.Fatal(err)
			}
			status, receipt := call(key, "CONFIRM_FORCED_REFUND", reason)
			if status != 200 {
				t.Fatal("same intent did not recover", status)
			}
			status, repeated := call(key, "CONFIRM_FORCED_REFUND", reason)
			if status != 200 || repeated != receipt {
				t.Fatal("completed receipt changed or replay reapplied finance", status)
			}
			if status, _ := call(key, "CONFIRM_FORCED_REFUND", reason+" changed"); status != 409 {
				t.Fatal("changed result facts accepted", status)
			}
			assertAnomalyFinancialFacts(t, r, wallet.ID, 0, 1)
			var intents, commands int
			if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_anomaly_resolution_intents),(SELECT count(*) FROM payment_anomaly_commands)`).Scan(&intents, &commands); err != nil || intents != 1 || commands != 1 {
				t.Fatal("duplicate intent or receipt", err)
			}
		})
	}
}

func TestPostgresAnomalyConcurrentIntentHasOneWinner(t *testing.T) {
	r, wallet, item, actor, _ := anomalyResolutionFixture(t)
	type outcome struct {
		key string
		err error
	}
	start, results := make(chan struct{}), make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			key := uuid.NewString()
			<-start
			_, _, err := r.PreparePaymentAnomalyResolution(context.Background(), *item.ManualReviewCaseID, actor, key, "CONFIRM_FORCED_REFUND", "controlled concurrent resolution")
			results <- outcome{key, err}
		}()
	}
	close(start)
	winners := 0
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err == nil {
			winners++
			continue
		}
		var serialization *pgconn.PgError
		if !errors.Is(result.err, ErrConflict) && !(errors.As(result.err, &serialization) && serialization.Code == "40001") {
			t.Fatal(result.err)
		}
	}
	if winners != 1 {
		t.Fatal("not exactly one intent winner", winners)
	}
	assertAnomalyFinancialFacts(t, r, wallet.ID, wallet.PostedMicrocredits, 0)
}

func TestPostgresAnomalyMigrationRollbackProtectsIntent(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty-roundtrip", true: "intent-guard"}[populated], func(t *testing.T) {
			ctx := context.Background()
			pool := testdb.Open(t, "billing")
			if populated {
				r, _, item, actor, _ := anomalyResolutionFixture(t)
				pool = r.pool
				if _, _, err := r.PreparePaymentAnomalyResolution(ctx, *item.ManualReviewCaseID, actor, uuid.NewString(), "CONFIRM_FORCED_REFUND", "controlled rollback protection"); err != nil {
					t.Fatal(err)
				}
			}
			all, err := migrations.Load("../../migrations/billing")
			if err != nil {
				t.Fatal(err)
			}
			connection, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Release()
			defer migrations.Unlock(ctx, connection.Conn())
			err = migrations.DownOne(ctx, connection.Conn(), all)
			if populated {
				if err == nil {
					t.Fatal("durable recovery intent was erased")
				}
				var version, intents int
				if err := connection.QueryRow(ctx, `SELECT (SELECT max(version) FROM schema_migrations),(SELECT count(*) FROM payment_anomaly_resolution_intents)`).Scan(&version, &intents); err != nil || version != 29 || intents != 1 {
					t.Fatal("rollback was not atomic", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := migrations.Up(ctx, connection.Conn(), all); err != nil {
				t.Fatal(err)
			}
		})
	}
}
