package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPricingSpaceRecoveryPostgresIntegration uses a real PostgreSQL instance
// because the repair depends on grouping, conflict handling, and the exact
// repository query that previously returned HTTP 500 in production. CI
// supplies a dedicated database; local runs skip safely.
func TestPricingSpaceRecoveryPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect integration database: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "billing_pricing_recovery_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatalf("create isolated integration schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = connection.Exec(cleanupCtx, `DROP SCHEMA `+identifier+` CASCADE`)
	})
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatalf("select isolated integration schema: %v", err)
	}

	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	baseline := make([]migrations.Migration, 0, len(loaded))
	var repair migrations.Migration
	for _, migration := range loaded {
		switch {
		case migration.Version < 51:
			baseline = append(baseline, migration)
		case migration.Version == 51:
			repair = migration
		}
	}
	if repair.Version != 51 {
		t.Fatal("pricing-space recovery migration is missing")
	}
	if err := migrations.Up(ctx, connection, baseline); err != nil {
		t.Fatalf("migrate isolated schema through version 50: %v", err)
	}
	migrations.Unlock(ctx, connection)

	now := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	formulaID := uuid.MustParse("00000000-0000-7000-8200-000000000001")
	missingModel := uuid.NewString()
	existingModel := uuid.NewString()
	insertPrice := func(model string, version, etag int64, effectiveFrom time.Time, effectiveTo *time.Time) {
		t.Helper()
		_, insertErr := connection.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,effective_to,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,version,space_etag,published_at) VALUES($1,$2,1,2,1,$3,$4,$5,'MODEL',$2,'CHAT',$6,'MICROCREDIT','PUBLISHED',$7,$8,$5)`, uuid.New(), model, effectiveFrom, effectiveTo, effectiveFrom.Add(-time.Hour), formulaID, version, etag)
		if insertErr != nil {
			t.Fatalf("insert pricing fixture: %v", insertErr)
		}
	}
	firstEnd := now.Add(-24 * time.Hour)
	insertPrice(missingModel, 1, 2, now.Add(-48*time.Hour), &firstEnd)
	insertPrice(missingModel, 2, 4, firstEnd, nil)
	insertPrice(existingModel, 1, 5, now.Add(-24*time.Hour), nil)
	if _, err := connection.Exec(ctx, `INSERT INTO pricing_space_versions(scope_type,scope_key,protocol_code,formula_version_id,credit_unit,etag,updated_at) VALUES('MODEL',$1,'CHAT',$2,'MICROCREDIT',9,$3)`, existingModel, formulaID, now); err != nil {
		t.Fatalf("insert existing pricing-space fixture: %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := connection.Exec(ctx, repair.UpSQL); err != nil {
			t.Fatalf("execute repair attempt %d: %v", attempt, err)
		}
	}
	assertPricingSpace := func(model string, wantETag int64) {
		t.Helper()
		var count int
		var etag int64
		if err := connection.QueryRow(ctx, `SELECT count(*),max(etag) FROM pricing_space_versions WHERE scope_type='MODEL' AND scope_key=$1 AND protocol_code='CHAT' AND formula_version_id=$2 AND credit_unit='MICROCREDIT'`, model, formulaID).Scan(&count, &etag); err != nil {
			t.Fatal(err)
		}
		if count != 1 || etag != wantETag {
			t.Fatalf("pricing space %s = count %d etag %d, want count 1 etag %d", model, count, etag, wantETag)
		}
	}
	assertPricingSpace(missingModel, 4)
	assertPricingSpace(existingModel, 9)

	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ConnConfig.RuntimeParams == nil {
		configuration.ConnConfig.RuntimeParams = make(map[string]string)
	}
	configuration.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &PostgresRepository{pool: pool, now: func() time.Time { return now }}
	items, err := repository.ListManagedPrices(ctx, 100)
	if err != nil || len(items) != 3 {
		t.Fatalf("list managed prices after repair: items=%d err=%v", len(items), err)
	}
	response := httptest.NewRecorder()
	NewService(repository).adminManagedPrices(response, httptest.NewRequest(http.MethodGet, "/admin/v1/pricing?limit=100", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("pricing lifecycle endpoint status=%d body=%s", response.Code, response.Body.String())
	}

	if _, err := connection.Exec(ctx, repair.DownSQL); err != nil {
		t.Fatalf("execute repair rollback: %v", err)
	}
	assertPricingSpace(missingModel, 4)
	assertPricingSpace(existingModel, 9)
}
