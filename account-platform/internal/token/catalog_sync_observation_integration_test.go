package token

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/requestdeadline"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type catalogSyncTestProvider struct {
	lock        sync.Mutex
	model       string
	calls       int
	nextEntered chan struct{}
	nextRelease chan struct{}
}

func (provider *catalogSyncTestProvider) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	provider.lock.Lock()
	provider.calls++
	model := provider.model
	entered, release := provider.nextEntered, provider.nextRelease
	provider.nextEntered, provider.nextRelease = nil, nil
	provider.lock.Unlock()
	if entered != nil {
		close(entered)
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": model}}})
}

func (provider *catalogSyncTestProvider) set(model string) {
	provider.lock.Lock()
	defer provider.lock.Unlock()
	provider.model = model
}

func (provider *catalogSyncTestProvider) count() int {
	provider.lock.Lock()
	defer provider.lock.Unlock()
	return provider.calls
}

func (provider *catalogSyncTestProvider) blockNext() (<-chan struct{}, chan<- struct{}) {
	provider.lock.Lock()
	defer provider.lock.Unlock()
	provider.nextEntered = make(chan struct{})
	provider.nextRelease = make(chan struct{})
	return provider.nextEntered, provider.nextRelease
}

type catalogSyncActivationStore struct {
	repository *PostgresRepository
}

func newCatalogSyncActivationStore(t *testing.T, ctx context.Context, repository *PostgresRepository) *catalogSyncActivationStore {
	t.Helper()
	if _, err := repository.pool.Exec(ctx, `CREATE TABLE catalog_sync_test_activation(singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),state TEXT NOT NULL,epoch BIGINT NOT NULL); INSERT INTO catalog_sync_test_activation(singleton,state,epoch) VALUES(TRUE,'ACTIVE',1)`); err != nil {
		t.Fatal(err)
	}
	return &catalogSyncActivationStore{repository: repository}
}

func (store *catalogSyncActivationStore) CurrentTokenCommandGovernanceActivation(ctx context.Context) (TokenCommandGovernanceActivation, error) {
	var result TokenCommandGovernanceActivation
	err := store.repository.pool.QueryRow(ctx, `SELECT state,epoch FROM catalog_sync_test_activation WHERE singleton=TRUE`).Scan(&result.State, &result.Epoch)
	return result, err
}

func (store *catalogSyncActivationStore) LockAndCheckTokenRouteActivation(ctx context.Context, tx pgx.Tx, selected TokenCommandGovernanceActivation) error {
	var state string
	var epoch int64
	if err := tx.QueryRow(ctx, `SELECT state,epoch FROM catalog_sync_test_activation WHERE singleton=TRUE FOR UPDATE`).Scan(&state, &epoch); err != nil {
		return err
	}
	if state != selected.State || epoch != selected.Epoch || state != "ACTIVE" || epoch < 1 {
		return ErrTokenRouteActivationChanged
	}
	return nil
}

func (store *catalogSyncActivationStore) set(t *testing.T, ctx context.Context, state string, epoch int64) {
	t.Helper()
	if _, err := store.repository.pool.Exec(ctx, `UPDATE catalog_sync_test_activation SET state=$1,epoch=$2 WHERE singleton=TRUE`, state, epoch); err != nil {
		t.Fatal(err)
	}
}

type catalogSyncFactCounts struct {
	identities, entries, versions, catalogAudit, commands, commandAudit, outbox int
}

