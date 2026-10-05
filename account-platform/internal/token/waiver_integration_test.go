package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type waiverFixture struct {
	token          *PostgresRepository
	billing        *billingdomain.PostgresRepository
	billingPool    *pgxpool.Pool
	usage          UsageRequest
	hold           billingdomain.Hold
	wallet         billingdomain.Wallet
	client         *HTTPBillingClient
	admin          http.Handler
	internal       http.Handler
	intent         WaiverIntent
	blockCommit    atomic.Bool
	commitEntered  chan struct{}
	commitContinue chan struct{}
}

// Two real PostgreSQL domains and production HTTP routes/clients; OIDC
// verification and model consumption are controlled fixtures, not live KC/model.
func newWaiverFixture(t *testing.T, active bool) *waiverFixture {
	t.Helper()
	ctx := context.Background()
	repo, usage := settlementFixture(t)
	pool := testdb.Open(t, "billing")
	dsn, err := url.Parse(pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	q := dsn.Query()
	q.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
	dsn.RawQuery = q.Encode()
	billing, err := billingdomain.NewPostgresRepository(ctx, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(billing.Close)
	wallet, _, err := billing.EnsureWallet(ctx, "PERSONAL", usage.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = billing.Credit(ctx, wallet.ID, "waiver-fixture", 100, true); err != nil {
		t.Fatal(err)
	}
	price := uuid.New()
	if _, err = pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at) VALUES($1,$2,1000000,0,0,now()-interval '1 hour',now())`, price, usage.Model); err != nil {
		t.Fatal(err)
	}
	hold, err := billing.Reserve(ctx, usage.ID, wallet.ID, 100, &price, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.pool.Exec(ctx, `UPDATE usage_requests SET hold_id=$2,price_version_id=$3,wallet_id=$4 WHERE id=$1`, usage.ID, hold.ID, price, wallet.ID); err != nil {
		t.Fatal(err)
	}
	if err = repo.RecordUpstreamRequest(ctx, usage.ID, "owned-waiver-model-request"); err != nil {
		t.Fatal(err)
	}
	if !active {
		if err = repo.MarkReview(ctx, usage.ID, "fixture_uncertain", map[string]any{"usage": Usage{InputTokens: 7}, "actual_microcredits": int64(7)}); err != nil {
			t.Fatal(err)
		}
	}
	token := strings.Repeat("w", 32)
	f := &waiverFixture{token: repo, billing: billing, billingPool: pool, usage: usage, hold: hold, wallet: wallet, commitEntered: make(chan struct{}), commitContinue: make(chan struct{}), intent: WaiverIntent{UsageID: usage.ID, HoldID: hold.ID, Reason: "operator final waiver", Actor: "fixture-issuer|fixture-admin"}}
	service := billingdomain.NewService(billing).WithAuthorizer(tokenTestAuthorizer(t))
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "fixture-issuer", Subject: "fixture-admin", RealmRoles: []string{"platform-admin"}}, nil
	}
	billingHandler := service.Handler(token, verify)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/holds/commit" && f.blockCommit.CompareAndSwap(true, false) {
			close(f.commitEntered)
			select {
			case <-f.commitContinue:
			case <-r.Context().Done():
				return
			}
		}
		billingHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := NewHTTPBillingClient(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	f.client = client
	tokenService := NewService(repo, nil, nil, nil, ProviderURLPolicy{}, client, nil, 1).WithAuthorizer(tokenTestAuthorizer(t)).WithWaivers(token)
	f.internal = tokenService.Handler(verify)
	tokenServer := httptest.NewServer(f.internal)
	t.Cleanup(tokenServer.Close)
	coordinator, err := billingdomain.NewHTTPWaiverCoordinator(tokenServer.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	service.WithWaiverCoordinator(coordinator)
	f.admin = billingHandler
	return f
}

func (f *waiverFixture) release(t *testing.T, reason string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"reason": reason})
	r := httptest.NewRequest("POST", "/admin/v1/usage-holds/"+f.hold.ID.String()+"/release", strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer fixture-admin")
	w := httptest.NewRecorder()
	f.admin.ServeHTTP(w, r)
	return w.Code
}

func (f *waiverFixture) assertTerminal(t *testing.T, status string, charged int64) {
	t.Helper()
	ctx := context.Background()
	u, err := f.token.GetUsage(ctx, f.usage.ID)
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := f.billing.GetWallet(ctx, f.wallet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != status || u.ActualMicrocredits == nil || *u.ActualMicrocredits != charged || wallet.PostedMicrocredits != 100-charged || wallet.ReservedMicrocredits != 0 {
		t.Fatal("cross-domain terminal or monetary projection disagrees with authority")
	}
	var holdStatus string
	var count int
	if err = f.billingPool.QueryRow(ctx, `SELECT status FROM usage_holds WHERE id=$1`, f.hold.ID).Scan(&holdStatus); err != nil || holdStatus != status {
		t.Fatal("hold and usage terminal differ")
	}
	if err = f.token.pool.QueryRow(ctx, `SELECT count(*) FROM usage_requests WHERE id=$1 AND (reconciliation_lease_until IS NOT NULL OR reconciliation_next_at IS NOT NULL)`, f.usage.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("terminal usage retained scheduled settlement")
	}
}

func TestPostgresHTTPAdminWaiverPreservesEvidenceAndLateSettlementCannotCharge(t *testing.T) {
	f := newWaiverFixture(t, false)
	ctx := context.Background()
	if f.release(t, f.intent.Reason) != 200 || f.release(t, "different replay reason") != 200 {
		t.Fatal("waiver/replay did not complete")
	}
	f.assertTerminal(t, "RELEASED", 0)
	if err := f.client.Commit(ctx, f.usage.ID, f.hold.ID, f.wallet.ID, 9, Usage{InputTokens: 9}); err == nil {
		t.Fatal("late consumption charged a waived hold")
	}
	if err := f.token.MarkReview(ctx, f.usage.ID, "settlement_pending", map[string]any{"usage": Usage{InputTokens: 9}, "actual_microcredits": int64(9)}); err != nil {
		t.Fatal(err)
	}
	f.assertTerminal(t, "RELEASED", 0)
	var input, calculated int64
	var actor, reason, attempt string
	if err := f.token.pool.QueryRow(ctx, `SELECT (u.usage_snapshot->'usage'->>'input_tokens')::bigint,(u.usage_snapshot->>'calculated_microcredits')::bigint,u.usage_snapshot->>'admin_actor',u.usage_snapshot->>'admin_release_reason',a.status FROM usage_requests u JOIN usage_attempts a ON a.usage_request_id=u.id WHERE u.id=$1`, f.usage.ID).Scan(&input, &calculated, &actor, &reason, &attempt); err != nil || input != 9 || calculated != 9 || actor != f.intent.Actor || reason != f.intent.Reason || attempt == "FAILED" {
		t.Fatal("waiver falsified consumption or immutable operator evidence")
	}
	if items, err := f.token.ClaimReconciliation(ctx, 1, time.Minute); err != nil || len(items) != 0 {
		t.Fatal("waived usage was claimed for further charging")
	}
}

func TestPostgresHTTPAdminWaiverRejectsActiveAndCommitWinner(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		f := newWaiverFixture(t, true)
		if f.release(t, f.intent.Reason) != 409 {
			t.Fatal("active upstream was waived")
		}
		u, _ := f.token.GetUsage(context.Background(), f.usage.ID)
		w, _ := f.billing.GetWallet(context.Background(), f.wallet.ID)
		if u.Status != "UPSTREAM_ACTIVE" || w.ReservedMicrocredits != 100 {
			t.Fatal("active refusal mutated funds")
		}
	})
	t.Run("commit-before-local-projection", func(t *testing.T) {
		f := newWaiverFixture(t, false)
		if err := f.client.Commit(context.Background(), f.usage.ID, f.hold.ID, f.wallet.ID, 5, Usage{InputTokens: 5}); err != nil {
			t.Fatal(err)
		}
		if f.release(t, f.intent.Reason) != 409 {
			t.Fatal("already charged authority was reported as successful waiver")
		}
		f.assertTerminal(t, "COMMITTED", 5)
		var tokens int64
		if err := f.token.pool.QueryRow(context.Background(), `SELECT (usage_snapshot->'usage'->>'input_tokens')::bigint FROM usage_requests WHERE id=$1`, f.usage.ID).Scan(&tokens); err != nil || tokens != 5 {
			t.Fatal("commit winner lost real usage evidence")
		}
	})
	t.Run("waiver-before-old-commit", func(t *testing.T) {
		f := newWaiverFixture(t, false)
		items, err := f.token.ClaimReconciliation(context.Background(), 1, time.Minute)
		if err != nil || len(items) != 1 {
			t.Fatal("old reconciliation claim missing")
		}
		reconciler := NewUsageReconciler(f.token, f.client, evidenceClientFake{evidence: UsageEvidence{Usage: &Usage{InputTokens: 5}}}, time.Second)
		f.blockCommit.Store(true)
		done := make(chan error, 1)
		go func() { done <- reconciler.reconcile(context.Background(), items[0]) }()
		select {
		case <-f.commitEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("old commit did not reach real HTTP boundary")
		}
		if f.release(t, f.intent.Reason) != 200 {
			close(f.commitContinue)
			t.Fatal("waiver did not win")
		}
		close(f.commitContinue)
		if err := <-done; err == nil {
			t.Fatal("old commit debited the waiver")
		}
		f.assertTerminal(t, "RELEASED", 0)
	})
}

func TestPostgresHTTPAdminWaiverFailuresRecoverPersistedIntent(t *testing.T) {
	for _, point := range []string{"intent", "authority", "projection"} {
		t.Run(point, func(t *testing.T) {
			f := newWaiverFixture(t, false)
			ctx := context.Background()
			pool := f.token.pool
			table, condition := "usage_requests", "NEW.error_class='admin_waiver_pending'"
			if point == "authority" {
				pool = f.billingPool
				table = "usage_holds"
				condition = "NEW.status='RELEASED'"
			}
			if point == "projection" {
				condition = "NEW.status='RELEASED'"
			}
			if _, err := pool.Exec(ctx, `CREATE FUNCTION owned_waiver_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF `+condition+` THEN RAISE EXCEPTION 'controlled waiver persistence failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER owned_waiver_failure BEFORE UPDATE ON `+table+` FOR EACH ROW EXECUTE FUNCTION owned_waiver_failure()`); err != nil {
				t.Fatal(err)
			}
			if f.release(t, f.intent.Reason) != 503 {
				t.Fatal("persistence failure reported waiver success")
			}
			var status string
			if err := f.billingPool.QueryRow(ctx, `SELECT status FROM usage_holds WHERE id=$1`, f.hold.ID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if (point == "projection" && status != "RELEASED") || (point != "projection" && status != "ACTIVE") {
				t.Fatal("failure crossed the wrong financial boundary")
			}
			if _, err := pool.Exec(ctx, `DROP TRIGGER owned_waiver_failure ON `+table+`; DROP FUNCTION owned_waiver_failure()`); err != nil {
				t.Fatal(err)
			}
			if point == "intent" {
				if f.release(t, f.intent.Reason) != 200 {
					t.Fatal("intent failure retry failed")
				}
			} else {
				// A new worker/process uses only persisted intent, even past ordinary
				// evidence-reconciliation age, without a live admin HTTP request.
				if _, err := f.token.pool.Exec(ctx, `UPDATE usage_requests SET started_at=now()-interval '2 days',reconciliation_next_at=now()-interval '1 second' WHERE id=$1`, f.usage.ID); err != nil {
					t.Fatal(err)
				}
				NewUsageReconciler(f.token, f.client, nil, time.Second).runOnce(ctx)
			}
			f.assertTerminal(t, "RELEASED", 0)
		})
	}
}

