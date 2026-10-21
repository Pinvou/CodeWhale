package billing

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/paymentsim"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentsimulator"
	"github.com/Pinvou/pinvou3/account-platform/internal/simulatorschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The owned UUID database runs public simulator migrations and business SQL as
// the original limited payment_simulator role, including actual protected
// HTTP adapter/SQL. No supplier or real money is used. The counters distinguish
// read-only original-result recovery from creating/repeating any refund.
func refundQuerySimulator(t *testing.T, ctx context.Context) (*paymentsimulator.Provider, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	t.Setenv("APP_ENV", "test")
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	const actor = "payment_simulator"
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('owned:refund-query-simulator-runtime',0))`); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	var existing bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, actor).Scan(&existing); err != nil || existing {
		admin.Close(ctx)
		t.Fatal("existing simulator actor: no fixture writes permitted")
	}
	if _, err := admin.Exec(ctx, `CREATE ROLE payment_simulator LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS`); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	database := "refund_query_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{database}.Sanitize()
	created := false
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		defer admin.Close(cleanup)
		if created {
			var owner string
			var active int
			deadline := time.Now().Add(5 * time.Second)
			for {
				if err := admin.QueryRow(cleanup, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, database).Scan(&owner, &active); err != nil || owner != actor {
					t.Error("owned simulator database identity guard")
					return
				}
				if active == 0 || !time.Now().Before(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if active != 0 {
				t.Error("owned simulator database still has sessions")
				return
			}
			if _, err := admin.Exec(cleanup, "DROP DATABASE "+quoted); err != nil {
				t.Error("owned simulator database cleanup", err)
				return
			}
		}
		var outside bool
		if err := admin.QueryRow(cleanup, `SELECT EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass AND refobjid=(SELECT oid FROM pg_roles WHERE rolname=$1) AND dbid<>0) OR EXISTS(SELECT 1 FROM pg_auth_members WHERE roleid=(SELECT oid FROM pg_roles WHERE rolname=$1) OR member=(SELECT oid FROM pg_roles WHERE rolname=$1))`, actor).Scan(&outside); err != nil || outside {
			t.Error("simulator actor has external dependencies")
			return
		}
		if _, err := admin.Exec(cleanup, `DROP ROLE payment_simulator`); err != nil {
			t.Error("owned simulator actor cleanup", err)
		}
	})
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER payment_simulator"); err != nil {
		t.Fatal(err)
	}
	created = true
	configuration, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	configuration.Database, configuration.User, configuration.Password = database, actor, ""
	setup, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	var actual bool
	if err := setup.QueryRow(ctx, `SELECT current_user=$1 AND session_user=$1 AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls FROM pg_roles WHERE rolname=current_user`, actor).Scan(&actual); err != nil || !actual {
		setup.Close(ctx)
		t.Fatal("simulator actual runtime authority differs")
	}
	t.Log("actual simulator migration/runtime role=payment_simulator; superuser/createDB/createRole/replication/bypassRLS=false")
	loaded, err := migrations.Load("../../migrations/payment-simulator")
	if err == nil {
		err = simulatorschema.Up(ctx, setup, loaded)
	}
	setup.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.User(actor)
	u.Path = "/" + database
	q := u.Query()
	q.Del("search_path")
	u.RawQuery = q.Encode()
	const secret = "public-fixture-hmac-key-at-least-32-bytes"
	const token = "public-fixture-simulator-service-token"
	sim, err := paymentsim.NewPostgresRepository(ctx, u.String(), "http://127.0.0.1", "public-fixture-pay-token-at-least-32-bytes", map[string]string{"ALIPAY": secret, "WECHAT": secret, "UNIONPAY": secret})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sim.Close)
	reads, writes := &atomic.Int32{}, &atomic.Int32{}
	handler := paymentsim.NewService(sim).Handler(token)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/refunds") {
			if r.Method == http.MethodGet {
				reads.Add(1)
			}
			if r.Method == http.MethodPost {
				writes.Add(1)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	provider, err := paymentsimulator.New(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	// Public payment URLs deliberately point to the configured base. Rewrite
	// only that fixture base for completion; order creation/query use the adapter.
	refundSimulatorCompletionServers.Store(provider, server.URL)
	t.Cleanup(func() { refundSimulatorCompletionServers.Delete(provider) })
	return provider, reads, writes
}

var refundSimulatorCompletionServers sync.Map

func refundQuerySimulatorPaidOrder(t *testing.T, r *PostgresRepository, ctx context.Context, provider *paymentsimulator.Provider) (*PaymentCoordinator, PaymentOrder, uuid.UUID, uuid.UUID) {
	t.Helper()
	_, order, _, _ := retainedOrganizationFinanceOrder(t, r, ctx, 100, false)
	order.ProviderID, order.ProviderVersion = provider.Capabilities().ProviderID, provider.Capabilities().Version
	const sku = "controlled-real-mapped-sku"
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_id=$2,provider_version=$3,provider_sku=$4,product_mapping_snapshot='{"status":"TEST_ONLY"}'::jsonb WHERE id=$1`, order.ID, order.ProviderID, order.ProviderVersion, sku); err != nil {
		t.Fatal(err)
	}
	created, err := provider.Create(ctx, payment.Order{MerchantOrderNo: order.MerchantOrderNo, MerchantCustomerKey: order.RequesterID.String(), Mobile: "+8613800138000", ExternalSKU: sku, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency}, "http://127.0.0.1/internal/payment/notifications")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(created.PaymentURL)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := refundSimulatorCompletionServers.Load(provider)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.(string)+u.Path+"/complete", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("simulator payment rejected", response.StatusCode)
	}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	order, err = c.SyncOrder(ctx, order.ID)
	if err != nil || order.FulfillmentStatus != "CREDITED" {
		t.Fatal("real simulator payment credit", err)
	}
	var transaction, pack uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT t.id,p.id FROM payment_transactions t JOIN entitlement_packages p ON p.source_payment_transaction_id=t.id WHERE t.payment_order_id=$1`, order.ID).Scan(&transaction, &pack); err != nil {
		t.Fatal(err)
	}
	return c, order, transaction, pack
}

func TestRefundLegacyUnknownSuccessQueriesOriginalSimulatorThenRepairsPostgres(t *testing.T) {
	for _, reason := range []string{"MEMBER_REMOVED", "ACCOUNT_CLOSED"} {
		t.Run(reason, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			provider, reads, writes := refundQuerySimulator(t, ctx)
			c, order, transaction, pack := refundQuerySimulatorPaidOrder(t, r, ctx, provider)
			grant := refundRecoveryGrant(t, r, ctx, order, pack, reason)
			refund := refundRecoveryLegacyReservation(t, ctx, r, order, transaction, pack)
			// The predecessor dispatched and lost its ACK. The actual simulator
			// stores the original SUCCESS; recovery may only GET that exact key.
			if _, err := provider.Refund(ctx, order.MerchantOrderNo, refund.MerchantRefundNo, refund.AmountFen); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN' WHERE id=$1`, refund.ID); err != nil {
				t.Fatal(err)
			}
			before := refundRecoveryMoneySnapshot(t, ctx, r)
			refund, err := c.processRefund(ctx, refund.ID, true)
			if !errors.Is(err, ErrConflict) || refund.Status != "MANUAL_REVIEW" || refund.ChannelRefundID == nil || reads.Load() != 1 || writes.Load() != 1 || !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
				t.Fatal("coordinator failed read-only original SUCCESS recovery", err, reads.Load(), writes.Load())
			}
			input := refundRecoveryEscalate(t, ctx, r, refund, order)
			// Result-first recovery after the plan freezes must leave it executable.
			facts := refundEffectSnapshot(t, ctx, r)
			if _, err := c.processRefund(ctx, refund.ID, true); !errors.Is(err, ErrConflict) || reads.Load() != 1 || writes.Load() != 1 || facts != refundEffectSnapshot(t, ctx, r) {
				t.Fatal("persisted SUCCESS was requeried or changed frozen facts", err)
			}
			result, err := r.ApplyRefundExceptionRepair(ctx, input)
			if err != nil || result.RevokedMicrocredits != 100 || result.Status != "SUCCEEDED" {
				t.Fatal("governed simulator result repair", err)
			}
			refundRecoveryExactPostings(t, ctx, r, result, 100)
			var valid bool
			if err := r.pool.QueryRow(ctx, `SELECT g.status='REVOKED' AND g.revoked_microcredits=10 AND g.available_microcredits=0 AND l.status='REVOKED' FROM member_quota_grants g JOIN organization_member_lifecycle_states l ON l.organization_id=g.organization_id AND l.user_id=g.member_user_id WHERE g.id=$1`, grant.ID).Scan(&valid); err != nil || !valid {
				t.Fatal("financial repair restored member rights", err)
			}
			after := refundRecoveryMoneySnapshot(t, ctx, r)
			if before["organization_member_lifecycle_states"] != after["organization_member_lifecycle_states"] || before["account_closure_grant_isolations"] != after["account_closure_grant_isolations"] {
				t.Fatal("identity/closed-account facts changed")
			}
			if replay, err := r.ApplyRefundExceptionRepair(ctx, input); err != nil || replay != result || !reflect.DeepEqual(after, refundRecoveryMoneySnapshot(t, ctx, r)) || writes.Load() != 1 {
				t.Fatal("lost repair ACK repeated money", err)
			}
		})
	}
}