// TestCatalogSyncObservationPostgresIntegration traverses the production
// precheck and governed command handlers. The ACTIVE reader is a test-only
// seam; production remains PRE_ACTIVATION until APT-138 installs its reader.
func TestCatalogSyncObservationPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	provider := &catalogSyncTestProvider{model: "model-a"}
	providerServer := httptest.NewServer(provider)
	defer providerServer.Close()
	synchronizer, err := NewCatalogSynchronizer(repository, NewHTTPUpstreamClient(), "provider", providerServer.URL, []byte("secret-value"), 128000, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.MustParse("0199a136-0001-7000-8000-000000000001")
	activation := newCatalogSyncActivationStore(t, ctx, repository)
	handler := newGovernedCatalogSyncIntegrationHandler(t, repository, synchronizer, activation, actor)

	precheck := performCatalogSyncPrecheck(t, handler)
	baseline := loadCatalogSyncFactCounts(t, ctx, repository)
	provider.set("model-b")
	response := performCatalogSyncCommand(t, handler, catalogSyncEnvelope(t, precheck), "catalog-sync-provider-drift")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "COMMAND_PRECHECK_CHANGED") {
		t.Fatalf("provider drift=%d %s", response.Code, response.Body.String())
	}
	assertCatalogSyncFactCounts(t, baseline, loadCatalogSyncFactCounts(t, ctx, repository))

	// Middleware selects ACTIVE/epoch=1 before the provider call. Changing both
	// state and epoch while that call is blocked must be detected by the
	// publication transaction guard before any immutable fact is written.
	activation.set(t, ctx, "ACTIVE", 1)
	precheck = performCatalogSyncPrecheck(t, handler)
	baseline = loadCatalogSyncFactCounts(t, ctx, repository)
	entered, release := provider.blockNext()
	traced := make(chan *httptest.ResponseRecorder, 1)
	raceEnvelope := catalogSyncEnvelope(t, precheck)
	go func() {
		traced <- performCatalogSyncCommand(t, handler, raceEnvelope, "catalog-sync-activation-race")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider observation did not block")
	}
	activation.set(t, ctx, "SAFE_ROLLBACK", 2)
	close(release)
	select {
	case response = <-traced:
	case <-time.After(5 * time.Second):
		t.Fatal("activation race did not finish")
	}
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "TOKEN_COMMAND_GOVERNANCE_INACTIVE") {
		t.Fatalf("activation race=%d %s", response.Code, response.Body.String())
	}
	assertCatalogSyncFactCounts(t, baseline, loadCatalogSyncFactCounts(t, ctx, repository))

	// Epoch-only drift is also rejected even when the state remains ACTIVE.
	activation.set(t, ctx, "ACTIVE", 3)
	precheck = performCatalogSyncPrecheck(t, handler)
	baseline = loadCatalogSyncFactCounts(t, ctx, repository)
	entered, release = provider.blockNext()
	traced = make(chan *httptest.ResponseRecorder, 1)
	raceEnvelope = catalogSyncEnvelope(t, precheck)
	go func() {
		traced <- performCatalogSyncCommand(t, handler, raceEnvelope, "catalog-sync-epoch-race")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider observation did not block for epoch race")
	}
	activation.set(t, ctx, "ACTIVE", 4)
	close(release)
	select {
	case response = <-traced:
	case <-time.After(5 * time.Second):
		t.Fatal("epoch race did not finish")
	}
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "TOKEN_COMMAND_GOVERNANCE_INACTIVE") {
		t.Fatalf("epoch race=%d %s", response.Code, response.Body.String())
	}
	assertCatalogSyncFactCounts(t, baseline, loadCatalogSyncFactCounts(t, ctx, repository))

	provider.set("model-a")
	activation.set(t, ctx, "ACTIVE", 5)
	precheck = performCatalogSyncPrecheck(t, handler)
	envelope := catalogSyncEnvelope(t, precheck)
	response = performCatalogSyncCommand(t, handler, envelope, "catalog-sync-success-0001")
	var succeeded TokenCommandResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &succeeded) != nil || succeeded.Status != "SUCCEEDED" || succeeded.CommandReference == uuid.Nil {
		t.Fatalf("matching sync=%d %s", response.Code, response.Body.String())
	}
	var synchronizedRef string
	if err := repository.pool.QueryRow(ctx, `SELECT upstream_model_ref FROM model_identities`).Scan(&synchronizedRef); err != nil || synchronizedRef != "model-a" {
		t.Fatalf("checked observation not persisted: ref=%q err=%v", synchronizedRef, err)
	}

	callsAfterSuccess := provider.count()
	get := httptest.NewRequest(http.MethodGet, "/admin/v1/commands/"+succeeded.CommandReference.String(), nil)
	get.Header.Set("Authorization", "Bearer integration")
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), succeeded.CommandReference.String()) || provider.count() != callsAfterSuccess {
		t.Fatalf("reference recovery=%d calls=%d/%d body=%s", getResponse.Code, provider.count(), callsAfterSuccess, getResponse.Body.String())
	}
	replayed := performCatalogSyncCommand(t, handler, envelope, "catalog-sync-success-0001")
	var replayedResult TokenCommandResult
	if replayed.Code != http.StatusOK || json.Unmarshal(replayed.Body.Bytes(), &replayedResult) != nil || replayedResult.CommandReference != succeeded.CommandReference || provider.count() != callsAfterSuccess {
		t.Fatalf("idempotent recovery=%d calls=%d/%d body=%s", replayed.Code, provider.count(), callsAfterSuccess, replayed.Body.String())
	}

	precheck = performCatalogSyncPrecheck(t, handler)
	manuallyAdvanceCatalogVersion(t, ctx, repository, actor)
	baseline = loadCatalogSyncFactCounts(t, ctx, repository)
	response = performCatalogSyncCommand(t, handler, catalogSyncEnvelope(t, precheck), "catalog-sync-baseline-drift")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "COMMAND_PRECHECK_CHANGED") {
		t.Fatalf("catalog baseline drift=%d %s", response.Code, response.Body.String())
	}
	assertCatalogSyncFactCounts(t, baseline, loadCatalogSyncFactCounts(t, ctx, repository))

	legacyCalls := provider.count()
	legacy := httptest.NewRequest(http.MethodPost, "/admin/v1/models:sync", nil)
	legacy.Header.Set("Authorization", "Bearer integration")
	legacyResponse := httptest.NewRecorder()
	handler.ServeHTTP(legacyResponse, legacy)
	if legacyResponse.Code != http.StatusServiceUnavailable || provider.count() != legacyCalls {
		t.Fatalf("legacy sync bypass=%d calls=%d/%d body=%s", legacyResponse.Code, provider.count(), legacyCalls, legacyResponse.Body.String())
	}
}

