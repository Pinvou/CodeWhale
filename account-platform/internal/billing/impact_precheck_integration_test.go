package billing

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestBillingImpactPriceCoveragePostgresIntegration is a release gate. It
// exercises the production batch repository against the real Billing migration
// chain and must not be skipped in CI.
func TestBillingImpactPriceCoveragePostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	formulaID := uuid.MustParse("00000000-0000-7000-8200-000000000001")
	unitIDs := []uuid.UUID{
		uuid.MustParse("00000000-0000-7000-8100-000000000001"),
		uuid.MustParse("00000000-0000-7000-8100-000000000002"),
		uuid.MustParse("00000000-0000-7000-8100-000000000003"),
	}
	globalID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,version,space_etag) VALUES($1,'*',1,1,1,transaction_timestamp()-interval '1 hour',transaction_timestamp()-interval '1 hour','GLOBAL',NULL,'CHAT',$2,'MICROCREDIT','DRAFT',1,1)`, globalID, formulaID); err != nil {
		t.Fatal(err)
	}
	for _, unitID := range unitIDs {
		if _, err := repository.pool.Exec(ctx, `INSERT INTO pricing_version_unit_configs(price_version_id,unit_definition_id,config_type,fixed_price_per_denominator,created_at) VALUES($1,$2,'FIXED_PRICE',1,transaction_timestamp())`, globalID, unitID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE pricing_versions SET lifecycle_status='PUBLISHED',published_at=transaction_timestamp() WHERE id=$1`, globalID); err != nil {
		t.Fatal(err)
	}
	models := make([]ImpactPriceQuery, 5000)
	for index := range models {
		models[index] = ImpactPriceQuery{ModelID: uuid.New(), ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}
	}
	started := time.Now()
	coverage, asOf, err := repository.BatchCurrentPriceCoverage(ctx, models)
	if err != nil || len(coverage) != 5000 || asOf.IsZero() || time.Since(started) > 5*time.Second {
		t.Fatalf("5000 batch count=%d as_of=%s duration=%s err=%v", len(coverage), asOf, time.Since(started), err)
	}
	for _, item := range coverage {
		if !item.Covered || item.PriceVersionID == nil || *item.PriceVersionID != globalID || item.FormulaVersionID == nil || *item.FormulaVersionID != formulaID {
			t.Fatalf("global inheritance result=%+v", item)
		}
	}
	tooLarge := append(models, ImpactPriceQuery{ModelID: uuid.New(), ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"})
	if _, _, err := repository.BatchCurrentPriceCoverage(ctx, tooLarge); !errors.Is(err, ErrConflict) {
		t.Fatalf("5001 err=%v", err)
	}

	modelID, firstID, secondID := uuid.New(), uuid.New(), uuid.New()
	insertModelPrice := func(id uuid.UUID) {
		t.Helper()
		if _, err := repository.pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,version,space_etag) VALUES($1,$2,1,1,1,transaction_timestamp()-interval '1 minute',transaction_timestamp()-interval '1 minute','MODEL',$2,'CHAT',$3,'MICROCREDIT','DRAFT',1,1)`, id, modelID.String(), formulaID); err != nil {
			t.Fatal(err)
		}
		for _, unitID := range unitIDs {
			if _, err := repository.pool.Exec(ctx, `INSERT INTO pricing_version_unit_configs(price_version_id,unit_definition_id,config_type,created_at) VALUES($1,$2,'INHERIT_GLOBAL',transaction_timestamp())`, id, unitID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := repository.pool.Exec(ctx, `UPDATE pricing_versions SET lifecycle_status='PUBLISHED',published_at=transaction_timestamp() WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	insertModelPrice(firstID)
	query := []ImpactPriceQuery{{ModelID: modelID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}}
	modelCoverage, _, err := repository.BatchCurrentPriceCoverage(ctx, query)
	if err != nil || len(modelCoverage) != 1 || !modelCoverage[0].Covered || modelCoverage[0].PriceVersionID == nil || *modelCoverage[0].PriceVersionID != firstID {
		t.Fatalf("model inheritance=%+v err=%v", modelCoverage, err)
	}
	secondGlobalID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,version,space_etag) VALUES($1,'*',1,1,1,transaction_timestamp()-interval '30 minutes',transaction_timestamp()-interval '30 minutes','GLOBAL',NULL,'CHAT',$2,'MICROCREDIT','DRAFT',2,2)`, secondGlobalID, formulaID); err != nil {
		t.Fatal(err)
	}
	for _, unitID := range unitIDs {
		if _, err := repository.pool.Exec(ctx, `INSERT INTO pricing_version_unit_configs(price_version_id,unit_definition_id,config_type,fixed_price_per_denominator,created_at) VALUES($1,$2,'FIXED_PRICE',2,transaction_timestamp())`, secondGlobalID, unitID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE pricing_versions SET lifecycle_status='PUBLISHED',published_at=transaction_timestamp() WHERE id=$1`, secondGlobalID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.BatchCurrentPriceCoverage(ctx, query); !errors.Is(err, ErrPriceAmbiguous) {
		t.Fatalf("multiple inherited GLOBAL prices err=%v", err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE pricing_versions SET lifecycle_status='ENDED',ended_at=transaction_timestamp() WHERE id=$1`, secondGlobalID); err != nil {
		t.Fatal(err)
	}
	insertModelPrice(secondID)
	if _, _, err := repository.BatchCurrentPriceCoverage(ctx, query); !errors.Is(err, ErrPriceAmbiguous) {
		t.Fatalf("ambiguous err=%v", err)
	}

	lockTx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `LOCK TABLE pricing_versions IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer timeoutCancel()
	_, _, timeoutErr := repository.BatchCurrentPriceCoverage(timeoutCtx, query)
	_ = lockTx.Rollback(ctx)
	if !errors.Is(timeoutErr, context.DeadlineExceeded) {
		t.Fatalf("deadline err=%v", timeoutErr)
	}
}