func TestRefundUnknownMissingSimulatorResultNeverCreatesRefundPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	provider, reads, writes := refundQuerySimulator(t, ctx)
	c, order, transaction, pack := refundQuerySimulatorPaidOrder(t, r, ctx, provider)
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "original never acknowledged")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN' WHERE id=$1`, refund.ID); err != nil {
		t.Fatal(err)
	}
	before := refundRecoveryMoneySnapshot(t, ctx, r)
	if err := c.ProcessRefund(ctx, refund.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.processRefund(ctx, refund.ID, true); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if reads.Load() != 2 || writes.Load() != 0 || !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
		t.Fatal("NOT_FOUND query dispatched money", reads.Load(), writes.Load())
	}
}

func TestRefundOriginalUnknownSuccessQueriesAndSettlesOrdinaryGrantsPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	provider, reads, writes := refundQuerySimulator(t, ctx)
	c, order, transaction, pack := refundQuerySimulatorPaidOrder(t, r, ctx, provider)
	grant := refundRecoveryGrant(t, r, ctx, order, pack, "")
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "original ACK lost with ordinary eligible grants")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Refund(ctx, order.MerchantOrderNo, refund.MerchantRefundNo, refund.AmountFen); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN' WHERE id=$1`, refund.ID); err != nil {
		t.Fatal(err)
	}
	got, err := c.processRefund(ctx, refund.ID, true)
	if err != nil || got.Status != "SUCCEEDED" || reads.Load() != 1 || writes.Load() != 1 {
		t.Fatal("original query failed ordinary settlement", err)
	}
	var journal uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id FROM journal_entries WHERE business_reference=$1`, "refund:"+refund.ID.String()).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	refundRecoveryExactPostings(t, ctx, r, RefundExceptionRepairResult{JournalEntryID: journal, WalletID: order.WalletID, PackageID: pack}, 100)
	var valid bool
	if err := r.pool.QueryRow(ctx, `SELECT g.status='REVOKED' AND g.revoked_microcredits=10 AND g.available_microcredits=0 AND l.status='ACTIVE' FROM member_quota_grants g JOIN organization_member_lifecycle_states l ON l.organization_id=g.organization_id AND l.user_id=g.member_user_id WHERE g.id=$1`, grant.ID).Scan(&valid); err != nil || !valid {
		t.Fatal("ordinary grant refund changed membership or failed precise revoke", err)
	}
	before := refundRecoveryMoneySnapshot(t, ctx, r)
	if err := c.ProcessRefund(ctx, refund.ID); err != nil || reads.Load() != 1 || writes.Load() != 1 || !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
		t.Fatal("lost ordinary ACK repeated effect", err)
	}
}
