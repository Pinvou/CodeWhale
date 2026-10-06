package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func TestPostgresFormulaBindingMatchesAuthoritativeUnitCodes(t *testing.T) {
	ctx := context.Background()
	repo := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	input := uuid.MustParse("00000000-0000-7000-8100-000000000001")
	second := uuid.New()
	if _, err := repo.pool.Exec(ctx, `INSERT INTO metering_unit_definitions(id,unit_code,version,minimum_precision,price_denominator,rounding_rule,status,created_at) VALUES($1,'INPUT_TOKEN',2,1,1000000,'CEILING','ACTIVE',now())`, second); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		ids        []uuid.UUID
		expression map[string]string
	}{
		{"unknown-code", []uuid.UUID{input}, map[string]string{"FAKE_UNIT": "request_count"}},
		{"missing-expression", []uuid.UUID{input}, map[string]string{}},
		{"multiple-versions-same-code", []uuid.UUID{input, second}, map[string]string{"INPUT_TOKEN": "estimated_input_tokens", "FAKE_UNIT": "request_count"}},
		{"duplicate-id", []uuid.UUID{input, input}, map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id := uuid.New()
			unitMap := map[string]uuid.UUID{}
			for code := range test.expression {
				unitMap[code] = input
			}
			snapshot, _ := json.Marshal(map[string]any{"evaluator_version": "SAFE_PARAMETER_MAP_V1", "expression": test.expression, "required_unit_versions": unitMap})
			err := repo.RegisterFormulaBinding(ctx, id, "CHAT", test.ids, snapshot, time.Now().UTC())
			if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrPriceMissing) {
				t.Fatalf("invalid unit binding accepted: %v", err)
			}
			var count int
			if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM billing_protocol_formula_bindings WHERE formula_version_id=$1`, id).Scan(&count); err != nil || count != 0 {
				t.Fatalf("invalid binding persisted: %d %v", count, err)
			}
		})
	}
	for _, kind := range []string{"swapped-input-output", "same-code-wrong-version"} {
		t.Run(kind, func(t *testing.T) {
			output := uuid.MustParse("00000000-0000-7000-8100-000000000003")
			ids := []uuid.UUID{input, output}
			unitMap := map[string]uuid.UUID{"INPUT_TOKEN": output, "OUTPUT_TOKEN": input}
			if kind == "same-code-wrong-version" {
				ids = []uuid.UUID{second, output}
				unitMap = map[string]uuid.UUID{"INPUT_TOKEN": input, "OUTPUT_TOKEN": output}
			}
			snapshot, _ := json.Marshal(map[string]any{"evaluator_version": "SAFE_PARAMETER_MAP_V1", "required_unit_versions": unitMap, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens", "OUTPUT_TOKEN": "max_output_tokens"}})
			id := uuid.New()
			if err := repo.RegisterFormulaBinding(ctx, id, "CHAT", ids, snapshot, time.Now().UTC()); !errors.Is(err, ErrConflict) {
				t.Fatalf("wrong unit version accepted: %v", err)
			}
			var count int
			if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM billing_protocol_formula_bindings WHERE formula_version_id=$1`, id).Scan(&count); err != nil || count != 0 {
				t.Fatalf("bad map persisted: %d %v", count, err)
			}
		})
	}
	t.Run("valid-replay-after-unit-inactive", func(t *testing.T) {
		id := uuid.New()
		at := time.Now().UTC()
		snapshot := json.RawMessage(`{"evaluator_version":"SAFE_PARAMETER_MAP_V1","required_unit_versions":{"INPUT_TOKEN":"00000000-0000-7000-8100-000000000001"},"expression":{"INPUT_TOKEN":"estimated_input_tokens"}}`)
		if err := repo.RegisterFormulaBinding(ctx, id, "CHAT", []uuid.UUID{input}, snapshot, at); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.DeactivateMeteringUnit(ctx, input); err != nil {
			t.Fatal(err)
		}
		if err := repo.RegisterFormulaBinding(ctx, id, "CHAT", []uuid.UUID{input}, snapshot, at); err != nil {
			t.Fatalf("immutable binding replay rejected: %v", err)
		}
	})
}

func TestPostgresFormulaBindingRetainsExactNumericFacts(t *testing.T) {
	ctx := context.Background()
	repo := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	handler := NewService(repo).Handler("controlled-formula-service-proof")
	input := uuid.MustParse("00000000-0000-7000-8100-000000000001")
	for _, example := range []struct{ name, first, same, changed string }{
		{"large-integer", "9007199254740992", "9007199254740992.0", "9007199254740993"},
		{"exponent", "1e3", "1000", "1001"},
		{"decimal", "1.0", "1", "1.00000000000000000001"},
	} {
		t.Run(example.name, func(t *testing.T) {
			id := uuid.New()
			at := time.Now().UTC().Format(time.RFC3339Nano)
			invoke := func(number string) int {
				snapshot := `{"evaluator_version":"SAFE_PARAMETER_MAP_V1","required_unit_versions":{"INPUT_TOKEN":"` + input.String() + `"},"expression":{"INPUT_TOKEN":"estimated_input_tokens"},"numeric_fact":` + number + `}`
				body := `{"protocol_code":"CHAT","required_unit_definition_ids":["` + input.String() + `"],"formula_snapshot":` + snapshot + `,"published_at":"` + at + `"}`
				request := httptest.NewRequest(http.MethodPut, "/internal/v1/pricing/formula-bindings/"+id.String(), strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer controlled-formula-service-proof")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				return response.Code
			}
			if status := invoke(example.first); status != http.StatusOK {
				t.Fatalf("initial binding: %d", status)
			}
			if status := invoke(example.same); status != http.StatusOK {
				t.Fatalf("same exact numeric fact: %d", status)
			}
			if status := invoke(example.changed); status != http.StatusConflict {
				t.Fatalf("changed numeric fact accepted: %d", status)
			}
			var retained bool
			if err := repo.pool.QueryRow(ctx, `SELECT (formula_snapshot->>'numeric_fact')::numeric=$2::numeric FROM billing_protocol_formula_bindings WHERE formula_version_id=$1`, id, example.first).Scan(&retained); err != nil || !retained {
				t.Fatalf("immutable numeric fact changed: %v %v", retained, err)
			}
		})
	}
}
