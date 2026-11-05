package token

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestGovernedRouteCommandClosedPayloadAndTarget(t *testing.T) {
	account, model, protocol, route := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, item := range []struct {
		kind, target string
		payload      map[string]any
	}{
		{"TOKEN_ROUTE_UPSERT", account.String(), map[string]any{"token_account_id": account, "model_id": model, "protocol_version_id": protocol, "priority": 100, "default_scope": "NONE"}},
		{"TOKEN_ROUTE_DELETE", route.String(), map[string]any{"token_account_id": account, "route_id": route}},
	} {
		raw := mustMarshalJSON(t, item.payload)
		if _, err := parseGovernedRouteCommand(item.kind, item.target, raw); err != nil {
			t.Fatal(err)
		}
		if _, err := parseGovernedRouteCommand(item.kind, uuid.NewString(), raw); !errors.Is(err, ErrTokenCommandInvalid) {
			t.Fatal("mismatched target accepted")
		}
		for _, field := range []string{"health_status", "upstream_model_ref", "provider", "reason", "expected_account_version", "ignore_blocker"} {
			altered := map[string]any{}
			for key, value := range item.payload {
				altered[key] = value
			}
			altered[field] = "HEALTHY"
			if _, err := parseGovernedRouteCommand(item.kind, item.target, mustMarshalJSON(t, altered)); !errors.Is(err, ErrTokenCommandInvalid) {
				t.Fatalf("%s accepted forbidden %s", item.kind, field)
			}
		}
		for field := range item.payload {
			altered := map[string]any{}
			for key, value := range item.payload {
				if key != field {
					altered[key] = value
				}
			}
			if _, err := parseGovernedRouteCommand(item.kind, item.target, mustMarshalJSON(t, altered)); !errors.Is(err, ErrTokenCommandInvalid) {
				t.Fatalf("%s missing %s accepted", item.kind, field)
			}
		}
	}
}

func TestGovernedRouteEnvelopeBindsExpectedVersionsAndActionParameters(t *testing.T) {
	account, model, protocol := uuid.New(), uuid.New(), uuid.New()
	command := mustMarshalJSON(t, map[string]any{"token_account_id": account, "model_id": model, "protocol_version_id": protocol, "priority": 100, "default_scope": "NONE"})
	expected := map[string]any{"token_account_id": account, "model_id": model, "protocol_version_id": protocol, "expected_account_version": 1, "expected_route_version": 0}
	precheck := TokenCommandPrecheckResult{ImpactPrecheckResult: ImpactPrecheckResult{PrecheckFingerprint: strings.Repeat("a", 64)}, ExpectedVersions: mustMarshalJSON(t, expected)}
	envelope := modelAccountEnvelope(t, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), command, "configure the governed route", precheck)
	if _, _, _, _, err := normalizeTokenCommand(envelope, "governed-route-unit-key", ""); err != nil {
		t.Fatal(err)
	}
	for _, alteration := range []func(*TokenCommandEnvelope){
		func(e *TokenCommandEnvelope) { e.PrecheckFingerprint = strings.Repeat("b", 64) },
		func(e *TokenCommandEnvelope) {
			e.Command = json.RawMessage(strings.Replace(string(e.Command), `"priority":100`, `"priority":101`, 1))
		},
		func(e *TokenCommandEnvelope) {
			e.ExpectedVersions = json.RawMessage(strings.Replace(string(e.ExpectedVersions), `"expected_account_version":1`, `"expected_account_version":2`, 1))
		},
	} {
		changed := envelope
		alteration(&changed)
		if _, _, _, _, err := normalizeTokenCommand(changed, "governed-route-unit-key", ""); !errors.Is(err, ErrTokenCommandInvalid) {
			t.Fatalf("tampered envelope accepted: %s", changed.Command)
		}
	}
}
