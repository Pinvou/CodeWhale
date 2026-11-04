package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestProductAdmissionRetirementBindingPostgres(t *testing.T) {
	for _, source := range []string{"human", "provider"} {
		t.Run("historical-revoke-"+source+"-keeps-renewed-binding", func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			item, old := catalogAtomicFixture(t, r, ctx, "unpublish")
			var oldVerification uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT verification_id FROM product_admission_evidence WHERE evidence_id=$1`, old).Scan(&oldVerification); err != nil {
				t.Fatal(err)
			}
			newer := admissionRetirementRenew(t, r, ctx, item)
			before := admissionRetirementCurrentFacts(t, r, ctx, item.CatalogItemID)
			if source == "human" {
				body := catalogTransitionRequest{ExpectedVersion: item.Version, Reason: "controlled historical evidence retirement"}
				response := catalogAtomicAdmissionCall(r, item.CatalogItemID, oldVerification, "revoke", uuid.NewString(), "c06-history-"+uuid.NewString(), body)
				if response.Code != http.StatusOK {
					t.Fatal(response.Code, response.Body.String())
				}
			} else {
				admissionRetirementProviderCall(t, r, old, "controlled historical evidence retirement")
			}
			if after := admissionRetirementCurrentFacts(t, r, ctx, item.CatalogItemID); after != before {
				t.Fatalf("historical evidence revoke mutated renewed binding/control: before=%s after=%s", before, after)
			}
			var revoked, current int
			if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM product_admission_revocations WHERE evidence_id=$1),(SELECT count(*) FROM product_admission_bindings WHERE evidence_id=$2 AND effective_to IS NULL)`, old, newer).Scan(&revoked, &current); err != nil || revoked != 1 || current != 1 {
				t.Fatal(revoked, current, err)
			}
		})
	}
	admissionRetirementAdditionalCases(t)
	admissionRetirementRenewalDeadlineCases(t)
	admissionRetirementConcurrencyCases(t)
	admissionRetirementLockOrderCases(t)
	admissionRetirementRecoveryLockOrderCases(t)

}

