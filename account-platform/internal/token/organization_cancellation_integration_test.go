package token

// Public Token/Billing migrations and runtime roles execute the actual CHAT,
// reservation, cancellation and receipt paths. Only the directory, route,
// issuer facts and external provider are controlled; no deployed issuer or paid
// provider acceptance is implied. A channel fixes the reserve-ACK interleaving.
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type organizationCancellationReadyRoute struct {
	f05DeadlineReadyRoute
	repository *PostgresRepository
}

func (p organizationCancellationReadyRoute) ValidateContextModelPolicy(ctx context.Context, resolved AuthorizationContext, model uuid.UUID) error {
	allowed, err := p.repository.OrganizationModelAllowed(ctx, *resolved.OrganizationID, model)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return nil
}

type organizationCancellationFixture struct {
	ctx                               context.Context
	repository                        *PostgresRepository
	funds                             *billing.PostgresRepository
	pool                              *pgxpool.Pool
	service                           *Service
	decisions                         *authorizationDecisionRecorder
	user, organization, wallet, grant uuid.UUID
	body                              []byte
	key                               string
	providerCalls                     *atomic.Int64
	reserveCommitted                  chan struct{}
	allowReserve                      chan struct{}
	releaseOnce                       sync.Once
	loseReleaseACK                    atomic.Bool
}

