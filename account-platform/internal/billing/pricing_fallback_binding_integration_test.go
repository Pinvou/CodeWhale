package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestPriceFallbackMissingBindingPostgres(t *testing.T) {
	repo, ctx := retainedFinanceRepository(t)
	fixed := time.Date(2026, 10, 29, 12, 0, 0, 0, shanghaiLocation)
	repo.now = func() time.Time { return fixed }
	actor, price, formula := uuid.New(), uuid.New(), uuid.New()
	model := "controlled-missing-binding-" + uuid.NewString()
	// Deliberately damaged historical input, NOT a normal CreatePriceDraft
	// producer. Keep all production schema/roles/immutable guards installed;
	// never delete a published binding or disable its trigger to simulate loss.
	if _, err := repo.pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,version,space_etag) VALUES($1,$2,1,1,1,$3,$3,'MODEL',$2,'CHAT',$4,'MICROCREDIT','DRAFT',1,1)`, price, model, fixed.Add(-time.Hour), formula); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO pricing_version_unit_configs(price_version_id,unit_definition_id,config_type,fixed_price_per_denominator,created_at) SELECT $1,id,'FIXED_PRICE',1,$2 FROM metering_unit_definitions WHERE unit_code IN ('INPUT_TOKEN','CACHED_INPUT_TOKEN','OUTPUT_TOKEN') AND version=1`, price, fixed); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE pricing_versions SET lifecycle_status='ACTIVE',published_at=effective_from WHERE id=$1`, price); err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Bool
	allowed.Store(true)
	handler := priceFallbackBindingHandler(t, repo, &allowed)
	before := priceFallbackBindingSnapshot(t, repo, ctx)
	call := func(id uuid.UUID, key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/pricing/"+id.String()+"/transition", strings.NewReader(`{"action":"END","mode":"FALLBACK_GLOBAL","reason":"controlled fallback validation"}`))
		request.Header.Set("Authorization", "Bearer "+actor.String())
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	assertFailure := func(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		var body struct{ Code, Message string }
		if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != status || body.Code != code {
			t.Fatalf("status=%d body=%s; want %d/%s", response.Code, response.Body.String(), status, code)
		}
		if strings.Contains(body.Message, formula.String()) || strings.Contains(body.Message, "SQLSTATE") || strings.Contains(body.Message, "permission denied") {
			t.Fatal("raw dependency failure escaped the registered handler")
		}
	}
	assertUnchanged := func(t *testing.T) {
		t.Helper()
		if !reflect.DeepEqual(before, priceFallbackBindingSnapshot(t, repo, ctx)) {
			t.Fatal("failed fallback changed lifecycle, prices, ETags, units, blocks, audit or receipts")
		}
	}
	t.Run("missing-binding-and-rollback", func(t *testing.T) {
		key := uuid.NewString()
		assertFailure(t, call(price, key), http.StatusUnprocessableEntity, "PRICE_MISSING")
		_, err := repo.TransitionPriceVersion(ctx, price, actor, "END", "FALLBACK_GLOBAL", "controlled fallback validation", nil, key)
		if !errors.Is(err, ErrPriceMissing) || errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("missing binding classification: %v", err)
		}
		assertUnchanged(t)
	})
	t.Run("permission-denied-before-mutation", func(t *testing.T) {
		allowed.Store(false)
		defer allowed.Store(true)
		assertFailure(t, call(price, uuid.NewString()), http.StatusForbidden, "PERMISSION_DENIED")
		assertUnchanged(t)
	})
	t.Run("binding-read-fault-and-later-retry", func(t *testing.T) {
		// Table ACL is local to this owned UUID database; no role/global or
		// shared database privileges are changed. Runtime still billing_service.
		if _, err := retainedFinanceOwnerExec(t, repo, ctx, `REVOKE SELECT ON billing_protocol_formula_bindings FROM billing_service`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := retainedFinanceOwnerExec(t, repo, ctx, `GRANT SELECT ON billing_protocol_formula_bindings TO billing_service`); err != nil {
				t.Error(err)
			}
		}()
		key := uuid.NewString()
		assertFailure(t, call(price, key), http.StatusInternalServerError, "INTERNAL_ERROR")
		assertUnchanged(t)
		if _, err := retainedFinanceOwnerExec(t, repo, ctx, `GRANT SELECT ON billing_protocol_formula_bindings TO billing_service`); err != nil {
			t.Fatal(err)
		}
		assertFailure(t, call(price, key), http.StatusUnprocessableEntity, "PRICE_MISSING")
		assertUnchanged(t)
	})
	t.Run("valid-binding-fallback-and-receipt-replay", func(t *testing.T) {
		from := time.Date(2026, 10, 30, 0, 0, 0, 0, shanghaiLocation)
		one := int64(1)
		command := PriceDraftCommand{ScopeType: "GLOBAL", ProtocolCode: "CHAT", FormulaVersionID: chatFormulaVersionID, CreditUnit: "MICROCREDIT", EffectiveFrom: from, ExpectedETag: 1}
		for i := 1; i <= 3; i++ {
			command.Units = append(command.Units, PriceUnitConfiguration{UnitDefinitionID: uuid.MustParse(fmt.Sprintf("00000000-0000-7000-8100-%012d", i)), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &one})
		}
		for _, scope := range []string{"GLOBAL", "MODEL"} {
			command.ScopeType = scope
			if scope == "MODEL" {
				m := "controlled-valid-binding-" + uuid.NewString()
				command.ScopeID = &m
			}
			draft, err := repo.CreatePriceDraft(ctx, command, actor, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			published, err := repo.PublishPriceVersion(ctx, draft.PriceVersionID, actor, 1, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			if scope == "MODEL" {
				price = published.PriceVersionID
			}
		}
		repo.now = func() time.Time { return from.Add(time.Hour) }
		key := uuid.NewString()
		response := call(price, key)
		if response.Code != http.StatusOK {
			t.Fatalf("valid fallback: %d %s", response.Code, response.Body.String())
		}
		var result struct {
			LifecycleStatus string     `json:"lifecycle_status"`
			Successor       *uuid.UUID `json:"successor_price_version_id"`
		}
		if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.LifecycleStatus != "ENDED" || result.Successor == nil {
			t.Fatalf("missing confirmed fallback result: %s", response.Body.String())
		}
		after := priceFallbackBindingSnapshot(t, repo, ctx)
		replay := call(price, key)
		if replay.Code != http.StatusOK || replay.Body.String() != response.Body.String() || !reflect.DeepEqual(after, priceFallbackBindingSnapshot(t, repo, ctx)) {
			t.Fatal("confirmed fallback replay changed response or durable effects")
		}
	})
}

func priceFallbackBindingSnapshot(t *testing.T, repo *PostgresRepository, ctx context.Context) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, table := range []string{"pricing_versions", "pricing_version_unit_configs", "pricing_space_versions", "pricing_blocks", "pricing_audits", "pricing_lifecycle_commands"} {
		var value string
		if err := repo.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(item) ORDER BY to_jsonb(item)::text),'[]'::jsonb)::text FROM `+table+` item`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		result[table] = value
	}
	return result
}

func priceFallbackBindingHandler(t *testing.T, repo *PostgresRepository, allowed *atomic.Bool) http.Handler {
	t.Helper()
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Subject    string `json:"subject"`
			Permission string `json:"permission"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/authorization/check" || json.NewDecoder(r.Body).Decode(&input) != nil {
			t.Error("unexpected controlled authorization protocol")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed.Load() && input.Permission == "platform.pricing.manage", "user_id": input.Subject})
	}))
	t.Cleanup(authority.Close)
	authorizer, err := authorization.NewClient(authority.URL, "controlled-pricing-service")
	if err != nil {
		t.Fatal(err)
	}
	return NewService(repo).WithAuthorizer(authorizer).Handler("controlled-pricing-service", func(_ context.Context, token string) (identity.Principal, error) {
		if _, err := uuid.Parse(token); err != nil {
			return identity.Principal{}, err
		}
		return identity.Principal{Issuer: "controlled", Subject: token, AuthenticatedAt: repo.now()}, nil
	})
}