func admissionRetirementRenew(t *testing.T, r *PostgresRepository, ctx context.Context, item RechargeProductCatalogItem) uuid.UUID {
	t.Helper()
	v, _, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := r.CompleteProductAdmissionVerification(ctx, v.VerificationID, "controlled-admission-verifier", "CONTROLLED", "controlled-renewal-"+uuid.NewString(), r.now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}
func admissionRetirementCurrentFacts(t *testing.T, r *PostgresRepository, ctx context.Context, id uuid.UUID) string {
	t.Helper()
	var facts string
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('item',to_jsonb(item),'bindings',(SELECT jsonb_agg(to_jsonb(b) ORDER BY admission_version) FROM product_admission_bindings b WHERE b.catalog_item_id=item.catalog_item_id),'controls',(SELECT jsonb_agg(to_jsonb(c) ORDER BY provider_id,provider_version) FROM payment_product_controls c WHERE c.product_id=item.product_id))::text FROM recharge_product_catalog_items item WHERE catalog_item_id=$1`, id).Scan(&facts)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(facts)
}

func admissionRetirementProviderCall(t *testing.T, r *PostgresRepository, evidence uuid.UUID, reason string) {
	t.Helper()
	token := tokenWalletWorkload(t)
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"admission-provider","service":"billing-admission-provider","permissions":["internal.billing.product-admission.revoke"]}]`)
	handler := NewService(r).WithRechargeProductCatalog(r).Handler("", nil)
	raw, _ := json.Marshal(map[string]string{"reason": reason})
	request := httptest.NewRequest("POST", "/internal/v1/product-admission-evidence/"+evidence.String()+"/revoke", strings.NewReader(string(raw)))
	request.Header.Set("Authorization", "Bearer "+token("admission-provider"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
}

func admissionRetirementAdditionalCases(t *testing.T) {
	t.Run("unbound-draft-history-cannot-pause-a-new-provider-draft", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, old := catalogAtomicFixture(t, r, ctx, "publish")
		next := item.Mapping
		next.ProviderID = "replacement-provider"
		next.ProviderVersion = "replacement-v2"
		updated, err := r.ExecuteRechargeProductDraftUpdate(ctx, RechargeProductDraftCommand{CatalogItemID: item.CatalogItemID, ExpectedVersion: item.Version, ExpectedDraftVersion: item.Mapping.DraftVersion, DisplayNameZhCN: item.DisplayNameZhCN, DisplayNameEN: item.DisplayNameEN, DisplayNameJA: item.DisplayNameJA, DisplayOrder: item.DisplayOrder, BasePoints: item.BasePoints, BonusPoints: item.BonusPoints, ValidityDays: item.ValidityDays, InvoiceProductName: item.InvoiceProductName, Mapping: next, IdempotencyKey: "c06-draft-" + uuid.NewString(), RequestHash: uuid.NewString(), Actor: uuid.NewString(), Reason: "controlled changed draft provider"})
		if err != nil {
			t.Fatal(err)
		}
		before := admissionRetirementCurrentFacts(t, r, ctx, item.CatalogItemID)
		if err = r.RevokeProductAdmission(ctx, old, "PROVIDER", "controlled-provider", "withdraw old unbound draft proof"); err != nil {
			t.Fatal(err)
		}
		if after := admissionRetirementCurrentFacts(t, r, ctx, item.CatalogItemID); after != before {
			t.Fatal("unbound evidence paused or changed current draft", updated.Version)
		}
	})
	for _, source := range []string{"human", "provider"} {
		t.Run("current-revoke-"+source+"-closes-exact-binding", func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			item, old := catalogAtomicFixture(t, r, ctx, "unpublish")
			var verification uuid.UUID
			var beforeEvidence string
			if err := r.pool.QueryRow(ctx, `SELECT verification_id,to_jsonb(e)::text FROM product_admission_evidence e WHERE evidence_id=$1`, old).Scan(&verification, &beforeEvidence); err != nil {
				t.Fatal(err)
			}
			current, err := r.RechargeProductCatalogItem(ctx, item.CatalogItemID)
			if err != nil {
				t.Fatal(err)
			}
			key, actor := "c06-current-"+uuid.NewString(), uuid.NewString()
			body := catalogTransitionRequest{ExpectedVersion: item.Version, Reason: "controlled current evidence revoke"}
			if source == "human" {
				response := catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, "revoke", actor, key, body)
				if response.Code != 200 {
					t.Fatal(response.Code, response.Body.String())
				}
				before := catalogAtomicFacts(t, r, ctx)
				replay := catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, "revoke", actor, key, body)
				if replay.Code != 200 || catalogAtomicBody(t, response.Body.Bytes()) != catalogAtomicBody(t, replay.Body.Bytes()) || before != catalogAtomicFacts(t, r, ctx) {
					t.Fatal("original-key replay changed facts or receipt")
				}
				body.Reason = "conflicting facts"
				conflict := catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, "revoke", actor, key, body)
				if conflict.Code != 409 || before != catalogAtomicFacts(t, r, ctx) {
					t.Fatal("same-key changed facts not rejected without writes")
				}
			} else {
				admissionRetirementProviderCall(t, r, old, body.Reason)
			}
			after, err := r.RechargeProductCatalogItem(ctx, item.CatalogItemID)
			if err != nil || after.Version != current.Version || after.LifecycleStatus != "ON_SALE" || after.AdmissionVersion != current.AdmissionVersion+1 {
				t.Fatal(after, err)
			}
			var binding int
			var enabled bool
			var evidence string
			if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM product_admission_bindings WHERE evidence_id=$1 AND effective_to IS NULL),(SELECT new_orders_enabled FROM payment_product_controls WHERE product_id=$2 AND provider_id=$3 AND provider_version=$4),(SELECT to_jsonb(e)::text FROM product_admission_evidence e WHERE evidence_id=$1)`, old, item.ProductID, item.Mapping.ProviderID, item.Mapping.ProviderVersion).Scan(&binding, &enabled, &evidence); err != nil || binding != 0 || enabled || evidence != beforeEvidence {
				t.Fatal(binding, enabled, evidence == beforeEvidence, err)
			}
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, gateErr := activeExternalProductForUpdate(ctx, tx, item.ProductID, item.Mapping.ProviderID, item.Mapping.ProviderVersion, item.Mapping.Channels[0], true, time.Now())
			_ = tx.Rollback(ctx)
			if !errors.Is(gateErr, pgx.ErrNoRows) {
				t.Fatal("revoked current evidence still passed production order admission", gateErr)
			}
			admissionRetirementRenew(t, r, ctx, item)
			var stillPaused bool
			if err = r.pool.QueryRow(ctx, `SELECT NOT new_orders_enabled FROM payment_product_controls WHERE product_id=$1`, item.ProductID).Scan(&stillPaused); err != nil || !stillPaused {
				t.Fatal("renewal reopened revoked product", stillPaused, err)
			}
		})
	}
	t.Run("historical-revoke-preserves-existing-pause-byte-for-byte", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, old := catalogAtomicFixture(t, r, ctx, "unpublish")
		admissionRetirementRenew(t, r, ctx, item)
		if _, err := r.pool.Exec(ctx, `INSERT INTO payment_product_controls(provider_id,provider_version,product_id,new_orders_enabled,reason_code,reason,version,updated_at) VALUES($1,$2,$3,FALSE,'MANUAL_SAFETY_PAUSE','existing pause',7,now())`, item.Mapping.ProviderID, item.Mapping.ProviderVersion, item.ProductID); err != nil {
			t.Fatal(err)
		}
		before := admissionRetirementCurrentFacts(t, r, ctx, item.CatalogItemID)
		if err := r.RevokeProductAdmission(ctx, old, "PROVIDER", "controlled-provider", "old proof invalid"); err != nil {
			t.Fatal(err)
		}
		if before != admissionRetirementCurrentFacts(t, r, ctx, item.CatalogItemID) {
			t.Fatal("historical revoke rewrote existing control")
		}
	})
	for _, table := range []string{"recharge_product_command_records", "recharge_product_audits"} {
		t.Run("historical-human-"+table+"-failure-rolls-back-revocation", func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			item, old := catalogAtomicFixture(t, r, ctx, "unpublish")
			admissionRetirementRenew(t, r, ctx, item)
			var verification uuid.UUID
			if err := r.pool.QueryRow(ctx, `SELECT verification_id FROM product_admission_evidence WHERE evidence_id=$1`, old).Scan(&verification); err != nil {
				t.Fatal(err)
			}
			before := catalogAtomicFacts(t, r, ctx)
			if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION c06_reject_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled receipt/audit failure'; END $$;CREATE TRIGGER c06_reject_write BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION c06_reject_write()`); err != nil {
				t.Fatal(err)
			}
			key, actor := "c06-history-failure-"+uuid.NewString(), uuid.NewString()
			body := catalogTransitionRequest{ExpectedVersion: item.Version, Reason: "controlled historical revoke"}
			response := catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, "revoke", actor, key, body)
			if response.Code != 500 || before != catalogAtomicFacts(t, r, ctx) {
				t.Fatal("failed command changed facts", response.Code, response.Body.String())
			}
			if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER c06_reject_write ON `+table+`;DROP FUNCTION c06_reject_write()`); err != nil {
				t.Fatal(err)
			}
			response = catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, "revoke", actor, key, body)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
}

// No expiry worker runs between the real deadline and completion. Renewal must
// still preserve the PAY-49 closed-until-reviewed boundary itself.
func admissionRetirementRenewalDeadlineCases(t *testing.T) {
	t.Run("expired-current-renewal-before-worker-cannot-reopen-sale", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, old := admissionRetirementExpiryFixture(t, r, ctx)
		admissionRetirementWaitExpired(t, r, ctx, old)
		evidence := admissionRetirementRenew(t, r, ctx, item)
		var paused bool
		var current int
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT NOT new_orders_enabled FROM payment_product_controls WHERE product_id=$1),FALSE),(SELECT count(*) FROM product_admission_bindings WHERE evidence_id=$2 AND effective_to IS NULL)`, item.ProductID, evidence).Scan(&paused, &current); err != nil || !paused || current != 1 {
			t.Fatal("late renewal bypassed expiry pause before worker", paused, current, err)
		}
	})
	t.Run("late-renewal-pause-write-failure-rolls-back-new-proof-and-binding", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, old := admissionRetirementExpiryFixture(t, r, ctx)
		verification, _, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
		if err != nil {
			t.Fatal(err)
		}
		admissionRetirementWaitExpired(t, r, ctx, old)
		before := catalogAtomicFacts(t, r, ctx)
		if _, err = retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION c06_reject_pause() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled pause failure'; END $$;CREATE TRIGGER c06_reject_pause BEFORE INSERT ON payment_product_controls FOR EACH ROW EXECUTE FUNCTION c06_reject_pause()`); err != nil {
			t.Fatal(err)
		}
		_, err = r.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "controlled-admission-verifier", "CONTROLLED", "controlled-pause-failure", time.Now().Add(time.Hour))
		if err == nil || before != catalogAtomicFacts(t, r, ctx) {
			t.Fatal("late renewal committed new proof without mandatory pause", err)
		}
		if _, err = retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER c06_reject_pause ON payment_product_controls;DROP FUNCTION c06_reject_pause()`); err != nil {
			t.Fatal(err)
		}
		if _, err = r.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "controlled-admission-verifier", "CONTROLLED", "controlled-pause-failure", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("renewal-waits-across-real-expiry-retains-pause", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, old := admissionRetirementExpiryFixture(t, r, ctx)
		verification, _, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
		if err != nil {
			t.Fatal(err)
		}
		blocker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err = blocker.Exec(ctx, `SELECT catalog_item_id FROM recharge_product_catalog_items WHERE catalog_item_id=$1 FOR NO KEY UPDATE`, item.CatalogItemID); err != nil {
			t.Fatal(err)
		}
		done := make(chan admissionRetirementRenewResult, 1)
		go func() {
			id, err := r.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "controlled-admission-verifier", "CONTROLLED", "controlled-lock-wait-expiry", time.Now().Add(time.Hour))
			done <- admissionRetirementRenewResult{id, err}
		}()
		deadline := time.Now().Add(time.Second)
		waiting := false
		for time.Now().Before(deadline) {
			if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='transactionid' AND query LIKE 'SELECT catalog_item_id FROM recharge_product_catalog_items%')`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !waiting {
			t.Fatal("renewal did not reach real pre-expiry catalog lock barrier")
		}
		admissionRetirementWaitExpired(t, r, ctx, old)
		if err = blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		result := <-done
		if result.err != nil {
			t.Fatal(result.err)
		}
		var paused bool
		if err = r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT NOT new_orders_enabled FROM payment_product_controls WHERE product_id=$1),FALSE)`, item.ProductID).Scan(&paused); err != nil || !paused {
			t.Fatal("transaction-start timestamp hid expiry during lock wait", paused, err)
		}
	})

	t.Run("unexpired-current-early-renewal-keeps-continuous-sale", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, _ := catalogAtomicFixture(t, r, ctx, "unpublish")
		evidence := admissionRetirementRenew(t, r, ctx, item)
		var enabled bool
		var current int
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT new_orders_enabled FROM payment_product_controls WHERE product_id=$1),TRUE),(SELECT count(*) FROM product_admission_bindings WHERE evidence_id=$2 AND effective_to IS NULL)`, item.ProductID, evidence).Scan(&enabled, &current); err != nil || !enabled || current != 1 {
			t.Fatal("early valid renewal interrupted sale", enabled, current, err)
		}
	})
}

