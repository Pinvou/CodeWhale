package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
)

// Real public migrations, runtime token_gateway, candidate selection, accepted
// usage and rotation. Only authorization, catalog/tariff and external money /
// provider boundaries are controlled; no fabricated failed row proves the race.
type credentialWindowRepository struct {
	*PostgresRepository
	model       CatalogModel
	secretHook  func(context.Context, uuid.UUID, uuid.UUID) (Credential, []byte, error)
	failureHook func(context.Context, uuid.UUID, ReplayResult) (bool, error)
	replayHook  func(context.Context, uuid.UUID, int64) (ReplaySnapshot, error)
	accepted    chan UsageRequest
}

func (r *credentialWindowRepository) ResolveCatalogModel(context.Context, string, string, time.Time) (CatalogModel, error) {
	return r.model, nil
}
func (r *credentialWindowRepository) BeginUsage(ctx context.Context, u UsageRequest, s map[string]any) (UsageRequest, bool, error) {
	got, created, err := r.PostgresRepository.BeginUsage(ctx, u, s)
	if created && err == nil && r.accepted != nil {
		r.accepted <- got
	}
	return got, created, err
}
func (r *credentialWindowRepository) CredentialSecret(ctx context.Context, a, c uuid.UUID) (Credential, []byte, error) {
	if r.secretHook != nil {
		return r.secretHook(ctx, a, c)
	}
	return r.PostgresRepository.CredentialSecret(ctx, a, c)
}
func (r *credentialWindowRepository) FailCredentialBeforeDispatch(ctx context.Context, id uuid.UUID, result ReplayResult) (bool, error) {
	if r.failureHook != nil {
		return r.failureHook(ctx, id, result)
	}
	return r.PostgresRepository.FailCredentialBeforeDispatch(ctx, id, result)
}
func (r *credentialWindowRepository) LoadReplay(ctx context.Context, id uuid.UUID, after int64) (ReplaySnapshot, error) {
	if r.replayHook != nil {
		return r.replayHook(ctx, id, after)
	}
	return r.PostgresRepository.LoadReplay(ctx, id, after)
}

type credentialWindowWallet uuid.UUID

func (w credentialWindowWallet) Ensure(context.Context, string, uuid.UUID) (uuid.UUID, error) {
	return uuid.UUID(w), nil
}

type credentialWindowRoutes struct{}

func (credentialWindowRoutes) Ensure(context.Context, AuthorizationContext, uuid.UUID, CatalogModel) (RouteProvisioning, error) {
	return RouteProvisioning{Status: "READY"}, nil
}

type credentialWindowBilling struct {
	ModelExecutionBillingClient
	price    PriceVersion
	reserves atomic.Int64
	commits  atomic.Int64
	releases atomic.Int64
}

func (b *credentialWindowBilling) Price(context.Context, string) (PriceVersion, error) {
	return b.price, nil
}
func (b *credentialWindowBilling) ReservePriced(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID, HoldPricingContext) (BillingHold, error) {
	b.reserves.Add(1)
	return BillingHold{ID: uuid.New()}, nil
}
func (b *credentialWindowBilling) Commit(context.Context, uuid.UUID, uuid.UUID, int64, Usage) error {
	b.commits.Add(1)
	return nil
}
func (b *credentialWindowBilling) Release(context.Context, uuid.UUID, uuid.UUID, string) error {
	b.releases.Add(1)
	return nil
}
func (b *credentialWindowBilling) Extend(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID) error {
	return nil
}

type credentialWindowFixture struct {
	r                         *credentialWindowRepository
	ctx                       context.Context
	service                   *Service
	body                      []byte
	key                       string
	user, account, credential uuid.UUID
	money                     *credentialWindowBilling
	providerCalls             atomic.Int64
	providerEntered           chan struct{}
	providerContinue          chan struct{}
}