func newOrganizationCancellationFixture(t *testing.T, pauseReserve bool) *organizationCancellationFixture {
	t.Helper()
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, pool := retainedManagementBillingRepository(t)
	f := &organizationCancellationFixture{ctx: ctx, repository: r, funds: funds, pool: pool, user: uuid.New(), organization: uuid.New(), key: uuid.NewString(), reserveCommitted: make(chan struct{}), allowReserve: make(chan struct{})}
	t.Cleanup(func() { f.releaseOnce.Do(func() { close(f.allowReserve) }) })
	if _, err := r.ApplyOrganizationLifecycle(ctx, f.organization, "ACTIVE", 1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := funds.ApplyOrganizationLifecycle(ctx, f.organization, "ACTIVE", 1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := funds.ApplyOrganizationMemberLifecycle(ctx, f.organization, f.user, "ACTIVE", 1); err != nil {
		t.Fatal(err)
	}
	wallet, _, err := funds.EnsureWallet(ctx, "ORGANIZATION", f.organization)
	if err != nil {
		t.Fatal(err)
	}
	f.wallet = wallet.ID
	reference := "organization-cancellation-credit-" + f.organization.String()
	if _, err = funds.Credit(ctx, wallet.ID, reference, 1000, false); err != nil {
		t.Fatal(err)
	}
	var packageID, componentID uuid.UUID
	if err = pool.QueryRow(ctx, `SELECT p.id,c.id FROM entitlement_packages p JOIN entitlement_components c ON c.package_id=p.id WHERE p.wallet_id=$1 AND p.source_reference=$2 AND c.component_type='BASE'`, wallet.ID, reference).Scan(&packageID, &componentID); err != nil {
		t.Fatal(err)
	}
	grant, created, err := funds.AllocateMemberQuota(ctx, billing.MemberQuotaAllocation{OrganizationID: f.organization, MemberUserID: f.user, PackageID: packageID, ComponentID: componentID, ComponentType: "BASE", AmountMicrocredits: 1000, MembershipVersion: 1, CreatedBy: f.user, IdempotencyKey: uuid.NewString()})
	if err != nil || !created {
		t.Fatalf("actual member grant: created=%v err=%v", created, err)
	}
	f.grant = grant.ID
	handler := billing.NewService(funds).Handler("controlled-organization-cancellation")
	var reserveCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, q)
		if q.URL.Path == "/internal/v1/holds/reserve" && reserveCalls.Add(1) == 1 && pauseReserve {
			if result.Code != http.StatusOK && result.Code != http.StatusCreated {
				t.Errorf("actual original reservation failed: HTTP%d %s", result.Code, result.Body.String())
			}
			close(f.reserveCommitted)
			select {
			case <-f.allowReserve:
			case <-q.Context().Done():
				return
			}
		}
		if q.URL.Path == "/internal/v1/holds/release" && f.loseReleaseACK.Swap(false) {
			if result.Code != http.StatusOK {
				t.Errorf("actual release must commit before ACK loss: HTTP%d", result.Code)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"code":"BILLING_UNAVAILABLE"}`)
			return
		}
		for name, values := range result.Header() {
			w.Header()[name] = values
		}
		w.WriteHeader(result.Code)
		w.Write(result.Body.Bytes())
	}))
	t.Cleanup(func() {
		f.releaseOnce.Do(func() { close(f.allowReserve) })
		server.Close()
	})
	client, err := NewHTTPBillingClient(server.URL, "controlled-organization-cancellation")
	if err != nil {
		t.Fatal(err)
	}
	f.service, f.body, f.providerCalls, f.decisions = organizationCancellationModel(t, ctx, r, pool, client, f.user, f.organization, wallet.ID)
	return f
}
func (f *organizationCancellationFixture) request(method, path, key string, body []byte) *httptest.ResponseRecorder {
	ctx := observability.WithCorrelation(f.ctx, strings.ReplaceAll(uuid.NewString(), "-", ""), "")
	q := httptest.NewRequest(method, path, bytes.NewReader(body)).WithContext(ctx)
	q.Header.Set("Authorization", "Bearer controlled-user")
	q.Header.Set("Idempotency-Key", key)
	q.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.service.Handler(successfulBillingVerify).ServeHTTP(w, q)
	return w
}
func (f *organizationCancellationFixture) original(t *testing.T) UsageRequest {
	t.Helper()
	u, err := f.repository.FindUsageByIdempotency(f.ctx, f.user, digestString([]byte(f.key)))
	if err != nil {
		t.Fatal(err)
	}
	if u.UserID != f.user || u.OrganizationID == nil || *u.OrganizationID != f.organization || u.WalletID != f.wallet {
		t.Fatal("actual CHAT lost original owner/organization/wallet")
	}
	return u
}
func (f *organizationCancellationFixture) revokeAndSwitch(t *testing.T) {
	t.Helper()
	if _, err := f.funds.ApplyOrganizationMemberLifecycle(f.ctx, f.organization, f.user, "REVOKED", 2); err != nil {
		t.Fatal(err)
	}
	f.service.contexts = fixedBillingContextResolver{resolved: AuthorizationContext{UserID: f.user, SessionID: "new-login", ContextType: "PERSONAL"}}
}
func (f *organizationCancellationFixture) assertMoney(t *testing.T, u UsageRequest, status string, reserved int64) {
	t.Helper()
	var ownerType, holdStatus string
	var owner, usageID uuid.UUID
	var walletReserved, grantReserved, grantUsed int64
	if err := f.pool.QueryRow(f.ctx, `SELECT w.owner_type,w.owner_id,w.reserved_microcredits,h.status,h.usage_request_id,g.reserved_microcredits,g.used_microcredits FROM usage_holds h JOIN wallets w ON w.id=h.wallet_id JOIN member_quota_grants g ON g.id=$2 WHERE h.id=$1`, u.HoldID, f.grant).Scan(&ownerType, &owner, &walletReserved, &holdStatus, &usageID, &grantReserved, &grantUsed); err != nil {
		t.Fatal(err)
	}
	if ownerType != "ORGANIZATION" || owner != f.organization || usageID != u.ID || holdStatus != status || walletReserved != reserved || grantReserved != reserved || grantUsed != 0 {
		t.Fatalf("original money identity/amount: %s/%s hold=%s wallet=%d grant=%d used=%d", ownerType, owner, holdStatus, walletReserved, grantReserved, grantUsed)
	}
	var holds, journals int
	if err := f.pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM usage_holds WHERE usage_request_id=$1),(SELECT count(*) FROM journal_entries WHERE business_reference=$2)`, u.ID, "usage:"+u.ID.String()).Scan(&holds, &journals); err != nil || holds != 1 || journals != 0 {
		t.Fatalf("cancellation changed invocation financial identity: holds=%d journals=%d err=%v", holds, journals, err)
	}
}

func TestOrganizationOriginalOwnerCancellationPublicPostgres(t *testing.T) {
	for _, lostReleaseACK := range []bool{false, true} {
		name := "before-dispatch-lost-reserve-ack-after-revocation"
		if lostReleaseACK {
			name = "before-dispatch-unknown-release-keeps-existing-recovery"
		}
		t.Run(name, func(t *testing.T) {
			f := newOrganizationCancellationFixture(t, true)
			done := make(chan *httptest.ResponseRecorder, 1)
			completed := make(chan struct{})
			go func() {
				defer close(completed)
				done <- f.request(http.MethodPost, "/v1/chat/completions", f.key, f.body)
			}()
			t.Cleanup(func() {
				f.releaseOnce.Do(func() { close(f.allowReserve) })
				select {
				case <-completed:
				case <-time.After(10 * time.Second):
					t.Error("original CHAT cleanup did not finish")
				}
			})
			select {
			case <-f.reserveCommitted:
			case <-time.After(10 * time.Second):
				t.Fatal("original actual reservation did not reach barrier")
			}
			u := f.original(t)
			if u.ExecutionStatus != "HOLD_RESERVING" || u.HoldID != nil {
				t.Fatal("barrier did not preserve lost reservation ACK")
			}
			f.revokeAndSwitch(t)
			lookup := f.request(http.MethodGet, "/v1/usage-requests/by-idempotency-key", f.key, nil)
			if lookup.Code != http.StatusOK || !strings.Contains(lookup.Body.String(), u.ID.String()) {
				t.Fatalf("lost invocation header not recovered: HTTP%d %s", lookup.Code, lookup.Body.String())
			}
			cancelKey := uuid.NewString()
			f.loseReleaseACK.Store(lostReleaseACK)
			first := f.request(http.MethodPost, "/v1/usage-requests/"+u.ID.String()+"/cancel", cancelKey, nil)
			wantStatus := http.StatusOK
			if lostReleaseACK {
				wantStatus = http.StatusAccepted
			}
			if first.Code != wantStatus {
				t.Fatalf("actual cancellation acknowledgement: HTTP%d %s", first.Code, first.Body.String())
			}
			pending := f.original(t)
			if lostReleaseACK && (pending.HoldID == nil || pending.ErrorClass == nil || *pending.ErrorClass != "cancel_hold_recovery_unconfirmed") {
				t.Fatal("lost release acknowledgement erased original hold recovery facts")
			}
			// Existing background recovery owns this uncertainty. The same public command
			// replays its pending receipt rather than inventing a second release decision.
			replay := f.request(http.MethodPost, "/v1/usage-requests/"+u.ID.String()+"/cancel", cancelKey, nil)
			if replay.Code != http.StatusOK {
				t.Fatalf("pending cancellation receipt did not replay: HTTP%d", replay.Code)
			}
			f.releaseOnce.Do(func() { close(f.allowReserve) })
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("original call did not finish after cancellation")
			}
			final := f.original(t)
			if lostReleaseACK {
				// R3 does not widen the financial recovery policy. An uncertain
				// release retains its original pending proof even when Billing has
				// already released; no second effect or false confirmation follows.
				if final.ExecutionStatus != "REVIEW" || final.CancelStatus != "CANCEL_REQUESTED" || f.providerCalls.Load() != 0 {
					t.Fatalf("unknown release lost pending facts: %s/%s calls=%d", final.ExecutionStatus, final.CancelStatus, f.providerCalls.Load())
				}
				f.assertMoney(t, final, "RELEASED", 0)
				return
			}
			if final.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || final.CancelStatus != "LOCAL_CONFIRMED" || f.providerCalls.Load() != 0 {
				t.Fatalf("cancel before dispatch failed: %s/%s calls=%d", final.ExecutionStatus, final.CancelStatus, f.providerCalls.Load())
			}
			f.assertMoney(t, final, "RELEASED", 0)
		})
	}
	t.Run("dispatched-unknown-original-key-scope-foreign-user-and-context", func(t *testing.T) {
		f := newOrganizationCancellationFixture(t, false)
		response := f.request(http.MethodPost, "/v1/chat/completions", f.key, f.body)
		if response.Code != http.StatusOK || f.providerCalls.Load() != 1 {
			t.Fatalf("real organization CHAT failed: HTTP%d %s", response.Code, response.Body.String())
		}
		u := f.original(t)
		if u.HoldID == nil || u.SettlementStatus != "USAGE_PENDING" {
			t.Fatal("provider unknown usage did not retain hold")
		}
		maximum := u.MaximumMicrocredits
		// Original personal-only repository entry remains isolated even for this user.
		if _, _, err := f.repository.RequestCancellation(f.ctx, u.ID, f.user, digestString([]byte(uuid.NewString())), cancellationFingerprint(u.ID), f.service.executionUpstream.AdapterVersion()); !errors.Is(err, ErrNotFound) {
			t.Fatal("personal repository crossed org scope", err)
		}
		if _, _, err := f.repository.RequestOrganizationCancellation(f.ctx, u.ID, f.user, uuid.New(), digestString([]byte(uuid.NewString())), cancellationFingerprint(u.ID), f.service.executionUpstream.AdapterVersion()); !errors.Is(err, ErrNotFound) {
			t.Fatal("wrong original organization accepted", err)
		}
		f.decisions.userID = uuid.New()
		if got := f.request(http.MethodGet, "/v1/usage-requests/by-idempotency-key", f.key, nil); got.Code != http.StatusNotFound {
			t.Fatal("same raw invocation key exposed another owner")
		}
		if got := f.request(http.MethodPost, "/v1/usage-requests/"+u.ID.String()+"/cancel", uuid.NewString(), nil); got.Code != http.StatusNotFound {
			t.Fatal("foreign user cancelled original org call")
		}
		f.decisions.userID = f.user
		f.revokeAndSwitch(t)
		lookup := f.request(http.MethodGet, "/v1/usage-requests/by-idempotency-key", f.key, nil)
		if lookup.Code != http.StatusOK || !strings.Contains(lookup.Body.String(), u.ID.String()) || lookup.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("revoked original owner lost recovery: HTTP%d %s", lookup.Code, lookup.Body.String())
		}
		key := uuid.NewString()
		for range 2 {
			got := f.request(http.MethodPost, "/v1/usage-requests/"+u.ID.String()+"/cancel", key, nil)
			if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"cancel_status":"UNSUPPORTED"`) {
				t.Fatalf("dispatched cancellation lied or failed: HTTP%d %s", got.Code, got.Body.String())
			}
		}
		final := f.original(t)
		if final.DeliveryStatus != "USER_STOPPED" || final.SettlementStatus != "USAGE_PENDING" || final.CancelStatus != "UNSUPPORTED" || f.providerCalls.Load() != 1 {
			t.Fatal("unsupported cancel altered provider or settlement facts")
		}
		f.assertMoney(t, final, "ACTIVE", maximum)
		var commands int
		if err := f.repository.pool.QueryRow(f.ctx, `SELECT count(*) FROM usage_cancel_commands WHERE usage_request_id=$1 AND user_id=$2`, u.ID, f.user).Scan(&commands); err != nil || commands != 1 {
			t.Fatal("duplicate or foreign cancellation wrote commands", commands, err)
		}
		// This old operation never grants a disabled identity a new permission.
		f.decisions.allowed = false
		if got := f.request(http.MethodPost, "/v1/usage-requests/"+u.ID.String()+"/cancel", key, nil); got.Code != http.StatusForbidden {
			t.Fatal("permission denial bypassed for original owner")
		}
	})
}

func organizationCancellationModel(t *testing.T, ctx context.Context, r *PostgresRepository, pool *pgxpool.Pool, client *HTTPBillingClient, user, organization, walletID uuid.UUID) (*Service, []byte, *atomic.Int64, *authorizationDecisionRecorder) {
	t.Helper()
	account, credential, modelID, capabilityID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var calls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer controlled-organization-cancellation-secret" {
			t.Error("real adapter provider contract missing")
		}
		_, _ = io.Copy(io.Discard, request.Body)
		w.Header().Set("Content-Type", "application/json")
		// Deliberately no configured reference header, even for the known-usage
		// control: received usage must independently block the deadline policy.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(provider.Close)
	upstream := NewHTTPUpstreamClient()
	protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	if _, err := r.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,owner_organization_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'ORG',$2,$2,$3,$4,'ACTIVE',100,$2,now(),now(),'PLATFORM')`, account, organization, walletID, account.String()); err != nil {
		t.Fatal(err)
	}
	ciphertext, err := r.secrets.Encrypt([]byte("controlled-organization-cancellation-secret"), []byte(account.String()+":"+credential.String()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at) VALUES($1,$2,'controlled',$3,$4,$5,$6,1,'ACTIVE',ARRAY[$7],$8,now())`, credential, account, provider.URL, ciphertext, r.secrets.KeyID(), strings.Repeat("a", 64), modelID.String(), user); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'controlled',$2,$3,$4,$5,now())`, modelID, upstream.AdapterVersion(), modelID.String(), digestString([]byte(modelID.String())), user); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["TEXT"]','controlled',$4,now())`, capabilityID, modelID, protocol, user); err != nil {
		t.Fatal(err)
	}
	var catalogVersion int64
	if err := r.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled organization cancellation invocation',$2,now(),now()) RETURNING version`, uuid.New(), user).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	price, err := new(readinessBillingFake).Price(ctx, modelID.String())
	if err != nil {
		t.Fatal(err)
	}
	for i := range price.Units {
		price.Units[i].RoundingRule = "CEILING"
	}
	if _, err := pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_id,published_at) VALUES($1,$2,0,0,0,now(),now(),$2,now())`, price.ID, modelID.String()); err != nil {
		t.Fatal(err)
	}
	model := CatalogModel{ModelID: modelID, CatalogVersion: catalogVersion, DisplayName: "Controlled organization cancellation model", LifecycleStatus: "PUBLISHED", ProtocolCode: "CHAT", ProtocolVersion: 1, ProtocolDefinitionID: protocol, ModelCapabilityVersionID: capabilityID, CapabilityVersion: 1, ContextLength: 1000, MaximumOutputTokens: 100, SupportedModalities: []string{"TEXT"}, TokenizerVersion: "controlled", SourceAvailable: true, ProviderID: "controlled", AdapterVersion: upstream.AdapterVersion(), UpstreamModelRef: modelID.String()}
	candidate := Candidate{ManagementMode: "PLATFORM", ProviderID: "controlled", CredentialVersion: 1, TokenAccountID: account, OwnerType: "ORG", OwnerID: organization, OwnerOrganization: &organization, ShareScope: "ORG_ALL", WalletID: walletID, Status: "ACTIVE", AllowedModels: []string{modelID.String()}, Priority: 100, HealthStatus: "HEALTHY", CredentialID: credential, ProtocolCode: "CHAT", ModelID: modelID, ProtocolVersionID: protocol, RouteGeneration: 1}
	repo := &f05DeadlineCatalogRepository{PostgresRepository: r, model: model, route: candidate, clock: new(atomic.Int64), acquiredAt: time.Now().UTC(), acquire: true}
	membership := int64(1)
	resolved := AuthorizationContext{UserID: user, SessionID: "controlled-organization-cancel-session", ContextType: "ORGANIZATION", OrganizationID: &organization, OrganizationMembershipVersion: &membership, ContextVersion: 1, AuthorizationVersion: 1, MembershipVersion: 1, Permissions: []string{"org.model.invoke"}}
	service, decisions, closeAuthorization := newModelUsageAuthorizationService(t, resolved)
	t.Cleanup(closeAuthorization)
	walletResolver, err := NewHTTPWalletResolver(client.baseURL.String(), client.serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewProviderURLPolicy("test", []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	service.repository, service.wallets, service.billing = repo, walletResolver, client
	service.executionBilling = &f05DeadlineTariffClient{HTTPBillingClient: client, price: price}
	service.cancellationBilling, service.cancellationStore = client, r
	service.executionUpstream, service.providerURLs, service.defaultRoutes = upstream, policy, organizationCancellationReadyRoute{repository: r}
	service.maximumInputTokens, service.maximumOutputTokens, service.defaultMaxOutputTokens = 1000, 100, 10
	service.breaker = resilience.NewBreaker()
	body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"max_tokens":10}`, modelID.String()))
	return service, body, &calls, decisions
}
