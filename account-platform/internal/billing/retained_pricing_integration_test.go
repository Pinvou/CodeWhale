package billing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// The retained role fixture installs the whole current public migration set
// under isolated migrator/runtime actors in an owned UUID database. HTTP here
// uses controlled test service credentials, not a production identity issuer.
func TestRetainedF04PricingFirstDelivery(t *testing.T) {
	repository, ctx := retainedFinanceRepository(t)
	unit := uuid.MustParse("00000000-0000-7000-8100-000000000005")
	formula := uuid.New()
	first, err := repository.RegisterFormulaBinding(ctx, formula, "REQUEST", []uuid.UUID{unit}, json.RawMessage(`{"large":9007199254740992,"decimal":1e0}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("large-integer-distinct-replay-rejected", func(t *testing.T) {
		_, err := repository.RegisterFormulaBinding(ctx, formula, "REQUEST", []uuid.UUID{unit}, json.RawMessage(`{"large":9007199254740993,"decimal":1}`))
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("different exact formula accepted: %v", err)
		}
	})
	t.Run("equivalent-decimal-replay-retains-original-receipt", func(t *testing.T) {
		got, err := repository.RegisterFormulaBinding(ctx, formula, "REQUEST", []uuid.UUID{unit}, json.RawMessage(`{"decimal":1.000,"large":9007199254740992}`))
		if err != nil || !reflect.DeepEqual(got, first) {
			t.Fatalf("equivalent replay changed original receipt: %v", err)
		}
	})
	t.Run("all-other-binding-facts-remain-strict", func(t *testing.T) {
		for _, change := range []struct {
			protocol string
			units    []uuid.UUID
		}{{"OTHER", []uuid.UUID{unit}}, {"REQUEST", []uuid.UUID{uuid.MustParse("00000000-0000-7000-8100-000000000004")}}} {
			_, err := repository.RegisterFormulaBinding(ctx, formula, change.protocol, change.units, json.RawMessage(`{"large":9007199254740992,"decimal":1}`))
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("other binding fact accepted: %v", err)
			}
		}
	})
	t.Run("competing-first-bindings-preserve-exact-winner", func(t *testing.T) {
		id := uuid.New()
		gate := make(chan struct{})
		var wait sync.WaitGroup
		results := make(chan error, 2)
		for _, number := range []string{"9007199254740992", "9007199254740993"} {
			wait.Add(1)
			go func(value string) {
				defer wait.Done()
				<-gate
				_, err := repository.RegisterFormulaBinding(ctx, id, "REQUEST", []uuid.UUID{unit}, json.RawMessage(`{"large":`+value+`}`))
				results <- err
			}(number)
		}
		close(gate)
		wait.Wait()
		close(results)
		passed, rejected := 0, 0
		for err := range results {
			if err == nil {
				passed++
			} else if errors.Is(err, ErrConflict) {
				rejected++
			} else {
				t.Fatal(err)
			}
		}
		if passed != 1 || rejected != 1 {
			t.Fatalf("first-binding race: success=%d conflict=%d", passed, rejected)
		}
	})
	t.Run("formula-public-HTTP-receipt-and-permission", func(t *testing.T) {
		handler := NewService(repository).Handler("controlled-pricing")
		body := []byte(fmt.Sprintf(`{"protocol_code":"REQUEST","required_unit_definition_ids":["%s"],"formula_snapshot":{"large":9007199254740992,"decimal":1}}`, unit))
		request := httptest.NewRequest(http.MethodPut, "/internal/v1/pricing/formula-bindings/"+formula.String(), bytes.NewReader(body))
		denied := httptest.NewRecorder()
		handler.ServeHTTP(denied, request)
		if denied.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated binding status=%d", denied.Code)
		}
		request = httptest.NewRequest(http.MethodPut, "/internal/v1/pricing/formula-bindings/"+formula.String(), bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer controlled-pricing")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var got FormulaBindingReceipt
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &got) != nil || !reflect.DeepEqual(got, first) {
			t.Fatalf("real formula HTTP replay status=%d", response.Code)
		}
	})
	t.Run("published-binding-replay-survives-unit-deactivation", func(t *testing.T) {
		if _, err := repository.DeactivateMeteringUnit(ctx, unit); err != nil {
			t.Fatal(err)
		}
		got, err := repository.RegisterFormulaBinding(ctx, formula, "REQUEST", []uuid.UUID{unit}, json.RawMessage(`{"decimal":1,"large":9007199254740992}`))
		if err != nil || !reflect.DeepEqual(got, first) {
			t.Fatalf("immutable publication reinterpreted later lifecycle: %v", err)
		}
		if _, err := repository.ValidateFormulaDraftUnits(ctx, map[string]uuid.UUID{"REQUEST": unit}); !errors.Is(err, ErrPriceMissing) {
			t.Fatalf("new draft accepted inactive unit: %v", err)
		}
	})

	actor := uuid.New()
	model := uuid.NewString()
	from := time.Date(2030, 1, 2, 0, 0, 0, 0, shanghaiLocation)
	repository.now = func() time.Time { return from.Add(-24 * time.Hour) }
	zero, normal := int64(0), int64(100)
	command := PriceDraftCommand{ScopeType: "MODEL", ScopeID: &model, ProtocolCode: "CHAT", FormulaVersionID: chatFormulaVersionID, CreditUnit: "MICROCREDIT", EffectiveFrom: from, ExpectedETag: 1, Units: []PriceUnitConfiguration{
		{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000001"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &normal},
		{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000002"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &zero},
		{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000003"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &normal},
	}}
	draft, err := repository.CreatePriceDraft(ctx, command, actor, "retained-draft-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	t.Run("price-preview-preserves-exact-units", func(t *testing.T) {
		price, err := repository.PreviewPriceVersion(ctx, draft.PriceVersionID)
		if err != nil || len(price.Units) != 3 || price.CachedInputPerMillion != 0 || price.FormulaVersionID != chatFormulaVersionID {
			t.Fatalf("actual draft preview: %v", err)
		}
	})
	key := "retained-price-publish-" + uuid.NewString()
	publication, err := repository.PublishPriceVersion(ctx, draft.PriceVersionID, actor, 1, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("lost-publication-ack-replay-is-stable", func(t *testing.T) {
		got, err := repository.PublishPriceVersion(ctx, draft.PriceVersionID, actor, 1, key)
		left, _ := json.Marshal(publication)
		right, _ := json.Marshal(got)
		if err != nil || !bytes.Equal(left, right) {
			t.Fatalf("publication replay changed immutable response: %v", err)
		}
		if _, err := repository.PublishPriceVersion(ctx, draft.PriceVersionID, actor, 2, key); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("replay accepted changed ETag: %v", err)
		}
	})
	t.Run("stale-space-etag-rejected", func(t *testing.T) {
		if _, err := repository.CreatePriceDraft(ctx, command, actor, "retained-draft-"+uuid.NewString()); !errors.Is(err, ErrPriceETagConflict) {
			t.Fatalf("stale draft write accepted: %v", err)
		}
	})
	repository.now = func() time.Time { return from.Add(time.Hour) }
	t.Run("real-price-read-and-impact-facts", func(t *testing.T) {
		price, err := repository.CurrentPrice(ctx, model)
		if err != nil || price.ID != draft.PriceVersionID || len(price.Units) != 3 {
			t.Fatalf("current read unavailable: %v", err)
		}
		id := uuid.MustParse(model)
		facts, _, revision, fingerprint, err := repository.BatchCurrentPriceCoverage(ctx, []ImpactPriceQuery{{ModelID: id, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}})
		// Impact uses its database transaction clock. This controlled 2030 price
		// remains a future price at that real clock and must report not covered.
		if err != nil || len(facts) != 1 || facts[0].Covered || revision < 1 || len(fingerprint) != 64 {
			t.Fatalf("real DB price coverage facts: %v", err)
		}
		byID, err := repository.PriceByID(ctx, draft.PriceVersionID)
		if err != nil || byID.ID != price.ID {
			t.Fatalf("stable price ID read: %v", err)
		}
	})
	t.Run("managed-space-and-finance-projection", func(t *testing.T) {
		etag, items, more, err := repository.ReadManagedPriceSpace(ctx, ManagedPriceSpaceQuery{ScopeType: "MODEL", ScopeID: &model, ProtocolCode: "CHAT", FormulaVersionID: chatFormulaVersionID, CreditUnit: "MICROCREDIT"}, 20)
		if err != nil || etag != 2 || len(items) != 1 || more {
			t.Fatalf("space projection lost publication: %v", err)
		}
		facts, err := repository.AdminFinancePricingOverview(ctx, repository.now())
		if err != nil || facts.PriceActiveCount < 1 || facts.MeteringInactiveCount != 1 {
			t.Fatalf("finance pricing facts: %v", err)
		}
	})
	t.Run("competing-publishers-cas-one-current-space", func(t *testing.T) {
		otherModel := uuid.NewString()
		command.ScopeID = &otherModel
		a, err := repository.CreatePriceDraft(ctx, command, actor, "retained-draft-"+uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		b, err := repository.CreatePriceDraft(ctx, command, actor, "retained-draft-"+uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		results := make(chan error, 2)
		var wait sync.WaitGroup
		for _, id := range []uuid.UUID{a.PriceVersionID, b.PriceVersionID} {
			wait.Add(1)
			go func(id uuid.UUID) {
				defer wait.Done()
				<-gate
				_, err := repository.PublishPriceVersion(ctx, id, actor, 1, "retained-cas-"+id.String())
				results <- err
			}(id)
		}
		close(gate)
		wait.Wait()
		close(results)
		success, rejected := 0, 0
		for err := range results {
			if err == nil {
				success++
				continue
			}
			var pg *pgconn.PgError
			if errors.Is(err, ErrPriceETagConflict) || errors.As(err, &pg) && pg.Code == "40001" {
				rejected++
				continue
			}
			t.Fatal(err)
		}
		if success != 1 || rejected != 1 {
			t.Fatalf("CAS results success=%d rejected=%d", success, rejected)
		}
		var published int
		if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM pricing_versions WHERE scope_id=$1 AND lifecycle_status='PUBLISHED'`, otherModel).Scan(&published); err != nil || published != 1 {
			t.Fatalf("CAS stored duplicates: %v", err)
		}
	})
	t.Run("internal-pricing-read-route-is-actually-wired", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/internal/v1/pricing/current?model="+model, nil)
		request.Header.Set("Authorization", "Bearer controlled-pricing")
		response := httptest.NewRecorder()
		NewService(repository).Handler("controlled-pricing").ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), draft.PriceVersionID.String()) {
			t.Fatalf("price route status=%d", response.Code)
		}
	})
}
