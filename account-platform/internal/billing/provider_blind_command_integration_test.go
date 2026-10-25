package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Account permission decisions and IdP decoding are controlled. Installed
// Billing routes, owner checks, public migrations, billing_service writes,
// durable evidence and the provider recovery final writer are real.
func TestProviderBlindCommandReceiptPostgres(t *testing.T) {
	t.Run("lost-ack-replay-after-real-provider-resume", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		order, late, actor := providerBlindFixture(t, r, ctx)
		key, reason := "blind-command-"+uuid.NewString(), "controlled provider history is unavailable"
		originalACK := make(chan []byte, 1)
		var drop atomic.Bool
		drop.Store(true)
		allowed := &atomic.Bool{}
		allowed.Store(true)
		handler := providerBlindHandler(t, r, actor, allowed)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if drop.Swap(false) {
				capture := httptest.NewRecorder()
				handler.ServeHTTP(capture, request)
				if capture.Code != 200 {
					t.Error("first command did not commit", capture.Code, capture.Body.String())
				}
				originalACK <- bytes.Clone(capture.Body.Bytes())
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close() // The committed ACK is deliberately lost.
				return
			}
			handler.ServeHTTP(w, request)
		}))
		defer server.Close()
		response, err := providerBlindHTTP(server.URL, late.ID, actor, key, reason)
		if response != nil {
			response.Body.Close()
		}
		if err == nil {
			t.Fatal("lost ACK was incorrectly observable as success")
		}
		original := <-originalACK
		providerBlindResume(t, r, ctx, order, actor)
		before := providerBlindFacts(t, r, ctx)
		response, err = providerBlindHTTP(server.URL, late.ID, actor, key, reason)
		if err != nil {
			t.Fatal(err)
		}
		replay, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || !bytes.Equal(original, replay) || providerBlindFacts(t, r, ctx) != before {
			t.Fatalf("same-key replay re-paused the resumed provider or changed its original receipt: HTTP%d", response.StatusCode)
		}
		var enabled bool
		var version int
		if err := r.pool.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2`, order.ProviderID, order.ProviderVersion).Scan(&enabled, &version); err != nil || !enabled || version != 2 {
			t.Fatal("provider resume was overwritten", enabled, version, err)
		}
		allowed.Store(false)
		response, err = providerBlindHTTP(server.URL, late.ID, actor, key, reason)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 403 || providerBlindFacts(t, r, ctx) != before {
			t.Fatal("receipt bypassed current permission", response.StatusCode)
		}
		allowed.Store(true)
		for _, change := range []struct {
			who    uuid.UUID
			reason string
			key    string
			want   int
		}{
			{actor, "changed facts under the original command key", key, 409},
			{uuid.New(), reason, key, 409},
			{actor, reason, "", 400},
		} {
			response, err = providerBlindHTTP(server.URL, late.ID, change.who, change.key, change.reason)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != change.want || providerBlindFacts(t, r, ctx) != before {
				t.Fatal("rejected command changed state", response.StatusCode, change.want)
			}
		}
		response, err = providerBlindHTTP(server.URL, late.ID, actor, "explicit-new-"+uuid.NewString(), reason)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("a distinct explicit command was swallowed", response.StatusCode)
		}
		if err := r.pool.QueryRow(ctx, `SELECT new_orders_enabled,version FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2`, order.ProviderID, order.ProviderVersion).Scan(&enabled, &version); err != nil || enabled || version != 3 {
			t.Fatal("new command did not legitimately pause again", enabled, version, err)
		}
	})
	t.Run("first-receipt-matches-actual-database-case", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		_, late, actor := providerBlindFixture(t, r, ctx)
		now := time.Now().UTC().Truncate(time.Microsecond)
		r.now = func() time.Time { return now }
		oldPlan := now.Add(2 * time.Hour)
		if _, err := r.ScheduleLatePaymentReview(ctx, late.ID, actor, oldPlan, "controlled distinct original review plan", "provider-blind-prior-plan-"+uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		key, reason := "blind-snapshot-"+uuid.NewString(), "controlled actual review receipt snapshot"
		first, err := r.EscalateProviderBlind(ctx, late.ID, actor, reason, key)
		if err != nil {
			t.Fatal(err)
		}
		actual, found, err := scanLatePaymentReviewCase(r.pool.QueryRow(ctx, latePaymentReviewSelect+` WHERE late.id=$1`, late.ID))
		left, _ := json.Marshal(first)
		right, _ := json.Marshal(actual)
		var next, updated time.Time
		if queryErr := r.pool.QueryRow(ctx, `SELECT next_review_at,updated_at FROM late_payment_review_cases WHERE id=$1`, late.ID).Scan(&next, &updated); queryErr != nil {
			t.Fatal(queryErr)
		}
		if err != nil || !found || !bytes.Equal(left, right) || !next.Equal(now.Add(24*time.Hour)) || !updated.Equal(now) || next.Equal(oldPlan) {
			t.Fatalf("first receipt does not match the actual database case: receipt=%s database=%s next=%s updated=%s err=%v", left, right, next, updated, err)
		}
		before := providerBlindFacts(t, r, ctx)
		r.now = func() time.Time { return now.Add(time.Hour) }
		again, err := r.EscalateProviderBlind(ctx, late.ID, actor, reason, key)
		replay, _ := json.Marshal(again)
		if err != nil || !bytes.Equal(left, replay) || before != providerBlindFacts(t, r, ctx) {
			t.Fatal("accurate original database snapshot did not replay read-only", err)
		}
	})
	for _, failure := range []string{"receipt", "commit"} {
		t.Run(failure+"-failure-rolls-back-all-facts", func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, late, actor := providerBlindFixture(t, r, ctx)
			before := providerBlindFacts(t, r, ctx)
			ddl := `CREATE FUNCTION blind_command_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'owned provider blind command failure'; END $$; `
			table := "late_payment_review_evidence"
			if failure == "receipt" {
				ddl += `CREATE TRIGGER blind_command_reject BEFORE INSERT ON late_payment_review_evidence FOR EACH ROW EXECUTE FUNCTION blind_command_reject()`
			} else {
				table = "outbox_events"
				ddl += `CREATE CONSTRAINT TRIGGER blind_command_reject AFTER INSERT ON outbox_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION blind_command_reject()`
			}
			if _, err := retainedFinanceOwnerExec(t, r, ctx, ddl); err != nil {
				t.Fatal(err)
			}
			key, reason := "blind-failure-"+uuid.NewString(), "controlled immutable command recovery"
			if _, err := r.EscalateProviderBlind(ctx, late.ID, actor, reason, key); err == nil || providerBlindFacts(t, r, ctx) != before {
				t.Fatal("failed command committed partial state", err)
			}
			if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER blind_command_reject ON `+table+`; DROP FUNCTION blind_command_reject()`); err != nil {
				t.Fatal(err)
			}
			first, err := r.EscalateProviderBlind(ctx, late.ID, actor, reason, key)
			if err != nil {
				t.Fatal("same-key recovery after rollback", err)
			}
			committed := providerBlindFacts(t, r, ctx)
			again, err := r.EscalateProviderBlind(ctx, late.ID, actor, reason, key)
			left, _ := json.Marshal(first)
			right, _ := json.Marshal(again)
			if err != nil || !bytes.Equal(left, right) || providerBlindFacts(t, r, ctx) != committed {
				t.Fatal("recovered command did not replay exactly", err)
			}
		})
	}
	t.Run("immutable-evidence-and-case-facts-remain-bound", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		_, first, actor := providerBlindFixture(t, r, ctx)
		_, secondOrder, _, _ := retainedFinanceOrder(t, r, ctx, 10000, false)
		second, err := r.EscalateLatePaymentReview(ctx, secondOrder.ID, "CLOSED")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.ClaimLatePaymentReview(ctx, second.ID, actor); err != nil {
			t.Fatal(err)
		}
		key, reason := "blind-scope-"+uuid.NewString(), "controlled original request facts"
		if _, err := r.EscalateProviderBlind(ctx, first.ID, actor, reason, key); err != nil {
			t.Fatal(err)
		}
		before := providerBlindFacts(t, r, ctx)
		if _, err := r.EscalateProviderBlind(ctx, second.ID, actor, reason, key); err != ErrConflict || before != providerBlindFacts(t, r, ctx) {
			t.Fatal("original key was rebound to another case", err)
		}
		for _, statement := range []string{
			`UPDATE late_payment_review_evidence SET payload_snapshot='{}'::jsonb WHERE case_id=$1 AND evidence_type='PROVIDER_BLIND'`,
			`DELETE FROM late_payment_review_evidence WHERE case_id=$1 AND evidence_type='PROVIDER_BLIND'`,
		} {
			if _, err := r.pool.Exec(ctx, statement, first.ID); err == nil || !strings.Contains(err.Error(), "late payment review evidence is immutable") {
				t.Fatal("runtime receipt mutation did not hit the actual immutable trigger", err)
			}
		}
		if before != providerBlindFacts(t, r, ctx) {
			t.Fatal("immutable-evidence checks changed data")
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO late_payment_review_evidence(id,case_id,evidence_type,actor_id,payload_digest,payload_snapshot,created_at) SELECT $2,case_id,evidence_type,actor_id,repeat('a',64),payload_snapshot,created_at FROM late_payment_review_evidence WHERE case_id=$1 AND evidence_type='PROVIDER_BLIND' AND payload_snapshot->'provider_blind_command'->>'version'='1'`, first.ID, uuid.New()); err == nil || !strings.Contains(err.Error(), "late_payment_provider_blind_command_key") || before != providerBlindFacts(t, r, ctx) {
			t.Fatal("database did not enforce immutable actor/key uniqueness", err)
		}
		var truncate, ownerMembership bool
		var tableOwner string
		if err := r.pool.QueryRow(ctx, `SELECT has_table_privilege(current_user,'public.late_payment_review_evidence','TRUNCATE'),pg_has_role(current_user,'billing_schema_owner','MEMBER'),pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.late_payment_review_evidence'::regclass`).Scan(&truncate, &ownerMembership, &tableOwner); err != nil || truncate || ownerMembership || tableOwner != "billing_schema_owner" {
			t.Fatal("runtime receipt authority is broader than the installed deployment contract", truncate, ownerMembership, tableOwner, err)
		}
		if _, err := r.pool.Exec(ctx, `TRUNCATE late_payment_review_evidence`); err == nil || !strings.Contains(err.Error(), "42501") || before != providerBlindFacts(t, r, ctx) {
			t.Fatal("runtime TRUNCATE did not fail closed without changing facts", err)
		}
	})
	t.Run("highest-down-preserves-key-proof-and-migration-ledger", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		_, late, actor := providerBlindFixture(t, r, ctx)
		if _, err := r.EscalateProviderBlind(ctx, late.ID, actor, "controlled retained immutable command", "blind-down-"+uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		before := providerBlindFacts(t, r, ctx)
		var ledger string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(item) ORDER BY to_jsonb(item)::text)::text FROM schema_migrations item`).Scan(&ledger); err != nil {
			t.Fatal(err)
		}
		cfg := r.pool.Config().ConnConfig.Copy()
		cfg.User, cfg.Password = "billing_migrator", ""
		connection, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close(context.Background())
		if _, err := connection.Exec(ctx, `SET ROLE billing_service`); err != nil {
			t.Fatal(err)
		}
		loaded, err := migrations.Load("../../migrations/billing")
		if err != nil {
			t.Fatal(err)
		}
		if err = billingschema.DownOne(ctx, connection, loaded); err == nil || !strings.Contains(err.Error(), "55000") || !strings.Contains(err.Error(), "provider blind command receipts are forward-only") {
			t.Fatal("highest migration rollback lost the proof boundary", err)
		}
		var after string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(item) ORDER BY to_jsonb(item)::text)::text FROM schema_migrations item`).Scan(&after); err != nil || ledger != after || before != providerBlindFacts(t, r, ctx) {
			t.Fatal("failed down changed ledger or domain facts", err)
		}
		if _, err := billingschema.VerifyApplied(ctx, connection, loaded); err != nil {
			t.Fatal("failed down changed migration identity", err)
		}
	})
}

