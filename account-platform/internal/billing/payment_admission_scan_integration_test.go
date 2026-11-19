package billing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPaymentActionAdmissionPostgres(t *testing.T) {
	f06WalletActors(t)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	payer, org := uuid.New(), uuid.New()
	source := "192.0.2.10"
	digest := sha256.Sum256([]byte(source))
	run := func(name string, check func(*testing.T, *PostgresRepository, context.Context, *PaymentCoordinator)) {
		t.Run(name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			p, err := newBillingFixturePaymentCoordinator(r, &retainedFinancialProvider{}, "test")
			if err != nil {
				t.Fatal(err)
			}
			p.now = func() time.Time { return now }
			check(t, r, ctx, p)
		})
	}
	seed := func(t *testing.T, r *PostgresRepository, ctx context.Context, action string, who uuid.UUID, organization *uuid.UUID, hash []byte, provider, result string, at time.Time, count int) {
		t.Helper()
		// Nil dimensions request independent nuisance identities for every row.
		for i := 0; i < count; i++ {
			attemptPayer := who
			if attemptPayer == uuid.Nil {
				attemptPayer = uuid.New()
			}
			attemptSource := hash
			if attemptSource == nil {
				independentSource := sha256.Sum256([]byte(uuid.NewString()))
				attemptSource = independentSource[:]
			}
			if _, err := r.pool.Exec(ctx, `INSERT INTO payment_risk_attempts(id,action,payer_user_id,beneficiary_organization_id,source_hash,provider_id,result,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, uuid.New(), action, attemptPayer, organization, attemptSource, provider, result, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	limited := func(t *testing.T, err error, retry time.Duration) {
		t.Helper()
		var rate *PaymentRateLimitError
		if !errors.As(err, &rate) || rate.RetryAfter != retry {
			t.Fatalf("rate error=%v want retry=%v", err, retry)
		}
	}
	run("cross-provider-max-retry-and-http", func(t *testing.T, r *PostgresRepository, ctx context.Context, p *PaymentCoordinator) {
		wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", payer)
		if err != nil {
			t.Fatal(err)
		}
		product := retainedSameKeyProduct(t, r, ctx, payer)
		provider := &coreRecoveryProvider{retainedFinancialProvider: &retainedFinancialProvider{}}
		coordinator, err := newBillingFixturePaymentCoordinator(r, provider, "test")
		if err != nil {
			t.Fatal(err)
		}
		coordinator.AllowTentativePrices(true)
		admissionNow := time.Now().UTC().Truncate(time.Second)
		coordinator.now = func() time.Time { return admissionNow }
		seed(t, r, ctx, "CREATE", payer, nil, digest[:], "other", "ACCEPTED", admissionNow.Add(-20*time.Second), 5)
		for i := 0; i < 15; i++ {
			seed(t, r, ctx, "CREATE", payer, nil, digest[:], "other", "ACCEPTED", admissionNow.Add(-2*time.Hour+time.Duration(i)*time.Minute), 1)
		}
		body, err := json.Marshal(map[string]any{"requester_id": payer, "wallet_id": wallet.ID, "product_id": product, "channel": "ALIPAY", "mobile": "+8613800138000"})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/recharge/orders", bytes.NewReader(body))
		request.RemoteAddr = source + ":1234"
		request.Header.Set("Idempotency-Key", uuid.NewString())
		response := httptest.NewRecorder()
		signed := tokenWalletWorkload(t)
		t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"token","service":"token-gateway","permissions":["internal.billing.recharge-order.create"]}]`)
		request.Header.Set("Authorization", "Bearer "+signed("token"))
		request.Header.Set("Content-Type", "application/json")
		NewService(r).WithPayments(coordinator).Handler("").ServeHTTP(response, request)
		if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "79200" {
			t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
		}
		if provider.creates.Load() != 0 {
			t.Fatal("limited request dispatched a provider create")
		}
		var rejected int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_risk_attempts WHERE action='CREATE' AND result='RATE_LIMITED'`).Scan(&rejected); err != nil || rejected != 1 {
			t.Fatal("rejected attempt must commit", rejected, err)
		}
	})
	run("strict-minute-boundary-and-rejected-rows", func(t *testing.T, r *PostgresRepository, ctx context.Context, p *PaymentCoordinator) {
		seed(t, r, ctx, "CREATE", payer, nil, digest[:], "other", "ACCEPTED", now.Add(-time.Minute), 1)
		seed(t, r, ctx, "CREATE", payer, nil, digest[:], "other", "ACCEPTED", now.Add(-20*time.Second), 4)
		seed(t, r, ctx, "CREATE", payer, nil, digest[:], "other", "RATE_LIMITED", now.Add(-time.Second), 20)
		if err := p.AdmitCreate(ctx, payer, nil, source); err != nil {
			t.Fatal(err)
		}
		limited(t, p.AdmitCreate(ctx, payer, nil, source), 40*time.Second)
	})
	run("strict-day-boundary", func(t *testing.T, r *PostgresRepository, ctx context.Context, p *PaymentCoordinator) {
		for i := 0; i < 20; i++ {
			seed(t, r, ctx, "CREATE", payer, nil, digest[:], "other", "ACCEPTED", now.Add(-24*time.Hour-time.Duration(i)*time.Minute), 1)
		}
		if err := p.AdmitCreate(ctx, payer, nil, source); err != nil {
			t.Fatal(err)
		}
	})
	run("organization-cross-provider-and-null", func(t *testing.T, r *PostgresRepository, ctx context.Context, p *PaymentCoordinator) {
		boundaryOrg := uuid.New()
		seed(t, r, ctx, "CREATE", uuid.Nil, &boundaryOrg, nil, "other", "ACCEPTED", now.Add(-time.Hour), 30)
		if err := p.AdmitCreate(ctx, payer, &boundaryOrg, "192.0.2.11"); err != nil {
			t.Fatal(err)
		}
		seed(t, r, ctx, "CREATE", uuid.Nil, &org, nil, "other", "ACCEPTED", now.Add(-30*time.Minute), 30)
		limited(t, p.AdmitCreate(ctx, payer, &org, "192.0.2.11"), 30*time.Minute)
		if err := p.AdmitCreate(ctx, payer, nil, "192.0.2.11"); err != nil {
			t.Fatal(err)
		}
	})
	run("source-cross-provider", func(t *testing.T, r *PostgresRepository, ctx context.Context, p *PaymentCoordinator) {
		seed(t, r, ctx, "CREATE", uuid.Nil, nil, digest[:], "other", "ACCEPTED", now.Add(-10*time.Second), 20)
		limited(t, p.AdmitCreate(ctx, payer, nil, source), 50*time.Second)
	})
	run("provider-minute-not-day", func(t *testing.T, r *PostgresRepository, ctx context.Context, p *PaymentCoordinator) {
		seed(t, r, ctx, "CREATE", uuid.Nil, nil, nil, "controlled", "ACCEPTED", now.Add(-time.Minute), 80)
		if err := p.AdmitCreate(ctx, payer, nil, "192.0.2.11"); err != nil {
			t.Fatal(err)
		}
		// Keep these two histories separate. Eighty accepted boundary attempts
		// would have prevented another accepted batch ten seconds before now.
		r, ctx = retainedFinanceRepository(t)
		p, err := newBillingFixturePaymentCoordinator(r, &retainedFinancialProvider{}, "test")
		if err != nil {
			t.Fatal(err)
		}
		p.now = func() time.Time { return now }
		seed(t, r, ctx, "CREATE", uuid.Nil, nil, nil, "controlled", "ACCEPTED", now.Add(-10*time.Second), 80)
		limited(t, p.AdmitCreate(ctx, uuid.New(), nil, "192.0.2.12"), 50*time.Second)
	})
	run("refund-hour-action-and-provider", func(t *testing.T, r *PostgresRepository, ctx context.Context, p *PaymentCoordinator) {
		_, order, transaction, packageID := retainedFinanceOrder(t, r, ctx, 1000000, true)
		seedRefund := func(at time.Time, count int) {
			t.Helper()
			for i := 0; i < count; i++ {
				if _, err := r.pool.Exec(ctx, `INSERT INTO payment_risk_attempts(id,action,payer_user_id,source_hash,provider_id,result,payment_transaction_id,entitlement_package_id,occurred_at) VALUES($1,'REFUND',$2,$3,'other','ACCEPTED',$4,$5,$6)`, uuid.New(), order.RequesterID, digest[:], transaction, packageID, at); err != nil {
					t.Fatal(err)
				}
			}
		}
		seedRefund(now.Add(-time.Hour), 3)
		seed(t, r, ctx, "CREATE", order.RequesterID, nil, digest[:], "other", "ACCEPTED", now.Add(-time.Second), 3)
		if err := p.AdmitRefund(ctx, order.RequesterID, transaction, packageID, source); err != nil {
			t.Fatalf("refund at the strict hour boundary must be accepted: %v", err)
		}
		var accepted int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_risk_attempts WHERE action='REFUND' AND payer_user_id=$1 AND payment_transaction_id=$2 AND entitlement_package_id=$3 AND result='ACCEPTED' AND occurred_at=$4`, order.RequesterID, transaction, packageID, now).Scan(&accepted); err != nil || accepted != 1 {
			t.Fatal("accepted refund admission must commit its valid financial references", accepted, err)
		}
		// A separate valid payer can have two accepted attempts half an hour ago.
		// The boundary payer already had three then and could not have done so.
		_, order, transaction, packageID = retainedFinanceOrder(t, r, ctx, 1000000, true)
		seedRefund(now.Add(-30*time.Minute), 2)
		if err := p.AdmitRefund(ctx, order.RequesterID, transaction, packageID, source); err != nil {
			t.Fatal("third refund admission must be accepted", err)
		}
		limited(t, p.AdmitRefund(ctx, order.RequesterID, transaction, packageID, source), 30*time.Minute)
		var rejected int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_risk_attempts WHERE action='REFUND' AND payer_user_id=$1 AND payment_transaction_id=$2 AND entitlement_package_id=$3 AND result='RATE_LIMITED' AND occurred_at=$4`, order.RequesterID, transaction, packageID, now).Scan(&rejected); err != nil || rejected != 1 {
			t.Fatal("limited refund admission must commit its valid financial references", rejected, err)
		}
	})
	run("concurrent-payer-limit", func(t *testing.T, r *PostgresRepository, ctx context.Context, p *PaymentCoordinator) {
		start := make(chan struct{})
		results := make(chan error, 12)
		var workers sync.WaitGroup
		for i := 0; i < 12; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				results <- p.AdmitCreate(ctx, payer, nil, source)
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		accepted, rejected := 0, 0
		for err := range results {
			if err == nil {
				accepted++
			} else {
				limited(t, err, time.Minute)
				rejected++
			}
		}
		if accepted != 5 || rejected != 7 {
			t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
		}
		var stored int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_risk_attempts`).Scan(&stored); err != nil || stored != 12 {
			t.Fatal(stored, err)
		}
	})
	run("bounded-scan-single-provider", func(t *testing.T, r *PostgresRepository, ctx context.Context, p *PaymentCoordinator) {
		if _, err := r.pool.Exec(ctx, `INSERT INTO payment_risk_attempts(id,action,payer_user_id,source_hash,provider_id,result,occurred_at) SELECT md5('unrelated-'||i)::uuid,'CREATE',md5('payer-'||i)::uuid,decode(md5('source-'||i)||md5('source-'||i),'hex'),'controlled','ACCEPTED',$1::timestamptz-interval '2 hours'-i*interval '2 seconds' FROM generate_series(1,30000) i`, now); err != nil {
			t.Fatal(err)
		}
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `ANALYZE payment_risk_attempts`); err != nil {
			t.Fatal(err)
		}
		var raw []byte
		if err := r.pool.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+paymentActionAdmissionSQL, payer, nil, digest[:], now, "controlled", "CREATE").Scan(&raw); err != nil {
			t.Fatal(err)
		}
		type plan struct {
			Node      string            `json:"Node Type"`
			Relation  string            `json:"Relation Name"`
			Rows      float64           `json:"Actual Rows"`
			Loops     float64           `json:"Actual Loops"`
			Removed   float64           `json:"Rows Removed by Filter"`
			Rechecked float64           `json:"Rows Removed by Index Recheck"`
			Plans     []json.RawMessage `json:"Plans"`
		}
		var plans []struct{ Plan json.RawMessage }
		if err := json.Unmarshal(raw, &plans); err != nil {
			t.Fatal(err)
		}
		var visited float64
		var relationScans int
		var visit func(json.RawMessage)
		visit = func(raw json.RawMessage) {
			var node plan
			if err := json.Unmarshal(raw, &node); err != nil {
				t.Fatal(err)
			}
			switch node.Node {
			case "Seq Scan", "Index Scan", "Index Only Scan", "Bitmap Heap Scan":
				if node.Relation == "payment_risk_attempts" {
					relationScans++
					visited += (node.Rows + node.Removed + node.Rechecked) * node.Loops
				}
			}
			for _, child := range node.Plans {
				visit(child)
			}
		}
		visit(plans[0].Plan)
		if relationScans == 0 {
			t.Fatalf("EXPLAIN did not execute a payment_risk_attempts relation scan: %s", raw)
		}
		t.Logf("actual relation scan rows including filtered rows = %.0f", visited)
		if visited >= 1000 {
			t.Fatalf("admission scanned %.0f unrelated rows; plan=%s", visited, raw)
		}
		if err := p.AdmitCreate(ctx, payer, nil, source); err != nil {
			t.Fatal(err)
		}
	})
}