func TestPostgresHTTPAdminWaiverHandlesNormalReleaseRaceAndMissingLocalHold(t *testing.T) {
	for _, scenario := range []string{"normal-release-wins", "missing-local-hold"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWaiverFixture(t, false)
			ctx := context.Background()
			if scenario == "missing-local-hold" {
				snapshot, _ := json.Marshal(map[string]any{"hold_id": f.hold.ID, "release_reason": "unused", "usage": Usage{InputTokens: 7}})
				if _, err := f.token.pool.Exec(ctx, `UPDATE usage_requests SET hold_id=NULL,error_class='release_pending',usage_snapshot=$2 WHERE id=$1`, f.usage.ID, snapshot); err != nil {
					t.Fatal(err)
				}
			}
			_, intent, err := f.token.PrepareWaiver(ctx, f.intent)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "normal-release-wins" {
				if err = f.client.Release(ctx, f.usage.ID, f.hold.ID, f.wallet.ID, "upstream_not_started"); err != nil {
					t.Fatal(err)
				}
			}
			result, err := f.client.ResolveWaiver(ctx, f.usage.ID, intent)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.token.FinishWaiver(ctx, f.usage.ID, intent, result); err != nil {
				t.Fatal(err)
			}
			f.assertTerminal(t, "RELEASED", 0)
		})
	}
}

