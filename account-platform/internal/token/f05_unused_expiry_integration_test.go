package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/holdexpiry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Real public Token/Billing storage, current Begin/Prepare/Reserve/Complete and
// expiry worker. Only Billing's HTTP carrier is controlled; no supplier call,
// deployed workload issuer, or external database is involved.
func f05UnusedExpirySeed(t *testing.T, r *PostgresRepository, funds *billing.PostgresRepository, pool *pgxpool.Pool, client *HTTPBillingClient, ctx context.Context) holdexpiry.Identity {
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
	if err = r.CompleteHoldAndDispatch(ctx, u.ID, hold.ID, e.Price.ID); err != nil {
		t.Fatal(err)
	}
	var expires time.Time
	if err = pool.QueryRow(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1 RETURNING expires_at`, hold.ID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	return holdexpiry.Identity{UsageRequestID: u.ID, HoldID: hold.ID, WalletID: u.WalletID, MaxMicrocredits: u.MaximumMicrocredits, ExpiresAt: expires}
}

func f05ExpiryRequestLocalStop(t *testing.T, r *PostgresRepository, ctx context.Context, id uuid.UUID) {
	t.Helper()
	u, err := r.GetUsage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	result, preDispatch, err := r.RequestCancellation(ctx, id, u.UserID, digestString([]byte("expiry-stop-"+id.String())), cancellationFingerprint(id), "controlled-v1")
	if err != nil || preDispatch || result.CancelStatus != "UNSUPPORTED" || result.DeliveryStatus != "USER_STOPPED" {
		t.Fatal("actual persisted-intent local stop", result, preDispatch, err)
	}
}

func TestF05ExpiredUnacquiredDispatchRecovery(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, pool := retainedManagementBillingRepository(t)
	var loseACK atomic.Bool
	var releases atomic.Int64
	handler := billing.NewService(funds).Handler("controlled-f05-expiry")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/internal/v1/holds/release" {
			releases.Add(1)
			if loseACK.Swap(false) {
				receipt := httptest.NewRecorder()
				handler.ServeHTTP(receipt, q)
				if receipt.Code != 200 {
					t.Error("release did not commit", receipt.Code)
				}
				w.WriteHeader(503)
				return
			}
		}
		handler.ServeHTTP(w, q)
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-f05-expiry")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{repository: r, billing: client, cancellationBilling: client, cancellationStore: r}
	lostACK := func(t *testing.T, stopped bool) {
		i := f05UnusedExpirySeed(t, r, funds, pool, client, ctx)
		if stopped {
			f05ExpiryRequestLocalStop(t, r, ctx, i.UsageRequestID)
		}
		loseACK.Store(true)
		worker := billing.NewExpiredHoldRecoveryWorker(funds, f04LocalExpiryClient{service}, time.Minute)
		if err = worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		u, err := r.GetUsage(ctx, i.UsageRequestID)
		if err != nil || u.ExecutionStatus != "REVIEW" || u.CancelStatus != "CANCEL_REQUESTED" {
			t.Fatal("lost ACK falsely confirmed", u, err)
		}
		var proofID uuid.UUID
		if err = r.pool.QueryRow(ctx, `SELECT id FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND adapter_version='hold-expiry-v1'`, i.UsageRequestID).Scan(&proofID); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, i.HoldID); err != nil {
			t.Fatal(err)
		}
		if err = worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		proof, err := service.recoverExpiredHold(ctx, i)
		if err != nil || proof.ProofID != proofID || !proof.Matches(i) {
			t.Fatal("same frozen proof lost", proof, err)
		}
		wallet, err := funds.GetWallet(ctx, i.WalletID)
		if err != nil || wallet.ReservedMicrocredits != 0 || wallet.AvailableMicrocredits != 1000 {
			t.Fatal("incorrect release balance", wallet, err)
		}
		var state string
		if err = pool.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, i.HoldID).Scan(&state); err != nil || state != "COMPLETED" {
			t.Fatal(state, err)
		}
		var count int
		var acquired *time.Time
		if err = r.pool.QueryRow(ctx, `SELECT count(*),max(worker_acquired_at) FROM usage_dispatch_intents WHERE usage_request_id=$1`, i.UsageRequestID).Scan(&count, &acquired); err != nil || count != 1 || acquired != nil {
			t.Fatal("intent history changed", count, err)
		}
		if _, ok, err := r.AcquireDispatch(ctx, i.UsageRequestID, "late-worker", time.Minute); err == nil || ok {
			t.Fatal("fence permitted dispatch", ok, err)
		}
		if stopped {
			current, e := r.GetUsage(ctx, i.UsageRequestID)
			if e != nil || current.DeliveryStatus != "USER_STOPPED" || current.CancelStatus != "LOCAL_CONFIRMED" {
				t.Fatal("local delivery stop lost", current, e)
			}
			var commands, localEvidence, convergence, confirmations int
			e = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_cancel_commands WHERE usage_request_id=$1),(SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND evidence_type='UNSUPPORTED'),(SELECT count(*) FROM usage_convergence_evidence WHERE usage_request_id=$1 AND evidence_key='requested:UNSUPPORTED'),(SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND evidence_type='LOCAL_CONFIRMED')`, i.UsageRequestID).Scan(&commands, &localEvidence, &convergence, &confirmations)
			if e != nil || commands != 1 || localEvidence != 1 || convergence != 1 || confirmations != 1 {
				t.Fatal("stop/proof/release history duplicated or removed", commands, localEvidence, convergence, confirmations, e)
			}
		}
	}
	t.Run("workerLostReleaseACKRetainsProofAndIntent", func(t *testing.T) { lostACK(t, false) })
	t.Run("userStoppedWorkerLostACKAndTerminalReplay", func(t *testing.T) { lostACK(t, true) })
	t.Run("acquisitionWinsNoRelease", func(t *testing.T) {
		i := f05UnusedExpirySeed(t, r, funds, pool, client, ctx)
		before := releases.Load()
		if _, ok, err := r.AcquireDispatch(ctx, i.UsageRequestID, "actual-winner", time.Minute); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if _, err = service.recoverExpiredHold(ctx, i); err == nil {
			t.Fatal("acquired dispatch released")
		}
		w, err := funds.GetWallet(ctx, i.WalletID)
		if err != nil || w.ReservedMicrocredits != i.MaxMicrocredits || releases.Load() != before {
			t.Fatal("acquired hold changed", w, err)
		}
	})
	t.Run("fenceWinsAgainstIntentLockHolder", func(t *testing.T) {
		i := f05UnusedExpirySeed(t, r, funds, pool, client, ctx)
		originalClock := r.now
		locked, proceed := make(chan struct{}), make(chan struct{})
		bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(proceed) }) }
		var workers sync.WaitGroup
		defer func() {
			cancel()
			release()
			workers.Wait()
			r.now = originalClock
		}()
		var calls atomic.Int64
		r.now = func() time.Time {
			if calls.Add(1) == 2 {
				close(locked)
				select {
				case <-proceed:
				case <-bounded.Done():
				}
			}
			return time.Now().UTC()
		}
		fenced := make(chan error, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, e := r.fenceExpiredHold(bounded, i)
			fenced <- e
		}()
		select {
		case <-locked:
		case <-bounded.Done():
			t.Fatal("fence did not reach locked CAS")
		}
		acquired := make(chan error, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, ok, e := r.AcquireDispatch(bounded, i.UsageRequestID, "waiting-acquirer", time.Minute)
			if ok {
				e = errors.New("waiting acquisition won after fence")
			}
			acquired <- e
		}()
		// Prove Acquire holds the actual intent row while waiting on usage;
		// a NOWAIT reader cannot take it. Do not use a sleep as lock evidence.
		holdsIntent := false
		for !holdsIntent && bounded.Err() == nil {
			tx, e := r.pool.Begin(bounded)
			if e != nil {
				break
			}
			_, e = tx.Exec(bounded, `SELECT usage_request_id FROM usage_dispatch_intents WHERE usage_request_id=$1 FOR UPDATE NOWAIT`, i.UsageRequestID)
			tx.Rollback(bounded)
			var pgerr *pgconn.PgError
			holdsIntent = errors.As(e, &pgerr) && pgerr.Code == "55P03"
			if !holdsIntent {
				time.Sleep(time.Millisecond)
			}
		}
		release()
		if !holdsIntent {
			t.Fatal("actual acquirer never held intent")
		}
		if e := <-fenced; e != nil {
			t.Fatal("fence deadlocked or failed", e)
		}
		if e := <-acquired; e == nil {
			t.Fatal("waiting actual acquisition was not rejected")
		}
		if _, e := service.recoverExpiredHold(ctx, i); e != nil {
			t.Fatal("saved fence did not converge", e)
		}
	})
	for _, scenario := range []string{"leaseOwner", "leaseUntil", "badHash", "missingKey", "frozenProvider", "trustedUsage", "attempt", "nonexpired", "legacy", "forgedLocalStop", "providerSent", "providerConfirmed", "otherConvergence"} {
		t.Run("deny_"+scenario, func(t *testing.T) {
			i := f05UnusedExpirySeed(t, r, funds, pool, client, ctx)
			before := releases.Load()
			if scenario == "providerSent" || scenario == "providerConfirmed" || scenario == "otherConvergence" {
				f05ExpiryRequestLocalStop(t, r, ctx, i.UsageRequestID)
			}
			var e error
			switch scenario {
			case "leaseOwner":
				_, e = r.pool.Exec(ctx, `UPDATE usage_dispatch_intents SET worker_lease_owner='unproven' WHERE usage_request_id=$1`, i.UsageRequestID)
			case "leaseUntil":
				_, e = r.pool.Exec(ctx, `UPDATE usage_dispatch_intents SET worker_lease_until=clock_timestamp() WHERE usage_request_id=$1`, i.UsageRequestID)
			case "badHash":
				_, e = r.pool.Exec(ctx, `UPDATE usage_dispatch_intents SET envelope_integrity='wrong' WHERE usage_request_id=$1`, i.UsageRequestID)
			case "missingKey":
				_, e = r.pool.Exec(ctx, `UPDATE usage_dispatch_intents SET key_id=NULL WHERE usage_request_id=$1`, i.UsageRequestID)
			case "frozenProvider":
				_, e = r.pool.Exec(ctx, `UPDATE usage_requests SET request_snapshot=jsonb_set(request_snapshot,'{provider_id}','"other"') WHERE id=$1`, i.UsageRequestID)
			case "trustedUsage":
				_, e = r.pool.Exec(ctx, `UPDATE usage_requests SET trusted_usage_snapshot='{}' WHERE id=$1`, i.UsageRequestID)
			case "attempt":
				e = r.RecordUpstreamRequest(ctx, i.UsageRequestID, UpstreamRequestEvidence{ProviderID: "controlled", AdapterVersion: "controlled-v1", RequestID: "contradictory-request"})
			case "nonexpired":
				i.ExpiresAt = time.Now().UTC().Add(time.Hour)
			case "legacy":
				_, e = r.pool.Exec(ctx, `UPDATE usage_requests SET canonicalization_version=0 WHERE id=$1`, i.UsageRequestID)
			case "forgedLocalStop":
				_, e = r.pool.Exec(ctx, `UPDATE usage_requests SET delivery_status='USER_STOPPED',cancel_status='UNSUPPORTED',cancellation_origin='USER',user_stopped_at=now(),cancel_requested_at=now() WHERE id=$1`, i.UsageRequestID)
			case "providerSent":
				_, e = r.pool.Exec(ctx, `UPDATE usage_requests SET provider_cancel_sent_at=clock_timestamp() WHERE id=$1`, i.UsageRequestID)
			case "providerConfirmed":
				_, e = r.pool.Exec(ctx, `UPDATE usage_requests SET provider_cancel_confirmed_at=clock_timestamp() WHERE id=$1`, i.UsageRequestID)
			case "otherConvergence":
				e = r.RecordConvergenceEvidence(ctx, i.UsageRequestID, "CANCELLATION", "GATEWAY", "RECORDED", "foreign-cancel", map[string]any{"provider_success_inferred": true})
			}
			if e != nil {
				t.Fatal("negative setup", e)
			}
			if _, e = service.recoverExpiredHold(ctx, i); e == nil {
				t.Fatal("unproven intent recovered", scenario)
			}
			wallet, e := funds.GetWallet(ctx, i.WalletID)
			if e != nil || wallet.ReservedMicrocredits != i.MaxMicrocredits || releases.Load() != before {
				t.Fatal("denied proof mutated funds", wallet, e)
			}
			var count int
			if e = r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND adapter_version='hold-expiry-v1'`, i.UsageRequestID).Scan(&count); e != nil || count != 0 {
				t.Fatal("denied proof persisted evidence", count, e)
			}
		})
	}
}