func admissionRetirementExpiryFixture(t *testing.T, r *PostgresRepository, ctx context.Context) (RechargeProductCatalogItem, uuid.UUID) {
	t.Helper()
	item, _ := catalogAtomicFixture(t, r, ctx, "delete-draft")
	v, _, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := r.CompleteProductAdmissionVerification(ctx, v.VerificationID, "controlled-admission-verifier", "CONTROLLED", "controlled-short-validity-"+uuid.NewString(), time.Now().Add(1500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	item, err = r.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &old)
	if err != nil {
		t.Fatal(err)
	}
	return item, old
}
func admissionRetirementWaitExpired(t *testing.T, r *PostgresRepository, ctx context.Context, evidence uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var expired bool
		if err := r.pool.QueryRow(ctx, `SELECT expires_at<=clock_timestamp() FROM product_admission_evidence WHERE evidence_id=$1`, evidence).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("real evidence deadline not reached")
}

type admissionRetirementRenewResult struct {
	id  uuid.UUID
	err error
}

func admissionRetirementGate(t *testing.T, r *PostgresRepository, ctx context.Context, table string) (func(), func(int)) {
	t.Helper()
	gate := time.Now().UnixNano()
	control, err := r.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = control.Exec(ctx, `SELECT pg_advisory_lock($1)`, gate); err != nil {
		t.Fatal(err)
	}
	statement := fmt.Sprintf(`CREATE FUNCTION c06_retirement_gate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(%d::bigint); RETURN NEW; END $$;CREATE TRIGGER c06_retirement_gate BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION c06_retirement_gate()`, gate, table)
	if _, err = retainedFinanceOwnerExec(t, r, ctx, statement); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_, err := control.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, gate)
			control.Release()
			if err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(release)
	wait := func(count int) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			var n int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event='advisory'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n >= count {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("real PostgreSQL lock cutpoint not reached", count)
	}
	return release, wait
}
func admissionRetirementDropGate(t *testing.T, r *PostgresRepository, ctx context.Context, table string) {
	t.Helper()
	if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER c06_retirement_gate ON `+table+`;DROP FUNCTION c06_retirement_gate()`); err != nil {
		t.Fatal(err)
	}
}
func admissionRetirementConcurrencyCases(t *testing.T) {
	for _, renewFirst := range []bool{true, false} {
		name := "revoke-before-renewal-keeps-pause"
		if renewFirst {
			name = "renewal-before-historical-revoke-keeps-current-sale"
		}
		t.Run(name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			item, old := catalogAtomicFixture(t, r, ctx, "unpublish")
			verification, _, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
			if err != nil {
				t.Fatal(err)
			}
			table := "product_admission_revocations"
			if renewFirst {
				table = "product_admission_bindings"
			}
			release, wait := admissionRetirementGate(t, r, ctx, table)
			renew := make(chan admissionRetirementRenewResult, 1)
			revoke := make(chan error, 1)
			complete := func() {
				id, err := r.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "controlled-admission-verifier", "CONTROLLED", "controlled-concurrent-renewal", time.Now().Add(time.Hour))
				renew <- admissionRetirementRenewResult{id, err}
			}
			retire := func() {
				revoke <- r.RevokeProductAdmission(ctx, old, "PROVIDER", "controlled-provider", "controlled concurrent revoke")
			}
			if renewFirst {
				go complete()
			} else {
				go retire()
			}
			wait(1)
			if renewFirst {
				go retire()
			} else {
				go complete()
			}
			wait(2)
			release()
			result := <-renew
			if err := <-revoke; err != nil || result.err != nil {
				t.Fatal(err, result.err)
			}
			admissionRetirementDropGate(t, r, ctx, table)
			var enabled bool
			var current int
			if err := r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT new_orders_enabled FROM payment_product_controls WHERE product_id=$1),TRUE),(SELECT count(*) FROM product_admission_bindings WHERE evidence_id=$2 AND effective_to IS NULL)`, item.ProductID, result.id).Scan(&enabled, &current); err != nil || enabled != renewFirst || current != 1 {
				t.Fatal(enabled, current, err)
			}
		})
	}
	for _, renewFirst := range []bool{true, false} {
		name := "expiry-before-renewal-keeps-pause"
		if renewFirst {
			name = "renewal-holds-admission-expiry-skips-stale-candidate"
		}
		t.Run(name, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			item, old := admissionRetirementExpiryFixture(t, r, ctx)
			verification, _, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
			if err != nil {
				t.Fatal(err)
			}
			admissionRetirementWaitExpired(t, r, ctx, old)
			table := "payment_product_controls"
			if renewFirst {
				table = "product_admission_bindings"
			}
			release, wait := admissionRetirementGate(t, r, ctx, table)
			renew := make(chan admissionRetirementRenewResult, 1)
			expiry := make(chan struct {
				n   int
				err error
			}, 1)
			complete := func() {
				id, err := r.CompleteProductAdmissionVerification(ctx, verification.VerificationID, "controlled-admission-verifier", "CONTROLLED", "controlled-expiry-renewal", time.Now().Add(time.Hour))
				renew <- admissionRetirementRenewResult{id, err}
			}
			expire := func() {
				n, err := r.ExpireProductAdmissions(ctx, 100)
				expiry <- struct {
					n   int
					err error
				}{n, err}
			}
			if renewFirst {
				go complete()
				wait(1)
				go expire()
				result := <-expiry
				if result.err != nil || result.n != 0 {
					t.Fatal(result)
				}
				release()
			} else {
				go expire()
				wait(1)
				go complete()
				wait(2)
				release()
				result := <-expiry
				if result.err != nil || result.n != 1 {
					t.Fatal(result)
				}
			}
			result := <-renew
			if result.err != nil {
				t.Fatal(result.err)
			}
			admissionRetirementDropGate(t, r, ctx, table)
			before := admissionRetirementCurrentFacts(t, r, ctx, item.CatalogItemID)
			if n, err := r.ExpireProductAdmissions(ctx, 100); err != nil || n != 0 || before != admissionRetirementCurrentFacts(t, r, ctx, item.CatalogItemID) {
				t.Fatal("expiry reconsidered historical expired binding", n, err)
			}
			var enabled bool
			var current int
			if err := r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT new_orders_enabled FROM payment_product_controls WHERE product_id=$1),TRUE),(SELECT count(*) FROM product_admission_bindings WHERE evidence_id=$2 AND effective_to IS NULL)`, item.ProductID, result.id).Scan(&enabled, &current); err != nil || enabled || current != 1 {
				t.Fatal(enabled, current, err)
			}
		})
	}
}

func admissionRetirementLockOrderCases(t *testing.T) {
	t.Run("busy-expiry-candidate-does-not-hide-later-expired-product", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		first, oldFirst := admissionRetirementExpiryFixture(t, r, ctx)
		_, oldSecond := admissionRetirementExpiryFixture(t, r, ctx)
		admissionRetirementWaitExpired(t, r, ctx, oldSecond)
		blocker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if err = lockTransactionKey(ctx, blocker, "catalog.admission", first.CatalogItemID.String()); err != nil {
			t.Fatal(err)
		}
		n, err := r.ExpireProductAdmissions(ctx, 1)
		if err != nil || n != 1 {
			t.Fatal("skip-busy consumed limit instead of processing later expired candidate", n, err)
		}
		var currentFirst, currentSecond int
		if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM product_admission_bindings WHERE evidence_id=$1 AND effective_to IS NULL),(SELECT count(*) FROM product_admission_bindings WHERE evidence_id=$2 AND effective_to IS NULL)`, oldFirst, oldSecond).Scan(&currentFirst, &currentSecond); err != nil || currentFirst != 1 || currentSecond != 0 {
			t.Fatal(currentFirst, currentSecond, err)
		}
		if err = blocker.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if n, err = r.ExpireProductAdmissions(ctx, 1); err != nil || n != 1 {
			t.Fatal("released busy expiry not processed", n, err)
		}
	})
	t.Run("human-revoke-and-expiry-share-admission-before-catalog-binding-order", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, old := admissionRetirementExpiryFixture(t, r, ctx)
		admissionRetirementWaitExpired(t, r, ctx, old)
		var verification uuid.UUID
		if err := r.pool.QueryRow(ctx, `SELECT verification_id FROM product_admission_evidence WHERE evidence_id=$1`, old).Scan(&verification); err != nil {
			t.Fatal(err)
		}
		blocker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err = blocker.Exec(ctx, `SELECT catalog_item_id FROM recharge_product_catalog_items WHERE catalog_item_id=$1 FOR NO KEY UPDATE`, item.CatalogItemID); err != nil {
			t.Fatal(err)
		}
		actor, key := uuid.NewString(), "c06-human-lock-order-"+uuid.NewString()
		body := catalogTransitionRequest{ExpectedVersion: item.Version, Reason: "controlled concurrent revoke and expiry"}
		human := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			human <- catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, "revoke", actor, key, body)
		}()
		deadline := time.Now().Add(4 * time.Second)
		waiting := false
		for time.Now().Before(deadline) {
			if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='transactionid' AND query LIKE 'SELECT version FROM recharge_product_catalog_items%')`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !waiting {
			t.Fatal("human command did not reach actual catalog row barrier with admission held")
		}
		type expiryResult struct {
			n   int
			err error
		}
		expiry := make(chan expiryResult, 1)
		go func() { n, err := r.ExpireProductAdmissions(ctx, 100); expiry <- expiryResult{n, err} }()
		var result expiryResult
		completed := false
		oldBlocked := false
		deadline = time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case result = <-expiry:
				completed = true
			default:
			}
			if completed {
				break
			}
			if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query NOT LIKE 'SELECT version FROM recharge_product_catalog_items%')`).Scan(&oldBlocked); err != nil {
				t.Fatal(err)
			}
			if oldBlocked {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !completed && !oldBlocked {
			t.Fatal("expiry neither skipped the busy admission nor reached the original catalog/binding inversion")
		}
		if err = blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if !completed {
			result = <-expiry
		}
		response := <-human
		if result.err != nil || result.n != 0 || response.Code != 200 {
			t.Fatalf("human/expiry lock-order regression: expiry=%d err=%v human=%d body=%s oldBindingFirstCutpoint=%v", result.n, result.err, response.Code, response.Body.String(), oldBlocked)
		}
		var receipts, audits, revocations int
		if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM recharge_product_command_records WHERE idempotency_key=$1),(SELECT count(*) FROM recharge_product_audits WHERE idempotency_key=$1),(SELECT count(*) FROM product_admission_revocations WHERE evidence_id=$2)`, key, old).Scan(&receipts, &audits, &revocations); err != nil || receipts != 1 || audits != 1 || revocations != 1 {
			t.Fatal(receipts, audits, revocations, err)
		}
		before := catalogAtomicFacts(t, r, ctx)
		replay := catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, "revoke", actor, key, body)
		if replay.Code != 200 || catalogAtomicBody(t, response.Body.Bytes()) != catalogAtomicBody(t, replay.Body.Bytes()) || before != catalogAtomicFacts(t, r, ctx) {
			t.Fatal("serial winner's immutable receipt/audit changed on replay")
		}
	})
}

func admissionRetirementResumeFixture(t *testing.T, r *PostgresRepository, ctx context.Context, validity ...time.Duration) (RechargeProductCatalogItem, uuid.UUID, ProductResumeVersions) {
	t.Helper()
	input := validRechargeProductForEditTests()
	input.ProductID = "C06_resume_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	input.Mapping.ExternalSKUBOM = input.ProductID
	input.Mapping.AdmissionClass = "FORMAL"
	item, err := r.CreateRechargeProduct(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	lifetime := time.Hour
	if len(validity) == 1 {
		lifetime = validity[0]
	}
	evidence := completeManualAdmissionFixture(t, r, ctx, item, time.Now().Add(lifetime))
	item, err = r.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &evidence)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.pool.Exec(ctx, `INSERT INTO payment_provider_controls(provider_id,provider_version,new_orders_enabled,reason_code,reason,version,updated_at) VALUES($1,$2,TRUE,'AVAILABLE','',1,now());`, item.Mapping.ProviderID, item.Mapping.ProviderVersion)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.pool.Exec(ctx, `INSERT INTO payment_product_controls(provider_id,provider_version,product_id,new_orders_enabled,reason_code,reason,version,updated_at) VALUES($1,$2,$3,FALSE,'MANUAL_SAFETY_PAUSE','controlled existing pause',1,now())`, item.Mapping.ProviderID, item.Mapping.ProviderVersion, item.ProductID)
	if err != nil {
		t.Fatal(err)
	}
	expected := ProductResumeVersions{ProviderID: item.Mapping.ProviderID, ProviderVersion: item.Mapping.ProviderVersion, ProductID: item.ProductID, ProviderControlVersion: 1, ProductControlVersion: 1, CatalogItemID: item.CatalogItemID, CatalogVersion: item.Version, AdmissionVersion: item.AdmissionVersion}
	err = r.pool.QueryRow(ctx, `SELECT id,mapping_version FROM payment_product_mappings WHERE product_id=$1 AND effective_to IS NULL`, item.ProductID).Scan(&expected.MappingID, &expected.MappingVersion)
	if err != nil {
		t.Fatal(err)
	}
	var verificationID uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT verification_id FROM product_admission_evidence WHERE evidence_id=$1`, evidence).Scan(&verificationID); err != nil {
		t.Fatal(err)
	}
	return item, verificationID, expected
}