func TestPostgresHTTPConcurrentAdminWaiverAndCommitHaveOneFinancialWinner(t *testing.T) {
	f := newWaiverFixture(t, false)
	ctx := context.Background()
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			if i%2 == 0 {
				f.release(t, f.intent.Reason)
			} else {
				_ = f.client.Commit(ctx, f.usage.ID, f.hold.ID, f.wallet.ID, 5, Usage{InputTokens: 5})
			}
		}()
	}
	close(start)
	group.Wait()
	// Resolve a commit that won before its Token projection was recorded.
	_ = f.release(t, f.intent.Reason)
	wallet, err := f.billing.GetWallet(ctx, f.wallet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wallet.PostedMicrocredits == 100 {
		f.assertTerminal(t, "RELEASED", 0)
	} else if wallet.PostedMicrocredits == 95 {
		f.assertTerminal(t, "COMMITTED", 5)
	} else {
		t.Fatal("concurrent authority created more than one financial effect")
	}
}

func TestPostgresWaiverAuthorityRequiresCompleteUsageWalletAndCostBinding(t *testing.T) {
	for _, scenario := range []string{"wallet", "maximum", "negative-cost", "overbound-cost", "usage", "proof", "actor"} {
		t.Run(scenario, func(t *testing.T) {
			f := newWaiverFixture(t, false)
			ctx := context.Background()
			_, intent, err := f.token.PrepareWaiver(ctx, f.intent)
			if err != nil {
				t.Fatal(err)
			}
			authority, err := f.client.ResolveWaiver(ctx, f.usage.ID, intent)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(authority)
			var forged WaiverAuthority
			json.Unmarshal(encoded, &forged)
			switch scenario {
			case "wallet":
				forged.Hold.WalletID = uuid.New()
			case "maximum":
				forged.Hold.Maximum++
			case "negative-cost":
				negative := int64(-1)
				forged.Hold.Actual = &negative
			case "overbound-cost":
				cost := int64(101)
				forged.Hold.Actual = &cost
			case "usage":
				forged.Hold.UsageID = uuid.New()
			case "proof":
				forged.UsageSnapshot = json.RawMessage(`{}`)
			case "actor":
				forged.UsageSnapshot = json.RawMessage(`{"admin_waiver":true,"admin_actor":"forged","admin_release_reason":"operator final waiver"}`)
			}
			if err = f.token.FinishWaiver(ctx, f.usage.ID, intent, forged); err == nil {
				t.Fatal("unbound authority was projected into a terminal")
			}
			u, err := f.token.GetUsage(ctx, f.usage.ID)
			if err != nil || u.Status != "REVIEW" {
				t.Fatal("invalid authority changed local finality")
			}
			if err = f.token.FinishWaiver(ctx, f.usage.ID, intent, authority); err != nil {
				t.Fatal(err)
			}
			f.assertTerminal(t, "RELEASED", 0)
		})
	}
}

