package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Uses the current guarded migration wrapper, a unique database, and actual
// PostgreSQL transactions. Balance fixture funding is explicit SQL setup;
// it is not payment or ledger-posting acceptance.
func TestPersonalWalletPostgresIntegration(t *testing.T) {
	repository, ctx := retainedFinanceRepository(t)
	pool := repository.pool
	config := pool.Config().ConnConfig.Copy()
	config.User = "billing_migrator"
	config.Password = ""
	control, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close(context.Background())
	if _, err := control.Exec(ctx, "SET ROLE billing_service"); err != nil {
		t.Fatal(err)
	}
	installed, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.New()
	wallet, created, err := repository.EnsureWallet(ctx, "PERSONAL", owner)
	if err != nil || !created || wallet.ID == uuid.Nil || wallet.OwnerID != owner || wallet.OwnerType != "PERSONAL" || wallet.Status != "ACTIVE" || wallet.Version != 1 || wallet.AvailableMicrocredits != 0 {
		t.Fatalf("initial wallet: %+v created=%v err=%v", wallet, created, err)
	}
	t.Run("concurrent-idempotency-and-liability", func(t *testing.T) {
		var workers sync.WaitGroup
		for range 16 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				got, fresh, err := repository.EnsureWallet(ctx, "PERSONAL", owner)
				if err != nil || fresh || got.ID != wallet.ID {
					t.Errorf("concurrent ensure wallet=%+v fresh=%v err=%v", got, fresh, err)
				}
			}()
		}
		workers.Wait()
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM ledger_accounts WHERE wallet_id=$1 AND code=$2 AND account_type='WALLET_LIABILITY'`, wallet.ID, "wallet:"+wallet.ID.String()).Scan(&count); err != nil || count != 1 {
			t.Fatalf("liability count=%d err=%v", count, err)
		}
	})
	t.Run("reads-actual-persisted-balance", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE wallets SET posted_microcredits=12345,reserved_microcredits=2345,version=version+1 WHERE id=$1`, wallet.ID); err != nil {
			t.Fatal(err)
		}
		got, err := repository.GetWallet(ctx, wallet.ID)
		if err != nil || got.PostedMicrocredits != 12345 || got.ReservedMicrocredits != 2345 || got.AvailableMicrocredits != 10000 || got.Version != 2 {
			t.Fatalf("actual balance %+v err=%v", got, err)
		}
	})
	t.Run("invalid-owner-and-not-found", func(t *testing.T) {
		for _, value := range []struct {
			kind string
			id   uuid.UUID
		}{{"PERSONAL", uuid.Nil}, {"ORGANIZATION", owner}, {"ORG_UNIT", owner}} {
			if _, _, err := repository.EnsureWallet(ctx, value.kind, value.id); !errors.Is(err, ErrConflict) {
				t.Errorf("accepted owner %s/%s: %v", value.kind, value.id, err)
			}
		}
		if _, err := repository.GetWallet(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing wallet=%v", err)
		}
	})
	t.Run("liability-failure-rolls-back-wallet", func(t *testing.T) {
		// Fault injection is deployment-owner setup; the repository keeps its
		// runtime identity and must not receive table ownership for this test.
		if _, err := control.Exec(ctx, "SET ROLE billing_schema_owner"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := control.Exec(ctx, `ALTER TABLE ledger_accounts DROP CONSTRAINT IF EXISTS fixture_reject_insert`); err != nil {
				t.Error(err)
			}
			if _, err := control.Exec(ctx, "SET ROLE billing_service"); err != nil {
				t.Error(err)
			}
		}()
		if _, err := control.Exec(ctx, `ALTER TABLE ledger_accounts ADD CONSTRAINT fixture_reject_insert CHECK(false) NOT VALID`); err != nil {
			t.Fatal(err)
		}
		badOwner := uuid.New()
		_, _, ensureErr := repository.EnsureWallet(ctx, "PERSONAL", badOwner)
		if ensureErr == nil {
			t.Fatal("wallet accepted without its liability account")
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM wallets WHERE owner_type='PERSONAL' AND owner_id=$1`, badOwner).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial wallet count=%d err=%v", count, err)
		}
	})
	t.Run("actual-http-and-closed-route-set", func(t *testing.T) {
		t.Setenv("APP_ENV", "test")
		t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
		t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
		handler := NewService(repository).Handler("fixture-service")
		request := httptest.NewRequest(http.MethodGet, "/internal/v1/wallets/"+wallet.ID.String()+"/balance", nil)
		request.Header.Set("Authorization", "Bearer fixture-service")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, request)
		var got Wallet
		if err := json.Unmarshal(out.Body.Bytes(), &got); err != nil || out.Code != 200 || got.ID != wallet.ID || got.AvailableMicrocredits != 10000 {
			t.Fatalf("status=%d balance=%+v err=%v", out.Code, got, err)
		}
		unauthorized := httptest.NewRecorder()
		handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, request.URL.String(), nil))
		if unauthorized.Code != 401 {
			t.Fatalf("missing service credential status=%d", unauthorized.Code)
		}
		protectedHold := httptest.NewRecorder()
		handler.ServeHTTP(protectedHold, httptest.NewRequest(http.MethodPost, "/internal/v1/holds/reserve", nil))
		if protectedHold.Code != http.StatusUnauthorized {
			t.Fatalf("registered hold route without service credential status=%d", protectedHold.Code)
		}
		for _, path := range []string{"/internal/v1/registration-bonus-grants", "/internal/v1/model-mappings", "/internal/v1/organizations"} {
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, httptest.NewRequest(http.MethodPost, path, nil))
			if out.Code != 404 {
				t.Errorf("future producer %s status=%d", path, out.Code)
			}
		}
	})
	t.Run("migration-rerun-preserves-wallet", func(t *testing.T) {
		if err := billingschema.Up(ctx, control, installed); err != nil {
			t.Fatal(err)
		}
		got, err := repository.GetWallet(ctx, wallet.ID)
		if err != nil || got.Version != 2 || got.AvailableMicrocredits != 10000 {
			t.Fatalf("rerun changed actual wallet: %+v err=%v", got, err)
		}
	})
}
