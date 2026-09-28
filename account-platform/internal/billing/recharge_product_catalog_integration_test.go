package billing

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRechargeProductCatalogRelistsMigratedInfiniteEvidenceIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "billing_catalog_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err = connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = connection.Exec(cleanupCtx, `RESET SESSION AUTHORIZATION; RESET ROLE; DROP SCHEMA `+identifier+` CASCADE`)
	})
	if _, err = connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadBillingIntegrationMigrations("../../migrations/billing", schema)
	if err != nil {
		t.Fatal(err)
	}
	through52 := loaded[:0]
	for _, migration := range loaded {
		if migration.Version > 52 {
			break
		}
		through52 = append(through52, migration)
	}
	if len(through52) == 0 || through52[len(through52)-1].Version != 52 {
		t.Fatal("integration fixture must explicitly isolate the Billing migration 52 boundary")
	}
	if err = migrations.Up(ctx, connection, through52); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)

	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.ConnConfig.RuntimeParams = map[string]string{"search_path": schema}
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }}

	var catalogItemID, legacyEvidenceID uuid.UUID
	if err = pool.QueryRow(ctx, `SELECT item.catalog_item_id,evidence.evidence_id
FROM recharge_product_catalog_items item
JOIN product_admission_evidence evidence USING(catalog_item_id)
WHERE item.product_id='PLAN_TRIAL' AND evidence.expires_at='infinity'::timestamptz
	  AND evidence.issuer_workload='billing-catalog-migration'
ORDER BY evidence.created_at DESC,evidence.evidence_id DESC LIMIT 1`).Scan(&catalogItemID, &legacyEvidenceID); err != nil {
		t.Fatalf("load migrated infinite admission evidence: %v", err)
	}
	before, err := repository.RechargeProductCatalogItem(ctx, catalogItemID)
	if err != nil || before.LifecycleStatus != "ON_SALE" {
		t.Fatalf("unexpected migrated catalog item: %#v err=%v", before, err)
	}
	verification, _, err := repository.StartProductAdmissionVerification(ctx, catalogItemID, uuid.New(), nil)
	if err != nil {
		t.Fatalf("start current fingerprint verification: %v", err)
	}
	evidenceID, err := repository.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "integration-test", "CATALOG_FACT_CHECK", "current-sha256", repository.now().Add(time.Hour))
	if err != nil {
		t.Fatalf("complete current fingerprint verification: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE product_admission_evidence SET expires_at='infinity'::timestamptz WHERE evidence_id=$1`, evidenceID); err != nil {
		t.Fatalf("preserve non-expiring current evidence fixture: %v", err)
	}
	offSale, err := repository.TransitionRechargeProduct(ctx, catalogItemID, before.Version, "OFF_SALE", nil)
	if err != nil || offSale.LifecycleStatus != "OFF_SALE" || offSale.Version != before.Version+1 {
		t.Fatalf("unpublish migrated product: %#v err=%v", offSale, err)
	}
	if _, err = repository.TransitionRechargeProduct(ctx, catalogItemID, offSale.Version, "ON_SALE", &legacyEvidenceID); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy MD5 migration fingerprint was not rejected: %v", err)
	}
	relisted, err := repository.TransitionRechargeProduct(ctx, catalogItemID, offSale.Version, "ON_SALE", &evidenceID)
	if err != nil {
		t.Fatalf("relist with migrated infinite evidence: %v", err)
	}
	if relisted.LifecycleStatus != "ON_SALE" || relisted.Version != offSale.Version+1 {
		t.Fatalf("unexpected relisted product: %#v", relisted)
	}
	var infinite, currentBinding bool
	if err = pool.QueryRow(ctx, `SELECT expires_at='infinity'::timestamptz FROM product_admission_evidence WHERE evidence_id=$1`, evidenceID).Scan(&infinite); err != nil || !infinite {
		t.Fatalf("historical evidence validity changed: infinite=%v err=%v", infinite, err)
	}
	if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM product_admission_bindings binding JOIN payment_product_mappings mapping ON mapping.id=binding.mapping_id WHERE binding.catalog_item_id=$1 AND binding.evidence_id=$2 AND binding.effective_to IS NULL AND mapping.effective_to IS NULL AND mapping.status IN ('FROZEN','TEST_ONLY'))`, catalogItemID, evidenceID).Scan(&currentBinding); err != nil || !currentBinding {
		t.Fatalf("relisted product has no current verified mapping binding: current=%v err=%v", currentBinding, err)
	}

	t.Run("finite future remains valid", func(t *testing.T) {
		item, validEvidence := createVerifiedRechargeProduct(t, ctx, repository, "future", repository.now().Add(time.Hour))
		published, transitionErr := repository.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &validEvidence)
		if transitionErr != nil || published.LifecycleStatus != "ON_SALE" {
			t.Fatalf("finite future evidence was rejected: %#v err=%v", published, transitionErr)
		}
	})

	t.Run("renaming an on-sale product fails closed until the new fingerprint is verified", func(t *testing.T) {
		item, oldEvidence := createVerifiedRechargeProduct(t, ctx, repository, "rename", repository.now().Add(time.Hour))
		published, transitionErr := repository.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &oldEvidence)
		if transitionErr != nil {
			t.Fatal(transitionErr)
		}
		staleVerification, _, err := repository.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
		if err != nil {
			t.Fatal(err)
		}
		renamed, renameErr := repository.ExecuteRechargeProductDisplayNamesUpdate(ctx, RechargeProductDisplayNamesCommand{CatalogItemID: item.CatalogItemID, ExpectedVersion: published.Version, DisplayNameZhCN: "新中文名", DisplayNameEN: "New English name", DisplayNameJA: "新しい日本語名", IdempotencyKey: "catalog-rename-test-0001", RequestHash: "rename-hash-1", Actor: uuid.NewString(), Reason: "integration rename"})
		if renameErr != nil || renamed.LifecycleStatus != "OFF_SALE" || renamed.Version != published.Version+1 || renamed.AdmissionVersion != published.AdmissionVersion+1 {
			t.Fatalf("renamed=%+v err=%v", renamed, renameErr)
		}
		var activeBindings, activeMappings int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM product_admission_bindings WHERE catalog_item_id=$1 AND effective_to IS NULL`, item.CatalogItemID).Scan(&activeBindings); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_product_mappings WHERE product_id=$1 AND effective_to IS NULL AND status IN ('FROZEN','TEST_ONLY')`, item.ProductID).Scan(&activeMappings); err != nil {
			t.Fatal(err)
		}
		if activeBindings != 0 || activeMappings != 0 {
			t.Fatalf("rename retained admission: bindings=%d mappings=%d", activeBindings, activeMappings)
		}
		if _, err = repository.CompleteProductAdmissionVerification(ctx, staleVerification.VerificationID, "integration-test", "CATALOG_FACT_CHECK", "stale-sha256", repository.now().Add(time.Hour)); !errors.Is(err, ErrConflict) {
			t.Fatalf("verification completed after rename: %v", err)
		}
		renamedBack, err := repository.ExecuteRechargeProductDisplayNamesUpdate(ctx, RechargeProductDisplayNamesCommand{CatalogItemID: item.CatalogItemID, ExpectedVersion: renamed.Version, DisplayNameZhCN: item.DisplayNameZhCN, DisplayNameEN: item.DisplayNameEN, DisplayNameJA: item.DisplayNameJA, IdempotencyKey: "catalog-rename-test-0002", RequestHash: "rename-hash-2", Actor: uuid.NewString(), Reason: "integration rename back"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.TransitionRechargeProduct(ctx, item.CatalogItemID, renamedBack.Version, "ON_SALE", &oldEvidence); !errors.Is(err, ErrConflict) {
			t.Fatalf("old fingerprint unexpectedly republished product: %v", err)
		}
		verification, _, err := repository.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
		if err != nil {
			t.Fatal(err)
		}
		newEvidence, err := repository.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "integration-test", "CATALOG_FACT_CHECK", "renamed-sha256", repository.now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		republished, err := repository.TransitionRechargeProduct(ctx, item.CatalogItemID, renamedBack.Version, "ON_SALE", &newEvidence)
		if err != nil || republished.LifecycleStatus != "ON_SALE" {
			t.Fatalf("republished=%+v err=%v", republished, err)
		}
	})

	for _, fixture := range []struct {
		name      string
		expiresAt func(time.Time) time.Time
	}{
		{name: "equal to transition time", expiresAt: func(now time.Time) time.Time { return now }},
		{name: "expired", expiresAt: func(now time.Time) time.Time { return now.Add(-time.Microsecond) }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			now := repository.now().Truncate(time.Microsecond)
			item, invalidEvidence := createVerifiedRechargeProduct(t, ctx, repository, strings.ReplaceAll(fixture.name, " ", "-"), now.Add(time.Hour))
			if _, updateErr := pool.Exec(ctx, `UPDATE product_admission_evidence SET issued_at=$2,expires_at=$3 WHERE evidence_id=$1`, invalidEvidence, now.Add(-time.Hour), fixture.expiresAt(now)); updateErr != nil {
				t.Fatal(updateErr)
			}
			originalNow := repository.now
			repository.now = func() time.Time { return now }
			t.Cleanup(func() { repository.now = originalNow })
			_, transitionErr := repository.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &invalidEvidence)
			if !errors.Is(transitionErr, ErrConflict) {
				t.Fatalf("invalid evidence expiry was not rejected: %v", transitionErr)
			}
		})
	}

	t.Run("revoked evidence", func(t *testing.T) {
		item, revokedEvidence := createVerifiedRechargeProduct(t, ctx, repository, "revoked", repository.now().Add(time.Hour))
		if revokeErr := repository.RevokeProductAdmission(ctx, revokedEvidence, "HUMAN", "integration-test", "revoked for validation"); revokeErr != nil {
			t.Fatal(revokeErr)
		}
		_, transitionErr := repository.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &revokedEvidence)
		if !errors.Is(transitionErr, ErrConflict) {
			t.Fatalf("revoked evidence was not rejected: %v", transitionErr)
		}
	})

	t.Run("wrong catalog evidence", func(t *testing.T) {
		item, _ := createVerifiedRechargeProduct(t, ctx, repository, "wrong-catalog-target", repository.now().Add(time.Hour))
		_, otherEvidence := createVerifiedRechargeProduct(t, ctx, repository, "wrong-catalog-source", repository.now().Add(time.Hour))
		_, transitionErr := repository.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &otherEvidence)
		if !errors.Is(transitionErr, ErrConflict) {
			t.Fatalf("evidence from another catalog item was not rejected: %v", transitionErr)
		}
	})

	t.Run("fingerprint drift", func(t *testing.T) {
		item, driftedEvidence := createVerifiedRechargeProduct(t, ctx, repository, "fingerprint", repository.now().Add(time.Hour))
		if _, updateErr := pool.Exec(ctx, `UPDATE recharge_product_mapping_drafts SET amount_fen=amount_fen+1,updated_at=now() WHERE catalog_item_id=$1`, item.CatalogItemID); updateErr != nil {
			t.Fatal(updateErr)
		}
		_, transitionErr := repository.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &driftedEvidence)
		if !errors.Is(transitionErr, ErrConflict) {
			t.Fatalf("fingerprint drift was not rejected: %v", transitionErr)
		}
	})

	t.Run("stale version", func(t *testing.T) {
		item, validEvidence := createVerifiedRechargeProduct(t, ctx, repository, "stale-version", repository.now().Add(time.Hour))
		_, transitionErr := repository.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version+1, "ON_SALE", &validEvidence)
		if !errors.Is(transitionErr, ErrConflict) {
			t.Fatalf("stale catalog version was not rejected: %v", transitionErr)
		}
	})

	t.Run("concurrent version change", func(t *testing.T) {
		item, validEvidence := createVerifiedRechargeProduct(t, ctx, repository, "concurrent-version", repository.now().Add(time.Hour))
		lockTx, beginErr := pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer lockTx.Rollback(context.Background())
		if _, lockErr := lockTx.Exec(ctx, `SELECT catalog_item_id FROM recharge_product_catalog_items WHERE catalog_item_id=$1 FOR UPDATE`, item.CatalogItemID); lockErr != nil {
			t.Fatal(lockErr)
		}
		result := make(chan error, 1)
		go func() {
			_, transitionErr := repository.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &validEvidence)
			result <- transitionErr
		}()
		waitForBlockedCatalogTransition(t, ctx, pool, lockTx.Conn().PgConn().PID(), "%SELECT product_id,lifecycle_status,version,ever_published FROM recharge_product_catalog_items%")
		if _, updateErr := lockTx.Exec(ctx, `UPDATE recharge_product_catalog_items SET version=version+1,updated_at=now() WHERE catalog_item_id=$1`, item.CatalogItemID); updateErr != nil {
			t.Fatal(updateErr)
		}
		if commitErr := lockTx.Commit(ctx); commitErr != nil {
			t.Fatal(commitErr)
		}
		if transitionErr := <-result; !errors.Is(transitionErr, ErrConflict) {
			t.Fatalf("transition ignored a concurrent catalog version change: %v", transitionErr)
		}
	})

	t.Run("concurrent evidence revocation", func(t *testing.T) {
		item, validEvidence := createVerifiedRechargeProduct(t, ctx, repository, "concurrent-revocation", repository.now().Add(time.Hour))
		published, publishErr := repository.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &validEvidence)
		if publishErr != nil {
			t.Fatal(publishErr)
		}
		item, publishErr = repository.TransitionRechargeProduct(ctx, item.CatalogItemID, published.Version, "OFF_SALE", nil)
		if publishErr != nil {
			t.Fatal(publishErr)
		}
		lockTx, beginErr := pool.Begin(ctx)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer lockTx.Rollback(context.Background())
		if lockErr := lockTransactionKey(ctx, lockTx, "catalog.admission", item.CatalogItemID.String()); lockErr != nil {
			t.Fatal(lockErr)
		}
		result := make(chan error, 1)
		go func() {
			_, transitionErr := repository.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &validEvidence)
			result <- transitionErr
		}()
		waitForBlockedCatalogTransition(t, ctx, pool, lockTx.Conn().PgConn().PID(), "%pg_advisory_xact_lock%")
		if _, revokeErr := lockTx.Exec(ctx, `INSERT INTO product_admission_revocations(revocation_id,evidence_id,revocation_sequence,source_type,actor_subject,reason,observed_at,created_at) VALUES($1,$2,1,'HUMAN','integration-test','concurrent revocation',now(),now())`, uuid.New(), validEvidence); revokeErr != nil {
			t.Fatal(revokeErr)
		}
		if commitErr := lockTx.Commit(ctx); commitErr != nil {
			t.Fatal(commitErr)
		}
		if transitionErr := <-result; !errors.Is(transitionErr, ErrConflict) {
			t.Fatalf("transition ignored a concurrent evidence revocation: %v", transitionErr)
		}
		unchanged, loadErr := repository.RechargeProductCatalogItem(ctx, item.CatalogItemID)
		var activeBindings int
		if loadErr != nil || unchanged.LifecycleStatus != "OFF_SALE" || unchanged.Version != item.Version {
			t.Fatalf("revocation race changed catalog state: %#v err=%v", unchanged, loadErr)
		}
		if countErr := pool.QueryRow(ctx, `SELECT count(*) FROM product_admission_bindings WHERE catalog_item_id=$1 AND effective_to IS NULL`, item.CatalogItemID).Scan(&activeBindings); countErr != nil || activeBindings != 0 {
			t.Fatalf("revocation race created an active binding: count=%d err=%v", activeBindings, countErr)
		}
	})
}

func createVerifiedRechargeProduct(t *testing.T, ctx context.Context, repository *PostgresRepository, suffix string, expiresAt time.Time) (RechargeProductCatalogItem, uuid.UUID) {
	t.Helper()
	productID := "REL_" + strings.ToUpper(strings.ReplaceAll(suffix, "-", "_")) + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	item, err := repository.CreateRechargeProduct(ctx, CreateRechargeProduct{
		ProductID:          productID,
		DisplayNameZhCN:    suffix,
		DisplayNameEN:      suffix,
		DisplayNameJA:      suffix,
		BasePoints:         100,
		BonusPoints:        10,
		CreditMicrocredits: 110000,
		ValidityDays:       30,
		InvoiceProductName: suffix,
		Mapping: RechargeProductMappingDraft{
			ProviderID:      "development_simulator",
			ProviderVersion: "development-simulator-v1",
			ExternalSKUBOM:  "REL_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
			AmountFen:       100,
			Currency:        "CNY",
			Channels:        []string{"ALIPAY"},
			AdmissionClass:  "TEST_ONLY",
		},
	})
	if err != nil {
		t.Fatalf("create product %s: %v", suffix, err)
	}
	verification, _, err := repository.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
	if err != nil {
		t.Fatalf("start verification %s: %v", suffix, err)
	}
	evidenceID, err := repository.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "integration-test", "CATALOG_FACT_CHECK", suffix, expiresAt)
	if err != nil {
		t.Fatalf("complete verification %s: %v", suffix, err)
	}
	return item, evidenceID
}

func waitForBlockedCatalogTransition(t *testing.T, ctx context.Context, pool *pgxpool.Pool, blockerPID uint32, queryPattern string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		err := pool.QueryRow(ctx, `SELECT EXISTS(
SELECT 1 FROM pg_stat_activity
WHERE datname=current_database() AND pid<>pg_backend_pid()
  AND state='active' AND wait_event_type='Lock'
  AND $1::integer=ANY(pg_blocking_pids(pid)) AND query LIKE $2
)`, blockerPID, queryPattern).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("catalog transition did not block on the expected row lock")
}