func providerBlindFixture(t *testing.T, r *PostgresRepository, ctx context.Context) (PaymentOrder, LatePaymentReviewCase, uuid.UUID) {
	t.Helper()
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 10000, false)
	input := validRechargeProductForEditTests()
	input.ProductID = "BLIND_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	input.Mapping.ProviderID, input.Mapping.ProviderVersion = order.ProviderID, order.ProviderVersion
	input.Mapping.ExternalSKUBOM, input.Mapping.AdmissionClass = input.ProductID, "FORMAL"
	item, err := r.CreateRechargeProduct(ctx, input)
	if err != nil {
		t.Fatal("create actual provider mapping", err)
	}
	verification, _, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
	if err != nil {
		t.Fatal("start actual mapping verification", err)
	}
	evidence, err := r.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "controlled-verifier", "CONTROLLED", "controlled-blind-mapping", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal("complete controlled provider evidence", err)
	}
	if _, err := r.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &evidence); err != nil {
		t.Fatal("publish verified mapping", err)
	}
	late, err := r.EscalateLatePaymentReview(ctx, order.ID, "CLOSED")
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	late, err = r.ClaimLatePaymentReview(ctx, late.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	return order, late, actor
}

func providerBlindResume(t *testing.T, r *PostgresRepository, ctx context.Context, order PaymentOrder, requester uuid.UUID) {
	t.Helper()
	frozen, err := r.providerResumeVersions(ctx, ProviderResumeVersions{ProviderID: order.ProviderID, ProviderVersion: order.ProviderVersion, ProbeOrderID: order.ID})
	if err != nil {
		t.Fatal("freeze actual provider recovery versions", err)
	}
	attempt, err := r.beginProviderAttempt(ctx, order.ProviderID, order.ProviderVersion, "PAYMENT_STATUS_QUERY", order.Channel, "controlled-blind-recovery-probe", ProviderCallScope{OrderID: order.ID, ProductID: order.ProductID, SubjectID: order.RequesterID, TrustedProbe: true})
	if err != nil {
		t.Fatal("admit actual recovery probe", err)
	}
	if err = r.finishProviderAttempt(ctx, attempt, "SUCCESS", nil); err != nil {
		t.Fatal("record actual recovery observation", err)
	}
	probe, err := latestTrustedProbeForOrder(ctx, r.pool, order.ID)
	if err != nil || probe == nil {
		t.Fatal(probe, err)
	}
	// Exercise the unchanged post-approval final writer with a real fresh probe;
	// full Account approval/IdP acceptance is deliberately not claimed here.
	if _, err = r.executeProviderResume(ctx, frozen, *probe, uuid.New(), requester, uuid.New(), "controlled approved provider recovery", order, ProviderOrder{Status: "PENDING", ProviderStatus: "PENDING", ProviderTraceID: "controlled-probe"}); err != nil {
		t.Fatal("execute unchanged provider recovery final writer", err)
	}
}