func admissionRetirementRecoveryLockOrderCases(t *testing.T) {

	t.Run("valid-recovery-still-commits-control-execution-and-outbox", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, _, expected := admissionRetirementResumeFixture(t, r, ctx)
		order, provider, probe := admissionRetirementFreshResumeProbe(t, r, ctx, item, &expected)
		request, actor, approver := uuid.New(), uuid.New(), uuid.New()
		result, err := r.executeProductResume(ctx, expected, probe, request, actor, approver, "controlled independently approved recovery", order, provider)
		if err != nil || result.Status != "RESUMED" {
			t.Fatal(result, err)
		}
		var enabled bool
		var version, executions, outbox int
		if err = r.pool.QueryRow(ctx, `SELECT (SELECT new_orders_enabled FROM payment_product_controls WHERE product_id=$1),(SELECT version FROM payment_product_controls WHERE product_id=$1),(SELECT count(*) FROM payment_product_resume_executions WHERE id=$2 AND approval_request_id=$3),(SELECT count(*) FROM outbox_events WHERE aggregate_id=$2 AND event_type='billing.payment-product.resumed.v1')`, item.ProductID, result.ExecutionID, request).Scan(&enabled, &version, &executions, &outbox); err != nil || !enabled || version != 2 || executions != 1 || outbox != 1 {
			t.Fatal(enabled, version, executions, outbox, err)
		}
	})

	t.Run("recovery-rechecks-admission-validity-after-control-wait", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, _, expected := admissionRetirementResumeFixture(t, r, ctx, 2*time.Second)
		order, provider, probe := admissionRetirementFreshResumeProbe(t, r, ctx, item, &expected)
		var evidence uuid.UUID
		if err := r.pool.QueryRow(ctx, `SELECT evidence_id FROM product_admission_bindings WHERE catalog_item_id=$1 AND effective_to IS NULL`, item.CatalogItemID).Scan(&evidence); err != nil {
			t.Fatal(err)
		}
		blocker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err = blocker.Exec(ctx, `SELECT provider_id FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2 FOR UPDATE`, item.Mapping.ProviderID, item.Mapping.ProviderVersion); err != nil {
			t.Fatal(err)
		}
		restore := make(chan error, 1)
		go func() {
			_, err := r.executeProductResume(ctx, expected, probe, uuid.New(), uuid.New(), uuid.New(), "controlled validity lock wait", order, provider)
			restore <- err
		}()
		deadline := time.Now().Add(4 * time.Second)
		waiting := false
		for time.Now().Before(deadline) {
			if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT new_orders_enabled,version FROM payment_provider_controls%')`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !waiting {
			t.Fatal("restore did not reach actual control wait before expiry")
		}
		admissionRetirementWaitExpired(t, r, ctx, evidence)
		if err = blocker.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err = <-restore; !errors.Is(err, ErrApprovalSnapshotMismatch) {
			t.Fatal("expired admission must invalidate snapshot before paid probe execution", err)
		}
		var enabled bool
		var executions int
		if err = r.pool.QueryRow(ctx, `SELECT (SELECT new_orders_enabled FROM payment_product_controls WHERE product_id=$1),(SELECT count(*) FROM payment_product_resume_executions WHERE product_id=$1)`, item.ProductID).Scan(&enabled, &executions); err != nil || enabled || executions != 0 {
			t.Fatal(enabled, executions, err)
		}
	})
	t.Run("human-revoke-does-not-wait-on-recovery-controls", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, verification, expected := admissionRetirementResumeFixture(t, r, ctx)
		release, wait := admissionRetirementGate(t, r, ctx, "product_admission_revocations")
		human := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			human <- catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, "revoke", uuid.NewString(), "c06-resume-human-"+uuid.NewString(), catalogTransitionRequest{ExpectedVersion: item.Version, Reason: "controlled concurrent restore"})
		}()
		wait(1)
		restore := make(chan error, 1)
		go func() {
			_, err := r.executeProductResume(ctx, expected, uuid.New(), uuid.New(), uuid.New(), uuid.New(), "controlled restore lock boundary", PaymentOrder{}, ProviderOrder{})
			restore <- err
		}()
		// Observe an actual lock wait, including the old catalogue-first inversion.
		// The assertion is on independent control-row availability, not the PG victim.
		admissionRetirementWaitRecoveryLock(t, r, ctx)
		checker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, controlErr := checker.Exec(ctx, `SELECT provider_id FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2 FOR UPDATE NOWAIT`, item.Mapping.ProviderID, item.Mapping.ProviderVersion)
		if controlErr == nil {
			_, controlErr = checker.Exec(ctx, `SELECT product_id FROM payment_product_controls WHERE provider_id=$1 AND provider_version=$2 AND product_id=$3 FOR UPDATE NOWAIT`, item.Mapping.ProviderID, item.Mapping.ProviderVersion, item.ProductID)
		}
		_ = checker.Rollback(ctx)
		release()
		response := <-human
		resumeErr := <-restore
		if controlErr != nil {
			t.Fatalf("recovery held controls while waiting on revoke catalogue: %v; human=%d restore=%v", controlErr, response.Code, resumeErr)
		}
		if response.Code != http.StatusOK {
			t.Fatal(response.Code, response.Body.String(), resumeErr)
		}
		if resumeErr == nil || strings.Contains(resumeErr.Error(), "40P01") {
			t.Fatal("stale/no-probe restore must fail closed without a deadlock", resumeErr)
		}
		admissionRetirementDropGate(t, r, ctx, "product_admission_revocations")
		admissionRetirementAssertPaused(t, r, ctx, item)
	})
	t.Run("recovery-reserves-catalogue-before-waiting-on-controls", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, verification, expected := admissionRetirementResumeFixture(t, r, ctx)
		blocker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err = blocker.Exec(ctx, `SELECT provider_id FROM payment_provider_controls WHERE provider_id=$1 AND provider_version=$2 FOR UPDATE`, item.Mapping.ProviderID, item.Mapping.ProviderVersion); err != nil {
			t.Fatal(err)
		}
		restore := make(chan error, 1)
		go func() {
			_, err := r.executeProductResume(ctx, expected, uuid.New(), uuid.New(), uuid.New(), uuid.New(), "controlled restore waiting on provider", PaymentOrder{}, ProviderOrder{})
			restore <- err
		}()
		deadline := time.Now().Add(4 * time.Second)
		waiting := false
		for time.Now().Before(deadline) {
			if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT new_orders_enabled,version FROM payment_provider_controls%')`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !waiting {
			t.Fatal("restore did not reach actual provider row barrier")
		}
		// A separate contender cannot take the catalogue during recovery. This also
		// holds for catalogue writers that do not acquire the advisory admission lock.
		checker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, catalogErr := checker.Exec(ctx, `SELECT catalog_item_id FROM recharge_product_catalog_items WHERE catalog_item_id=$1 FOR UPDATE NOWAIT`, item.CatalogItemID)
		_ = checker.Rollback(ctx)
		human := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			human <- catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, "revoke", uuid.NewString(), "c06-control-first-"+uuid.NewString(), catalogTransitionRequest{ExpectedVersion: item.Version, Reason: "controlled revoke during recovery"})
		}()
		if err = blocker.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		resumeErr := <-restore
		response := <-human
		if catalogErr == nil || !strings.Contains(catalogErr.Error(), "55P03") {
			t.Fatal("recovery failed to reserve current catalogue before waiting on controls", catalogErr)
		}
		if resumeErr == nil || strings.Contains(resumeErr.Error(), "40P01") || response.Code != http.StatusOK {
			t.Fatal("restore must reject missing probe while human revoke succeeds", resumeErr, response.Code, response.Body.String())
		}
		admissionRetirementAssertPaused(t, r, ctx, item)
	})
}
func admissionRetirementWaitRecoveryLock(t *testing.T, r *PostgresRepository, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND (query LIKE 'SELECT pg_advisory_xact_lock(hashtextextended%' OR query LIKE 'SELECT item.catalog_item_id,item.version,item.admission_version%'))`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("real recovery did not reach its lock boundary")
}
func admissionRetirementAssertPaused(t *testing.T, r *PostgresRepository, ctx context.Context, item RechargeProductCatalogItem) {
	t.Helper()
	var enabled bool
	var bindings, executions int
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT new_orders_enabled FROM payment_product_controls WHERE product_id=$1),(SELECT count(*) FROM product_admission_bindings WHERE catalog_item_id=$2 AND effective_to IS NULL),(SELECT count(*) FROM payment_product_resume_executions WHERE product_id=$1)`, item.ProductID, item.CatalogItemID).Scan(&enabled, &bindings, &executions); err != nil || enabled || bindings != 0 || executions != 0 {
		t.Fatal("revoke must keep admission closed and failed restore must not write an execution", enabled, bindings, executions, err)
	}
}

