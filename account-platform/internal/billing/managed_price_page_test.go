package billing

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestManagedPriceCursorValidation(t *testing.T) {
	at := time.Date(2026, 10, 29, 0, 0, 0, 123456000, time.UTC)
	raw := encodeManagedPriceCursor(at, uuid.New(), 100)
	if _, err := decodeManagedPriceCursor(raw, 100); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{" ", "***", raw + "=", base64.RawURLEncoding.EncodeToString([]byte(`{"v":2}`)), encodeManagedPriceCursor(at, uuid.Nil, 100), encodeManagedPriceCursor(at.Add(time.Nanosecond), uuid.New(), 100)} {
		if _, err := decodeManagedPriceCursor(invalid, 100); err == nil {
			t.Fatal("accepted invalid cursor")
		}
	}
	if _, err := decodeManagedPriceCursor(raw, 50); err == nil {
		t.Fatal("accepted changed limit")
	}
}

// Uses the real repository, production migrations, read projection and handler.
// Exported pages are synthetic public fixture data, never deployment records.
func TestPostgresManagedPricePagination(t *testing.T) {
	repo, ctx := retainedFinanceRepository(t)
	actor := uuid.New()
	from := time.Date(2026, 10, 30, 0, 0, 0, 0, shanghaiLocation)
	one := int64(1)
	command := PriceDraftCommand{ScopeType: "GLOBAL", ProtocolCode: "CHAT", FormulaVersionID: chatFormulaVersionID, CreditUnit: "MICROCREDIT", EffectiveFrom: from, ExpectedETag: 1}
	for i := 1; i <= 3; i++ {
		command.Units = append(command.Units, PriceUnitConfiguration{UnitDefinitionID: uuid.MustParse(fmt.Sprintf("00000000-0000-7000-8100-%012d", i)), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &one})
	}
	fixed := time.Date(2026, 10, 29, 0, 0, 0, 123456000, time.UTC)
	repo.now = func() time.Time { return fixed }
	var ids []uuid.UUID
	for index := range 101 {
		item, err := repo.CreatePriceDraft(ctx, command, actor, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item.PriceVersionID)
		if index == 0 {
			if _, err := repo.PublishPriceVersion(ctx, item.PriceVersionID, actor, 1, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			command.ExpectedETag = 2
		}
	}
	repo.now = func() time.Time { return fixed.Add(48 * time.Hour) }
	svc := NewService(repo)
	read := func(query string) (map[string]json.RawMessage, int) {
		t.Helper()
		out := httptest.NewRecorder()
		svc.adminManagedPrices(out, httptest.NewRequest("GET", "/admin/v1/pricing?"+query, nil))
		var body map[string]json.RawMessage
		if err := json.Unmarshal(out.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode handler body: %v; body=%s", err, out.Body.String())
		}
		return body, out.Code
	}
	first, status := read("limit=100")
	if status != 200 {
		t.Fatalf("first status %d", status)
	}
	page := decodeManagedPriceWirePage(t, first, 100)
	items, cursor := page.Items, *page.NextCursor
	if len(items) != 100 || cursor == "" || string(first["has_more"]) != "true" {
		t.Fatal("first page truncated without continuation")
	}
	firstIDs := map[uuid.UUID]bool{}
	for _, item := range items {
		firstIDs[item.PriceVersionID] = true
	}
	// A newer concurrent insertion must not shift the continuation boundary.
	repo.now = func() time.Time { return fixed.Add(time.Second) }
	if _, err := repo.CreatePriceDraft(ctx, command, actor, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	repo.now = func() time.Time { return fixed.Add(48 * time.Hour) }
	second, status := read("limit=100&cursor=" + url.QueryEscape(cursor))
	if status != 200 {
		t.Fatalf("second status %d", status)
	}
	page = decodeManagedPriceWirePage(t, second, 100)
	items = page.Items
	if len(items) != 1 || firstIDs[items[0].PriceVersionID] || string(second["has_more"]) != "false" || string(second["next_cursor"]) != `""` {
		t.Fatal("seek boundary duplicated or omitted original records")
	}
	if items[0].DerivedStatus != "ACTIVE" || items[0].PriceVersionID != ids[0] {
		t.Fatal("101st ACTIVE missing")
	}
	activeID := items[0].PriceVersionID
	// A separate older DRAFT remains editable by its actual ID and latest CAS.
	command.ExpectedETag = 2
	saved, err := repo.UpdatePriceDraft(ctx, ids[1], actor, command)
	if err != nil || saved.PriceVersionID != ids[1] {
		t.Fatalf("older draft save: %v", err)
	}
	successor := ids[2]
	ended, err := repo.TransitionPriceVersion(ctx, activeID, actor, "END", "CONTINUATION", "controlled pagination lifecycle", &successor, uuid.NewString())
	if err != nil || ended.PriceVersionID != activeID || ended.LifecycleStatus != "ENDED" {
		t.Fatalf("101st ACTIVE action: %v", err)
	}
	for _, query := range []string{"limit=201", "limit=0", "limit=no", "limit=100&cursor=***", "limit=50&cursor=" + cursor, "limit=100&cursor=" + cursor + "&scope_type=MODEL", "limit=100&cursor=" + cursor + "&price_version_id=" + ids[0].String(), "limit=100&cursor=", "limit=100&limit=50", "limit=100&protocol_code=CHAT"} {
		if _, status := read(query); status == 200 {
			t.Fatalf("accepted %s", query)
		}
	}
	if body, status := read(""); status != 200 {
		t.Fatal("valid no-cursor initial read failed")
	} else {
		decodeManagedPriceWirePage(t, body, 50)
	}
	if target := os.Getenv("PINVOU_MANAGED_PRICE_FIXTURE_OUTPUT"); target != "" {
		payload, err := json.MarshalIndent(map[string]any{"first": first, "second": second}, "", "  ")
		if err != nil {
			t.Fatalf("marshal producer fixture: %v", err)
		}
		if err := os.WriteFile(target, append(payload, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// Decode the actual administration wire DTO: financial unit values are strings.
// Embedded ManagedPriceVersion alone would partially decode before returning an
// error for these values, so every envelope/item/metadata decode is checked.
type managedPriceWirePage struct {
	Items      []AdminManagedPriceVersion
	HasMore    *bool
	NextCursor *string
}

func decodeManagedPriceWirePage(t *testing.T, body map[string]json.RawMessage, limit int) managedPriceWirePage {
	t.Helper()
	var page managedPriceWirePage
	if err := json.Unmarshal(body["items"], &page.Items); err != nil {
		t.Fatalf("decode wire items: %v", err)
	}
	if page.Items == nil {
		t.Fatal("wire items missing or null")
	}
	if err := json.Unmarshal(body["has_more"], &page.HasMore); err != nil {
		t.Fatalf("decode wire has_more: %v", err)
	}
	if err := json.Unmarshal(body["next_cursor"], &page.NextCursor); err != nil {
		t.Fatalf("decode wire next_cursor: %v", err)
	}
	if page.HasMore == nil || page.NextCursor == nil || *page.HasMore != (*page.NextCursor != "") {
		t.Fatal("invalid wire continuation metadata")
	}
	if *page.HasMore {
		cursor, err := decodeManagedPriceCursor(*page.NextCursor, limit)
		if err != nil {
			t.Fatalf("decode wire cursor: %v", err)
		}
		if len(page.Items) == 0 || cursor.ID != page.Items[len(page.Items)-1].PriceVersionID {
			t.Fatal("wire cursor does not identify page boundary")
		}
	}
	for _, item := range page.Items {
		if len(item.Units) != 3 {
			t.Fatalf("wire unit count=%d", len(item.Units))
		}
		expected := map[string]bool{}
		for index := 1; index <= 3; index++ {
			expected[fmt.Sprintf("00000000-0000-7000-8100-%012d", index)] = true
		}
		for _, unit := range item.Units {
			if !expected[unit.UnitDefinitionID] || unit.ConfigType != "FIXED_PRICE" || unit.FixedPricePerDenominator == nil || *unit.FixedPricePerDenominator != "1" {
				t.Fatalf("wire unit fields do not preserve producer values: %+v", unit)
			}
			delete(expected, unit.UnitDefinitionID)
		}
		if len(expected) != 0 {
			t.Fatal("wire unit identity missing or duplicated")
		}
	}
	return page
}

func TestManagedPriceProducerFixtureWireDTO(t *testing.T) {
	raw, err := os.ReadFile("../../test/contract/fixtures/managed-price-pages.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode producer fixture body: %v", err)
	}
	first := decodeManagedPriceWirePage(t, fixture["first"], 100)
	second := decodeManagedPriceWirePage(t, fixture["second"], 100)
	if len(first.Items) != 100 || !*first.HasMore || len(second.Items) != 1 || *second.HasMore || second.Items[0].DerivedStatus != "ACTIVE" {
		t.Fatal("stored producer fixture lost 100/101 ACTIVE page contract")
	}
}
