package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Identity decoding is controlled; catalogue operations, SQL failures and
// transaction recovery use the public migrations and real billing_service.
func TestRechargeProductCommandAtomicityPostgres(t *testing.T) {
	for _, action := range []string{"publish", "unpublish", "delete-draft", "delete-published"} {
		for _, failure := range []string{"receipt", "audit"} {
			t.Run(action+"-"+failure+"-rollback-and-recovery", func(t *testing.T) {
				r, ctx := retainedFinanceRepository(t)
				item, evidence := catalogAtomicFixture(t, r, ctx, action)
				actor, key := uuid.NewString(), "catalog-atomic-"+uuid.NewString()
				body := catalogTransitionRequest{ExpectedVersion: item.Version, Reason: "controlled catalogue lifecycle command"}
				if action == "publish" {
					body.EvidenceID = &evidence
				}
				before := catalogAtomicFacts(t, r, ctx)
				table := "recharge_product_command_records"
				if failure == "audit" {
					table = "recharge_product_audits"
				}
				if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION c05_reject_command_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled catalog command write failure'; END $$; CREATE TRIGGER c05_command_write_failure BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION c05_reject_command_write()`); err != nil {
					t.Fatal(err)
				}
				response := catalogAtomicCall(r, item.CatalogItemID, action, actor, key, body)
				if response.Code != http.StatusInternalServerError {
					t.Fatalf("injected %s failure status=%d body=%s", failure, response.Code, response.Body.String())
				}
				if after := catalogAtomicFacts(t, r, ctx); after != before {
					current, _ := r.RechargeProductCatalogItem(ctx, item.CatalogItemID)
					t.Fatalf("failed command committed catalogue state without receipt/audit: before=%s/%d after=%s/%d", item.LifecycleStatus, item.Version, current.LifecycleStatus, current.Version)
				}
				if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER c05_command_write_failure ON `+table+`; DROP FUNCTION c05_reject_command_write()`); err != nil {
					t.Fatal(err)
				}
				success := catalogAtomicCall(r, item.CatalogItemID, action, actor, key, body)
				if success.Code != http.StatusOK {
					t.Fatalf("same-key recovery status=%d body=%s", success.Code, success.Body.String())
				}
				var result RechargeProductCatalogItem
				if err := json.Unmarshal(success.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				want := "DELETED"
				if action == "publish" {
					want = "ON_SALE"
				}
				if action == "unpublish" {
					want = "OFF_SALE"
				}
				if result.Version != item.Version+1 || result.LifecycleStatus != want {
					t.Fatal(result)
				}
				committed := catalogAtomicFacts(t, r, ctx)
				replay := catalogAtomicCall(r, item.CatalogItemID, action, actor, key, body)
				var replayItem RechargeProductCatalogItem
				if replay.Code != 200 || json.Unmarshal(replay.Body.Bytes(), &replayItem) != nil {
					t.Fatal(replay.Code, replay.Body.String())
				}
				left, _ := json.Marshal(result)
				right, _ := json.Marshal(replayItem)
				if string(left) != string(right) || committed != catalogAtomicFacts(t, r, ctx) {
					t.Fatal("replay changed receipt or wrote state")
				}
				body.Reason = "changed facts under the original key"
				if conflict := catalogAtomicCall(r, item.CatalogItemID, action, actor, key, body); conflict.Code != http.StatusConflict {
					t.Fatal("changed-facts key reuse", conflict.Code, conflict.Body.String())
				}
				if committed != catalogAtomicFacts(t, r, ctx) {
					t.Fatal("conflict wrote state")
				}
				var receipts, audits int
				if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM recharge_product_command_records WHERE idempotency_key=$1),(SELECT count(*) FROM recharge_product_audits WHERE idempotency_key=$1)`, key).Scan(&receipts, &audits); err != nil || receipts != 1 || audits != 1 {
					t.Fatal(receipts, audits, err)
				}
			})
		}
	}
	catalogAtomicRouteCase(t)
	catalogAtomicAdmissionCases(t)
	catalogAtomicVersionCases(t)
	t.Run("concurrent-identical-command-replays-once", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, _ := catalogAtomicFixture(t, r, ctx, "delete-draft")
		actor, key := uuid.NewString(), "catalog-concurrent-"+uuid.NewString()
		body := catalogTransitionRequest{ExpectedVersion: item.Version, Reason: "controlled concurrent command"}
		start := make(chan struct{})
		results := make(chan *httptest.ResponseRecorder, 4)
		var group sync.WaitGroup
		for range 4 {
			group.Add(1)
			go func() {
				defer group.Done()
				<-start
				results <- catalogAtomicCall(r, item.CatalogItemID, "delete-draft", actor, key, body)
			}()
		}
		close(start)
		group.Wait()
		close(results)
		var expected string
		for response := range results {
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			var got RechargeProductCatalogItem
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(got)
			if expected != "" && expected != string(raw) {
				t.Fatal("concurrent receipts diverged")
			}
			expected = string(raw)
		}
		current, err := r.RechargeProductCatalogItem(ctx, item.CatalogItemID)
		if err != nil || current.Version != item.Version+1 {
			t.Fatal(current, err)
		}
		var count int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM recharge_product_audits WHERE idempotency_key=$1`, key).Scan(&count); err != nil || count != 1 {
			t.Fatal(count, err)
		}
	})
}

func catalogAtomicFixture(t *testing.T, r *PostgresRepository, ctx context.Context, action string) (RechargeProductCatalogItem, uuid.UUID) {
	t.Helper()
	input := validRechargeProductForEditTests()
	input.ProductID = "C05_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	input.Mapping.ExternalSKUBOM = input.ProductID
	item, err := r.CreateRechargeProduct(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if action == "delete-draft" {
		return item, uuid.Nil
	}
	v, _, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := r.CompleteProductAdmissionVerification(ctx, v.VerificationID, "controlled-issuer", "CONTROLLED", "controlled-source-"+uuid.NewString(), r.now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if action == "publish" {
		return item, evidence
	}
	item, err = r.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", &evidence)
	if err != nil {
		t.Fatal(err)
	}
	if action == "delete-published" {
		item, err = r.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "OFF_SALE", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	return item, evidence
}

func catalogAtomicCall(r *PostgresRepository, id uuid.UUID, action, actor, key string, body catalogTransitionRequest) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	route := action
	if strings.HasPrefix(action, "delete-") {
		route = "delete"
	}
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/recharge-products/"+id.String()+"/"+route, strings.NewReader(string(raw)))
	request.SetPathValue("catalog_item_id", id.String())
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Authorization", "Bearer controlled")
	s := NewService(r).WithRechargeProductCatalog(r)
	handler := s.adminDeleteRechargeProduct
	if action == "publish" {
		handler = s.adminPublishRechargeProduct
	}
	if action == "unpublish" {
		handler = s.adminUnpublishRechargeProduct
	}
	response := httptest.NewRecorder()
	identity.Middleware(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "controlled", Subject: actor, AuthenticatedAt: r.now()}, nil
	}, http.HandlerFunc(handler)).ServeHTTP(response, request)
	return response
}

func catalogAtomicFacts(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	facts := map[string]json.RawMessage{}
	for _, table := range []string{"recharge_products", "recharge_product_catalog_items", "recharge_product_mapping_drafts", "payment_product_mappings", "product_admission_verifications", "product_admission_evidence", "product_admission_bindings", "product_admission_revocations", "payment_product_controls", "recharge_product_command_records", "recharge_product_audits", "wallets", "journal_entries", "ledger_postings", "receivables", "payment_orders", "payment_transactions", "entitlement_packages", "entitlement_components"} {
		var raw []byte
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb) FROM `+table+` t`).Scan(&raw); err != nil {
			t.Fatal(fmt.Errorf("snapshot %s: %w", table, err))
		}
		facts[table] = raw
	}
	raw, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func catalogAtomicAdmissionCases(t *testing.T) {
	for _, action := range []string{"start", "retry", "revoke"} {
		for _, failure := range []string{"receipt", "audit"} {
			t.Run("admission-"+action+"-"+failure+"-rollback-and-recovery", func(t *testing.T) {
				r, ctx := retainedFinanceRepository(t)
				item, verification := catalogAtomicAdmissionFixture(t, r, ctx, action)
				actor, key := uuid.NewString(), "catalog-admission-"+uuid.NewString()
				body := catalogTransitionRequest{ExpectedVersion: item.Version, Reason: "controlled admission management command"}
				before := catalogAtomicFacts(t, r, ctx)
				table := "recharge_product_command_records"
				if failure == "audit" {
					table = "recharge_product_audits"
				}
				if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION c05_reject_command_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled catalog command write failure'; END $$; CREATE TRIGGER c05_command_write_failure BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION c05_reject_command_write()`); err != nil {
					t.Fatal(err)
				}
				failed := catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, action, actor, key, body)
				if failed.Code != 500 {
					t.Fatal(failed.Code, failed.Body.String())
				}
				if before != catalogAtomicFacts(t, r, ctx) {
					t.Fatal("failed admission command committed effects without receipt/audit")
				}
				if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER c05_command_write_failure ON `+table+`; DROP FUNCTION c05_reject_command_write()`); err != nil {
					t.Fatal(err)
				}
				success := catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, action, actor, key, body)
				want := 202
				if action == "revoke" {
					want = 200
				}
				if success.Code != want {
					t.Fatal(success.Code, success.Body.String())
				}
				committed := catalogAtomicFacts(t, r, ctx)
				replay := catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, action, actor, key, body)
				if replay.Code != want || catalogAtomicBody(t, success.Body.Bytes()) != catalogAtomicBody(t, replay.Body.Bytes()) || committed != catalogAtomicFacts(t, r, ctx) {
					t.Fatal("admission replay changed original result or facts")
				}
				body.Reason = "different original-key facts"
				if response := catalogAtomicAdmissionCall(r, item.CatalogItemID, verification, action, actor, key, body); response.Code != 409 {
					t.Fatal(response.Code, response.Body.String())
				}
				if committed != catalogAtomicFacts(t, r, ctx) {
					t.Fatal("admission key conflict wrote facts")
				}
				var receipts, audits int
				var ttl bool
				if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM recharge_product_command_records WHERE idempotency_key=$1),(SELECT count(*) FROM recharge_product_audits WHERE idempotency_key=$1),(SELECT expires_at-created_at=interval '24 hours' FROM recharge_product_command_records WHERE idempotency_key=$1)`, key).Scan(&receipts, &audits, &ttl); err != nil || receipts != 1 || audits != 1 || !ttl {
					t.Fatal(receipts, audits, ttl, err)
				}
				if action == "revoke" {
					var count int
					var enabled bool
					if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM product_admission_revocations rev JOIN product_admission_evidence evidence USING(evidence_id) WHERE evidence.catalog_item_id=$1),(SELECT new_orders_enabled FROM payment_product_controls WHERE product_id=$2)`, item.CatalogItemID, item.ProductID).Scan(&count, &enabled); err != nil || count != 1 || enabled {
						t.Fatal("revoke was not applied exactly once", count, enabled, err)
					}
				}
			})
		}
	}
}

func catalogAtomicVersionCases(t *testing.T) {
	t.Run("expired-retained-key-never-rebinds-or-writes-new-target", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		currentClock := r.now
		past := time.Now().UTC().Add(-25 * time.Hour)
		r.now = func() time.Time { return past }
		original, _ := catalogAtomicFixture(t, r, ctx, "delete-draft")
		actor, key := uuid.NewString(), "catalog-retained-expired-"+uuid.NewString()
		body := catalogTransitionRequest{ExpectedVersion: original.Version, Reason: "controlled historical successful command"}
		first := catalogAtomicCall(r, original.CatalogItemID, "delete-draft", actor, key, body)
		if first.Code != 200 {
			t.Fatal(first.Code, first.Body.String())
		}
		r.now = currentClock
		var expired bool
		if err := r.pool.QueryRow(ctx, `SELECT expires_at<=clock_timestamp() FROM recharge_product_command_records WHERE idempotency_key=$1`, key).Scan(&expired); err != nil || !expired {
			t.Fatal("fixture did not retain an expired original result", expired, err)
		}
		second, _ := catalogAtomicFixture(t, r, ctx, "delete-draft")
		before := catalogAtomicFacts(t, r, ctx)
		conflict := catalogAtomicCall(r, second.CatalogItemID, "delete-draft", actor, key, body)
		if conflict.Code != 409 || before != catalogAtomicFacts(t, r, ctx) {
			t.Fatal("expired retained key changed identity, history or target", conflict.Code, conflict.Body.String())
		}
	})
	t.Run("different-keys-cannot-both-pass-version-cas", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, _ := catalogAtomicFixture(t, r, ctx, "delete-draft")
		actor := uuid.NewString()
		body := catalogTransitionRequest{ExpectedVersion: item.Version, Reason: "controlled competing lifecycle commands"}
		start := make(chan struct{})
		results := make(chan int, 2)
		for range 2 {
			go func() {
				<-start
				results <- catalogAtomicCall(r, item.CatalogItemID, "delete-draft", actor, "catalog-cas-"+uuid.NewString(), body).Code
			}()
		}
		close(start)
		first, second := <-results, <-results
		if !((first == 200 && second == 409) || (first == 409 && second == 200)) {
			t.Fatal(first, second)
		}
		var receipts, audits int
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM recharge_product_command_records),(SELECT count(*) FROM recharge_product_audits)`).Scan(&receipts, &audits); err != nil || receipts != 1 || audits != 1 {
			t.Fatal(receipts, audits, err)
		}
	})
	t.Run("admission-original-receipt-precedes-later-version", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, _ := catalogAtomicFixture(t, r, ctx, "delete-draft")
		actor, key := uuid.NewString(), "catalog-admission-original-"+uuid.NewString()
		body := catalogTransitionRequest{ExpectedVersion: item.Version, Reason: "controlled original admission command"}
		initial := catalogAtomicAdmissionCall(r, item.CatalogItemID, uuid.Nil, "start", actor, key, body)
		if initial.Code != 202 {
			t.Fatal(initial.Code, initial.Body.String())
		}
		updated := catalogAtomicEditDraft(t, r, ctx, item)
		if updated.Version <= item.Version {
			t.Fatal("did not advance actual draft version")
		}
		before := catalogAtomicFacts(t, r, ctx)
		replay := catalogAtomicAdmissionCall(r, item.CatalogItemID, uuid.Nil, "start", actor, key, body)
		if replay.Code != 202 || catalogAtomicBody(t, initial.Body.Bytes()) != catalogAtomicBody(t, replay.Body.Bytes()) || before != catalogAtomicFacts(t, r, ctx) {
			t.Fatal("later revision prevented original result recovery", replay.Code, replay.Body.String())
		}
		if response := catalogAtomicAdmissionCall(r, item.CatalogItemID, uuid.Nil, "start", actor, "catalog-stale-"+uuid.NewString(), body); response.Code != 409 {
			t.Fatal("fresh key bypassed current version CAS", response.Code, response.Body.String())
		}
		if before != catalogAtomicFacts(t, r, ctx) {
			t.Fatal("stale new command wrote facts")
		}
	})
}