func newGovernedCatalogSyncIntegrationHandler(t *testing.T, repository *PostgresRepository, synchronizer *CatalogSynchronizer, activation *catalogSyncActivationStore, actor uuid.UUID) http.Handler {
	t.Helper()
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Authorization-Version", "7")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": actor, "granted_permissions": []string{"platform.models.sync", "platform.models.read"}})
	}))
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "catalog-sync-integration")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).
		WithCatalogSynchronizer(synchronizer).
		WithAuthorizer(authorizer).
		WithTokenCommandGovernanceActivation(activation)
	repository.WithTokenRouteActivationTransactionGuard(activation)
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.integration.example", Subject: actor.String(), SessionID: "catalog-sync-session", AuthenticatedAt: time.Now().UTC()}, nil
	}
	return httpserver.CorrelationHandler(requestdeadline.TokenAdminMiddleware(service.Handler("", verify), time.Now))
}

func performCatalogSyncPrecheck(t *testing.T, handler http.Handler) TokenCommandPrecheckResult {
	t.Helper()
	body := []byte(`{"command_type":"TOKEN_MODEL_CATALOG_SYNC","target_resource_type":"MODEL_CATALOG","target_resource_id":"CURRENT","command":{}}`)
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/commands:precheck", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer integration")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var result TokenCommandPrecheckResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatalf("catalog sync precheck=%d %s", response.Code, response.Body.String())
	}
	return result
}

func catalogSyncEnvelope(t *testing.T, precheck TokenCommandPrecheckResult) TokenCommandEnvelope {
	t.Helper()
	var expected map[string]any
	if err := json.Unmarshal(precheck.ExpectedVersions, &expected); err != nil {
		t.Fatal(err)
	}
	expected["request_fingerprint"] = strings.Repeat("a", 64)
	encoded, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	return TokenCommandEnvelope{CommandType: "TOKEN_MODEL_CATALOG_SYNC", Reason: "synchronize the reviewed provider catalog", TargetResourceType: "MODEL_CATALOG", TargetResourceID: "CURRENT", ExpectedVersions: encoded, PrecheckFingerprint: precheck.PrecheckFingerprint, Command: json.RawMessage(`{}`)}
}

func performCatalogSyncCommand(t *testing.T, handler http.Handler, envelope TokenCommandEnvelope, key string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/commands", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer integration")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func loadCatalogSyncFactCounts(t *testing.T, ctx context.Context, repository *PostgresRepository) catalogSyncFactCounts {
	t.Helper()
	var result catalogSyncFactCounts
	if err := repository.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM model_identities),
 (SELECT count(*) FROM model_catalog_entries),
 (SELECT count(*) FROM model_catalog_versions),
 (SELECT count(*) FROM model_catalog_audit),
 (SELECT count(*) FROM token_command_results),
 (SELECT count(*) FROM token_audit_logs),
 (SELECT count(*) FROM outbox_events)`).Scan(&result.identities, &result.entries, &result.versions, &result.catalogAudit, &result.commands, &result.commandAudit, &result.outbox); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertCatalogSyncFactCounts(t *testing.T, want, got catalogSyncFactCounts) {
	t.Helper()
	if got != want {
		t.Fatalf("catalog sync drift wrote facts: got=%+v want=%+v", got, want)
	}
}

func manuallyAdvanceCatalogVersion(t *testing.T, ctx context.Context, repository *PostgresRepository, actor uuid.UUID) {
	t.Helper()
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := lockCatalogPublication(ctx, tx); err != nil {
		t.Fatal(err)
	}
	current, err := currentCatalogVersion(ctx, tx)
	if err != nil || current < 1 {
		t.Fatalf("current catalog=%d err=%v", current, err)
	}
	next, err := createCatalogVersion(ctx, tx, nil, "equal-count concurrent catalog change", actor, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := copyCatalogExcept(ctx, tx, current, next, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
