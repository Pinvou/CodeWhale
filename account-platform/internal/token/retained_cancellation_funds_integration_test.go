package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func retainedManagementTokenRepository(t *testing.T) (*PostgresRepository, context.Context) {
	t.Helper()
	dsn := os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL")
	if dsn == "" {
		t.Skip("PINVOU_TEST_TOKEN_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	roles := []string{"token_gateway", "token_cap_policy_deployer", "token_schema_owner", "token_migrator", "token_policy_operator"}
	var existing int
	if err = admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&existing); err != nil || existing != 0 {
		t.Fatalf("isolated role absence guard: count=%d err=%v", existing, err)
	}
	created := []string{}
	database := "f04_token_management_" + uuid.New().String()[:8] + fmt.Sprintf("%d", time.Now().UnixNano())
	databaseCreated := false
	t.Cleanup(func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if databaseCreated {
			var owner string
			var active int
			deadline := time.Now().Add(5 * time.Second)
			for {
				if err := admin.QueryRow(cleanupCtx, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, database).Scan(&owner, &active); err != nil {
					t.Error("owned database cleanup identity unavailable")
					return
				}
				if owner != admin.Config().User {
					t.Error("owned database owner changed; cleanup refused")
					return
				}
				if active == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Error("owned database still active; cleanup refused")
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := admin.Exec(cleanupCtx, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
				t.Error("owned database cleanup failed; roles retained")
				return
			}
		}
		for i := len(created) - 1; i >= 0; i-- {
			if _, err := admin.Exec(cleanupCtx, "DROP ROLE "+pgx.Identifier{created[i]}.Sanitize()); err != nil {
				t.Errorf("owned test role cleanup refused: %s", created[i])
			}
		}
	})
	for _, role := range roles {
		options := " NOLOGIN NOINHERIT"
		if role != "token_schema_owner" && role != "token_cap_policy_deployer" {
			options = " LOGIN NOINHERIT"
		}
		if _, err = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+options); err != nil {
			t.Fatal(err)
		}
		created = append(created, role)
	}
	if _, err = admin.Exec(ctx, `GRANT token_gateway,token_schema_owner,token_cap_policy_deployer TO token_migrator`); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	databaseCreated = true
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = database
	owner, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	// The existing production database initializer installs this trusted
	// extension as the bootstrap administrator before handing schema authority
	// to the dedicated non-owner runtime and migrator roles.
	if _, err = owner.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto; ALTER SCHEMA public OWNER TO token_schema_owner; GRANT USAGE,CREATE ON SCHEMA public TO token_gateway`); err != nil {
		owner.Close(context.Background())
		t.Fatal(err)
	}
	owner.Close(context.Background())
	config.User = "token_migrator"
	config.Password = ""
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close(context.Background()) })
	t.Cleanup(func() { migrations.Unlock(context.Background(), connection) })
	if _, err = connection.Exec(ctx, `SET ROLE token_gateway`); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	if err = tokenschema.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	verification, err := migrations.VerifyApplied(ctx, connection, loaded)
	if err != nil || verification.AppliedCount != 28 {
		t.Fatalf("public migration verification: %+v err=%v", verification, err)
	}
	migrations.Unlock(ctx, connection)
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.Database = database
	poolConfig.ConnConfig.User = "token_gateway"
	poolConfig.ConnConfig.Password = ""
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	secret, err := NewLocalAESProvider("controlled-key", bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &PostgresRepository{pool: pool, secrets: secret, now: func() time.Time { return time.Now().UTC() }}, ctx
}
func retainedManagementBillingRepository(t *testing.T) (*billing.PostgresRepository, context.Context, *pgxpool.Pool) {
	t.Helper()
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is required for actual PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	control, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("cannot connect to fixture database")
	}
	database := "retained_finance_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{database}.Sanitize()
	var exists bool
	if err = control.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, database).Scan(&exists); err != nil || exists {
		control.Close(context.Background())
		t.Fatal("new UUID database must not already exist")
	}
	if _, err = control.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER billing_schema_owner"); err != nil {
		control.Close(context.Background())
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		var owner string
		var active int
		// Closed client connections can remain visible until PostgreSQL finishes
		// their backend shutdown. Wait only for this fixture's own database.
		deadline := time.Now().Add(5 * time.Second)
		var queryErr error
		for {
			queryErr = control.QueryRow(cleanup, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, database).Scan(&owner, &active)
			if queryErr != nil || owner != "billing_schema_owner" || active == 0 || !time.Now().Before(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if queryErr != nil || owner != "billing_schema_owner" || active != 0 {
			control.Close(cleanup)
			t.Error("own-database identity/connection cleanup guard failed")
			return
		}
		if _, err := control.Exec(cleanup, "DROP DATABASE "+quoted); err != nil {
			t.Error("own-database cleanup failed")
		}
		control.Close(cleanup)
	})
	configuration, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("cannot parse fixture connection configuration")
	}
	configuration.Database = database
	configuration.User = "billing_migrator"
	configuration.Password = ""
	connection, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal("cannot connect to own UUID database")
	}
	t.Cleanup(func() { connection.Close(context.Background()) })
	if _, err = connection.Exec(ctx, `SET ROLE billing_schema_owner; ALTER SCHEMA public OWNER TO billing_schema_owner; REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE,CREATE ON SCHEMA public TO billing_service; SET ROLE billing_service`); err != nil {
		t.Fatal("owned deployment migration actor setup failed")
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err = billingschema.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	// Installation is complete. Retaining its session lock until fixture cleanup
	// would prevent an independent migration connection from validating recovery.
	migrations.Unlock(ctx, connection)
	config, err := pgxpool.ParseConfig(raw)
	if err != nil {
		t.Fatal("cannot parse fixture database configuration")
	}
	config.ConnConfig.Database = database
	config.ConnConfig.User = "billing_service"
	config.ConnConfig.Password = ""
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot open fixture pool")
	}
	t.Cleanup(pool.Close)
	return billing.NewPostgresRepositoryWithPool(pool), ctx, pool
}

// PostgreSQL facts are installed by the public migrators. The setup models an
// existing request whose reservation ACK was lost, not a new chat producer.
func retainedCancellationUsage(t *testing.T, r *PostgresRepository, ctx context.Context, user, wallet uuid.UUID, hold *uuid.UUID, status string) uuid.UUID {
	t.Helper()
	id, account, credential := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	if _, err := r.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,$4,'ACTIVE',100,$2,$5,$5,'PLATFORM')`, account, user, wallet, id.String(), now); err != nil {
		t.Fatal(err)
	}
	cipher, err := r.secrets.Encrypt([]byte("controlled-provider-fact"), []byte(account.String()+":"+credential.String()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at) VALUES($1,$2,'controlled','https://provider.invalid',$3,$4,$5,1,'ACTIVE',ARRAY['model'],$6,$7)`, credential, account, cipher, r.secrets.KeyID(), strings.Repeat("a", 64), user, now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO usage_requests(id,idempotency_key_digest,request_fingerprint,user_id,session_id,token_account_id,credential_id,wallet_id,hold_id,model,status,maximum_microcredits,request_snapshot,started_at,updated_at,canonicalization_version,execution_status,cancel_status,cancel_requested_at) VALUES($1,$2,$2,$3,'controlled-session',$4,$5,$6,$7,'model','CREATED',100,'{"context_type":"PERSONAL"}',$8,$8,1,$9,'CANCEL_REQUESTED',$8)`, id, strings.Repeat("b", 64), user, account, credential, wallet, hold, now, status); err != nil {
		t.Fatal(err)
	}
	envelope := DispatchEnvelope{Price: PriceVersion{ID: uuid.New(), FormulaVersionID: uuid.New(), Model: "model"}, ContextType: "PERSONAL", MaximumCost: 100}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := r.secrets.Encrypt(payload, replayAAD(id, "dispatch", 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO usage_dispatch_preparations(usage_request_id,encrypted_envelope,key_id,envelope_integrity,created_at) VALUES($1,$2,$3,$4,$5)`, id, encrypted, r.secrets.KeyID(), replayDigest(encrypted), now); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestManagementCancellationFundsRemainTruthful(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	repository, ctx := retainedManagementTokenRepository(t)
	funds, _, billingPool := retainedManagementBillingRepository(t)
	t.Run("lost-reserve-ack-then-forbidden-cannot-prove-no-hold", func(t *testing.T) {
		user := uuid.New()
		wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = funds.Credit(ctx, wallet.ID, "cancel-credit-"+user.String(), 1000, false); err != nil {
			t.Fatal(err)
		}
		usageID := retainedCancellationUsage(t, repository, ctx, user, wallet.ID, nil, "HOLD_RESERVING")
		// Real reservation is committed; deliberately do not attach its ACK to Token.
		actualHold, err := funds.Reserve(ctx, usageID, wallet.ID, 100, nil, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/internal/v1/holds/reserve" {
				t.Error("unexpected recovery operation")
			}
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"code":"FORBIDDEN"}`)
		}))
		defer server.Close()
		client, err := NewHTTPBillingClient(server.URL, "controlled-service")
		if err != nil {
			t.Fatal(err)
		}
		service := &Service{billing: client, cancellationBilling: client, cancellationStore: repository}
		_, cancelErr := service.finishPreDispatchCancellation(ctx, usageID)
		current, err := repository.GetUsage(ctx, usageID)
		if err != nil {
			t.Fatal(err)
		}
		gotWallet, err := funds.GetWallet(ctx, wallet.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cancelErr == nil || current.Status != "REVIEW" || current.SettlementStatus != "REVIEW" || current.ExecutionStatus != "REVIEW" || current.CancelStatus == "LOCAL_CONFIRMED" || current.HoldID != nil || gotWallet.ReservedMicrocredits != 100 || actualHold.Status != "ACTIVE" {
			t.Fatalf("unknown hold was falsely confirmed: cancelErr=%v token=%s/%s/%s/%s reserved=%d", cancelErr, current.Status, current.ExecutionStatus, current.SettlementStatus, current.CancelStatus, gotWallet.ReservedMicrocredits)
		}
		var confirmed int
		if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND evidence_type='LOCAL_CONFIRMED'`, usageID).Scan(&confirmed); err != nil || confirmed != 0 {
			t.Fatalf("false local evidence count=%d err=%v", confirmed, err)
		}
	})
	t.Run("recorded-user-proof-recovers-original-priced-hold", func(t *testing.T) {
		user := uuid.New()
		wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = funds.Credit(ctx, wallet.ID, "user-recovery-credit-"+user.String(), 1000, false); err != nil {
			t.Fatal(err)
		}
		usageID := retainedCancellationUsage(t, repository, ctx, user, wallet.ID, nil, "HOLD_RESERVING")
		envelope, err := repository.LoadDispatchPreparation(ctx, usageID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = billingPool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_id,published_at) VALUES($1,'model',0,0,0,now(),now(),'model',now())`, envelope.Price.ID); err != nil {
			t.Fatal(err)
		}
		usage, err := repository.GetUsage(ctx, usageID)
		if err != nil {
			t.Fatal(err)
		}
		priceJSON, err := json.Marshal(envelope.Price)
		if err != nil {
			t.Fatal(err)
		}
		pricing := &billing.HoldPricingContext{ModelID: usage.Model, ProtocolCode: "CHAT", FormulaVersionID: envelope.Price.FormulaVersionID, PricingSnapshot: priceJSON, RequestSnapshot: usage.RequestSnapshot}
		hold, err := funds.ReservePriced(ctx, usageID, wallet.ID, 100, &envelope.Price.ID, time.Now().Add(time.Hour), pricing)
		if err != nil {
			t.Fatal(err)
		}
		if err = repository.MarkReview(ctx, usageID, "cancel_hold_recovery_unconfirmed", map[string]any{"provider_success_inferred": false}); err != nil {
			t.Fatal(err)
		}
		// This is a pre-existing user cancellation fact, separate from the model
		// policy producer exercised by the complete worker regression below.
		if _, err = repository.pool.Exec(ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'CANCEL_REQUESTED','controlled',jsonb_build_object('pre_dispatch',true),now())`, uuid.New(), usageID); err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(billing.NewService(funds).Handler("controlled-service"))
		defer server.Close()
		client, err := NewHTTPBillingClient(server.URL, "controlled-service")
		if err != nil {
			t.Fatal(err)
		}
		service := &Service{billing: client, cancellationBilling: client, cancellationStore: repository}
		result, err := service.finishPreDispatchCancellation(ctx, usageID)
		if err != nil {
			t.Fatal(err)
		}
		current, err := repository.GetUsage(ctx, usageID)
		if err != nil {
			t.Fatal(err)
		}
		wallet, err = funds.GetWallet(ctx, wallet.ID)
		if err != nil || result.CancelStatus != "LOCAL_CONFIRMED" || current.SettlementStatus != "RELEASED" || current.HoldID == nil || *current.HoldID != hold.ID || wallet.ReservedMicrocredits != 0 || wallet.AvailableMicrocredits != 1000 {
			t.Fatalf("user proof failed real hold recovery: err=%v status=%s/%s attached=%v reserved=%d", err, current.ExecutionStatus, current.SettlementStatus, current.HoldID != nil, wallet.ReservedMicrocredits)
		}
	})
	t.Run("actual-billing-release-confirms-persisted-hold", func(t *testing.T) {
		user := uuid.New()
		wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = funds.Credit(ctx, wallet.ID, "release-credit-"+user.String(), 1000, false); err != nil {
			t.Fatal(err)
		}
		usageID := retainedCancellationUsage(t, repository, ctx, user, wallet.ID, nil, "HOLD_RESERVED")
		hold, err := funds.Reserve(ctx, usageID, wallet.ID, 100, nil, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repository.pool.Exec(ctx, `UPDATE usage_requests SET hold_id=$2,settlement_status='HOLD_ACTIVE',status='RESERVED' WHERE id=$1`, usageID, hold.ID); err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(billing.NewService(funds).Handler("controlled-service"))
		defer server.Close()
		client, err := NewHTTPBillingClient(server.URL, "controlled-service")
		if err != nil {
			t.Fatal(err)
		}
		service := &Service{billing: client, cancellationBilling: client, cancellationStore: repository}
		result, err := service.finishPreDispatchCancellation(ctx, usageID)
		if err != nil {
			t.Fatal(err)
		}
		if result.CancelStatus != "LOCAL_CONFIRMED" || result.SettlementStatus != "RELEASED" {
			t.Fatalf("release not confirmed: %+v", result)
		}
		wallet, err = funds.GetWallet(ctx, wallet.ID)
		if err != nil || wallet.ReservedMicrocredits != 0 || wallet.AvailableMicrocredits != 1000 {
			t.Fatalf("funds not released: %+v %v", wallet, err)
		}
		if _, err = funds.Release(ctx, usageID, hold.ID, "repeat confirmed release"); err != nil {
			t.Fatalf("actual released hold replay: %v", err)
		}
	})
	t.Run("initial-received-with-no-reserve-may-confirm", func(t *testing.T) {
		user := uuid.New()
		wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
		if err != nil {
			t.Fatal(err)
		}
		usageID := retainedCancellationUsage(t, repository, ctx, user, wallet.ID, nil, "RECEIVED")
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
		defer server.Close()
		client, err := NewHTTPBillingClient(server.URL, "controlled-service")
		if err != nil {
			t.Fatal(err)
		}
		service := &Service{billing: client, cancellationBilling: client, cancellationStore: repository}
		result, err := service.finishPreDispatchCancellation(ctx, usageID)
		if err != nil || result.CancelStatus != "LOCAL_CONFIRMED" || calls != 0 {
			t.Fatalf("initial cancellation: %+v err=%v calls=%d", result, err, calls)
		}
	})
}

func TestManagementEmergencyCancellationRecoversLostReserveACK(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	repository, ctx := retainedManagementTokenRepository(t)
	funds, _, billingPool := retainedManagementBillingRepository(t)
	user := uuid.New()
	wallet, _, err := funds.EnsureWallet(ctx, "PERSONAL", user)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = funds.Credit(ctx, wallet.ID, "worker-credit-"+user.String(), 1000, false); err != nil {
		t.Fatal(err)
	}
	usageID := retainedCancellationUsage(t, repository, ctx, user, wallet.ID, nil, "HOLD_RESERVING")
	envelope, err := repository.LoadDispatchPreparation(ctx, usageID)
	if err != nil {
		t.Fatal(err)
	}
	// An existing persisted price and immutable request are used by both the lost
	// first response and the later recovery request; no alternate facts are passed.
	if _, err = billingPool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_id,published_at) VALUES($1,'model',0,0,0,now(),now(),'model',now())`, envelope.Price.ID); err != nil {
		t.Fatal(err)
	}
	usage, err := repository.GetUsage(ctx, usageID)
	if err != nil {
		t.Fatal(err)
	}
	priceJSON, err := json.Marshal(envelope.Price)
	if err != nil {
		t.Fatal(err)
	}
	pricing := &billing.HoldPricingContext{ModelID: usage.Model, ProtocolCode: "CHAT", FormulaVersionID: envelope.Price.FormulaVersionID, PricingSnapshot: priceJSON, RequestSnapshot: usage.RequestSnapshot}
	actualHold, err := funds.ReservePriced(ctx, usageID, wallet.ID, 100, &envelope.Price.ID, time.Now().Add(time.Hour), pricing)
	if err != nil {
		t.Fatal(err)
	}
	modelID, policyID := uuid.New(), uuid.New()
	var catalogVersion int64
	if _, err = repository.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'controlled','new-api-v1','model',$2,$3,now())`, modelID, strings.Repeat("c", 64), user); err != nil {
		t.Fatal(err)
	}
	if err = repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled lifecycle fact',$2,now(),now()) RETURNING version`, uuid.New(), user).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	capabilityID := uuid.New()
	protocolID := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	if _, err = repository.pool.Exec(ctx, `INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["text"]','controlled',$4,now())`, capabilityID, modelID, protocolID, user); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.pool.Exec(ctx, `UPDATE usage_requests SET model_id=$2,catalog_version=$3,model_capability_version_id=$4,protocol_definition_id=$5,route_generation=1,model_snapshot='{}',started_at=now()-interval '1 hour' WHERE id=$1`, usageID, modelID, catalogVersion, capabilityID, protocolID); err != nil {
		t.Fatal(err)
	}

	if _, err = repository.pool.Exec(ctx, `INSERT INTO model_emergency_cancellation_policies(id,catalog_version,model_id,effective_at,requested_by,reason,created_at) VALUES($1,$2,$3,now()-interval '1 minute',$4,'controlled force cancellation',now())`, policyID, catalogVersion, modelID, user); err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Bool
	handler := billing.NewService(funds).Handler("controlled-service")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			io.WriteString(w, `{"code":"FORBIDDEN"}`)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-service")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewNewAPIGatewayAdapter()
	service := &Service{billing: client, cancellationBilling: client, cancellationStore: repository, cancellationAdapter: adapter, upstream: adapter}
	worker := NewEmergencyModelCancellationWorker(repository, service, time.Second)
	if err = worker.runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := repository.GetUsage(ctx, usageID)
	if err != nil {
		t.Fatal(err)
	}
	wallet, err = funds.GetWallet(ctx, wallet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExecutionStatus != "REVIEW" || first.CancelStatus == "LOCAL_CONFIRMED" || wallet.ReservedMicrocredits != 100 {
		t.Fatalf("first rejection lost uncertainty: %s/%s reserved=%d", first.ExecutionStatus, first.CancelStatus, wallet.ReservedMicrocredits)
	}
	var cancellationID uuid.UUID
	if err = repository.pool.QueryRow(ctx, `SELECT id FROM model_emergency_cancellations WHERE usage_request_id=$1`, usageID).Scan(&cancellationID); err != nil {
		t.Fatal(err)
	}
	// A request and a completed reservation do not themselves prove release.
	if err = repository.recordEmergencyCancellationAttempt(ctx, cancellationID, ProviderCancelUnsupported, false, true, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unconfirmed attempt accepted: %v", err)
	}
	var attempted int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM model_emergency_cancellation_evidence WHERE emergency_cancellation_id=$1 AND evidence_type='ATTEMPTED'`, cancellationID).Scan(&attempted); err != nil || attempted != 0 {
		t.Fatalf("unconfirmed attempt evidence=%d err=%v", attempted, err)
	}
	allowed.Store(true)
	if err = worker.runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	final, err := repository.GetUsage(ctx, usageID)
	if err != nil {
		t.Fatal(err)
	}
	wallet, err = funds.GetWallet(ctx, wallet.ID)
	if err != nil {
		t.Fatal(err)
	}
	var targetStatus string
	if err = repository.pool.QueryRow(ctx, `SELECT status FROM model_emergency_cancellations WHERE usage_request_id=$1`, usageID).Scan(&targetStatus); err != nil {
		t.Fatal(err)
	}
	if final.ExecutionStatus != "CANCELLED_BEFORE_DISPATCH" || final.SettlementStatus != "RELEASED" || final.CancelStatus != "LOCAL_CONFIRMED" || final.HoldID == nil || *final.HoldID != actualHold.ID || wallet.ReservedMicrocredits != 0 || targetStatus != "CONVERGED" {
		t.Fatalf("recovery falsely confirmed or failed funds release: %s/%s/%s attached=%v reserved=%d target=%s", final.ExecutionStatus, final.SettlementStatus, final.CancelStatus, final.HoldID != nil, wallet.ReservedMicrocredits, targetStatus)
	}
	var localCount int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND evidence_type='LOCAL_CONFIRMED' AND sanitized_evidence->>'hold_released'='true'`, usageID).Scan(&localCount); err != nil || localCount != 1 {
		t.Fatalf("real release evidence count=%d err=%v", localCount, err)
	}
	var before, after string
	const billingFacts = `SELECT jsonb_build_object('holds',(SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id) FROM usage_holds h),'leases',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.id) FROM usage_hold_lease_events e),'journal',(SELECT jsonb_agg(to_jsonb(j) ORDER BY j.id) FROM journal_entries j),'postings',(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM ledger_postings p))::text`
	if err = billingPool.QueryRow(ctx, billingFacts).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = worker.runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err = billingPool.QueryRow(ctx, billingFacts).Scan(&after); err != nil || before != after {
		t.Fatalf("repeat worker changed hold or journal: err=%v same=%v", err, before == after)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM usage_cancellation_evidence WHERE usage_request_id=$1 AND evidence_type='LOCAL_CONFIRMED'`, usageID).Scan(&localCount); err != nil || localCount != 1 {
		t.Fatalf("repeat confirmation evidence=%d err=%v", localCount, err)
	}
}

func TestManagementCancellationHoldRecoveryRequiresRecordedProof(t *testing.T) {
	repository, ctx := retainedManagementTokenRepository(t)
	for _, test := range []struct {
		name, evidence, reason       string
		preDispatch, intent, allowed bool
	}{
		{"user-pre-dispatch-proof", "CANCEL_REQUESTED", "cancel_hold_recovery_unconfirmed", true, false, true},
		{"model-pre-dispatch-proof", "MODEL_EMERGENCY_REQUESTED", "cancel_hold_recovery_unconfirmed", true, false, true},
		{"missing-proof", "", "cancel_hold_recovery_unconfirmed", true, false, false},
		{"active-proof", "CANCEL_REQUESTED", "cancel_hold_recovery_unconfirmed", false, false, false},
		{"unrelated-proof", "MODEL_EMERGENCY_ATTEMPTED", "cancel_hold_recovery_unconfirmed", true, false, false},
		{"unrelated-review", "CANCEL_REQUESTED", "upstream_transport_unknown", true, false, false},
		{"persisted-dispatch-intent", "CANCEL_REQUESTED", "cancel_hold_recovery_unconfirmed", true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			usageID := retainedCancellationUsage(t, repository, ctx, uuid.New(), uuid.New(), nil, "HOLD_RESERVING")
			if err := repository.MarkReview(ctx, usageID, test.reason, map[string]any{"provider_success_inferred": false}); err != nil {
				t.Fatal(err)
			}
			if test.evidence != "" {
				if _, err := repository.pool.Exec(ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,$3,'controlled',jsonb_build_object('pre_dispatch',$4::boolean),now())`, uuid.New(), usageID, test.evidence, test.preDispatch); err != nil {
					t.Fatal(err)
				}
			}
			if test.intent {
				if _, err := repository.pool.Exec(ctx, `INSERT INTO usage_dispatch_intents(usage_request_id,upstream_idempotency_key,created_at) VALUES($1,'controlled-persisted-intent',now())`, usageID); err != nil {
					t.Fatal(err)
				}
			}
			holdID := uuid.New()
			err := repository.CompleteCancellationHold(ctx, usageID, holdID, uuid.New())
			current, readErr := repository.GetUsage(ctx, usageID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if test.allowed {
				if !errors.Is(err, ErrCancellationRequested) || current.HoldID == nil || *current.HoldID != holdID || current.ExecutionStatus != "HOLD_RESERVED" {
					t.Fatalf("recorded recovery failed: err=%v state=%s attached=%v", err, current.ExecutionStatus, current.HoldID != nil)
				}
			} else if !errors.Is(err, ErrForbidden) || current.HoldID != nil || current.ExecutionStatus != "REVIEW" {
				t.Fatalf("unproven recovery accepted: err=%v state=%s attached=%v", err, current.ExecutionStatus, current.HoldID != nil)
			}
		})
	}
}
