package billing

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestModelPriceProjectionPostgresIntegration is a release gate. CI must run
// it against the complete Billing migration chain without SKIP.
func TestModelPriceProjectionPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	modelID, globalPriceID, futurePriceID := uuid.New(), uuid.New(), uuid.New()
	formulaID := uuid.MustParse("00000000-0000-7000-8200-000000000001")
	unitIDs := []uuid.UUID{
		uuid.MustParse("00000000-0000-7000-8100-000000000001"),
		uuid.MustParse("00000000-0000-7000-8100-000000000002"),
		uuid.MustParse("00000000-0000-7000-8100-000000000003"),
	}
	var databaseNow time.Time
	if err := repository.pool.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&databaseNow); err != nil {
		t.Fatal(err)
	}
	futureAt := databaseNow.Add(time.Hour)
	if _, err := repository.pool.Exec(ctx, `
INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,version,space_etag,published_at) VALUES
($1,'*',10,30,5,$4::timestamptz-interval '1 hour',$4::timestamptz-interval '2 hours','GLOBAL',NULL,'CHAT',$3,'MICROCREDIT','PUBLISHED',1,7,$4::timestamptz-interval '2 hours'),
($2,$5,20,60,10,$6,$4,'MODEL',$5,'CHAT',$3,'MICROCREDIT','PUBLISHED',1,3,$4);
INSERT INTO pricing_space_versions(scope_type,scope_key,protocol_code,formula_version_id,credit_unit,etag,updated_at) VALUES
('GLOBAL','*','CHAT',$3,'MICROCREDIT',7,$4),('MODEL',$5,'CHAT',$3,'MICROCREDIT',3,$4)`, globalPriceID, futurePriceID, formulaID, databaseNow, modelID.String(), futureAt); err != nil {
		t.Fatal(err)
	}
	for _, unitID := range unitIDs {
		if _, err := repository.pool.Exec(ctx, `INSERT INTO pricing_version_unit_configs(price_version_id,unit_definition_id,config_type,fixed_price_per_denominator,created_at) VALUES($1,$3,'FIXED_PRICE',10,transaction_timestamp()),($2,$3,'INHERIT_GLOBAL',NULL,transaction_timestamp())`, globalPriceID, futurePriceID, unitID); err != nil {
			t.Fatal(err)
		}
	}
	projection, err := repository.AdminModelPriceProjection(ctx, modelID, "CHAT", formulaID, "MICROCREDIT")
	if err != nil {
		t.Fatal(err)
	}
	if projection.Source != "BILLING" || projection.Current.CoverageStatus != "AVAILABLE" || projection.Current.Price == nil || projection.Current.Price.PriceVersionID != globalPriceID || projection.Current.Price.SpaceETag != 7 {
		t.Fatalf("unexpected current projection: %+v", projection)
	}
	if projection.NextChange == nil || !projection.NextChange.EffectiveAt.Equal(futureAt) || projection.NextChange.Price == nil || projection.NextChange.Price.PriceVersionID != futurePriceID || projection.NextChange.Price.SpaceETag != 3 {
		t.Fatalf("unexpected future projection: %+v", projection.NextChange)
	}
	for _, unit := range projection.NextChange.Price.Units {
		if unit.ConfigurationType != "INHERIT_GLOBAL" || unit.PriceSourceScope != "GLOBAL" || unit.SourcePriceVersionID != globalPriceID || unit.ResolvedPrice != 10 {
			t.Fatalf("inheritance source not projected: %+v", unit)
		}
	}
	blockedAt := databaseNow.Add(-time.Minute)
	if _, err := repository.pool.Exec(ctx, `INSERT INTO pricing_blocks(id,model_id,protocol_code,formula_version_id,credit_unit,effective_from,reason,created_by,created_at) VALUES($1,$2,'CHAT',$3,'MICROCREDIT',$4,'integration',$5,$4)`, uuid.New(), modelID.String(), formulaID, blockedAt, uuid.New()); err != nil {
		t.Fatal(err)
	}
	blocked, err := repository.AdminModelPriceProjection(ctx, modelID, "CHAT", formulaID, "MICROCREDIT")
	if err != nil || blocked.Current.CoverageStatus != "BLOCKED" || blocked.Current.Price != nil {
		t.Fatalf("blocked projection=%+v err=%v", blocked, err)
	}
}
