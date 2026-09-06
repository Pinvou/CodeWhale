package token

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestGovernedFormulaUnitMetadataIsContextBound(t *testing.T) {
	id := uuid.NewString()
	for _, unit := range []string{"INPUT_TOKEN", "CACHED_INPUT_TOKEN", "OUTPUT_TOKEN", " input_token ", "REQUEST"} {
		pair := map[string]any{"required_unit_versions": map[string]string{unit: id}, "expression": map[string]string{unit: "estimated_input_tokens"}}
		for _, value := range []any{pair, map[string]any{"before_state": []any{pair}}} {
			raw, _ := json.Marshal(value)
			if containsSensitiveCommandField(raw) {
				t.Fatalf("safe formula unit %q rejected", unit)
			}
		}
	}
	bad := []any{
		map[string]any{"INPUT_TOKEN": id},
		map[string]any{"expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": id}},
		map[string]any{"Required_Unit_Versions": map[string]string{"INPUT_TOKEN": id}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": uuid.Nil.String()}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": "a-private-value"}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": id}, "expression": map[string]string{"INPUT_TOKEN": "eval(private_input)"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": id}, "expression": map[string]string{"OUTPUT_TOKEN": "estimated_input_tokens"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": id, "input_token": id}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens", "input_token": "request_count"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": id}, "expression": map[string]any{"INPUT_TOKEN": map[string]string{"secret": "hidden"}}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": id}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}, "nested": []any{map[string]string{"accessToken": "private"}}},
	}
	for _, key := range []string{"ACCESS_TOKEN", "api_key", "private_key", "SESSION_TOKEN", "INPUT_TOKEN_SECRET", "cookie"} {
		bad = append(bad, map[string]any{"required_unit_versions": map[string]string{key: id}, "expression": map[string]string{key: "request_count"}})
	}
	for index, value := range bad {
		raw, _ := json.Marshal(value)
		if !containsSensitiveCommandField(raw) {
			t.Fatalf("unsafe formula context %d accepted", index)
		}
	}
	for _, key := range []string{"INPUT_TOKEN", "CACHED_INPUT_TOKEN", "OUTPUT_TOKEN", "access_token", "api_key"} {
		if !sensitiveTokenGovernanceFieldName(normalizeTokenSensitiveFieldName(key)) {
			t.Fatalf("global sensitive-field rule was relaxed for %s", key)
		}
	}
}