func providerBlindFacts(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	facts := map[string]json.RawMessage{}
	for _, table := range []string{"payment_provider_controls", "payment_product_controls", "manual_review_cases", "manual_review_case_events", "late_payment_review_cases", "late_payment_review_evidence", "payment_provider_resume_executions", "outbox_events", "payment_orders", "payment_transactions", "wallets", "journal_entries", "ledger_postings", "entitlement_packages", "entitlement_components", "receivables"} {
		var raw []byte
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(item) ORDER BY to_jsonb(item)::text),'[]'::jsonb) FROM `+table+` item`).Scan(&raw); err != nil {
			t.Fatal(fmt.Errorf("snapshot %s: %w", table, err))
		}
		facts[table] = raw
	}
	body, _ := json.Marshal(facts)
	return string(body)
}

func providerBlindHandler(t *testing.T, r *PostgresRepository, owner uuid.UUID, allowed *atomic.Bool) http.Handler {
	t.Helper()
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Subject    string `json:"subject"`
			Permission string `json:"permission"`
		}
		if request.Method != "POST" || request.URL.Path != "/internal/v1/authorization/check" || json.NewDecoder(request.Body).Decode(&body) != nil {
			t.Error("unexpected authority protocol")
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed.Load() && body.Permission == "platform.billing.orders.sync", "user_id": body.Subject})
	}))
	t.Cleanup(authority.Close)
	authorizer, err := authorization.NewClient(authority.URL, "controlled-service")
	if err != nil {
		t.Fatal(err)
	}
	return NewService(r).WithAuthorizer(authorizer).Handler("controlled-service", func(_ context.Context, token string) (identity.Principal, error) {
		return identity.Principal{Issuer: "controlled", Subject: token, AuthenticatedAt: r.now()}, nil
	})
}

func providerBlindHTTP(origin string, id, actor uuid.UUID, key, reason string) (*http.Response, error) {
	body, _ := json.Marshal(map[string]any{"reason": reason, "command_protocol": 1})
	request, _ := http.NewRequest("POST", origin+"/admin/v1/late-payment-reviews/"+id.String()+"/provider-blind", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+actor.String())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	return (&http.Client{Timeout: 15 * time.Second}).Do(request)
}