func catalogAtomicAdmissionFixture(t *testing.T, r *PostgresRepository, ctx context.Context, action string) (RechargeProductCatalogItem, uuid.UUID) {
	t.Helper()
	if action == "revoke" {
		item, evidence := catalogAtomicFixture(t, r, ctx, "unpublish")
		var verification uuid.UUID
		if err := r.pool.QueryRow(ctx, `SELECT verification_id FROM product_admission_evidence WHERE evidence_id=$1`, evidence).Scan(&verification); err != nil {
			t.Fatal(err)
		}
		return item, verification
	}
	item, _ := catalogAtomicFixture(t, r, ctx, "delete-draft")
	if action == "start" {
		return item, uuid.Nil
	}
	previous, _, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The existing draft-update producer invalidates its pending verification;
	// retry starts from a real FAILED/DRAFT_CHANGED round, not an injected status.
	item = catalogAtomicEditDraft(t, r, ctx, item)
	var status string
	if err := r.pool.QueryRow(ctx, `SELECT status FROM product_admission_verifications WHERE verification_id=$1`, previous.VerificationID).Scan(&status); err != nil || status != "FAILED" {
		t.Fatal(status, err)
	}
	return item, previous.VerificationID
}

func catalogAtomicEditDraft(t *testing.T, r *PostgresRepository, ctx context.Context, item RechargeProductCatalogItem) RechargeProductCatalogItem {
	t.Helper()
	updated, err := r.ExecuteRechargeProductDraftUpdate(ctx, RechargeProductDraftCommand{CatalogItemID: item.CatalogItemID, ExpectedVersion: item.Version, ExpectedDraftVersion: item.Mapping.DraftVersion, DisplayNameZhCN: item.DisplayNameZhCN + "2", DisplayNameEN: item.DisplayNameEN + "2", DisplayNameJA: item.DisplayNameJA + "2", DisplayOrder: item.DisplayOrder, BasePoints: item.BasePoints, BonusPoints: item.BonusPoints, ValidityDays: item.ValidityDays, InvoiceProductName: item.InvoiceProductName, Mapping: item.Mapping, IdempotencyKey: "catalog-fixture-draft-" + uuid.NewString(), RequestHash: uuid.NewString(), Actor: uuid.NewString(), Reason: "controlled actual draft revision"})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func catalogAtomicAdmissionCall(r *PostgresRepository, id, verification uuid.UUID, action, actor, key string, body catalogTransitionRequest) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	path := "/admin/v1/recharge-products/" + id.String() + "/admission-verifications"
	if action != "start" {
		path += "/" + verification.String() + "/" + action
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	request.SetPathValue("catalog_item_id", id.String())
	if action != "start" {
		request.SetPathValue("verification_id", verification.String())
	}
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Authorization", "Bearer controlled")
	s := NewService(r).WithRechargeProductCatalog(r)
	handler := s.adminStartProductAdmissionVerification
	if action == "retry" {
		handler = s.adminRetryProductAdmissionVerification
	}
	if action == "revoke" {
		handler = s.adminRevokeProductAdmission
	}
	response := httptest.NewRecorder()
	identity.Middleware(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "controlled", Subject: actor, AuthenticatedAt: r.now()}, nil
	}, http.HandlerFunc(handler)).ServeHTTP(response, request)
	return response
}

