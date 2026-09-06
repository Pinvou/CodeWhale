package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestTokenCommandGovernancePostgresIntegration exercises migration 000024
// and the production repository path. CI supplies an isolated database and
// rejects SKIP; local runs without PostgreSQL remain explicit.
func TestTokenCommandGovernancePostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_TOKEN_DATABASE_URL is not configured")
	}
	if testing.Short() && os.Getenv("PINVOU_TEST_TOKEN_COMMAND_GATE") != "1" {
		t.Skip("Token command PostgreSQL gate is run explicitly before the short race suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "token_command_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = connection.Exec(context.Background(), `DROP SCHEMA `+identifier+` CASCADE`) })
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadTokenIntegrationMigrations("../../migrations/token", schema)
	if err != nil || len(loaded) == 0 || loaded[len(loaded)-1].Version != 31 {
		t.Fatalf("expected Token migration 31 last: %v", err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &PostgresRepository{pool: pool}
	actor := uuid.New()
	modelID := uuid.New()
	draftEnvelope := func(displayName string) TokenCommandEnvelope {
		t.Helper()
		command, marshalErr := json.Marshal(governedModelDraftCommand{
			ModelID: modelID, ExpectedCatalogVersion: 2, ExpectedDraftVersion: 3,
			DisplayName: displayName, Aliases: []string{}, ProtocolCode: "CHAT", ProtocolVersion: 1,
			ContextLength: 128000, MaximumOutputTokens: 8192, SupportedModalities: []string{"TEXT"}, TokenizerVersion: "v1",
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		envelope, canonicalErr := canonicalizeModelAccountEnvelope(TokenCommandEnvelope{
			CommandType: "TOKEN_MODEL_DRAFT_SAVE", Reason: "save the reviewed model draft safely",
			TargetResourceType: "MODEL", TargetResourceID: modelID.String(),
			PrecheckFingerprint: strings.Repeat("a", 64), Command: command,
		})
		if canonicalErr != nil {
			t.Fatal(canonicalErr)
		}
		facts := map[string]any{"model_id": modelID.String(), "expected_catalog_version": 2, "expected_draft_version": 3, "precheck_fingerprint": envelope.PrecheckFingerprint}
		facts["request_fingerprint"] = governedRequestFingerprint(envelope, facts)
		envelope.ExpectedVersions, marshalErr = json.Marshal(facts)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return envelope
	}
	envelope := draftEnvelope("reviewed model")
	input := tokenCommandBegin{Envelope: envelope, ActorUserID: actor, ActorPermission: "platform.models.manage", AuthorizationVersion: 7, IdempotencyKey: "token-command-key-0001", AuditRequestID: "request-1", BeforeState: json.RawMessage(`{"draft_version":2}`)}
	created, inserted, err := repository.BeginTokenCommand(ctx, input)
	if err != nil || !inserted || created.PolicyVersion != 2 || created.Status != "PENDING" {
		t.Fatalf("begin result=%#v inserted=%v err=%v", created, inserted, err)
	}
	replayed, inserted, err := repository.BeginTokenCommand(ctx, input)
	if err != nil || inserted || replayed.CommandReference != created.CommandReference {
		t.Fatalf("idempotent replay=%#v inserted=%v err=%v", replayed, inserted, err)
	}
	changed := input
	changed.Envelope = draftEnvelope("different")
	if _, _, err := repository.BeginTokenCommand(ctx, changed); !errors.Is(err, ErrTokenCommandResultMismatch) {
		t.Fatalf("idempotency mismatch returned %v", err)
	}
	completed, err := repository.CompleteTokenCommand(ctx, created.CommandReference, "SUCCEEDED", json.RawMessage(`{"draft_version":3}`), json.RawMessage(`{"model_id":"`+modelID.String()+`"}`), "")
	if err != nil || completed.Status != "SUCCEEDED" || completed.CompletedAt == nil {
		t.Fatalf("complete=%#v err=%v", completed, err)
	}
	var audits, outbox int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_MODEL_DRAFT_SAVE'),(SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1')`).Scan(&audits, &outbox); err != nil || audits != 1 || outbox != 1 {
		t.Fatalf("audit=%d outbox=%d err=%v", audits, outbox, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM token_command_results WHERE command_reference=$1`, created.CommandReference); err == nil {
		t.Fatal("immutable command result was deleted")
	}

	assertTokenCommandResultActorIsolation(t, ctx, repository, created.CommandReference, actor)
	assertApprovedCommandReplayUsesOneDomainEffect(t, ctx, repository, pool)
}

func assertTokenCommandResultActorIsolation(t *testing.T, ctx context.Context, repository *PostgresRepository, reference, owner uuid.UUID) {
	t.Helper()
	other := uuid.New()
	account := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Subject string `json:"subject"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Fatal("invalid Account authorization request")
		}
		resolved := owner
		if request.Subject == "other-external-subject" {
			resolved = other
		}
		w.Header().Set("X-Authorization-Version", "7")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": resolved})
	}))
	t.Cleanup(account.Close)
	authorizer, err := authorization.NewClient(account.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{repository: repository, authorizer: authorizer}
	for _, scenario := range []struct {
		name, subject string
		reference     uuid.UUID
		want          int
	}{
		{name: "owner non UUID subject", subject: "owner-external-subject", reference: reference, want: http.StatusOK},
		{name: "other actor hidden", subject: "other-external-subject", reference: reference, want: http.StatusNotFound},
		{name: "unknown hidden", subject: "owner-external-subject", reference: uuid.New(), want: http.StatusNotFound},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			handler := identity.Middleware(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "https://issuer.integration.example", Subject: scenario.subject}, nil
			}, http.HandlerFunc(service.adminTokenCommandResult))
			request := httptest.NewRequest(http.MethodGet, "/token/admin/v1/commands/"+scenario.reference.String(), nil)
			request.SetPathValue("command_reference", scenario.reference.String())
			request.Header.Set("Authorization", "Bearer test")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != scenario.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func assertApprovedCommandReplayUsesOneDomainEffect(t *testing.T, ctx context.Context, repository *PostgresRepository, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `CREATE TABLE test_token_command_domain_effects(execution_key text PRIMARY KEY,effect_id uuid NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	approvalID, actor := uuid.New(), uuid.New()
	fingerprint := strings.Repeat("b", 64)
	executionKey := "approval:" + approvalID.String() + ":TOKEN_MODEL_LIFECYCLE_CHANGE:" + fingerprint
	input := tokenCommandBegin{
		Envelope:    TokenCommandEnvelope{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", Reason: "execute approved lifecycle transition once", TargetResourceType: "MODEL", TargetResourceID: uuid.NewString(), ExpectedVersions: json.RawMessage(`{}`), PrecheckFingerprint: strings.Repeat("c", 64), Command: json.RawMessage(`{"target_lifecycle":"DEPRECATED"}`)},
		ActorUserID: actor, ActorPermission: "platform.models.lifecycle.request", AuthorizationVersion: 4, IdempotencyKey: executionKey,
		AuditRequestID: "approved-request", BeforeState: json.RawMessage(`{"lifecycle":"PUBLISHED"}`), ApprovalRequestID: approvalID, ApprovalFingerprint: fingerprint,
	}
	result, created, err := repository.BeginTokenCommand(ctx, input)
	if err != nil || !created {
		t.Fatalf("begin approved command created=%v err=%v", created, err)
	}
	effectID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO test_token_command_domain_effects(execution_key,effect_id) VALUES($1,$2) ON CONFLICT(execution_key) DO NOTHING`, executionKey, effectID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CompleteTokenCommand(ctx, result.CommandReference, "SUCCEEDED", json.RawMessage(`{"lifecycle":"DEPRECATED"}`), json.RawMessage(`{"secret":"must-not-persist"}`), ""); !errors.Is(err, ErrTokenCommandInvalid) {
		t.Fatalf("expected completion rejection, got %v", err)
	}
	replayed, created, err := repository.BeginTokenCommand(ctx, input)
	if err != nil || created || replayed.CommandReference != result.CommandReference || replayed.Status != "PENDING" {
		t.Fatalf("replay=%#v created=%v err=%v", replayed, created, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO test_token_command_domain_effects(execution_key,effect_id) VALUES($1,$2) ON CONFLICT(execution_key) DO NOTHING`, executionKey, uuid.New()); err != nil {
		t.Fatal(err)
	}
	var effects int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM test_token_command_domain_effects WHERE execution_key=$1`, executionKey).Scan(&effects); err != nil || effects != 1 {
		t.Fatalf("effects=%d err=%v", effects, err)
	}
	if _, err := repository.CompleteTokenCommand(ctx, result.CommandReference, "SUCCEEDED", json.RawMessage(`{"lifecycle":"DEPRECATED"}`), json.RawMessage(`{"credential_id":"`+uuid.NewString()+`","credential_status":"RETIRED"}`), ""); err != nil {
		t.Fatal(err)
	}
}
