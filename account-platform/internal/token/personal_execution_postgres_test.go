package token

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
)

// Public sparse migrations, genuine Token state/replay storage and genuine
// Billing hold/ledger HTTP. Only the upstream provider and identity facts are
// controlled; this does not exercise a deployed issuer or external supplier.
func TestPersonalExecutionPublicPostgresFundsAndReplay(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	repository, ctx := retainedManagementTokenRepository(t)
	funds, _, billingPool := retainedManagementBillingRepository(t)
	billingHTTP := httptest.NewServer(billing.NewService(funds).Handler("controlled-personal-execution"))
	defer billingHTTP.Close()
	client, err := NewHTTPBillingClient(billingHTTP.URL, "controlled-personal-execution")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"JSON", "SSE", "UNKNOWN_USAGE"} {
		t.Run(kind, func(t *testing.T) {
			user := uuid.New()
			wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = funds.Credit(ctx, wallet.ID, "personal-credit-"+user.String(), 1000, false); err != nil {
				t.Fatal(err)
			}
			account, credential := uuid.New(), uuid.New()
			if _, err = repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,$4,'ACTIVE',100,$2,now(),now(),'PLATFORM')`, account, user, wallet.ID, account.String()); err != nil {
				t.Fatal(err)
			}
			cipher, err := repository.secrets.Encrypt([]byte("controlled-provider-secret"), []byte(account.String()+":"+credential.String()))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = repository.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at) VALUES($1,$2,'controlled','https://provider.invalid',$3,$4,$5,1,'ACTIVE',ARRAY['model'],$6,now())`, credential, account, cipher, repository.secrets.KeyID(), strings.Repeat("a", 64), user); err != nil {
				t.Fatal(err)
			}
			price, err := new(readinessBillingFake).Price(ctx, "model")
			for i := range price.Units {
				price.Units[i].RoundingRule = "CEILING"
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = billingPool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_id,published_at) VALUES($1,'model',0,0,0,now(),now(),'model',now())`, price.ID); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer controlled-provider-secret" {
					t.Error("actual adapter omitted the controlled provider contract")
				}
				_, _ = io.Copy(io.Discard, r.Body)
				if kind == "SSE" {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"id\":\"controlled\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					if kind == "UNKNOWN_USAGE" {
						io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
					} else {
						io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
					}
				}
			}))
			defer provider.Close()
			upstream := NewHTTPUpstreamClient()
			body := []byte(`{"model":"model","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`)
			modelID, capabilityID := uuid.New(), uuid.New()
			protocolID := uuid.MustParse("00000000-0000-7000-8200-000000000000")
			if _, err = repository.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'controlled',$2,$3,$4,$5,now())`, modelID, upstream.AdapterVersion(), modelID.String(), digestString([]byte(modelID.String())), user); err != nil {
				t.Fatal(err)
			}
			if _, err = repository.pool.Exec(ctx, `INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["TEXT"]','controlled',$4,now())`, capabilityID, modelID, protocolID, user); err != nil {
				t.Fatal(err)
			}
			var catalogVersion int64
			if err = repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled personal invocation',$2,now(),now()) RETURNING version`, uuid.New(), user).Scan(&catalogVersion); err != nil {
				t.Fatal(err)
			}
			routeGeneration := int64(1)
			usage := UsageRequest{ID: uuid.New(), UserID: user, SessionID: "controlled-session", TokenAccountID: account, CredentialID: credential, WalletID: wallet.ID, Model: "model", ModelID: &modelID, CapabilityVersionID: &capabilityID, ProtocolDefinitionID: &protocolID, CatalogVersion: &catalogVersion, RouteGeneration: &routeGeneration, ModelSnapshot: json.RawMessage(`{"protocol_code":"CHAT"}`), MaximumMicrocredits: 100, IdempotencyKeyDigest: digestString([]byte(user.String())), IdempotencyNamespace: "model-inference", CanonicalVersion: 1, RequestFingerprint: inferenceFingerprint("CHAT", 1, body)}
			priceJSON, _ := json.Marshal(price)
			snapshot := map[string]any{"context_type": "PERSONAL", "provider_id": "controlled", "adapter_version": upstream.AdapterVersion(), "wallet_owner_type": "USER", "pricing_snapshot": json.RawMessage(priceJSON)}
			usage, created, err := repository.BeginUsage(ctx, usage, snapshot)
			if err != nil || !created {
				t.Fatalf("genuine BeginUsage: created=%v err=%v", created, err)
			}
			if replay, again, err := repository.BeginUsage(ctx, usage, snapshot); err != nil || again || replay.ID != usage.ID {
				t.Fatalf("same request did not bind original usage: %v", err)
			}
			changed := usage
			changed.RequestFingerprint = strings.Repeat("e", 64)
			if _, _, err = repository.BeginUsage(ctx, changed, snapshot); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("changed facts accepted: %v", err)
			}
			envelope := DispatchEnvelope{BaseURL: provider.URL, Provider: "controlled", AdapterVersion: upstream.AdapterVersion(), Credential: []byte("controlled-provider-secret"), RequestBody: body, Price: price, Stream: kind == "SSE", MaximumCost: 100, MaximumOutput: 10, ContextType: "PERSONAL"}
			if err = repository.PrepareDispatch(ctx, usage.ID, envelope); err != nil {
				t.Fatal(err)
			}
			if err = repository.MarkHoldReserving(ctx, usage.ID); err != nil {
				t.Fatal(err)
			}
			service := &Service{repository: repository, billing: client, cancellationBilling: client, executionBilling: client, executionUpstream: upstream, breaker: resilience.NewBreaker()}
			hold, err := service.ensureUsageHold(ctx, usage, envelope)
			if err != nil {
				t.Fatal(err)
			}
			if err = repository.CompleteHoldAndDispatch(ctx, usage.ID, hold.ID, price.ID); err != nil {
				t.Fatal(err)
			}
			envelope, acquired, err := repository.AcquireDispatch(ctx, usage.ID, "controlled-worker", time.Minute)
			if err != nil || !acquired {
				t.Fatalf("actual dispatch lease: %v", err)
			}
			usage, err = repository.GetUsage(ctx, usage.ID)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(ctx)
			response := httptest.NewRecorder()
			service.dispatchUsage(response, request, usage, envelope)
			current, err := repository.GetUsage(ctx, usage.ID)
			if err != nil {
				t.Fatal(err)
			}
			gotWallet, err := funds.GetWallet(ctx, wallet.ID)
			if err != nil {
				t.Fatal(err)
			}
			var journal int
			if err = billingPool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE business_reference=$1`, "usage:"+usage.ID.String()).Scan(&journal); err != nil {
				t.Fatal(err)
			}
			if kind == "UNKNOWN_USAGE" {
				if response.Code != http.StatusOK || current.SettlementStatus != "USAGE_PENDING" || gotWallet.ReservedMicrocredits != 100 || journal != 0 {
					t.Fatalf("untrusted usage became settled: HTTP%d %s reserved=%d journal=%d", response.Code, current.SettlementStatus, gotWallet.ReservedMicrocredits, journal)
				}
				return
			}
			if response.Code != http.StatusOK || current.ExecutionStatus != "SUCCEEDED" || current.SettlementStatus != "SETTLED" || current.ActualMicrocredits == nil || *current.ActualMicrocredits != 5 || gotWallet.ReservedMicrocredits != 0 || gotWallet.AvailableMicrocredits != 995 || journal != 1 {
				t.Fatalf("actual settlement incomplete: HTTP%d %s/%s actual=%v wallet=%d/%d journal=%d", response.Code, current.ExecutionStatus, current.SettlementStatus, current.ActualMicrocredits, gotWallet.AvailableMicrocredits, gotWallet.ReservedMicrocredits, journal)
			}
			replayed := httptest.NewRecorder()
			service.replayUsage(replayed, request, current, body)
			if replayed.Code != http.StatusOK || calls.Load() != 1 || !strings.Contains(replayed.Body.String(), "ok") {
				t.Fatalf("stored replay repeated provider or lost result: HTTP%d calls=%d", replayed.Code, calls.Load())
			}
			if kind == "SSE" && !strings.Contains(replayed.Body.String(), "[DONE]") {
				t.Fatal("settled replay lost terminal event")
			}
			if err = billingPool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE business_reference=$1`, "usage:"+usage.ID.String()).Scan(&journal); err != nil || journal != 1 {
				t.Fatalf("replay changed financial receipt: %v journal=%d", err, journal)
			}
		})
	}
}