func newCredentialWindowFixture(t *testing.T, base *PostgresRepository, ctx context.Context) *credentialWindowFixture {
	t.Helper()
	f := &credentialWindowFixture{ctx: ctx, key: "credential-window-" + uuid.NewString(), user: uuid.New(), account: uuid.New(), credential: uuid.New()}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		f.providerCalls.Add(1)
		if f.providerEntered != nil {
			close(f.providerEntered)
			<-f.providerContinue
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"controlled result"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(provider.Close)
	wallet, modelID, capability := uuid.New(), uuid.New(), uuid.New()
	upstream := NewHTTPUpstreamClient()
	protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	ref := "controlled-model-" + modelID.String()
	model := CatalogModel{ModelID: modelID, ModelCapabilityVersionID: capability, ProtocolDefinitionID: protocol, CatalogVersion: 1, CapabilityVersion: 1, ProtocolVersion: 1, ProtocolCode: "CHAT", ProviderID: "controlled", AdapterVersion: upstream.AdapterVersion(), UpstreamModelRef: ref, LifecycleStatus: "PUBLISHED", SourceAvailable: true, ContextLength: 1000, MaximumOutputTokens: 100, SupportedModalities: []string{"TEXT"}}
	sql := func(q string, args ...any) {
		t.Helper()
		if _, err := base.pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	sql(`INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,$4,'ACTIVE',100,$2,now(),now(),'PLATFORM')`, f.account, f.user, wallet, f.account.String())
	sql(`INSERT INTO token_share_policies(token_account_id,scope,updated_at,updated_by) VALUES($1,'OWNER_UNIT_ONLY',now(),$2)`, f.account, f.user)
	ciphertext, err := base.secrets.Encrypt([]byte("controlled-window-old-secret"), []byte(f.account.String()+":"+f.credential.String()))
	if err != nil {
		t.Fatal(err)
	}
	sql(`INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validation_status,created_by,created_at) VALUES($1,$2,'controlled',$3,$4,$5,$6,1,'ACTIVE',$7,'VALID',$8,now())`, f.credential, f.account, provider.URL, ciphertext, base.secrets.KeyID(), strings.Repeat("a", 64), []string{ref}, f.user)
	sql(`INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'controlled',$2,$3,$4,$5,now())`, modelID, upstream.AdapterVersion(), ref, digestString([]byte(ref)), f.user)
	sql(`INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["TEXT"]','controlled',$4,now())`, capability, modelID, protocol, f.user)
	var catalog int64
	if err := base.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled credential window',$2,now(),now()) RETURNING version`, uuid.New(), f.user).Scan(&catalog); err != nil {
		t.Fatal(err)
	}
	model.CatalogVersion = catalog
	sql(`INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,health_observed_at,health_expires_at,updated_at,updated_by,route_generation,model_id,protocol_version_id,reference_status) VALUES($1,$2,$3,'CHAT',100,'NONE','HEALTHY',now(),now()+interval '1 minute',now(),$4,1,$5,$6,'RESOLVED')`, uuid.New(), f.account, ref, f.user, modelID, protocol)
	sql(`INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES($1,'PERSONAL',$2,$3,$4,$5,'CHAT',$6,1,1,$7,'READY','VERIFIED',1,'{}',8,now()+interval '1 hour',$8,$9,$2,now(),now())`, uuid.New(), f.user, wallet, modelID, ref, protocol, strings.Repeat("b", 64), f.account, f.credential)
	price, err := new(readinessBillingFake).Price(ctx, modelID.String())
	if err != nil {
		t.Fatal(err)
	}
	for i := range price.Units {
		price.Units[i].RoundingRule = "CEILING"
	}
	f.money = &credentialWindowBilling{price: price}
	f.r = &credentialWindowRepository{PostgresRepository: base, model: model, accepted: make(chan UsageRequest, 2)}
	resolved := AuthorizationContext{UserID: f.user, SessionID: "controlled-window-session", ContextType: "PERSONAL", ContextVersion: 1, AuthorizationVersion: 1, MembershipVersion: 1, Permissions: []string{"personal.model.invoke"}}
	service, _, closeAuthorization := newModelUsageAuthorizationService(t, resolved)
	t.Cleanup(closeAuthorization)
	policy, err := NewProviderURLPolicy("test", []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	service.repository, service.wallets = f.r, credentialWindowWallet(wallet)
	service.executionBilling = f.money
	service.cancellationBilling = f.money
	service.executionUpstream, service.providerURLs, service.defaultRoutes = upstream, policy, credentialWindowRoutes{}
	service.maximumInputTokens, service.maximumOutputTokens, service.defaultMaxOutputTokens = 1000, 100, 10
	service.breaker = resilience.NewBreaker()
	f.service = service
	f.body = []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"max_tokens":10}`, modelID.String()))
	return f
}
func (f *credentialWindowFixture) request(body []byte) *httptest.ResponseRecorder {
	q := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(observability.WithCorrelation(f.ctx, strings.ReplaceAll(uuid.NewString(), "-", ""), ""))
	q.Header.Set("Authorization", "Bearer controlled-user")
	q.Header.Set("Idempotency-Key", f.key)
	q.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.service.Handler(successfulBillingVerify).ServeHTTP(w, q)
	return w
}
func (f *credentialWindowFixture) rotate(t *testing.T) {
	t.Helper()
	c, err := f.r.PostgresRepository.RotateCredential(f.ctx, f.account, f.user, "controlled", "https://provider.invalid", []byte("controlled-window-new-secret"), []string{f.r.model.UpstreamModelRef})
	if err != nil || c.ID == f.credential {
		t.Fatal("real rotation did not commit", err)
	}
	var oldStatus, newStatus string
	if err := f.r.pool.QueryRow(f.ctx, `SELECT a.status,b.status FROM provider_credentials a CROSS JOIN provider_credentials b WHERE a.id=$1 AND b.id=$2`, f.credential, c.ID).Scan(&oldStatus, &newStatus); err != nil || oldStatus != "RETIRED" || newStatus != "ACTIVE" {
		t.Fatal("rotation state mismatch", err)
	}
}
func (f *credentialWindowFixture) pauseOriginal(t *testing.T) (UsageRequest, chan *httptest.ResponseRecorder, chan struct{}) {
	t.Helper()
	entered, proceed := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	f.r.secretHook = func(ctx context.Context, a, c uuid.UUID) (Credential, []byte, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-proceed:
			case <-ctx.Done():
				return Credential{}, nil, ctx.Err()
			}
		}
		return f.r.PostgresRepository.CredentialSecret(ctx, a, c)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.request(f.body) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("original never reached exact secret barrier")
	}
	select {
	case accepted := <-f.r.accepted:
		return accepted, done, proceed
	case <-time.After(time.Second):
		t.Fatal("BeginUsage did not commit before secret barrier")
	}
	return UsageRequest{}, nil, nil
}
func credentialWindowAssertReceipt(t *testing.T, w *httptest.ResponseRecorder, id uuid.UUID) {
	t.Helper()
	var e struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	if json.Unmarshal(w.Body.Bytes(), &e) != nil || w.Code != http.StatusServiceUnavailable || e.Code != "PROVIDER_CREDENTIAL_UNAVAILABLE" {
		t.Errorf("credential terminal receipt status=%d code=%s", w.Code, e.Code)
		return
	}
	if e.Details["usage_request_id"] != id.String() || e.Details["execution_status"] != "FAILED" || e.Details["settlement_status"] != "RELEASED" || e.Details["status_url"] != "/v1/usage-requests/"+id.String() {
		t.Error("terminal receipt lost original identity/state")
	}
}
func TestCredentialRotationWindowReceiptPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	base, ctx := retainedManagementTokenRepository(t)
	t.Run("real-accepted-old-rotation-before-secret", func(t *testing.T) {
		f := newCredentialWindowFixture(t, base, ctx)
		u, done, proceed := f.pauseOriginal(t)
		f.rotate(t)
		close(proceed)
		first := <-done
		actual, err := base.GetUsage(ctx, u.ID)
		if err != nil || actual.CredentialID != f.credential || actual.ExecutionStatus != "FAILED" || actual.Status != "RELEASED" || actual.ErrorClass == nil || *actual.ErrorClass != "credential_unavailable" || actual.HoldID != nil {
			t.Fatal("real persisted rotation failure missing", err)
		}
		snapshot, err := base.LoadReplay(ctx, u.ID, 0)
		if err != nil || snapshot.TombstoneUntil == nil || len(snapshot.Events) != 0 {
			t.Fatal("actual live tombstone projection unavailable", err)
		}
		t.Logf("actual barrier accepted old credential; firstHTTP=%d liveTombstone=true resultExpired=%t holdCalls=%d providerCalls=%d", first.Code, snapshot.ResultExpired, f.money.reserves.Load(), f.providerCalls.Load())
		credentialWindowAssertReceipt(t, first, u.ID)
		replay := f.request(f.body)
		t.Logf("actual same-key HTTP=%d", replay.Code)
		credentialWindowAssertReceipt(t, replay, u.ID)
		if f.money.reserves.Load() != 0 || f.providerCalls.Load() != 0 {
			t.Fatal("credential failure dispatched or reserved")
		}
		wrong := bytes.Replace(f.body, []byte("hello"), []byte("changed"), 1)
		if w := f.request(wrong); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "IDEMPOTENCY_KEY_REUSED") {
			t.Error("different body lost conflict")
		}
	})
	_ = errors.New // used by persistence-boundary controls below
}

func (b *credentialWindowBilling) Reserve(ctx context.Context, u, w uuid.UUID, m int64, p uuid.UUID) (BillingHold, error) {
	return b.ReservePriced(ctx, u, w, m, p, HoldPricingContext{})
}

// This proves a separate existing cross-actor state hazard, not an approval of
// MarkReleased money policy. Billing is controlled; provider HTTP is actually
// reached with the old secret acquired before rotation. No money-loss claim.
func TestCredentialRotationOtherActorEffectPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	base, ctx := retainedManagementTokenRepository(t)
	f := newCredentialWindowFixture(t, base, ctx)
	f.providerEntered, f.providerContinue = make(chan struct{}), make(chan struct{})
	u, done, proceed := f.pauseOriginal(t)
	duplicate := make(chan *httptest.ResponseRecorder, 1)
	go func() { duplicate <- f.request(f.body) }()
	select {
	case <-f.providerEntered:
	case w := <-duplicate:
		stored, _ := base.GetUsage(ctx, u.ID)
		t.Fatalf("duplicate stopped before provider: status=%d phase=%s settlement=%s hasHold=%t reserves=%d code=%s", w.Code, stored.ExecutionStatus, stored.SettlementStatus, stored.HoldID != nil, f.money.reserves.Load(), w.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatal("duplicate did not reach actual provider HTTP")
	}
	before, err := base.GetUsage(ctx, u.ID)
	if err != nil || before.HoldID == nil || before.ExecutionStatus != "UPSTREAM_DISPATCHING" || before.SettlementStatus != "HOLD_ACTIVE" {
		t.Fatal("duplicate never persisted held/dispatch state", err)
	}
	f.rotate(t)
	close(proceed)
	first := <-done
	after, err := base.GetUsage(ctx, u.ID)
	if err != nil || after.HoldID == nil || *after.HoldID != *before.HoldID || after.Status != before.Status || after.SettlementStatus != before.SettlementStatus || after.ExecutionStatus != before.ExecutionStatus || after.ExecutionVersion != before.ExecutionVersion {
		t.Error("credential-failure actor overwrote another actor state", err)
	}
	if f.money.reserves.Load() != 1 || f.providerCalls.Load() != 1 {
		t.Fatal("controlled hold/actual provider side effect missing")
	}
	if strings.Contains(first.Body.String(), "PROVIDER_CREDENTIAL_UNAVAILABLE") {
		t.Error("later actor effect incorrectly represented as exact predispatch failure")
	}
	t.Logf("cross-actor fence: provider HTTP calls=%d controlled hold receipts=%d; before=%s/%s after=%s/%s; no Billing-ledger loss inference", f.providerCalls.Load(), f.money.reserves.Load(), before.ExecutionStatus, before.SettlementStatus, after.ExecutionStatus, after.SettlementStatus)
	close(f.providerContinue)
	select {
	case <-duplicate:
	case <-time.After(10 * time.Second):
		t.Fatal("provider response cleanup did not finish")
	}
}

// Real receipt insertion aborts after the state update/transition have executed
// inside the same transaction. No half terminal/tombstone is permitted.
func TestCredentialFailurePersistenceBoundariesPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	base, ctx := retainedManagementTokenRepository(t)
	for _, name := range []string{"receipt-insert-abort", "commit-ack-unknown", "not-committed-unknown", "read-ack-unknown", "existing-result", "expired-result-and-tombstone", "prepared-peer"} {
		t.Run(name, func(t *testing.T) {
			f := newCredentialWindowFixture(t, base, ctx)
			if name == "commit-ack-unknown" {
				f.r.failureHook = func(c context.Context, id uuid.UUID, result ReplayResult) (bool, error) {
					_, err := base.FailCredentialBeforeDispatch(c, id, result)
					if err != nil {
						return false, err
					}
					return false, context.DeadlineExceeded
				}
			}
			if name == "not-committed-unknown" {
				f.r.failureHook = func(c context.Context, id uuid.UUID, result ReplayResult) (bool, error) {
					cancelled, cancel := context.WithCancel(c)
					cancel()
					return base.FailCredentialBeforeDispatch(cancelled, id, result)
				}
			}
			if name == "read-ack-unknown" {
				var once atomic.Bool
				f.r.replayHook = func(c context.Context, id uuid.UUID, after int64) (ReplaySnapshot, error) {
					if !once.Swap(true) {
						return ReplaySnapshot{}, errors.New("controlled lost read acknowledgement")
					}
					return base.LoadReplay(c, id, after)
				}
			}
			u, done, proceed := f.pauseOriginal(t)
			var admin *pgx.Conn
			if name == "receipt-insert-abort" {
				config, err := pgx.ParseConfig(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
				if err != nil {
					t.Fatal(err)
				}
				config.Database = base.pool.Config().ConnConfig.Database
				if !strings.HasPrefix(config.Database, "f04_token_management_") {
					t.Fatal("unowned fault DB")
				}
				admin, err = pgx.ConnectConfig(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
				defer admin.Close(context.Background())
				_, err = admin.Exec(ctx, `CREATE FUNCTION credential_window_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.usage_request_id='`+u.ID.String()+`'::uuid THEN RAISE EXCEPTION 'controlled receipt transaction abort'; END IF; RETURN NEW; END $$; CREATE TRIGGER credential_window_fault BEFORE INSERT ON usage_replay_results FOR EACH ROW EXECUTE FUNCTION credential_window_fault()`)
				if err != nil {
					t.Fatal(err)
				}
			}
			if name == "existing-result" {
				if err := base.SaveReplayResult(ctx, u.ID, ReplayResult{Kind: "JSON", HTTPStatus: 429, ContentType: "application/json", Payload: []byte(`{"code":"CONTROLLED_ORIGINAL_RESULT"}`)}); err != nil {
					t.Fatal(err)
				}
				if err := base.MarkReleased(ctx, u.ID, "controlled_original_failure"); err != nil {
					t.Fatal(err)
				}
			}
			if name == "prepared-peer" {
				envelope, err := f.service.rebuildDispatchPreparation(ctx, u, f.body)
				if err != nil {
					t.Fatal(err)
				}
				if err := base.PrepareDispatch(ctx, u.ID, envelope); err != nil {
					t.Fatal(err)
				}
				clear(envelope.Credential)
			}
			f.rotate(t)
			close(proceed)
			first := <-done
			actual, err := base.GetUsage(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "receipt-insert-abort", "not-committed-unknown":
				if first.Code != 202 || actual.Status != "CREATED" || actual.ExecutionStatus != "RECEIVED" || actual.ExecutionVersion != 1 || strings.Contains(first.Body.String(), "PROVIDER_CREDENTIAL_UNAVAILABLE") {
					t.Error("uncommitted failure falsely confirmed terminal", first.Code, actual.ExecutionStatus)
				}
				var receipts, stones, failed int
				if err := base.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_replay_results WHERE usage_request_id=$1),(SELECT count(*) FROM usage_idempotency_tombstones WHERE usage_request_id=$1),(SELECT count(*) FROM usage_execution_transitions WHERE usage_request_id=$1 AND to_status='FAILED')`, u.ID).Scan(&receipts, &stones, &failed); err != nil || receipts != 0 || stones != 0 || failed != 0 {
					t.Fatal("atomic failure leaked partial terminal facts", err)
				}
				if admin != nil {
					if _, err := admin.Exec(ctx, `DROP TRIGGER credential_window_fault ON usage_replay_results; DROP FUNCTION credential_window_fault()`); err != nil {
						t.Fatal(err)
					}
				}
				retry := f.request(f.body)
				if retry.Code != 202 || !strings.Contains(retry.Body.String(), "USAGE_REVIEW") {
					t.Error("original recovery did not retain original usage review", retry.Code)
				}
			case "commit-ack-unknown":
				credentialWindowAssertReceipt(t, first, u.ID)
				credentialWindowAssertReceipt(t, f.request(f.body), u.ID)
			case "read-ack-unknown":
				if first.Code != 503 || !strings.Contains(first.Body.String(), "USAGE_STATE_UNCONFIRMED") || strings.Contains(first.Body.String(), `"execution_status"`) {
					t.Error("lost read ACK falsely claimed terminal")
				}
				credentialWindowAssertReceipt(t, f.request(f.body), u.ID)
			case "existing-result":
				if first.Code != 429 || !strings.Contains(first.Body.String(), "CONTROLLED_ORIGINAL_RESULT") {
					t.Error("original result lost precedence", first.Code)
				}
				if w := f.request(f.body); w.Code != 429 {
					t.Error("stored result replay changed")
				}
			case "prepared-peer":
				if first.Code != 202 || actual.ExecutionStatus != "RECEIVED" || actual.ExecutionVersion != 1 {
					t.Error("peer preparation was overwritten")
				}
				var count int
				if err := base.pool.QueryRow(ctx, `SELECT count(*) FROM usage_dispatch_preparations WHERE usage_request_id=$1`, u.ID).Scan(&count); err != nil || count != 1 {
					t.Fatal("peer preparation not retained", err)
				}
			case "expired-result-and-tombstone":
				credentialWindowAssertReceipt(t, first, u.ID)
				clock := base.now
				acceptedAt := clock()
				base.now = func() time.Time { return acceptedAt.Add(31 * 24 * time.Hour) }
				w := f.request(f.body)
				if w.Code != 410 || !strings.Contains(w.Body.String(), "IDEMPOTENCY_RESULT_EXPIRED") {
					t.Error("expired retained result no longer returns410", w.Code)
				}
				base.now = func() time.Time { return acceptedAt.Add(91 * 24 * time.Hour) }
				w = f.request(f.body)
				if w.Code != 202 || !strings.Contains(w.Body.String(), "USAGE_RESULT_PENDING") {
					t.Error("expired tombstone behavior changed", w.Code)
				}
				if err := base.CleanupReplayRetention(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := base.FindUsageByIdempotency(ctx, f.user, digestString([]byte(f.key))); !errors.Is(err, ErrNotFound) {
					t.Error("90-day cleanup did not retain existing digest expiry contract", err)
				}
				base.now = clock
			}
			if f.money.reserves.Load() != 0 || f.providerCalls.Load() != 0 {
				t.Fatal("persistence/negative path reserved or dispatched")
			}
		})
	}
}