func TestPostgresWaiverInternalRouteRequiresServiceAuthAndHoldIdentity(t *testing.T) {
	f := newWaiverFixture(t, false)
	ctx := context.Background()
	for _, scenario := range []string{"public-jwt", "missing-auth", "bad-json", "wrong-hold", "wrong-usage"} {
		t.Run(scenario, func(t *testing.T) {
			intent := f.intent
			auth := "Bearer " + strings.Repeat("w", 32)
			status := 409
			switch scenario {
			case "public-jwt":
				auth = "Bearer fixture-admin"
				status = 401
			case "missing-auth":
				auth = ""
				status = 401
			case "bad-json":
				status = 400
			case "wrong-hold":
				intent.HoldID = uuid.New()
			case "wrong-usage":
				intent.UsageID = uuid.New()
				status = 404
			}
			body, _ := json.Marshal(intent)
			if scenario == "bad-json" {
				body = []byte(`{"unexpected":true}`)
			}
			r := httptest.NewRequest("POST", "/internal/v1/usage-waivers", strings.NewReader(string(body)))
			r.Header.Set("Authorization", auth)
			w := httptest.NewRecorder()
			f.internal.ServeHTTP(w, r)
			if w.Code != status {
				t.Fatal("private waiver authentication/identity boundary changed")
			}
		})
	}
	u, err := f.token.GetUsage(ctx, f.usage.ID)
	wallet, _ := f.billing.GetWallet(ctx, f.wallet.ID)
	if err != nil || u.Status != "REVIEW" || wallet.ReservedMicrocredits != 100 {
		t.Fatal("rejected private requests mutated funds")
	}
	if _, err = f.client.ResolveWaiver(ctx, uuid.New(), f.intent); err == nil {
		t.Fatal("Billing authority allowed a wrong usage/hold binding")
	}
}

