package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type organizationPurchaseProvider struct {
	payment.Provider
	creates, queries atomic.Int32
	lose             atomic.Bool
}

func (p *organizationPurchaseProvider) Create(ctx context.Context, in payment.Order, callback string) (ProviderOrder, error) {
	p.creates.Add(1)
	out, err := p.Provider.Create(ctx, in, callback)
	if err == nil && p.lose.Swap(false) {
		return ProviderOrder{}, io.ErrUnexpectedEOF
	}
	return out, err
}
func (p *organizationPurchaseProvider) Get(ctx context.Context, key string) (ProviderOrder, error) {
	p.queries.Add(1)
	return p.Provider.Get(ctx, key)
}

// Uses complete public migrations, actual restricted Billing/simulator actors,
// actual Token HTTP serializer and protected supplier. The simulator's payment
// completion is controlled; this is not real collection or a production deploy.
func TestOrganizationPlanPurchaseActualClientPostgres(t *testing.T) {
	f06WalletActors(t)
	r, ctx := retainedFinanceRepository(t)
	simulator, _, _ := refundQuerySimulator(t, ctx)
	provider := &organizationPurchaseProvider{Provider: simulator}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	c.AllowTentativePrices(true)
	// Explicitly admit a controlled fixture product through current catalog domain APIs.
	// The imported singleton PLAN mapping is not a simulator publication contract.
	input := validRechargeProductForEditTests()
	input.ProductID = "ORG_PLAN_" + uuid.NewString()
	input.BasePoints = 1000
	input.BonusPoints = 1000
	input.Mapping.ProviderID = simulator.Capabilities().ProviderID
	input.Mapping.ProviderVersion = simulator.Capabilities().Version
	input.Mapping.ExternalSKUBOM = "SIM_" + input.ProductID
	input.Mapping.AmountFen = 10
	item, e := r.CreateRechargeProduct(ctx, input)
	if e != nil {
		t.Fatal("fixture catalog create", e)
	}
	verification, _, e := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
	if e != nil {
		t.Fatal(e)
	}
	evidence, e := r.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "owned-simulator-fixture", "controlled-current-admission", "organization-plan-fixture", time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &evidence); e != nil {
		t.Fatal(e)
	}
	product := input.ProductID
	signed := tokenWalletWorkload(t)
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"token","service":"token-gateway","permissions":["internal.billing.recharge-order.create","internal.billing.recharge-order.read","internal.billing.wallet.ensure"]},{"subject":"read","service":"token-gateway","permissions":["internal.billing.recharge-order.read"]}]`)
	handler := NewService(r).WithPayments(c).Handler("")
	var loseHTTP atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == http.MethodPost && q.URL.Path == "/internal/v1/recharge/orders" && loseHTTP.Swap(false) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, q)
			if rec.Code != http.StatusCreated {
				t.Errorf("supplier before lost ACK status%d", rec.Code)
			}
			conn, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			conn.Close()
			return
		}
		handler.ServeHTTP(w, q)
	}))
	defer server.Close()
	if err = c.ConfigureNotifications("owned-org-fixture", server.URL+"/internal/payment/notifications", map[string]string{"ALIPAY": "public-fixture-hmac-key-at-least-32-bytes", "WECHAT": "public-fixture-hmac-key-at-least-32-bytes", "UNIONPAY": "public-fixture-hmac-key-at-least-32-bytes"}); err != nil {
		t.Fatal(err)
	}
	client, err := token.NewHTTPBillingClient(server.URL, signed("token"))
	if err != nil {
		t.Fatal(err)
	}
	owner, org := uuid.New(), uuid.New()
	if _, err = r.ApplyOrganizationLifecycle(ctx, org, "ACTIVE", 1, false); err != nil {
		t.Fatal(err)
	}
	wallet, _, err := r.EnsureWallet(ctx, "ORGANIZATION", org)
	if err != nil {
		t.Fatal(err)
	}
	var currentRole string
	var restricted bool
	if err = r.pool.QueryRow(ctx, `SELECT current_user,NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&currentRole, &restricted); err != nil || currentRole != "billing_service" || !restricted {
		t.Fatal("runtime actor", currentRole, restricted, err)
	}
	t.Log("actual Billing runtime=billing_service, public migrations; simulator=payment_simulator; controlled upstream")
	key := "organization-plan-original-key-0001"
	var first PaymentOrder
	create := func(cl *token.HTTPBillingClient, payer, beneficiary, wid uuid.UUID, product, k string) (PaymentOrder, error) {
		raw, e := cl.CreateOrganizationRechargeOrder(ctx, payer, wid, beneficiary, product, "ALIPAY", "+8613800138000", "READY", k)
		var got PaymentOrder
		if e == nil {
			e = json.Unmarshal(raw, &got)
		}
		return got, e
	}
	t.Run("signed-client-explicit-beneficiary-and-original-key", func(t *testing.T) {
		first, err = create(client, owner, org, wallet.ID, product, key)
		if err != nil {
			t.Fatal(err)
		}
		if first.ID == uuid.Nil || first.RequesterID != owner || first.BeneficiaryOrganizationID == nil || *first.BeneficiaryOrganizationID != org || first.WalletID != wallet.ID || first.AmountFen != 10 || first.CreditMicrocredits != 20000000 {
			t.Fatalf("frozen order %+v", first)
		}
		again, e := create(client, owner, org, wallet.ID, product, key)
		if e != nil || !reflect.DeepEqual(first, again) || provider.creates.Load() != 1 {
			t.Fatal("samekey", e, again, provider.creates.Load())
		}
		var facts bool
		if e = r.pool.QueryRow(ctx, `SELECT payer_user_id=$2 AND requester_id=$2 AND beneficiary_organization_id=$3 AND wallet_id=$4 AND payer_commerce_mapping_snapshot->>'payer_user_id'=$2::text AND provider_sku=$5 AND provider_id='development_simulator' AND product_snapshot->>'product_id'=$6 FROM payment_orders WHERE id=$1`, first.ID, owner, org, wallet.ID, input.Mapping.ExternalSKUBOM, product).Scan(&facts); e != nil || !facts {
			t.Fatal("payer/beneficiary persisted", e)
		}
		var risk int
		if e = r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_risk_attempts WHERE action='CREATE' AND payer_user_id=$1 AND beneficiary_organization_id=$2 AND result='ACCEPTED'`, owner, org).Scan(&risk); e != nil || risk != 2 {
			t.Fatal("organization admission binding", risk, e)
		}
	})
	t.Run("wrong-workload-and-identity-rejections-no-financial-effects", func(t *testing.T) {
		before := refundRecoveryMoneySnapshot(t, ctx, r)
		calls := provider.creates.Load()
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/recharge/orders", nil)
		result := httptest.NewRecorder()
		NewService(r).WithPayments(c).Handler("").ServeHTTP(result, request)
		if result.Code != http.StatusUnauthorized {
			t.Fatal("anonymous", result.Code)
		}

		read, e := token.NewHTTPBillingClient(server.URL, signed("read"))
		if e != nil {
			t.Fatal(e)
		}
		if _, e = create(read, uuid.New(), org, wallet.ID, product, "scope-rejected-key-0001"); e == nil {
			t.Fatal("read-only workload created")
		}
		personal, _, e := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
		if e != nil {
			t.Fatal(e)
		}
		// Snapshot after explicitly provisioning this unrelated fixture wallet.
		before = refundRecoveryMoneySnapshot(t, ctx, r)
		for _, v := range []struct {
			payer, beneficiary, wallet uuid.UUID
			key                        string
		}{{owner, uuid.New(), wallet.ID, key}, {uuid.New(), uuid.New(), wallet.ID, "foreign-org-new-key-0001"}, {uuid.New(), org, personal.ID, "personal-as-org-key-0001"}, {uuid.New(), uuid.Nil, wallet.ID, "zero-beneficiary-key-0001"}} {
			if _, e = create(client, v.payer, v.beneficiary, v.wallet, product, v.key); e == nil {
				t.Fatal("changed identity accepted", v)
			}
		}
		if provider.creates.Load() != calls || !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
			t.Fatal("rejection changed money/provider")
		}
	})
	t.Run("simulator-success-exact-organization-credit-once", func(t *testing.T) {
		u, e := url.Parse(first.PaymentURL)
		if e != nil {
			t.Fatal(e)
		}
		base, _ := refundSimulatorCompletionServers.Load(simulator)
		response, e := http.Post(base.(string)+u.Path+"/complete", "application/json", nil)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatal("simulator completion", response.StatusCode)
		}
		got, e := c.SyncOrder(ctx, first.ID)
		if e != nil || got.FulfillmentStatus != "CREDITED" {
			t.Fatal("fulfillment", got, e)
		}
		if _, e = c.SyncOrder(ctx, first.ID); e != nil {
			t.Fatal(e)
		}
		var packageCount, components, postings int
		var total, liability, clearing, personalMoney int64
		var exact bool
		e = r.pool.QueryRow(ctx, `SELECT count(*),bool_and(user_id IS NULL AND wallet_id=$2 AND product_id=$3 AND valid_until=valid_from+interval '30 days' AND points_total=20000000) FROM entitlement_packages WHERE payment_order_id=$1`, first.ID, wallet.ID, product).Scan(&packageCount, &exact)
		if e != nil || packageCount != 1 || !exact {
			t.Fatal("org package", packageCount, exact, e)
		}
		e = r.pool.QueryRow(ctx, `SELECT count(*),sum(original_microcredits) FROM entitlement_components WHERE package_id IN (SELECT id FROM entitlement_packages WHERE payment_order_id=$1)`, first.ID).Scan(&components, &total)
		if e != nil || components != 2 || total != 20000000 {
			t.Fatal("components", components, total, e)
		}
		var parts bool
		e = r.pool.QueryRow(ctx, `SELECT bool_and(part.original_microcredits=10000000 AND part.available_microcredits=10000000 AND lot.wallet_id=$2 AND lot.expires_at=part.expires_at AND lot.refundable=(part.component_type='BASE')) FROM entitlement_components part JOIN credit_lots lot ON lot.id=part.credit_lot_id WHERE part.package_id IN (SELECT id FROM entitlement_packages WHERE payment_order_id=$1)`, first.ID, wallet.ID).Scan(&parts)
		if e != nil || !parts {
			t.Fatal("exact BASE/BONUS origin/expiry/refund split", e)
		}
		e = r.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(p.delta_microcredits) FILTER(WHERE a.wallet_id=$2),0),COALESCE(sum(p.delta_microcredits) FILTER(WHERE a.id=$3),0) FROM ledger_postings p JOIN ledger_accounts a ON a.id=p.ledger_account_id JOIN journal_entries j ON j.id=p.journal_entry_id WHERE j.business_reference='payment-transaction:'||(SELECT id::text FROM payment_transactions WHERE payment_order_id=$1)`, first.ID, wallet.ID, paymentClearingAccount).Scan(&postings, &liability, &clearing)
		if e != nil || postings != 3 || liability != 20000000 || clearing != -20000000 {
			t.Fatal("exact journal", postings, liability, clearing, e)
		}
		if e = r.pool.QueryRow(ctx, `SELECT COALESCE(sum(posted_microcredits),0) FROM wallets WHERE owner_type='PERSONAL'`).Scan(&personalMoney); e != nil || personalMoney != 0 {
			t.Fatal("personal credited", personalMoney, e)
		}
	})
	t.Run("provider-create-lost-ack-only-queries-original-order", func(t *testing.T) {
		payer := uuid.New()
		provider.lose.Store(true)
		beforeCreates := provider.creates.Load()
		beforeQueries := provider.queries.Load()
		k := "organization-plan-unknown-key-0001"
		if _, e := create(client, payer, org, wallet.ID, product, k); e == nil {
			t.Fatal("lost ACK not surfaced")
		}
		var original uuid.UUID
		var state string
		if e := r.pool.QueryRow(ctx, `SELECT id,provider_call_status FROM payment_orders WHERE requester_id=$1 AND wallet_id=$2 AND product_id=$3`, payer, wallet.ID, product).Scan(&original, &state); e != nil || state != "RESULT_UNKNOWN" {
			t.Fatal("unknown durable", state, e)
		}
		got, e := create(client, payer, org, wallet.ID, product, k)
		if e != nil || got.ID != original || got.BeneficiaryOrganizationID == nil || *got.BeneficiaryOrganizationID != org || provider.creates.Load() != beforeCreates+1 || provider.queries.Load() != beforeQueries+1 {
			t.Fatal("unknown original query", got, e, provider.creates.Load(), provider.queries.Load())
		}
	})
	t.Run("frozen-organization-wallet-rejects-before-provider", func(t *testing.T) {
		other := uuid.New()
		if _, e := r.ApplyOrganizationLifecycle(ctx, other, "DISABLED", 1, false); e != nil {
			t.Fatal(e)
		}
		frozen, _, e := r.EnsureWallet(ctx, "ORGANIZATION", other)
		if e != nil || frozen.Status != "FROZEN" {
			t.Fatal(frozen, e)
		}
		calls := provider.creates.Load()
		if _, _, e = c.CreateOrganizationMobileOrder(ctx, uuid.New(), frozen.ID, other, product, "ALIPAY", "+8613800138000", "frozen-org-original-key-0001"); !errors.Is(e, ErrConflict) {
			t.Fatal("frozen organization dispatched", e)
		}
		if calls != provider.creates.Load() {
			t.Fatal("frozen wallet provider effect")
		}
	})
	t.Run("ordinary-personal-plan-keeps-personal-beneficiary", func(t *testing.T) {
		payer := uuid.New()
		personal, _, e := r.EnsureWallet(ctx, "PERSONAL", payer)
		if e != nil {
			t.Fatal(e)
		}
		order, _, e := c.CreateMobileOrder(ctx, payer, personal.ID, product, "ALIPAY", "+8613800138000", "ordinary-personal-key-0001")
		if e != nil || order.BeneficiaryOrganizationID != nil || order.WalletID != personal.ID || order.RequesterID != payer {
			t.Fatal("personal path changed", order, e)
		}
	})
	t.Run("customer-http-lost-ack-replays-single-original-receipt", func(t *testing.T) {
		payer := uuid.New()
		k := "customer-http-ack-original-key-0001"
		calls := provider.creates.Load()
		queries := provider.queries.Load()
		server.CloseClientConnections()
		loseHTTP.Store(true)
		if _, e := create(client, payer, org, wallet.ID, product, k); e == nil {
			t.Fatal("controlled customer ACK loss not observed")
		}
		var original uuid.UUID
		var count int
		if e := r.pool.QueryRow(ctx, `SELECT count(*),min(id::text)::uuid FROM payment_orders WHERE requester_id=$1 AND wallet_id=$2`, payer, wallet.ID).Scan(&count, &original); e != nil || count != 1 {
			t.Fatal("lost ACK original row", count, e)
		}
		got, e := create(client, payer, org, wallet.ID, product, k)
		if e != nil || got.ID != original || provider.creates.Load() != calls+1 || provider.queries.Load() != queries {
			t.Fatal("customer ACK replay changed external effects", got, e)
		}
	})
	t.Run("actual-gateway-create-poll-denied-authority-keeps-owner", func(t *testing.T) {
		payer := uuid.New()
		var denied atomic.Bool
		var authorizationCalls atomic.Int32
		authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
			switch q.URL.Path {
			case "/internal/v1/authorization/check":
				var in struct {
					Permission     string     `json:"permission"`
					OrganizationID *uuid.UUID `json:"target_organization_id"`
				}
				if e := json.NewDecoder(q.Body).Decode(&in); e != nil || in.OrganizationID == nil || *in.OrganizationID != org || !strings.HasPrefix(in.Permission, "org.") {
					t.Error("incorrect authority target", e)
				}
				authorizationCalls.Add(1)
				w.Header().Set("X-Authorization-Version", "7")
				json.NewEncoder(w).Encode(map[string]any{"allowed": !denied.Load(), "user_id": payer})
			case "/v1/me/authorization-context":
				json.NewEncoder(w).Encode(map[string]any{"user_id": payer, "context_type": "ORGANIZATION", "organization_id": org, "organization_membership_version": 3, "membership_version": 5, "context_version": 2, "authorization_version": 7, "roles": []string{"OWNER"}, "mobile": "+8613800138000", "mobile_verified": true})
			case "/v1/me/commerce-customer-mapping":
				json.NewEncoder(w).Encode(map[string]string{"status": "READY"})
			default:
				t.Error("unexpected controlled Account authority route")
				http.NotFound(w, q)
			}
		}))
		defer authority.Close()
		contexts, e := token.NewHTTPContextResolver(authority.URL)
		if e != nil {
			t.Fatal(e)
		}
		wallets, e := token.NewHTTPWalletResolver(server.URL, signed("token"))
		if e != nil {
			t.Fatal(e)
		}
		authorizer, e := authorization.NewClient(authority.URL, "owned-authority-fixture")
		if e != nil {
			t.Fatal(e)
		}
		gateway := token.NewService(contexts, wallets, client).WithManagement(nil, nil, token.ProviderURLPolicy{}, "owned-authority-fixture", nil).WithAuthorizer(authorizer).Handler(func(context.Context, string) (identity.Principal, error) {
			return identity.Principal{Issuer: "controlled-identity", Subject: "owned-payer"}, nil
		})
		call := func(method, path string, body []byte) *httptest.ResponseRecorder {
			q := httptest.NewRequest(method, path, bytes.NewReader(body))
			q.Header.Set("Authorization", "Bearer owned-human-fixture")
			q.Header.Set("Idempotency-Key", "gateway-org-original-key-0001")
			out := httptest.NewRecorder()
			gateway.ServeHTTP(out, q)
			return out
		}
		path := "/v1/organizations/" + org.String() + "/recharge/orders"
		body, _ := json.Marshal(map[string]any{"product_id": product, "channel": "ALIPAY"})
		out := call(http.MethodPost, path, body)
		if out.Code != 201 {
			t.Fatal("actual gateway create", out.Code, out.Body.String())
		}
		var order PaymentOrder
		if e = json.Unmarshal(out.Body.Bytes(), &order); e != nil || order.RequesterID != payer || order.BeneficiaryOrganizationID == nil || *order.BeneficiaryOrganizationID != org || order.WalletID != wallet.ID {
			t.Fatal("gateway owner facts", order, e)
		}
		out = call(http.MethodGet, path+"/"+order.ID.String(), nil)
		if out.Code != 200 {
			t.Fatal("actual gateway poll", out.Code, out.Body.String())
		}
		creates, queries := provider.creates.Load(), provider.queries.Load()
		denied.Store(true)
		out = call(http.MethodPost, path, body)
		if out.Code != 403 || creates != provider.creates.Load() || queries != provider.queries.Load() {
			t.Fatal("denied human reached supplier", out.Code)
		}
		if authorizationCalls.Load() != 3 {
			t.Fatal("actual authority not consulted for create/poll/denial", authorizationCalls.Load())
		}
	})
	t.Run("current-catalog-off-sale-denies-new-key-preserves-history", func(t *testing.T) {
		if _, e := r.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version+1, "OFF_SALE", nil); e != nil {
			t.Fatal("fixture offsale", e)
		}
		calls := provider.creates.Load()
		if _, _, e := c.CreateOrganizationMobileOrder(ctx, uuid.New(), wallet.ID, org, product, "ALIPAY", "+8613800138000", "off-sale-organization-key-0001"); !errors.Is(e, ErrConflict) {
			t.Fatal("off sale bypass", e)
		}
		if provider.creates.Load() != calls {
			t.Fatal("off sale dispatched")
		}
		replay, e := create(client, owner, org, wallet.ID, product, key)
		if e != nil || replay.ID != first.ID {
			t.Fatal("original history lost", replay, e)
		}
	})
	t.Run("retired-four-denominations-stay-history-only", func(t *testing.T) {
		before := provider.creates.Load()
		for _, sku := range []string{"CNY_1", "CNY_10", "CNY_100", "CNY_200"} {
			if _, _, e := c.CreateOrganizationMobileOrder(ctx, uuid.New(), wallet.ID, org, sku, "ALIPAY", "+8613800138000", "retired-product-original-key-"+sku); !errors.Is(e, ErrConflict) {
				t.Fatal("retired product admitted", sku, e)
			}
		}
		var retained int
		if e := r.pool.QueryRow(ctx, `SELECT count(*) FROM recharge_products WHERE product_id IN ('CNY_1','CNY_10','CNY_100','CNY_200') AND NOT enabled`).Scan(&retained); e != nil || retained != 4 || provider.creates.Load() != before {
			t.Fatal("retired records/dispatch", retained, e)
		}
	})
}