// Exact row-lock barrier: pause failure at receipt insertion while a competing
// real PrepareDispatch transaction attempts its old RECEIVED snapshot.
func TestCredentialFailurePreparationFencePostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	base, ctx := retainedManagementTokenRepository(t)
	f := newCredentialWindowFixture(t, base, ctx)
	u, done, proceed := f.pauseOriginal(t)
	envelope, err := f.service.rebuildDispatchPreparation(ctx, u, f.body)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(envelope.Credential)
	cfg, err := pgx.ParseConfig(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = base.pool.Config().ConnConfig.Database
	if !strings.HasPrefix(cfg.Database, "f04_token_management_") {
		t.Fatal("unowned barrier DB")
	}
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	_, err = admin.Exec(ctx, `SELECT pg_advisory_lock(92841002); CREATE FUNCTION credential_window_pause() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(92841002); RETURN NEW; END $$; CREATE TRIGGER credential_window_pause BEFORE INSERT ON usage_replay_results FOR EACH ROW EXECUTE FUNCTION credential_window_pause()`)
	if err != nil {
		t.Fatal(err)
	}
	unlocked := false
	defer func() {
		if !unlocked {
			admin.Exec(context.Background(), `SELECT pg_advisory_unlock(92841002)`)
		}
	}()
	waitQuery := func(fragment string) bool {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			var count int
			err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1`, fragment).Scan(&count)
			if err != nil {
				t.Fatal(err)
			}
			if count > 0 {
				return true
			}
			time.Sleep(5 * time.Millisecond)
		}
		return false
	}
	f.rotate(t)
	close(proceed)
	if !waitQuery("INSERT INTO usage_replay_results%") {
		t.Fatal("failure did not reach receipt-insert barrier")
	}
	prepared := make(chan error, 1)
	go func() { prepared <- base.PrepareDispatch(ctx, u.ID, envelope) }()
	early := false
	var prepErr error
	deadline := time.Now().Add(8 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		select {
		case prepErr = <-prepared:
			early = true
		default:
		}
		if early {
			break
		}
		var count int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT id FROM usage_requests%'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			blocked = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !blocked {
		t.Error("competing preparation escaped the credential failure row fence", prepErr)
	}
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_unlock(92841002)`); err != nil {
		t.Fatal(err)
	}
	unlocked = true
	first := <-done
	credentialWindowAssertReceipt(t, first, u.ID)
	if !early {
		prepErr = <-prepared
	}
	if !errors.Is(prepErr, ErrForbidden) {
		t.Error("preparation survived terminal credential failure", prepErr)
	}
	var preparations int
	if err := base.pool.QueryRow(ctx, `SELECT count(*) FROM usage_dispatch_preparations WHERE usage_request_id=$1`, u.ID).Scan(&preparations); err != nil || preparations != 0 {
		t.Error("post-fence orphan preparation committed", err)
	}
	if _, err := admin.Exec(ctx, `DROP TRIGGER credential_window_pause ON usage_replay_results; DROP FUNCTION credential_window_pause()`); err != nil {
		t.Fatal(err)
	}
}