// This fixture exercises the real domain transaction and production observation
// writer. Its order is controlled pending data, not a live provider payment or
// the cross-service two-person approval protocol.
func admissionRetirementFreshResumeProbe(t *testing.T, r *PostgresRepository, ctx context.Context, item RechargeProductCatalogItem, expected *ProductResumeVersions) (PaymentOrder, ProviderOrder, uuid.UUID) {
	t.Helper()
	owner := uuid.New()
	wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", owner)
	if err != nil {
		t.Fatal(err)
	}
	products, err := r.ListProducts(ctx, "CNY", item.Mapping.Channels[0])
	if err != nil {
		t.Fatal(err)
	}
	var product Product
	for _, published := range products {
		if published.ProductID == item.ProductID {
			product = published
			break
		}
	}
	if product.ProductID != item.ProductID {
		t.Fatal("controlled probe must use the actual published product")
	}
	now := r.now()
	order := PaymentOrder{ID: uuid.New(), MerchantOrderNo: "c06-probe-" + uuid.NewString(), RequesterID: owner, WalletID: wallet.ID, ProductID: item.ProductID, Channel: item.Mapping.Channels[0], AmountFen: item.Mapping.AmountFen, Currency: "CNY", CreditMicrocredits: product.CreditMicrocredits, Status: "PENDING", FulfillmentStatus: "PENDING", CloseStatus: "OPEN", ExpiresAt: now.Add(time.Hour), ProviderID: item.Mapping.ProviderID, ProviderVersion: item.Mapping.ProviderVersion}
	snapshot, _ := json.Marshal(product)
	_, err = r.pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,$5,$6,$7,$8,$9,$10,'PENDING','PENDING',$11,$11,$11::timestamptz+interval '72 hours',$11::timestamptz+interval '8 days',$12,$12,$13,$14)`, order.ID, order.MerchantOrderNo, owner, wallet.ID, order.ProductID, snapshot, order.Channel, order.AmountFen, order.Currency, order.CreditMicrocredits, order.ExpiresAt, now, order.ProviderID, order.ProviderVersion)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := r.beginProviderAttempt(ctx, order.ProviderID, order.ProviderVersion, "PAYMENT_STATUS_QUERY", order.Channel, "controlled-provider-query", ProviderCallScope{OrderID: order.ID, ProductID: order.ProductID, SubjectID: owner, TrustedProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.finishProviderAttempt(ctx, attempt, "SUCCESS", nil); err != nil {
		t.Fatal(err)
	}
	probe, err := latestTrustedProbeForOrder(ctx, r.pool, order.ID)
	if err != nil || probe == nil {
		t.Fatal(probe, err)
	}
	expected.ProbeOrderID = order.ID
	return order, ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "PENDING", ProviderStatus: "PENDING", ProviderTraceID: "controlled-query-trace", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency}, *probe
}