func catalogAtomicBody(t *testing.T, body []byte) string {
	t.Helper()
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Account/IdP producers here are controlled HTTP/identity fixtures. Gin,
// authorization.Client, recent-auth, AuditOnly and Billing SQL are production.
func catalogAtomicRouteCase(t *testing.T) {
	t.Run("registered-routes-retain-permission-recent-auth-and-policy", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, _ := catalogAtomicFixture(t, r, ctx, "delete-draft")
		fresh, denied, stale := uuid.NewString(), uuid.NewString(), uuid.NewString()
		grants := map[string]bool{"platform.products.publish": true, "platform.products.unpublish": true, "platform.products.delete": true}
		authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			var body struct {
				Subject    string `json:"subject"`
				Permission string `json:"permission"`
			}
			if request.Method != "POST" || request.URL.Path != "/internal/v1/authorization/check" || json.NewDecoder(request.Body).Decode(&body) != nil {
				t.Error("invalid authority request")
				w.WriteHeader(400)
				return
			}
			allowed := grants[body.Permission] && (body.Subject == fresh || body.Subject == stale)
			_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed, "user_id": body.Subject})
		}))
		defer authority.Close()
		authorizer, err := authorization.NewClient(authority.URL, "controlled-service")
		if err != nil {
			t.Fatal(err)
		}
		handler := NewService(r).WithRechargeProductCatalog(r).WithAuthorizer(authorizer).Handler("controlled-service", func(_ context.Context, token string) (identity.Principal, error) {
			when := r.now()
			if token == stale {
				when = when.Add(-16 * time.Minute)
			}
			return identity.Principal{Issuer: "controlled", Subject: token, AuthenticatedAt: when}, nil
		})
		raw := `{"expected_version":1,"reason":"controlled registered catalogue command"}`
		base := "/admin/v1/recharge-products/" + item.CatalogItemID.String()
		paths := []string{base + "/publish", base + "/unpublish", base + "/delete", base + "/admission-verifications", base + "/admission-verifications/" + uuid.NewString() + "/retry", base + "/admission-verifications/" + uuid.NewString() + "/revoke"}
		before := catalogAtomicFacts(t, r, ctx)
		call := func(path, token string) *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(raw))
			if token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
			request.Header.Set("Idempotency-Key", "catalog-route-"+uuid.NewString())
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			return response
		}
		for _, path := range paths {
			for _, fixture := range []struct {
				token string
				want  int
			}{{"", 401}, {denied, 403}, {stale, 401}} {
				response := call(path, fixture.token)
				if response.Code != fixture.want {
					t.Fatalf("route %s status=%d want=%d body=%s", path, response.Code, fixture.want, response.Body.String())
				}
			}
		}
		if before != catalogAtomicFacts(t, r, ctx) {
			t.Fatal("denied route mutated facts")
		}
		response := call(base+"/delete", fresh)
		if response.Code != 200 || response.Header().Get("X-Command-Type") != "BILLING_RECHARGE_PRODUCT_DELETE" || response.Header().Get("X-Command-Policy-Version") == "" {
			t.Fatal(response.Code, response.Header(), response.Body.String())
		}
	})
}
