package token

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// TestTokenAdminProtocolFormulaCommandsPostgresIntegration runs against the
// real Token migration chain. CI supplies an isolated PostgreSQL database and
// rejects SKIP; local runs without that database remain explicit.
func TestTokenAdminProtocolFormulaCommandsPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	fixedNow := time.Date(2026, 9, 3, 9, 30, 0, 0, time.UTC)
	repository.now = func() time.Time { return fixedNow }
	actorID := uuid.MustParse("018f57a9-9c17-7efc-a450-35c445491101")
	unitID := uuid.MustParse("018f57a9-9c17-7efc-a450-35c445491102")
	var billingMode atomic.Int32
	billingServer := newProtocolFormulaBillingServer(t, unitID, &billingMode)
	for _, test := range []struct {
		name   string
		reader TokenCommandGovernanceActivationReader
	}{
		{name: "activation record missing"},
		{name: "activation read error", reader: tokenCommandActivationReaderFunc(func(context.Context) (TokenCommandGovernanceActivation, error) {
			return TokenCommandGovernanceActivation{}, errors.New("activation read failed")
		})},
		{name: "pre activation", reader: tokenCommandActivationReaderFunc(func(context.Context) (TokenCommandGovernanceActivation, error) {
			return TokenCommandGovernanceActivation{State: "PRE_ACTIVATION", Epoch: 1}, nil
		})},
		{name: "non active", reader: tokenCommandActivationReaderFunc(func(context.Context) (TokenCommandGovernanceActivation, error) {
			return TokenCommandGovernanceActivation{State: "SAFE_ROLLBACK", Epoch: 2}, nil
		})},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newProtocolFormulaIntegrationHandler(t, repository, billingServer.URL, actorID, test.reader, true)
			for index, target := range []struct{ method, path string }{
				{http.MethodPost, "/admin/v1/protocols"},
				{http.MethodPost, "/admin/v1/protocols/018f57a9-9c17-7efc-a450-35c445491201/publish"},
				{http.MethodPost, "/admin/v1/protocols/018f57a9-9c17-7efc-a450-35c445491201/formulas"},
				{http.MethodPost, "/admin/v1/formulas/018f57a9-9c17-7efc-a450-35c445491202/publish"},
			} {
				response := performProtocolFormulaCommand(handler, target.method, target.path, map[string]any{}, "activation-gate-000"+strconv.Itoa(index), 0)
				if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "TOKEN_COMMAND_GOVERNANCE_INACTIVE") {
					t.Fatalf("%s status=%d body=%s", target.path, response.Code, response.Body.String())
				}
			}
			assertProtocolFormulaWrites(t, repository, ctx, "GATED_ONLY", 0, 0, 0)
		})
	}
	denied := newProtocolFormulaIntegrationHandler(t, repository, billingServer.URL, actorID, tokenCommandActivationReaderFunc(func(context.Context) (TokenCommandGovernanceActivation, error) {
		return TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1}, nil
	}), false)
	for _, target := range []struct{ method, path string }{
		{http.MethodPost, "/admin/v1/protocols"},
		{http.MethodPost, "/admin/v1/protocols/018f57a9-9c17-7efc-a450-35c445491201/publish"},
		{http.MethodPost, "/admin/v1/protocols/018f57a9-9c17-7efc-a450-35c445491201/formulas"},
		{http.MethodPost, "/admin/v1/formulas/018f57a9-9c17-7efc-a450-35c445491202/publish"},
	} {
		response := performProtocolFormulaCommand(denied, target.method, target.path, map[string]any{}, "authorization-deny-0001", 0)
		if response.Code != http.StatusForbidden {
			t.Fatalf("unauthorized %s returned %d: %s", target.path, response.Code, response.Body.String())
		}
	}
	metadata := func(commandType, suffix string) ProtocolFormulaCommandMetadata {
		return ProtocolFormulaCommandMetadata{
			ActorID: actorID, CommandType: commandType,
			Reason:               "validate protocol and formula governance",
			IdempotencyKeyDigest: strings.Repeat(suffix, 64),
			RequestFingerprint:   strings.Repeat("a", 64),
			CommandReference:     strings.Repeat(suffix, 64), TraceID: "apt-122-integration",
		}
	}

	protocolResult, err := repository.CreateProtocolDraftCommand(ctx, metadata("TOKEN_PROTOCOL_DRAFT_CREATE", "1"), "integration_v2", json.RawMessage(`{"z":true,"a":{"type":"integer"}}`), 0)
	if err != nil || protocolResult.Protocol == nil || protocolResult.Protocol.Status != "DRAFT" || len(protocolResult.Protocol.ETag) != 64 {
		t.Fatalf("create protocol draft: result=%#v err=%v", protocolResult, err)
	}
	if _, err := repository.CreateProtocolDraftCommand(ctx, metadata("TOKEN_PROTOCOL_DRAFT_CREATE", "2"), "integration_v2", json.RawMessage(`{"a":1}`), 0); !errors.Is(err, ErrProtocolHeadVersionConflict) {
		t.Fatalf("stale protocol head must fail CAS: %v", err)
	}
	beforeAudit, beforeOutbox := protocolFormulaFactCounts(t, repository, ctx)
	emptyProtocolValidation := ProtocolPublicationValidation{ProtocolDefinitionID: protocolResult.Protocol.ID, ProtocolCode: protocolResult.Protocol.ProtocolCode, ProtocolVersion: protocolResult.Protocol.Version, FormulaSetSHA256: protocolPublicationFormulaDigest([]ProtocolFormulaVersion{})}
	if _, err := repository.PublishProtocolCommand(ctx, metadata("TOKEN_PROTOCOL_PUBLISH", "2"), protocolResult.Protocol.ID, 2, protocolResult.Protocol.ETag, emptyProtocolValidation); !errors.Is(err, ErrProtocolHeadVersionConflict) {
		t.Fatalf("stale protocol publish must fail CAS: %v", err)
	}
	afterAudit, afterOutbox := protocolFormulaFactCounts(t, repository, ctx)
	if beforeAudit != afterAudit || beforeOutbox != afterOutbox {
		t.Fatal("stale protocol publish must not append audit or outbox")
	}
	modelID := uuid.MustParse("018f57a9-9c17-7efc-a450-35c445491103")
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'APT-122 reference precheck',$2,$3,$3) RETURNING version`, uuid.New(), actorID, fixedNow).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'integration','v1','integration-model',$2,$3,$4)`, modelID, strings.Repeat("b", 64), actorID, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO model_catalog_drafts(model_id,base_catalog_version,draft_version,display_name,description,aliases,protocol_code,protocol_version,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,updated_by,updated_at) VALUES($1,$2,1,'Integration model','','{}',$3,$4,4096,1024,'["text"]','integration-v1',$5,$6)`, modelID, catalogVersion, protocolResult.Protocol.ProtocolCode, protocolResult.Protocol.Version, actorID, fixedNow); err != nil {
		t.Fatal(err)
	}
	protocolResult, err = repository.PublishProtocolCommand(ctx, metadata("TOKEN_PROTOCOL_PUBLISH", "3"), protocolResult.Protocol.ID, 1, protocolResult.Protocol.ETag, emptyProtocolValidation)
	if err != nil || protocolResult.Protocol == nil || protocolResult.Protocol.Status != "PUBLISHED" {
		t.Fatalf("publish protocol: result=%#v err=%v", protocolResult, err)
	}
	if protocolResult.Precheck["model_draft_reference_count"] != int64(1) || protocolResult.Precheck["protocol_code"] != "INTEGRATION_V2" || protocolResult.Precheck["protocol_version"] != int64(1) {
		t.Fatalf("protocol publication did not freeze exact draft references: %#v", protocolResult.Precheck)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE protocol_definitions SET request_schema='{"changed":true}' WHERE id=$1`, protocolResult.Protocol.ID); err == nil {
		t.Fatal("published protocol version must be immutable")
	}

	validation := FormulaDraftValidation{VerifiedCount: 1, UnitVersions: []MeteringUnitVersionReference{{UnitCode: "INPUT_TOKEN", UnitDefinitionID: unitID, Version: 1, Status: "ACTIVE"}}}
	formulaResult, err := repository.CreateFormulaDraftCommand(ctx, metadata("TOKEN_FORMULA_DRAFT_CREATE", "4"), protocolResult.Protocol.ID, 1, 0, map[string]uuid.UUID{"INPUT_TOKEN": unitID}, map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}, validation)
	if err != nil || formulaResult.Formula == nil || formulaResult.Formula.Status != "DRAFT" || len(formulaResult.Formula.ETag) != 64 {
		t.Fatalf("create formula draft: result=%#v err=%v", formulaResult, err)
	}
	requiredIDs, formulaSnapshotDigest := formulaBindingSnapshot(*formulaResult.Formula)
	billingSync := FormulaBindingSync{FormulaVersionID: formulaResult.Formula.ID, BillingStatus: "REGISTERED", ProtocolCode: formulaResult.Formula.ProtocolCode, RequiredUnitVersions: requiredIDs, FormulaSnapshotSHA256: formulaSnapshotDigest, PublishedAt: fixedNow}
	if _, err := repository.PublishFormulaCommand(ctx, metadata("TOKEN_FORMULA_PUBLISH", "5"), formulaResult.Formula.ID, 1, strings.Repeat("0", 64), billingSync); !errors.Is(err, ErrFormulaDraftETagMismatch) {
		t.Fatalf("wrong formula ETag must fail: %v", err)
	}
	formulaResult, err = repository.PublishFormulaCommand(ctx, metadata("TOKEN_FORMULA_PUBLISH", "6"), formulaResult.Formula.ID, 1, formulaResult.Formula.ETag, billingSync)
	if err != nil || formulaResult.Formula == nil || formulaResult.Formula.Status != "PUBLISHED" || formulaResult.BillingSync != "SYNCHRONIZED" {
		t.Fatalf("publish formula: result=%#v err=%v", formulaResult, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE protocol_formula_versions SET expression='{"INPUT_TOKEN":"request_count"}' WHERE id=$1`, formulaResult.Formula.ID); err == nil {
		t.Fatal("published formula version must be immutable")
	}

	var auditCount, outboxCount int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_audit_logs WHERE actor_user_id=$1 AND action LIKE 'TOKEN_%'`, actorID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE trace_id='apt-122-integration' AND event_type='token.management-command.executed.v1'`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 4 || outboxCount != 4 {
		t.Fatalf("commands must atomically append audit and outbox facts: audit=%d outbox=%d", auditCount, outboxCount)
	}
	var rawSecrets int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_audit_logs WHERE actor_user_id=$1 AND summary::text LIKE '%validate protocol and formula governance%'`, actorID).Scan(&rawSecrets); err != nil {
		t.Fatal(err)
	}
	if rawSecrets != 4 {
		t.Fatalf("all four audit facts must retain the structured reason: %d", rawSecrets)
	}
	incompatible, err := repository.CreateProtocolDraftCommand(ctx, metadata("TOKEN_PROTOCOL_DRAFT_CREATE", "7"), "INCOMPATIBLE_V2", json.RawMessage(`{"type":"object"}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	malformedFormulaID := uuid.MustParse("018f57a9-9c17-7efc-a450-35c445491104")
	if _, err := repository.pool.Exec(ctx, `INSERT INTO protocol_formula_versions(id,protocol_definition_id,version,status,required_unit_versions,expression,evaluator_version,created_at) VALUES($1,$2,1,'DRAFT','{"INPUT_TOKEN":"018f57a9-9c17-7efc-a450-35c445491102"}','{"OTHER":"estimated_input_tokens"}','SAFE_PARAMETER_MAP_V1',$3)`, malformedFormulaID, incompatible.Protocol.ID, fixedNow); err != nil {
		t.Fatal(err)
	}
	beforeAudit, beforeOutbox = protocolFormulaFactCounts(t, repository, ctx)
	incompatibleValidation := ProtocolPublicationValidation{ProtocolDefinitionID: incompatible.Protocol.ID, ProtocolCode: incompatible.Protocol.ProtocolCode, ProtocolVersion: incompatible.Protocol.Version, FormulaSetSHA256: protocolPublicationFormulaDigest([]ProtocolFormulaVersion{})}
	if _, err := repository.PublishProtocolCommand(ctx, metadata("TOKEN_PROTOCOL_PUBLISH", "8"), incompatible.Protocol.ID, 1, incompatible.Protocol.ETag, incompatibleValidation); !errors.Is(err, ErrProtocolFormulaPrecheck) {
		t.Fatalf("incompatible formula must block protocol publication: %v", err)
	}
	afterAudit, afterOutbox = protocolFormulaFactCounts(t, repository, ctx)
	if beforeAudit != afterAudit || beforeOutbox != afterOutbox {
		t.Fatal("formula compatibility failure must not append protocol audit or outbox")
	}
	var incompatibleStatus string
	if err := repository.pool.QueryRow(ctx, `SELECT status FROM protocol_definitions WHERE id=$1`, incompatible.Protocol.ID).Scan(&incompatibleStatus); err != nil || incompatibleStatus != "DRAFT" {
		t.Fatalf("incompatible protocol changed status=%s err=%v", incompatibleStatus, err)
	}

	concurrent, err := repository.CreateProtocolDraftCommand(ctx, metadata("TOKEN_PROTOCOL_DRAFT_CREATE", "9"), "CONCURRENT_V2", json.RawMessage(`{"type":"object"}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	_, initialFormulas, err := repository.ProtocolPublishDependencies(ctx, concurrent.Protocol.ID)
	if err != nil {
		t.Fatal(err)
	}
	staleValidation := ProtocolPublicationValidation{ProtocolDefinitionID: concurrent.Protocol.ID, ProtocolCode: concurrent.Protocol.ProtocolCode, ProtocolVersion: concurrent.Protocol.Version, FormulaSetSHA256: protocolPublicationFormulaDigest(initialFormulas)}
	concurrentFormulaID := uuid.MustParse("018f57a9-9c17-7efc-a450-35c445491105")
	if _, err := repository.pool.Exec(ctx, `INSERT INTO protocol_formula_versions(id,protocol_definition_id,version,status,required_unit_versions,expression,evaluator_version,created_at) VALUES($1,$2,1,'DRAFT','{"INPUT_TOKEN":"018f57a9-9c17-7efc-a450-35c445491102"}','{"INPUT_TOKEN":"estimated_input_tokens"}','SAFE_PARAMETER_MAP_V1',$3)`, concurrentFormulaID, concurrent.Protocol.ID, fixedNow); err != nil {
		t.Fatal(err)
	}
	beforeAudit, beforeOutbox = protocolFormulaFactCounts(t, repository, ctx)
	if _, err := repository.PublishProtocolCommand(ctx, metadata("TOKEN_PROTOCOL_PUBLISH", "a"), concurrent.Protocol.ID, 1, concurrent.Protocol.ETag, staleValidation); !errors.Is(err, ErrProtocolFormulaPrecheck) {
		t.Fatalf("concurrent formula reference change must invalidate preflight: %v", err)
	}
	afterAudit, afterOutbox = protocolFormulaFactCounts(t, repository, ctx)
	if beforeAudit != afterAudit || beforeOutbox != afterOutbox {
		t.Fatal("stale publication preflight must not append audit or outbox")
	}

	active := newProtocolFormulaIntegrationHandler(t, repository, billingServer.URL, actorID, tokenCommandActivationReaderFunc(func(context.Context) (TokenCommandGovernanceActivation, error) {
		return TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 3}, nil
	}), true)
	for _, target := range []struct{ method, path string }{
		{http.MethodPost, "/admin/v1/protocols"},
		{http.MethodPost, "/admin/v1/protocols/018f57a9-9c17-7efc-a450-35c445491201/publish"},
		{http.MethodPost, "/admin/v1/protocols/018f57a9-9c17-7efc-a450-35c445491201/formulas"},
		{http.MethodPost, "/admin/v1/formulas/018f57a9-9c17-7efc-a450-35c445491202/publish"},
	} {
		response := performProtocolFormulaCommand(active, target.method, target.path, map[string]any{}, "retired-direct-command-01", 0)
		if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), "DIRECT_TOKEN_COMMAND_RETIRED") {
			t.Fatalf("active direct path must be retired: %s status=%d body=%s", target.path, response.Code, response.Body.String())
		}
	}
}

