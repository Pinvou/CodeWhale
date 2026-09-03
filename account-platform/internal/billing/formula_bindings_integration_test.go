package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFormulaDraftUnitValidationPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	if testing.Short() && os.Getenv("PINVOU_TEST_FORMULA_VALIDATION_GATE") != "1" {
		t.Skip("formula validation PostgreSQL gate is run explicitly before the short race suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "formula_validation_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = connection.Exec(context.Background(), `DROP SCHEMA `+identifier+` CASCADE`) })
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.ConnConfig.RuntimeParams = map[string]string{"search_path": schema}
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }}
	activeID, secondActiveID, concurrentUnitID, inactiveID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO metering_unit_definitions(id,unit_code,version,minimum_precision,price_denominator,rounding_rule,status,created_at,deactivated_at) VALUES($1,'INPUT_TOKEN',7,1,1000000,'CEILING','ACTIVE',now(),NULL),($2,'CACHED_INPUT_TOKEN',2,1,1000000,'CEILING','ACTIVE',now(),NULL),($3,'REQUEST',4,1,1,'CEILING','ACTIVE',now(),NULL),($4,'OUTPUT_TOKEN',3,1,1000000,'CEILING','INACTIVE',now(),now())`, activeID, secondActiveID, concurrentUnitID, inactiveID); err != nil {
		t.Fatal(err)
	}
	result, err := repository.ValidateFormulaDraftUnits(ctx, map[string]uuid.UUID{"INPUT_TOKEN": activeID})
	if err != nil || result.VerifiedCount != 1 || len(result.UnitVersions) != 1 || result.UnitVersions[0].UnitDefinitionID != activeID || result.UnitVersions[0].Version != 7 || result.UnitVersions[0].Status != "ACTIVE" {
		t.Fatalf("unexpected authoritative validation: %#v err=%v", result, err)
	}
	handler := NewService(repository).Handler("billing-formula-validation-test-token")
	payload, _ := json.Marshal(map[string]any{"required_unit_versions": map[string]uuid.UUID{"INPUT_TOKEN": activeID}})
	unauthorized := httptest.NewRequest(http.MethodPost, "/internal/v1/pricing/formula-bindings/validate", bytes.NewReader(payload))
	unauthorized.Header.Set("Content-Type", "application/json")
	unauthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("internal validation accepted a request without service auth: %d %s", unauthorizedResponse.Code, unauthorizedResponse.Body.String())
	}
	authorized := httptest.NewRequest(http.MethodPost, "/internal/v1/pricing/formula-bindings/validate", bytes.NewReader(payload))
	authorized.Header.Set("Authorization", "Bearer billing-formula-validation-test-token")
	authorized.Header.Set("Content-Type", "application/json")
	authorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(authorizedResponse, authorized)
	if authorizedResponse.Code != http.StatusOK || !strings.Contains(authorizedResponse.Body.String(), `"verified_count":1`) {
		t.Fatalf("service-authenticated validation failed: %d %s", authorizedResponse.Code, authorizedResponse.Body.String())
	}
	var bindingFacts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM billing_protocol_formula_bindings`).Scan(&bindingFacts); err != nil || bindingFacts != 0 {
		t.Fatalf("read-only validation wrote Billing facts: count=%d err=%v", bindingFacts, err)
	}
	for name, required := range map[string]map[string]uuid.UUID{
		"wrong code":   {"OUTPUT_TOKEN": activeID},
		"inactive":     {"OUTPUT_TOKEN": inactiveID},
		"missing":      {"INPUT_TOKEN": uuid.New()},
		"duplicate id": {"INPUT_TOKEN": activeID, "OUTPUT_TOKEN": activeID},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := repository.ValidateFormulaDraftUnits(ctx, required); !errors.Is(err, ErrPriceMissing) {
				t.Fatalf("invalid reference must fail closed: %v", err)
			}
		})
	}

	formulaID := uuid.New()
	snapshot := json.RawMessage(`{"evaluator_version":"SAFE_PARAMETER_MAP_V1","expression":{"INPUT_TOKEN":"estimated_input_tokens","CACHED_INPUT_TOKEN":"cached_input_tokens"}}`)
	bindingPayload, _ := json.Marshal(map[string]any{"protocol_code": "CHAT_V2", "required_unit_definition_ids": []uuid.UUID{secondActiveID, activeID}, "formula_snapshot": snapshot})
	register := func(body []byte) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPut, "/internal/v1/pricing/formula-bindings/"+formulaID.String(), bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer billing-formula-validation-test-token")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	first := register(bindingPayload)
	var firstReceipt FormulaBindingReceipt
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &firstReceipt) != nil || firstReceipt.FormulaVersionID != formulaID || firstReceipt.Status != "REGISTERED" || firstReceipt.ProtocolCode != "CHAT_V2" || len(firstReceipt.RequiredMeteringUnitDefinitionIDs) != 2 || firstReceipt.RequiredMeteringUnitDefinitionIDs[0].String() > firstReceipt.RequiredMeteringUnitDefinitionIDs[1].String() || len(firstReceipt.FormulaSnapshotSHA256) != 64 || firstReceipt.PublishedAt.IsZero() {
		t.Fatalf("incomplete immutable Billing receipt: %d %#v body=%s", first.Code, firstReceipt, first.Body.String())
	}
	var databasePublishedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT published_at FROM billing_protocol_formula_bindings WHERE formula_version_id=$1`, formulaID).Scan(&databasePublishedAt); err != nil || !databasePublishedAt.Equal(firstReceipt.PublishedAt) {
		t.Fatalf("receipt does not use database published_at: db=%v receipt=%v err=%v", databasePublishedAt, firstReceipt.PublishedAt, err)
	}
	if _, err := repository.DeactivateMeteringUnit(ctx, activeID); err != nil {
		t.Fatal(err)
	}
	retry := register(bindingPayload)
	var retryReceipt FormulaBindingReceipt
	if retry.Code != http.StatusOK || json.Unmarshal(retry.Body.Bytes(), &retryReceipt) != nil || retryReceipt.FormulaVersionID != firstReceipt.FormulaVersionID || retryReceipt.Status != firstReceipt.Status || retryReceipt.ProtocolCode != firstReceipt.ProtocolCode || !uuidListsEqual(retryReceipt.RequiredMeteringUnitDefinitionIDs, firstReceipt.RequiredMeteringUnitDefinitionIDs) || retryReceipt.FormulaSnapshotSHA256 != firstReceipt.FormulaSnapshotSHA256 || !retryReceipt.PublishedAt.Equal(firstReceipt.PublishedAt) {
		t.Fatalf("retry after unit deactivation did not return original receipt: %d %#v body=%s", retry.Code, retryReceipt, retry.Body.String())
	}
	conflictingPayload, _ := json.Marshal(map[string]any{"protocol_code": "CHAT_V2", "required_unit_definition_ids": []uuid.UUID{secondActiveID, activeID}, "formula_snapshot": map[string]any{"evaluator_version": "SAFE_PARAMETER_MAP_V1", "expression": map[string]string{"INPUT_TOKEN": "request_count", "CACHED_INPUT_TOKEN": "cached_input_tokens"}}})
	conflict := register(conflictingPayload)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("different content reused formula ID: %d %s", conflict.Code, conflict.Body.String())
	}

	concurrentFormulaID := uuid.New()
	concurrentSnapshot := json.RawMessage(`{"evaluator_version":"SAFE_PARAMETER_MAP_V1","expression":{"REQUEST":"request_count"}}`)
	start := make(chan struct{})
	var wait sync.WaitGroup
	var registrationReceipt FormulaBindingReceipt
	var registrationErr, deactivationErr error
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		registrationReceipt, registrationErr = repository.RegisterFormulaBinding(ctx, concurrentFormulaID, "CHAT_V2", []uuid.UUID{concurrentUnitID}, concurrentSnapshot)
	}()
	go func() {
		defer wait.Done()
		<-start
		_, deactivationErr = repository.DeactivateMeteringUnit(ctx, concurrentUnitID)
	}()
	close(start)
	wait.Wait()
	if deactivationErr != nil || registrationErr != nil && !errors.Is(registrationErr, ErrPriceMissing) {
		t.Fatalf("registration/deactivation did not serialize to a legal result: receipt=%#v registration=%v deactivation=%v", registrationReceipt, registrationErr, deactivationErr)
	}
	var concurrentBindings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM billing_protocol_formula_bindings WHERE formula_version_id=$1`, concurrentFormulaID).Scan(&concurrentBindings); err != nil || registrationErr == nil && concurrentBindings != 1 || errors.Is(registrationErr, ErrPriceMissing) && concurrentBindings != 0 {
		t.Fatalf("serialized registration fact mismatch: count=%d registration=%v query=%v", concurrentBindings, registrationErr, err)
	}
}
