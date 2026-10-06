package token

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// New model calls use the same published unit configurations as the management
// API. An old three-rate INSERT is not a valid new published-price fixture.
func publishedBillingPriceFixture(t *testing.T, repo *billingdomain.PostgresRepository, model string, input, output, cached int64) PriceVersion {
	t.Helper()
	ctx := context.Background()
	location := time.FixedZone("Shanghai fixture", 8*60*60)
	today := time.Now().In(location)
	from := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, location)
	actor := uuid.New()
	command := billingdomain.PriceDraftCommand{ScopeType: "MODEL", ScopeID: &model, ProtocolCode: "CHAT", FormulaVersionID: uuid.MustParse("00000000-0000-7000-8200-000000000001"), CreditUnit: "MICROCREDIT", EffectiveFrom: from, ExpectedETag: 1, Units: []billingdomain.PriceUnitConfiguration{
		{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000001"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &input},
		{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000002"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &cached},
		{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000003"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &output},
	}}
	draft, err := repo.CreatePriceDraft(ctx, command, actor, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PublishPriceVersion(ctx, draft.PriceVersionID, actor, draft.SpaceETag, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	resolved, err := repo.CurrentPrice(ctx, model)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	var result PriceVersion
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != draft.PriceVersionID || ValidatePriceFormula(result) != nil {
		t.Fatal("price fixture did not publish complete versioned facts")
	}
	return result
}

// Historical tests insert their actual previous-schema row before Up22. Its
// production backfill must preserve the original ID/rates and publish metadata.
func migratedLegacyBillingPriceFixture(t *testing.T, pool *pgxpool.Pool, price PriceVersion) {
	t.Helper()
	ctx := context.Background()
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	defer connection.Conn().Close(ctx)
	for {
		var version int64
		if err := connection.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM schema_migrations`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version == 21 {
			break
		}
		if version < 21 {
			t.Fatal("legacy fixture is below the required existing schema")
		}
		if err := migrations.DownOne(ctx, connection.Conn(), loaded); err != nil {
			t.Fatalf("legacy fixture rollback %d: %v", version, err)
		}
	}
	if _, err := connection.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, price.ID, price.Model, price.InputPerMillion, price.OutputPerMillion, price.CachedInputPerMillion, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("legacy schema insert: %v", err)
	}
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatalf("legacy pricing upgrade: %v", err)
	}
	var id uuid.UUID
	var input, output, cached int64
	var published bool
	if err := connection.QueryRow(ctx, `SELECT id,input_per_million,output_per_million,cached_input_per_million,published_at IS NOT NULL FROM pricing_versions WHERE id=$1`, price.ID).Scan(&id, &input, &output, &cached, &published); err != nil || id != price.ID || input != price.InputPerMillion || output != price.OutputPerMillion || cached != price.CachedInputPerMillion || !published {
		t.Fatal("old price facts were not retained by the actual migration")
	}
}
