package token

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Public sparse migrations and service-role repositories; only the provider,
// issuer/catalog/tariff seams and selected Billing HTTP responses are controlled.
// SQL triggers abort real transactions rather than faking a repository error.
func TestHoldAcknowledgementSurvivesDispatchPersistenceFailurePostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, bp := retainedManagementBillingRepository(t)
	// The existing fixture owns a fresh UUID database. Use its configured admin
	// solely to install/remove fault triggers; runtime writes remain token_gateway.
	adminConfig, err := pgx.ParseConfig(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.Database = r.pool.Config().ConnConfig.Database
	if !strings.HasPrefix(adminConfig.Database, "f04_token_management_") {
		t.Fatal("unowned fixture database")
	}
	admin, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	var releases atomic.Int64
	var reservations atomic.Int64
	var loseACK atomic.Bool
	var loseBeforeCommit atomic.Bool
	handler := billing.NewService(funds).Handler("controlled-hold-ack")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/internal/v1/holds/reserve" {
			reservations.Add(1)
		}
		if q.URL.Path == "/internal/v1/holds/release" {
			releases.Add(1)
			if loseACK.Swap(false) {
				if !loseBeforeCommit.Load() {
					receipt := httptest.NewRecorder()
					handler.ServeHTTP(receipt, q)
					if receipt.Code != http.StatusOK {
						t.Error("real release before lost ACK", receipt.Code)
					}
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		handler.ServeHTTP(w, q)
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-hold-ack")
	if err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	r.now = func() time.Time {
		if n := clock.Load(); n != 0 {
			return time.Unix(0, n).UTC()
		}
		return time.Now().UTC()
	}

	fault := func(t *testing.T, table, clause, action string) func() {
		t.Helper()
		sql := `CREATE FUNCTION controlled_hold_ack_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN ` + action + `; RETURN NEW; END $$; CREATE TRIGGER controlled_hold_ack_fault BEFORE ` + clause + ` ON ` + table + ` FOR EACH ROW EXECUTE FUNCTION controlled_hold_ack_fault()`
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}

		var removed atomic.Bool
		cleanup := func() {
			if removed.Swap(true) {
				return
			}
			if _, err := admin.Exec(ctx, `DROP TRIGGER controlled_hold_ack_fault ON `+table+`; DROP FUNCTION controlled_hold_ack_fault()`); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(cleanup)
		return cleanup
	}
	seed := func(t *testing.T) f05DeadlineCase {
		return f05DeadlineSeedThroughChat(t, ctx, r, funds, bp, client, &clock, time.Now().UTC(), time.Now().UTC(), false, false)
	}
	retry := func(t *testing.T, c f05DeadlineCase) *httptest.ResponseRecorder {
		t.Helper()
		correlation := observability.WithCorrelation(ctx, strings.ReplaceAll(uuid.NewString(), "-", ""), "")
		q := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(c.body))).WithContext(correlation)
		q.Header.Set("Authorization", "Bearer controlled-user")
		q.Header.Set("Idempotency-Key", c.key)
		out := httptest.NewRecorder()
		identity.Middleware(successfulBillingVerify, http.HandlerFunc(c.service.chatCompletions)).ServeHTTP(out, q)
		return out
	}
	assertHeld := func(t *testing.T, c f05DeadlineCase) UsageRequest {
		t.Helper()
		u, err := r.GetUsage(ctx, c.usage.ID)
		if err != nil || u.HoldID == nil || u.PriceVersionID == nil || *u.PriceVersionID != c.price.ID || u.Status != "RESERVED" || u.ExecutionStatus != "HOLD_RESERVED" || u.SettlementStatus != "HOLD_ACTIVE" {
			t.Fatal("acknowledged hold identity was lost or falsely released", u, err)
		}
		var holdID uuid.UUID
		var count int
		if err := bp.QueryRow(ctx, `SELECT min(id::text)::uuid,count(*) FROM usage_holds WHERE usage_request_id=$1`, u.ID).Scan(&holdID, &count); err != nil || count != 1 || holdID != *u.HoldID {
			t.Fatal("not exactly the original Billing hold", holdID, count, err)
		}
		var intents int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_dispatch_intents WHERE usage_request_id=$1`, u.ID).Scan(&intents); err != nil || intents != 0 {
			t.Fatal("aborted promotion left an intent", intents, err)
		}
		if c.providerCalls.Load() != 0 {
			t.Fatal("provider ran before promotion")
		}
		return u
	}

	t.Run("intent-rollback-retains-hold-and-same-key-resumes-once", func(t *testing.T) {

		before := releases.Load()
		loseBeforeCommit.Store(true)
		loseACK.Store(true)
		defer loseACK.Store(false)
		clearFault := fault(t, "usage_dispatch_intents", "INSERT", `RAISE EXCEPTION 'controlled intent rollback' USING ERRCODE='40001'`)

		c := seed(t)
		loseACK.Store(false)
		clearFault()
		if c.responseCode != 500 {
			t.Fatal("controlled actual HTTP failure missing", c.responseCode)
		}
		u := assertHeld(t, c)
		if releases.Load() != before {
			t.Fatal("persistence uncertainty called Billing Release")
		}
		// Resume through the installed chat logic with the original idempotency key.
		c.service.repository.(*f05DeadlineCatalogRepository).acquire = true
		c.service.repository.(*f05DeadlineCatalogRepository).acquiredAt = time.Now().UTC()
		beforeRetryReservations := reservations.Load()
		out := retry(t, c)
		if out.Code != 200 || c.providerCalls.Load() != 1 {
			t.Fatal("same key did not dispatch once", out.Code, c.providerCalls.Load())
		}
		out = retry(t, c)
		if out.Code != 202 || c.providerCalls.Load() != 1 {
			t.Fatal("same key redispatched", out.Code, c.providerCalls.Load())
		}
		if reservations.Load() != beforeRetryReservations {
			t.Fatal("acknowledged HOLD_RESERVED re-entered Reserve")
		}
		current, err := r.GetUsage(ctx, u.ID)
		if err != nil || current.HoldID == nil || *current.HoldID != *u.HoldID {
			t.Fatal("retry changed original hold", err)
		}
	})

	for _, beforeCommit := range []bool{true, false} {
		name := "release-committed-ack-lost"
		if beforeCommit {
			name = "release-failed-before-commit"
		}
		t.Run(name, func(t *testing.T) {
			clearFault := fault(t, "usage_dispatch_intents", "INSERT", `RAISE EXCEPTION 'controlled intent rollback' USING ERRCODE='40001'`)
			c := seed(t)
			clearFault()
			u := assertHeld(t, c)
			// The existing Billing expiry worker owns this unused, expired hold.
			if _, err := bp.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, *u.HoldID); err != nil {
				t.Fatal(err)
			}
			service := &Service{repository: r, billing: client, cancellationBilling: client, cancellationStore: r}
			worker := billing.NewExpiredHoldRecoveryWorker(funds, f04LocalExpiryClient{service}, time.Minute)
			loseBeforeCommit.Store(beforeCommit)
			loseACK.Store(true)
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			pending, err := r.GetUsage(ctx, u.ID)
			if err != nil || pending.HoldID == nil || *pending.HoldID != *u.HoldID || pending.ExecutionStatus != "REVIEW" || pending.CancelStatus != "CANCEL_REQUESTED" || pending.SettlementStatus == "RELEASED" {
				t.Fatal("unknown release became terminal or lost identity", pending, err)
			}
			if _, err := bp.Exec(ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, *u.HoldID); err != nil {
				t.Fatal(err)
			}
			if err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			done, err := r.GetUsage(ctx, u.ID)
			if err != nil || done.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || done.CancelStatus != "LOCAL_CONFIRMED" || done.SettlementStatus != "RELEASED" || c.providerCalls.Load() != 0 {
				t.Fatal("same expiry proof did not finish confirmed release", done, err)
			}
			if out := retry(t, c); out.Code != 410 || c.providerCalls.Load() != 0 {
				t.Fatal("cancelled usage was dispatched", out.Code, c.providerCalls.Load())
			}
		})
	}

	t.Run("caller-cancellation-rolls-back-promotion-not-hold", func(t *testing.T) {
		clearFault := fault(t, "usage_dispatch_intents", "INSERT", `RAISE EXCEPTION 'controlled initial rollback' USING ERRCODE='40001'`)
		c := seed(t)
		clearFault()
		u := assertHeld(t, c)
		clearFault = fault(t, "usage_dispatch_intents", "INSERT", `PERFORM pg_sleep(2)`)
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		err := r.CompleteHoldAndDispatch(short, u.ID, *u.HoldID, c.price.ID)
		cancel()
		clearFault()
		if err == nil {
			t.Fatal("cancelled promotion unexpectedly succeeded")
		}
		assertHeld(t, c)
		if err := r.CompleteHoldAndDispatch(ctx, u.ID, *u.HoldID, c.price.ID); err != nil {
			t.Fatal("recovery after caller cancellation", err)
		}
	})
	t.Run("first-binding-rollback-recovers-exact-original-hold", func(t *testing.T) {
		u, e, hold := jR6UnboundSeed(t, r, funds, bp, client, ctx)
		before := releases.Load()
		clearFault := fault(t, "usage_requests", "UPDATE OF hold_id", `RAISE EXCEPTION 'controlled first binding rollback' USING ERRCODE='40001'`)
		err := r.CompleteHoldAndDispatch(ctx, u.ID, hold.ID, e.Price.ID)
		clearFault()
		var dbError *pgconn.PgError
		if !errors.As(err, &dbError) || dbError.Code != "40001" {
			t.Fatal("first binding fault did not abort actual transaction", err)
		}
		pending, err := r.GetUsage(ctx, u.ID)
		if err != nil || pending.ExecutionStatus != "HOLD_RESERVING" || pending.Status != "CREATED" || pending.HoldID != nil || releases.Load() != before {
			t.Fatal("unknown first binding was falsely finalized", pending, err)
		}
		service := &Service{repository: r, billing: client, cancellationBilling: client, cancellationStore: r}
		again, err := service.ensureUsageHold(ctx, pending, e)
		if err != nil || again.ID != hold.ID {
			t.Fatal("original reserve key failed to recover original hold", again, err)
		}
		if err := r.CompleteHoldAndDispatch(ctx, u.ID, again.ID, e.Price.ID); err != nil {
			t.Fatal(err)
		}
		done, err := r.GetUsage(ctx, u.ID)
		if err != nil || done.ExecutionStatus != "DISPATCH_INTENT_PERSISTED" || done.HoldID == nil || *done.HoldID != hold.ID {
			t.Fatal("first binding retry did not finish exact identity", done, err)
		}
	})
	t.Run("wrong-hold-or-price-cannot-replace-acknowledged-identity", func(t *testing.T) {
		clearFault := fault(t, "usage_dispatch_intents", "INSERT", `RAISE EXCEPTION 'controlled intent rollback' USING ERRCODE='40001'`)
		c := seed(t)
		clearFault()
		u := assertHeld(t, c)
		if err := r.CompleteHoldAndDispatch(ctx, u.ID, uuid.New(), c.price.ID); !errors.Is(err, ErrForbidden) {
			t.Fatal("different hold replaced binding", err)
		}
		if err := r.CompleteHoldAndDispatch(ctx, u.ID, *u.HoldID, uuid.New()); !errors.Is(err, ErrForbidden) {
			t.Fatal("different price replaced binding", err)
		}
		assertHeld(t, c)
	})
	t.Run("cancel-between-binding-and-promotion-keeps-original-release", func(t *testing.T) {
		clearFault := fault(t, "usage_dispatch_intents", "INSERT", `RAISE EXCEPTION 'controlled intent rollback' USING ERRCODE='40001'`)
		c := seed(t)
		clearFault()
		u := assertHeld(t, c)
		_, pre, err := r.RequestCancellation(ctx, u.ID, u.UserID, digestString([]byte("hold-ack-cancel-"+u.ID.String())), cancellationFingerprint(u.ID), "controlled")
		if err != nil || !pre {
			t.Fatal("original cancellation was not admitted", pre, err)
		}
		if err := r.CompleteHoldAndDispatch(ctx, u.ID, *u.HoldID, c.price.ID); !errors.Is(err, ErrCancellationRequested) {
			t.Fatal("cancellation lost promotion fence", err)
		}
		done, err := c.service.finishPreDispatchCancellation(ctx, u.ID)
		if err != nil || done.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || done.SettlementStatus != "RELEASED" {
			t.Fatal("original cancellation failed", done, err)
		}
		if c.providerCalls.Load() != 0 {
			t.Fatal("cancelled request reached provider")
		}
	})
}

func jR6UnboundSeed(t *testing.T, r *PostgresRepository, funds *billing.PostgresRepository, pool *pgxpool.Pool, client *HTTPBillingClient, ctx context.Context) (UsageRequest, DispatchEnvelope, BillingHold) {
	t.Helper()
	u, e, s := f05ExpiryFrozenFixture(t)
	upstreamModel := "frozen-upstream-" + u.ModelID.String()
	body, err := decodeChatCompletionPayload(e.RequestBody)
	if err != nil {
		t.Fatal(err)
	}
	body["model"] = upstreamModel
	e.RequestBody, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	s["upstream_model_ref"] = upstreamModel
	wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", u.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = funds.Credit(ctx, wallet.ID, "f05-expiry-"+u.UserID.String(), 1000, false); err != nil {
		t.Fatal(err)
	}
	u.WalletID = wallet.ID
	s["wallet_id"] = wallet.ID
	if _, err = r.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,$4,'ACTIVE',100,$2,now(),now(),'PLATFORM')`, u.TokenAccountID, u.UserID, u.WalletID, u.ID.String()); err != nil {
		t.Fatal(err)
	}
	cipher, err := r.secrets.Encrypt(e.Credential, []byte(u.TokenAccountID.String()+":"+u.CredentialID.String()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at) VALUES($1,$2,'controlled','https://provider.invalid',$3,$4,$5,1,'ACTIVE',ARRAY['frozen-upstream'],$6,now())`, u.CredentialID, u.TokenAccountID, cipher, r.secrets.KeyID(), strings.Repeat("a", 64), u.UserID); err != nil {
		t.Fatal(err)
	}
	protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	u.ProtocolDefinitionID = &protocol
	s["protocol_definition_id"] = protocol
	if _, err = r.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'controlled','controlled-v1',$2,$3,$4,now())`, *u.ModelID, upstreamModel, digestString([]byte(upstreamModel)), u.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["TEXT"]','controlled',$4,now())`, *u.CapabilityVersionID, *u.ModelID, protocol, u.UserID); err != nil {
		t.Fatal(err)
	}
	var catalog int64
	if err = r.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled F05 expiry',$2,now(),now()) RETURNING version`, uuid.New(), u.UserID).Scan(&catalog); err != nil {
		t.Fatal(err)
	}
	u.CatalogVersion = &catalog
	s["catalog_version"] = catalog
	u.ModelSnapshot = json.RawMessage(`{"protocol_code":"CHAT"}`)
	// An original caller alias is valid; dispatch replaced it with the frozen
	// upstream model before sealing. Preserve its true logical hash/fingerprint.
	logical := []byte(`{"model":"caller-alias","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`)
	u.RequestFingerprint = inferenceFingerprint("CHAT", 1, logical)
	s["request_sha256"] = digestString(logical)
	u.IdempotencyKeyDigest = digestString([]byte(u.ID.String()))
	u.PriceVersionID = nil
	if _, err = pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_id,published_at) VALUES($1,$2,0,0,0,now(),now(),$2,now())`, e.Price.ID, u.Model); err != nil {
		t.Fatal(err)
	}
	var created bool
	u, created, err = r.BeginUsage(ctx, u, s)
	if err != nil || !created {
		t.Fatal("actual BeginUsage", created, err)
	}
	if err = r.PrepareDispatch(ctx, u.ID, e); err != nil {
		t.Fatal(err)
	}
	if err = r.MarkHoldReserving(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	service := &Service{repository: r, billing: client, cancellationBilling: client, cancellationStore: r}
	hold, err := service.ensureUsageHold(ctx, u, e)
	if err != nil {
		t.Fatal(err)
	}
	return u, e, hold
}