type unboundWaiverCoordinator struct{ hold billingdomain.AdminHold }

func (c unboundWaiverCoordinator) Release(context.Context, uuid.UUID, uuid.UUID, string, string) (billingdomain.AdminHold, error) {
	return c.hold, nil
}

func TestPostgresAdminWaiverDoesNotReportUnboundOrChargedCoordinatorResultAsSuccess(t *testing.T) {
	f := newWaiverFixture(t, false)
	for _, scenario := range []string{"wallet", "usage", "charged", "positive-release"} {
		t.Run(scenario, func(t *testing.T) {
			current, err := f.billing.AdminUsageHold(context.Background(), f.hold.ID)
			if err != nil {
				t.Fatal(err)
			}
			zero := int64(0)
			current.Status = "RELEASED"
			current.ActualMicrocredits = &zero
			want := 503
			switch scenario {
			case "wallet":
				current.WalletID = uuid.New()
			case "usage":
				current.UsageRequestID = uuid.New()
			case "charged":
				current.Status = "COMMITTED"
				amount := int64(5)
				current.ActualMicrocredits = &amount
				want = 409
			case "positive-release":
				amount := int64(5)
				current.ActualMicrocredits = &amount
			}
			// A deliberately corrupt coordinator result tests the public handler's
			// contract; this fixture does not claim that any financial write occurred.
			handler := billingdomain.NewService(f.billing).WithAuthorizer(tokenTestAuthorizer(t)).WithWaiverCoordinator(unboundWaiverCoordinator{hold: current}).Handler(strings.Repeat("w", 32), func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "fixture-issuer", Subject: "fixture-admin", RealmRoles: []string{"platform-admin"}}, nil
			})
			request := httptest.NewRequest("POST", "/admin/v1/usage-holds/"+f.hold.ID.String()+"/release", strings.NewReader(`{"reason":"fixture"}`))
			request.Header.Set("Authorization", "Bearer fixture-admin")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != want {
				t.Fatal("public administration accepted an invalid successful financial projection")
			}
		})
	}
	wallet, err := f.billing.GetWallet(context.Background(), f.wallet.ID)
	if err != nil || wallet.ReservedMicrocredits != 100 {
		t.Fatal("invalid coordinator result changed funds")
	}
}