// Actual governed HTTP and Token PostgreSQL; Billing metadata/receipt is the
// existing bounded fixture. No dependency on unpublished APT-138 production.
func TestGovernedFormulaMeteringUnitPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, tokenPaginationIntegrationDatabaseURL(t))
	repository.now = func() time.Time { return time.Date(2026, 9, 5, 8, 0, 0, 0, time.UTC) }
	actor, unitID := uuid.New(), uuid.New()
	var mode, billingPuts atomic.Int32
	billing := newProtocolFormulaBillingServer(t, unitID, &mode, &billingPuts)
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error { return nil }))
	activation := tokenCommandActivationReaderFunc(func(context.Context) (TokenCommandGovernanceActivation, error) {
		return TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 9}, nil
	})
	handler := httpserver.CorrelationHandler(newProtocolFormulaIntegrationHandler(t, repository, billing.URL, actor, activation, true))
	protocol := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_PROTOCOL_DRAFT_CREATE", "PROTOCOL", "GOVERNED_V2", map[string]any{"protocol_code": "GOVERNED_V2", "request_schema": map[string]any{"type": "object", "parameters": []any{map[string]any{"name": "prompt"}}}}, "safe-metering-protocol-draft")
	p := decodeGovernedRedacted(t, protocol)
	protocolID := uuid.MustParse(p["protocol_definition_id"].(string))
	executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_PROTOCOL_PUBLISH", "PROTOCOL", protocolID.String(), map[string]any{"expected_protocol_version": 1, "draft_etag": p["etag"]}, "safe-metering-protocol-publish")
	command := map[string]any{"required_unit_versions": map[string]uuid.UUID{"INPUT_TOKEN": unitID}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}}
	formula := executeProtocolFormulaGovernedHTTP(t, handler, "TOKEN_FORMULA_DRAFT_CREATE", "FORMULA", protocolID.String(), command, "safe-metering-formula-draft")
	f := decodeGovernedRedacted(t, formula)
	formulaID := uuid.MustParse(f["formula_version_id"].(string))
	publishEnvelope := governedProtocolFormulaHTTPCommand(t, handler, "TOKEN_FORMULA_PUBLISH", "FORMULA", formulaID.String(), map[string]any{"expected_formula_version": 1, "draft_etag": f["etag"]}, "safe-metering-formula-publish")
	response := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", publishEnvelope, "safe-metering-formula-publish", 0)
	var published TokenCommandResult
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &published) != nil || published.Status != "SUCCEEDED" {
		t.Fatalf("formula publish=%d %s", response.Code, response.Body.String())
	}
	var status string
	var required, expression json.RawMessage
	var audits, outbox int
	if err := repository.pool.QueryRow(ctx, `SELECT status,required_unit_versions,expression,(SELECT count(*) FROM token_audit_logs WHERE action='TOKEN_FORMULA_PUBLISH' AND summary->>'command_reference'=($2::uuid)::text),(SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1' AND payload->>'command_reference'=($2::uuid)::text) FROM protocol_formula_versions WHERE id=$1`, formulaID, published.CommandReference).Scan(&status, &required, &expression, &audits, &outbox); err != nil || status != "PUBLISHED" || audits != 1 || outbox != 1 || !strings.Contains(string(required), "INPUT_TOKEN") || !strings.Contains(string(expression), "estimated_input_tokens") {
		t.Fatalf("actual formula status=%s audit=%d outbox=%d err=%v", status, audits, outbox, err)
	}
	before := formulaUnitRemediationFacts(t, ctx, repository)
	puts := billingPuts.Load()
	replayed := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands", publishEnvelope, "safe-metering-formula-publish", 0)
	var replay TokenCommandResult
	if replayed.Code != http.StatusOK || json.Unmarshal(replayed.Body.Bytes(), &replay) != nil || replay.CommandReference != published.CommandReference || billingPuts.Load() != puts || formulaUnitRemediationFacts(t, ctx, repository) != before {
		t.Fatalf("publish replay changed facts: %d %s", replayed.Code, replayed.Body.String())
	}
	for _, field := range []string{"access_token", "api_key", "INPUT_TOKEN_SECRET"} {
		unsafe := map[string]any{"required_unit_versions": map[string]uuid.UUID{field: unitID}, "expression": map[string]string{field: "request_count"}}
		denied := performProtocolFormulaCommand(handler, http.MethodPost, "/admin/v1/commands:precheck", map[string]any{"command_type": "TOKEN_FORMULA_DRAFT_CREATE", "target_resource_type": "FORMULA", "target_resource_id": protocolID, "command": unsafe}, "unsafe-formula-metadata", 0)
		if denied.Code != http.StatusBadRequest || formulaUnitRemediationFacts(t, ctx, repository) != before {
			t.Fatalf("unsafe metadata %s status=%d body=%s", field, denied.Code, denied.Body.String())
		}
	}
}

func formulaUnitRemediationFacts(t *testing.T, ctx context.Context, repository *PostgresRepository) string {
	t.Helper()
	var facts string
	if err := repository.pool.QueryRow(ctx, `SELECT jsonb_build_object('formula',(SELECT jsonb_agg(to_jsonb(f) ORDER BY id) FROM protocol_formula_versions f),'commands',(SELECT jsonb_agg(to_jsonb(c) ORDER BY command_reference) FROM token_command_results c),'audit',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM token_audit_logs a),'outbox',(SELECT jsonb_agg(to_jsonb(o) ORDER BY event_id) FROM outbox_events o))::text`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	return facts
}