// Existing event facts are produced by real constrained repository transitions,
// not fabricated FAILED SQL rows. The peer's hold receipt is controlled here;
// this tests preservation/readback, not real Billing or a full SSE producer.
func TestCredentialFailureExistingEventsPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	base, ctx := retainedManagementTokenRepository(t)
	f := newCredentialWindowFixture(t, base, ctx)
	u, done, proceed := f.pauseOriginal(t)
	envelope, err := f.service.rebuildDispatchPreparation(ctx, u, f.body)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(envelope.Credential)
	if err := base.PrepareDispatch(ctx, u.ID, envelope); err != nil {
		t.Fatal(err)
	}
	if err := base.MarkHoldReserving(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := base.CompleteHoldAndDispatch(ctx, u.ID, uuid.New(), envelope.Price.ID); err != nil {
		t.Fatal(err)
	}
	acquired, ok, err := base.AcquireDispatch(ctx, u.ID, "controlled-event-owner", time.Minute)
	if err != nil || !ok {
		t.Fatal("event owner missing", err)
	}
	defer clear(acquired.Credential)
	owned := withDispatchLeaseOwner(ctx, acquired.leaseOwner)
	if err := base.MarkUpstream(owned, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := base.AppendReplayEvent(owned, u.ID, "DELTA", []byte(`{"text":"controlled existing event"}`)); err != nil {
		t.Fatal(err)
	}
	before, err := base.GetUsage(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.rotate(t)
	close(proceed)
	first := <-done
	after, err := base.GetUsage(ctx, u.ID)
	if err != nil || after.ExecutionStatus != before.ExecutionStatus || after.ExecutionVersion != before.ExecutionVersion || after.SettlementStatus != before.SettlementStatus || after.LastPersistedSequence != 1 {
		t.Error("existing event state overwritten", err)
	}
	if first.Code != 200 || !strings.Contains(first.Body.String(), "controlled existing event") {
		t.Error("existing event lost read-only precedence", first.Code)
	}
	snapshot, err := base.LoadReplay(ctx, u.ID, 0)
	if err != nil || len(snapshot.Events) != 1 || snapshot.Result != nil {
		t.Error("event/result facts mutated", err)
	}
}