func protocolFormulaBillingPublishedAt() time.Time {
	return time.Date(2026, 9, 3, 10, 15, 0, 0, time.UTC)
}

func newProtocolFormulaBillingServer(t *testing.T, unitID uuid.UUID, mode *atomic.Int32, putCounters ...*atomic.Int32) *httptest.Server {
	t.Helper()
	var bindingMutex sync.Mutex
	bindingReceipts := make(map[uuid.UUID]FormulaBindingReceipt)
	bindingSnapshots := make(map[uuid.UUID]string)
	billingPublishedAt := protocolFormulaBillingPublishedAt()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/internal/v1/pricing/formula-bindings/validate" {
			switch mode.Load() {
			case 4:
				http.Error(w, `{"code":"PRICE_UNAVAILABLE"}`, http.StatusServiceUnavailable)
				return
			case 5:
				http.Error(w, `{"code":"PRICE_MISSING"}`, http.StatusUnprocessableEntity)
				return
			case 6:
				<-r.Context().Done()
				return
			}
			var body struct {
				Required map[string]uuid.UUID `json:"required_unit_versions"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Required) != 1 || body.Required["INPUT_TOKEN"] != unitID {
				http.Error(w, `{"code":"PRICE_MISSING"}`, http.StatusUnprocessableEntity)
				return
			}
			_ = json.NewEncoder(w).Encode(FormulaDraftValidation{VerifiedCount: 1, UnitVersions: []MeteringUnitVersionReference{{UnitCode: "INPUT_TOKEN", UnitDefinitionID: unitID, Version: 7, Status: "ACTIVE"}}, ObservedAt: time.Date(2026, 9, 3, 10, 10, 0, 0, time.UTC), UnitSetFingerprint: strings.Repeat("c", 64)})
			return
		}
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/internal/v1/pricing/formula-bindings/") {
			if len(putCounters) == 1 && putCounters[0] != nil {
				putCounters[0].Add(1)
			}
			switch mode.Load() {
			case 1:
				http.Error(w, `{"code":"PRICE_MISSING"}`, http.StatusUnprocessableEntity)
				return
			case 2:
				http.Error(w, `{"code":"PRICE_UNAVAILABLE"}`, http.StatusServiceUnavailable)
				return
			case 3:
				<-r.Context().Done()
				return
			}
			var body struct {
				ProtocolCode string          `json:"protocol_code"`
				Required     []uuid.UUID     `json:"required_unit_definition_ids"`
				Snapshot     json.RawMessage `json:"formula_snapshot"`
			}
			formulaID, formulaIDErr := uuid.Parse(strings.TrimPrefix(r.URL.Path, "/internal/v1/pricing/formula-bindings/"))
			if json.NewDecoder(r.Body).Decode(&body) != nil || formulaIDErr != nil || formulaID == uuid.Nil || body.ProtocolCode != "HANDLER_V2" && body.ProtocolCode != "GOVERNED_V2" || len(body.Required) != 1 || body.Required[0] != unitID || !strings.Contains(string(body.Snapshot), `"evaluator_version":"SAFE_PARAMETER_MAP_V1"`) || !strings.Contains(string(body.Snapshot), `"expression":`) {
				http.Error(w, `{"code":"BILLING_CONFLICT"}`, http.StatusConflict)
				return
			}
			digest := sha256.Sum256(body.Snapshot)
			digestText := hex.EncodeToString(digest[:])
			bindingMutex.Lock()
			receipt, exists := bindingReceipts[formulaID]
			if exists && (bindingSnapshots[formulaID] != digestText || receipt.ProtocolCode != body.ProtocolCode || !uuidSlicesEqual(receipt.RequiredMeteringUnitDefinitionIDs, body.Required)) {
				bindingMutex.Unlock()
				http.Error(w, `{"code":"BILLING_CONFLICT"}`, http.StatusConflict)
				return
			}
			if !exists {
				receipt = FormulaBindingReceipt{FormulaVersionID: formulaID, Status: "REGISTERED", ProtocolCode: body.ProtocolCode, RequiredMeteringUnitDefinitionIDs: append([]uuid.UUID(nil), body.Required...), FormulaSnapshotSHA256: digestText, PublishedAt: billingPublishedAt}
				bindingReceipts[formulaID], bindingSnapshots[formulaID] = receipt, digestText
			}
			bindingMutex.Unlock()
			if mode.Load() == 7 {
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("test server does not support response-loss injection")
				}
				connection, _, err := hijacker.Hijack()
				if err != nil {
					t.Fatal(err)
				}
				_ = connection.Close()
				return
			}
			_ = json.NewEncoder(w).Encode(receipt)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func newProtocolFormulaIntegrationHandler(t *testing.T, repository Repository, billingURL string, subjectID uuid.UUID, activation TokenCommandGovernanceActivationReader, allowed bool) http.Handler {
	t.Helper()
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Authorization-Version", "1")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed, "user_id": subjectID, "granted_permissions": []string{"platform.pricing.manage"}})
	}))
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "integration-service-token")
	if err != nil {
		t.Fatal(err)
	}
	billingClient, err := NewHTTPBillingClient(billingURL, "integration-service-token")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, nil, nil, nil, ProviderURLPolicy{}, billingClient, nil, 0).WithAuthorizer(authorizer)
	if activation != nil {
		service.WithTokenCommandGovernanceActivation(activation)
	}
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.integration.example", Subject: subjectID.String(), SessionID: "integration-session", AuthenticatedAt: time.Now().UTC()}, nil
	}
	return service.Handler("", verify)
}

func performProtocolFormulaCommand(handler http.Handler, method, path string, body any, idempotencyKey string, timeout time.Duration) *httptest.ResponseRecorder {
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	if timeout > 0 {
		ctx, cancel := context.WithTimeout(request.Context(), timeout)
		defer cancel()
		request = request.WithContext(ctx)
	}
	request.Header.Set("Authorization", "Bearer integration")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func protocolFormulaFactCounts(t *testing.T, repository *PostgresRepository, ctx context.Context) (int, int) {
	t.Helper()
	var audit, outbox int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_audit_logs`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1'`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	return audit, outbox
}

func assertProtocolFormulaWrites(t *testing.T, repository *PostgresRepository, ctx context.Context, protocolCode string, protocols, audits, outbox int) {
	t.Helper()
	var actualProtocols, actualAudits, actualOutbox int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM protocol_definitions WHERE protocol_code=$1`, protocolCode).Scan(&actualProtocols); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_audit_logs WHERE action IN ('TOKEN_PROTOCOL_DRAFT_CREATE','TOKEN_PROTOCOL_PUBLISH','TOKEN_FORMULA_DRAFT_CREATE','TOKEN_FORMULA_PUBLISH')`).Scan(&actualAudits); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1'`).Scan(&actualOutbox); err != nil {
		t.Fatal(err)
	}
	if actualProtocols != protocols || actualAudits != audits || actualOutbox != outbox {
		t.Fatalf("protocol=%d audit=%d outbox=%d, want %d/%d/%d", actualProtocols, actualAudits, actualOutbox, protocols, audits, outbox)
	}
}
