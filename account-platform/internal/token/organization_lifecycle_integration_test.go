package token

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type lifecycleCapabilityAbsent struct{ Repository }

func TestOrganizationLifecycleAbsentCapabilityFailsClosed(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	service := NewService(nil, nil, nil).WithManagement(lifecycleCapabilityAbsent{}, nil, ProviderURLPolicy{}, "controlled-lifecycle", nil)
	for _, path := range []string{
		"/internal/v1/organizations/" + uuid.NewString() + "/lifecycle",
		"/internal/v1/organizations/" + uuid.NewString() + "/units/" + uuid.NewString() + "/lifecycle",
	} {
		request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"status":"ACTIVE","account_version":1,"structure_version":1}`))
		// Each route has its own body contract.
		if strings.Contains(path, "/units/") {
			request = httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"status":"ACTIVE","structure_version":1}`))
		} else {
			request = httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"status":"ACTIVE","account_version":1}`))
		}
		request.Header.Set("Authorization", "Bearer controlled-lifecycle")
		response := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("missing actual repository capability got%d: %s", response.Code, response.Body.String())
		}
	}
}

// Actual public migrations/runtime repository and production Account HTTP client
// against Token's real router. Test-only static credentials do not prove a live issuer.
func TestOrganizationLifecyclePublicRepositoryAndAccountClient(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	repository, ctx := retainedManagementTokenRepository(t)
	// Use the shipped independent operator function, not invented policy rows.
	operatorConfig := repository.pool.Config().ConnConfig.Copy()
	operatorConfig.User = "token_policy_operator"
	operator, err := pgx.ConnectConfig(ctx, operatorConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, err = operator.Exec(ctx, `SELECT * FROM public.apply_token_account_owner_cap_policy(20,NULL,$1,'controlled organization lifecycle regression',$2)`, "controlled-organization-cap-key", uuid.New())
	operator.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, nil, nil).WithManagement(repository, nil, ProviderURLPolicy{}, "controlled-lifecycle", nil)
	server := httptest.NewServer(service.Handler(nil))
	defer server.Close()
	client, err := account.NewHTTPOrganizationLifecycleClient(server.URL, "controlled-lifecycle", "TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	organization := uuid.New()
	t.Run("authority-and-malformed", func(t *testing.T) {
		for _, tc := range []struct {
			path, body, credential string
			status                 int
		}{
			{"/internal/v1/organizations/" + organization.String() + "/lifecycle", `{"status":"ACTIVE","account_version":1}`, "wrong", 401},
			{"/internal/v1/organizations/" + organization.String() + "/lifecycle", `{`, "controlled-lifecycle", 400},
			{"/internal/v1/organizations/" + organization.String() + "/units/" + uuid.NewString() + "/lifecycle", `{`, "controlled-lifecycle", 400},
		} {
			request, _ := http.NewRequest(http.MethodPut, server.URL+tc.path, strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer "+tc.credential)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("%s got%d want%d", tc.path, response.StatusCode, tc.status)
			}
		}
	})
	t.Run("projection-replay-version-terminal", func(t *testing.T) {
		result, err := client.Apply(ctx, organization, "ACTIVE", 1)
		if err != nil || !result.Ready {
			t.Fatalf("active: %+v %v", result, err)
		}
		result, err = client.Apply(ctx, organization, "ACTIVE", 1)
		if err != nil || !result.Ready {
			t.Fatalf("replay: %+v %v", result, err)
		}
		var count int
		if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='token.organization.lifecycle-applied.v1'`, organization).Scan(&count); err != nil || count != 1 {
			t.Fatalf("single receipt: %d %v", count, err)
		}
		if _, err = repository.ApplyOrganizationLifecycle(ctx, organization, "DISABLED", 1, false); !errors.Is(err, ErrOrganizationLifecycleConflict) {
			t.Fatalf("changed same-version facts: %v", err)
		}
		result, err = client.Apply(ctx, organization, "DELETED", 2)
		if err != nil || result.Ready || len(result.Blockers) != 1 {
			t.Fatalf("must disable first: %+v %v", result, err)
		}
		if _, err = client.Apply(ctx, organization, "DISABLED", 2); err != nil {
			t.Fatal(err)
		}
		result, err = client.Preflight(ctx, organization, "DELETED", 3)
		if err != nil || !result.Ready {
			t.Fatalf("preflight: %+v %v", result, err)
		}
		var status string
		if err = repository.pool.QueryRow(ctx, `SELECT status FROM organization_lifecycle_states WHERE organization_id=$1`, organization).Scan(&status); err != nil || status != "DISABLED" {
			t.Fatalf("preflight mutated: %s %v", status, err)
		}
		if _, err = client.Apply(ctx, organization, "DELETED", 3); err != nil {
			t.Fatal(err)
		}
		if _, err = repository.ApplyOrganizationLifecycle(ctx, organization, "ACTIVE", 4, false); !errors.Is(err, ErrOrganizationLifecycleConflict) {
			t.Fatalf("terminal revived: %v", err)
		}
	})
	t.Run("unit-blockers-and-terminal", func(t *testing.T) {
		org, unit := uuid.New(), uuid.New()
		if _, err := repository.ApplyOrganizationUnitLifecycle(ctx, org, unit, "ACTIVE", 1); err != nil {
			t.Fatal(err)
		}
		if err := repository.SetOrganizationUnitGovernanceBlocker(ctx, org, unit, "CURRENT_GOVERNANCE", "command", "controlled", true, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		result, err := client.ApplyUnitLifecycle(ctx, org, unit, "DELETION_PENDING", 1)
		if err != nil || result.Ready || len(result.Blockers) != 1 {
			t.Fatalf("actual blocker: %+v %v", result, err)
		}
		result, err = client.ApplyUnitLifecycle(ctx, org, unit, "DELETED", 2)
		if err != nil || result.Ready {
			t.Fatalf("blocked deletion: %+v %v", result, err)
		}
		if err = repository.SetOrganizationUnitGovernanceBlocker(ctx, org, unit, "CURRENT_GOVERNANCE", "command", "controlled", false, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		result, err = client.ApplyUnitLifecycle(ctx, org, unit, "DELETED", 2)
		if err != nil || !result.Ready {
			t.Fatalf("unblocked deletion: %+v %v", result, err)
		}
		if _, err = repository.ApplyOrganizationUnitLifecycle(ctx, org, unit, "ACTIVE", 3); !errors.Is(err, ErrOrganizationLifecycleConflict) {
			t.Fatalf("unit revived: %v", err)
		}
	})
	t.Run("new-account-serialized-with-disable", func(t *testing.T) {
		org := uuid.New()
		if _, err := repository.ApplyOrganizationLifecycle(ctx, org, "ACTIVE", 1, false); err != nil {
			t.Fatal(err)
		}
		tx, err := repository.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err = lockActiveOrganization(ctx, tx, org); err != nil {
			t.Fatal(err)
		}
		finished := make(chan error, 1)
		go func() {
			_, err := repository.ApplyOrganizationLifecycle(ctx, org, "DISABLED", 2, false)
			finished <- err
		}()
		// Actual database lock observation proves ordering; elapsed sleep is not evidence.
		deadline := time.Now().Add(5 * time.Second)
		for {
			var waiting bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted)`).Scan(&waiting)
			if err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("disable never waited")
			}
			time.Sleep(time.Millisecond)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err = <-finished; err != nil {
			t.Fatal(err)
		}
		request := CreateAccountRequest{OwnerType: "ORG", OwnerID: org, OwnerOrganization: &org, WalletID: uuid.New(), CreatedBy: uuid.New(), Name: "blocked", ShareScope: "ORG_ALL", ManagementMode: "PLATFORM"}
		if _, err = repository.CreateAccount(ctx, request); !errors.Is(err, ErrForbidden) {
			t.Fatalf("disabled organization create: %v", err)
		}
	})
	t.Run("new-usage-denied-existing-receipt-replayable", func(t *testing.T) {
		org, user, wallet := uuid.New(), uuid.New(), uuid.New()
		if _, err := client.Apply(ctx, org, "ACTIVE", 1); err != nil {
			t.Fatal(err)
		}
		created, err := repository.CreateAccount(ctx, CreateAccountRequest{OwnerType: "ORG", OwnerID: org, OwnerOrganization: &org, WalletID: wallet, CreatedBy: user, Name: "usage", ShareScope: "ORG_ALL", ManagementMode: "PLATFORM"})
		if err != nil {
			t.Fatal(err)
		}
		credential := uuid.New()
		cipher, err := repository.secrets.Encrypt([]byte("controlled-provider-secret"), []byte(created.ID.String()+":"+credential.String()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repository.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at) VALUES($1,$2,'controlled','https://provider.invalid',$3,$4,$5,1,'ACTIVE',ARRAY['model'],$6,now())`, credential, created.ID, cipher, repository.secrets.KeyID(), strings.Repeat("a", 64), user); err != nil {
			t.Fatal(err)
		}
		request := UsageRequest{ID: uuid.New(), UserID: user, OrganizationID: &org, SessionID: "controlled-organization", TokenAccountID: created.ID, CredentialID: credential, WalletID: wallet, Model: "controlled-model", IdempotencyNamespace: "model-inference", IdempotencyKeyDigest: strings.Repeat("c", 64), CanonicalVersion: 1, RequestFingerprint: strings.Repeat("b", 64)}
		first, fresh, err := repository.BeginUsage(ctx, request, map[string]any{"context_type": "ORGANIZATION"})
		if err != nil || !fresh {
			t.Fatalf("new active usage: %+v %v", first, err)
		}
		if _, err = client.Apply(ctx, org, "DISABLED", 2); err != nil {
			t.Fatal(err)
		}
		replayed, fresh, err := repository.BeginUsage(ctx, request, map[string]any{"context_type": "ORGANIZATION"})
		if err != nil || fresh || replayed.ID != first.ID {
			t.Fatalf("original receipt lost: fresh=%v err=%v", fresh, err)
		}
		request.ID = uuid.New()
		request.IdempotencyKeyDigest = strings.Repeat("d", 64)
		if _, _, err = repository.BeginUsage(ctx, request, map[string]any{"context_type": "ORGANIZATION"}); !errors.Is(err, ErrForbidden) {
			t.Fatalf("new disabled usage: %v", err)
		}
		assessment, err := client.Preflight(ctx, org, "DELETED", 3)
		if err != nil || assessment.Ready || len(assessment.Blockers) != 1 || assessment.Blockers[0].ResourceID != first.ID.String() {
			t.Fatalf("unsettled usage lost: %+v %v", assessment, err)
		}
	})
}
