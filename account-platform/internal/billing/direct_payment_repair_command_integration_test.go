package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type directRepairProvider struct {
	*retainedFinancialProvider
	gets    atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (p *directRepairProvider) Get(ctx context.Context, merchant string) (ProviderOrder, error) {
	p.gets.Add(1)
	if p.entered != nil {
		p.entered <- struct{}{}
		select {
		case <-p.release:
		case <-ctx.Done():
			return ProviderOrder{}, ctx.Err()
		}
	}
	return p.retainedFinancialProvider.Get(ctx, merchant)
}

func TestDirectPaymentRepairCommandReceiptPostgres(t *testing.T) {
	t.Run("lost-ack-replays-original-accurate-receipt", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		c, order, provider := directRepairFixture(t, r, ctx)
		actor := uuid.New()
		key, reason := "direct-repair-"+uuid.NewString(), "controlled verified direct repair"
		allowed := &atomic.Bool{}
		allowed.Store(true)
		handler := directRepairHandler(t, r, c, allowed)
		ack := make(chan []byte, 1)
		var drop atomic.Bool
		drop.Store(true)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if drop.Swap(false) {
				capture := httptest.NewRecorder()
				handler.ServeHTTP(capture, request)
				if capture.Code != 200 {
					t.Error("first command rejected", capture.Code, capture.Body.String())
				}
				ack <- bytes.Clone(capture.Body.Bytes())
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				connection.Close()
				return
			}
			handler.ServeHTTP(w, request)
		}))
		defer server.Close()
		response, err := directRepairHTTP(server.URL, order.ID, actor, key, reason)
		if response != nil {
			response.Body.Close()
		}
		if err == nil {
			t.Fatal("lost ACK observed as success")
		}
		original := <-ack
		actual, err := getPaymentOrderByID(ctx, r.pool, order.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		var first, expected map[string]json.RawMessage
		if json.Unmarshal(original, &first) != nil {
			t.Fatal("invalid first HTTP ACK")
		}
		databaseJSON, _ := json.Marshal(actual)
		if err := json.Unmarshal(databaseJSON, &expected); err != nil {
			t.Fatal(err)
		}
		// Admin's existing wire view represents money as exact decimal strings,
		// unlike the internal PaymentOrder snapshot's int64 fields.
		expected["amount_fen"], _ = json.Marshal(strconv.FormatInt(actual.AmountFen, 10))
		expected["credit_microcredits"], _ = json.Marshal(strconv.FormatInt(actual.CreditMicrocredits, 10))
		left, _ := json.Marshal(first)
		right, _ := json.Marshal(expected)
		if !bytes.Equal(left, right) || actual.FulfillmentStatus != "CREDITED" {
			t.Fatal("first repair receipt is not the accurate database projection", string(left), string(right))
		}
		before := directRepairFacts(t, r, ctx)
		gets := provider.gets.Load()
		response, err = directRepairHTTP(server.URL, order.ID, actor, key, reason)
		if err != nil {
			t.Fatal(err)
		}
		replayed, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || !bytes.Equal(original, replayed) || directRepairFacts(t, r, ctx) != before || provider.gets.Load() != gets {
			t.Fatal("same-key direct repair did not recover its original committed ACK without another provider call", response.StatusCode)
		}
		allowed.Store(false)
		response, err = directRepairHTTP(server.URL, order.ID, actor, key, reason)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 403 || directRepairFacts(t, r, ctx) != before {
			t.Fatal("receipt bypassed current authority", response.StatusCode)
		}
		allowed.Store(true)
		for _, f := range []struct {
			actor       uuid.UUID
			reason, key string
		}{{actor, "different repair facts", key}, {uuid.New(), reason, key}, {actor, reason, "new-direct-key-" + uuid.NewString()}} {
			response, err = directRepairHTTP(server.URL, order.ID, f.actor, f.key, f.reason)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != 409 || directRepairFacts(t, r, ctx) != before {
				t.Fatal("new/changed command inferred success from paid order", response.StatusCode)
			}
		}
		_, other, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
		otherBefore := directRepairFacts(t, r, ctx)
		response, err = directRepairHTTP(server.URL, other.ID, actor, key, reason)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 409 || directRepairFacts(t, r, ctx) != otherBefore || provider.gets.Load() != gets {
			t.Fatal("original actor/key was rebound to another order", response.StatusCode)
		}
	})
	for _, failure := range []string{"receipt", "commit"} {
		t.Run(failure+"-failure-rolls-back-finance-and-key", func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, _ := directRepairFixture(t, r, ctx)
			before := directRepairFacts(t, r, ctx)
			ddl := `CREATE FUNCTION direct_repair_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled direct repair failure'; END $$; `
			table := "payment_order_repair_commands"
			if failure == "receipt" {
				ddl += `CREATE TRIGGER direct_repair_reject BEFORE INSERT ON payment_order_repair_commands FOR EACH ROW EXECUTE FUNCTION direct_repair_reject()`
			} else {
				table = "outbox_events"
				ddl += `CREATE CONSTRAINT TRIGGER direct_repair_reject AFTER INSERT ON outbox_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION direct_repair_reject()`
			}
			if _, err := retainedFinanceOwnerExec(t, r, ctx, ddl); err != nil {
				t.Fatal(err)
			}
			actor, key, reason := uuid.New(), "direct-failure-"+uuid.NewString(), "controlled original direct repair facts"
			if _, err := c.RepairReviewedOrder(ctx, order.ID, actor, reason, key); err == nil || directRepairFacts(t, r, ctx) != before {
				t.Fatal("failed direct repair committed partial financial/key facts", err)
			}
			if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER direct_repair_reject ON `+table+`; DROP FUNCTION direct_repair_reject()`); err != nil {
				t.Fatal(err)
			}
			first, err := c.RepairReviewedOrder(ctx, order.ID, actor, reason, key)
			if err != nil {
				t.Fatal(err)
			}
			committed := directRepairFacts(t, r, ctx)
			again, err := c.RepairReviewedOrder(ctx, order.ID, actor, reason, key)
			a, _ := json.Marshal(first)
			b, _ := json.Marshal(again)
			if err != nil || !bytes.Equal(a, b) || directRepairFacts(t, r, ctx) != committed {
				t.Fatal("same key after definite rollback did not recover exactly", err)
			}
		})
	}
	t.Run("concurrent-same-key-converges-after-unlocked-provider-read", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		c, order, provider := directRepairFixture(t, r, ctx)
		provider.entered = make(chan struct{}, 2)
		provider.release = make(chan struct{})
		actor, key, reason := uuid.New(), "direct-race-"+uuid.NewString(), "controlled concurrent repair identity"
		type result struct {
			order PaymentOrder
			err   error
		}
		done := make(chan result, 2)
		for range 2 {
			go func() {
				value, err := c.RepairReviewedOrder(ctx, order.ID, actor, reason, key)
				done <- result{value, err}
			}()
		}
		for range 2 {
			select {
			case <-provider.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("provider reads were serialized inside financial transaction")
			}
		}
		close(provider.release)
		a, b := <-done, <-done
		left, _ := json.Marshal(a.order)
		right, _ := json.Marshal(b.order)
		if a.err != nil || b.err != nil || !bytes.Equal(left, right) {
			t.Fatal("same command did not converge at final locked receipt check", a.err, b.err)
		}
		var commands, repairs, journals int
		var posted int64
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_order_repair_commands),(SELECT count(*) FROM payment_order_repairs),(SELECT count(*) FROM journal_entries WHERE entry_type IN ('RECHARGE','REPAIR_CREDIT')),posted_microcredits FROM wallets WHERE id=$1`, order.WalletID).Scan(&commands, &repairs, &journals, &posted); err != nil || commands != 1 || repairs != 1 || journals != 1 || posted != order.CreditMicrocredits {
			t.Fatal("concurrent repair duplicated facts", commands, repairs, journals, posted, err)
		}
	})
	t.Run("legacy-anomaly-audit-is-not-a-command-receipt", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		c, order, provider := directRepairFixture(t, r, ctx)
		actor := uuid.New()
		var caseID uuid.UUID
		if err := r.pool.QueryRow(ctx, `SELECT id FROM payment_anomaly_cases WHERE payment_order_id=$1 ORDER BY created_at,id LIMIT 1`, order.ID).Scan(&caseID); err != nil {
			t.Fatal(err)
		}
		legacyKey, reason := uuid.NewString(), "controlled original anomaly repair"
		retainedClaimAnomaly(t, r, ctx, caseID, actor)
		if _, _, err := r.PreparePaymentAnomalyResolution(ctx, caseID, actor, legacyKey, "CONFIRM_PAYMENT", reason); err != nil {
			t.Fatal(err)
		}
		if _, err := c.repairReviewedOrder(ctx, order.ID, actor, reason, caseID, legacyKey); err != nil {
			t.Fatal("existing anomaly financial repair", err)
		}
		if _, err := r.ResolvePaymentAnomaly(ctx, caseID, actor, legacyKey, "CONFIRM_PAYMENT", reason, "APPLIED"); err != nil {
			t.Fatal("existing anomaly completion", err)
		}
		before := directRepairFacts(t, r, ctx)
		if _, err := c.RepairReviewedOrder(ctx, order.ID, actor, reason, "unknown-historic-key-"+uuid.NewString()); err != ErrConflict || directRepairFacts(t, r, ctx) != before {
			t.Fatal("historical audit was guessed to belong to new key", err)
		}
		original := provider.result
		next := uuid.NewString()
		provider.result.ChannelTransactionID = &next
		provider.result.PaidAt = original.PaidAt
		review, err := c.SyncOrder(ctx, order.ID)
		if err != nil || review.Status != "MANUAL_REVIEW" {
			t.Fatal("actual second payment did not re-enter review", review.Status, err)
		}
		var oldAudit string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM payment_order_repairs a`).Scan(&oldAudit); err != nil {
			t.Fatal(err)
		}
		provider.result = original
		key := "new-direct-existing-audit-" + uuid.NewString()
		first, err := c.RepairReviewedOrder(ctx, order.ID, actor, "controlled recheck of original transaction", key)
		if err != nil {
			t.Fatal("existing transaction audit swallowed a legal new command receipt", err)
		}
		var unchanged string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM payment_order_repairs a`).Scan(&unchanged); err != nil || unchanged != oldAudit {
			t.Fatal("old audit was rewritten", err)
		}
		committed := directRepairFacts(t, r, ctx)
		replay, err := c.RepairReviewedOrder(ctx, order.ID, actor, "controlled recheck of original transaction", key)
		a, _ := json.Marshal(first)
		b, _ := json.Marshal(replay)
		if err != nil || !bytes.Equal(a, b) || directRepairFacts(t, r, ctx) != committed {
			t.Fatal("independent command identity did not recover", err)
		}
	})
	t.Run("pre-admission-fixture-rejects-current-schema", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		c, order, provider := directRepairFixture(t, r, ctx)
		before := directRepairFacts(t, r, ctx)
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, _, err := c.mergePreAdmissionFixturePayment(ctx, tx, order, "QUERY", "forbidden-modern-legacy-seed", nil, provider.result, false, r.now()); !errors.Is(err, errPreAdmissionFixtureSchema) {
			t.Fatal("historical seed model accepted the current schema", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.repairPreAdmissionFixtureOrder(ctx, order.ID, uuid.New(), "forbidden modern legacy repair", "forbidden-modern-legacy-repair"); !errors.Is(err, errPreAdmissionFixtureSchema) {
			t.Fatal("historical repair model accepted the current schema", err)
		}
		if directRepairFacts(t, r, ctx) != before {
			t.Fatal("rejected historical fixture changed current financial facts")
		}
	})
	t.Run("runtime-immutable-command-and-exact-prefix-down", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t, 900031)
		c, order, _ := directRepairFixture(t, r, ctx)
		if _, err := c.repairPreAdmissionFixtureOrder(ctx, order.ID, uuid.New(), "controlled retained direct command", "direct-down-"+uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		before := directRepairFacts(t, r, ctx)
		var truncate, membership bool
		var owner string
		if err := r.pool.QueryRow(ctx, `SELECT has_table_privilege(current_user,'public.payment_order_repair_commands','TRUNCATE'),pg_has_role(current_user,'billing_schema_owner','MEMBER'),pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.payment_order_repair_commands'::regclass`).Scan(&truncate, &membership, &owner); err != nil || truncate || membership || owner != "billing_schema_owner" {
			t.Fatal("runtime command authority is broader than installed contract", truncate, membership, owner, err)
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO payment_order_repair_commands SELECT $1,payment_order_id,actor_id,idempotency_key,command_protocol,reason,request_hash,response_body,created_at FROM payment_order_repair_commands LIMIT 1`, uuid.New()); err == nil || !strings.Contains(err.Error(), "payment_order_repair_commands_actor_key") {
			t.Fatal("database actor/key uniqueness missing", err)
		}
		for _, statement := range []string{`UPDATE payment_order_repair_commands SET reason=reason||'tamper'`, `DELETE FROM payment_order_repair_commands`, `TRUNCATE payment_order_repair_commands`} {
			if _, err := r.pool.Exec(ctx, statement); err == nil {
				t.Fatal("runtime command mutation allowed", statement)
			}
		}
		if directRepairFacts(t, r, ctx) != before {
			t.Fatal("rejected runtime mutation changed facts")
		}
		var ledger string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&ledger); err != nil {
			t.Fatal(err)
		}
		cfg := r.pool.Config().ConnConfig.Copy()
		cfg.User, cfg.Password = "billing_migrator", ""
		connection, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close(context.Background())
		if _, err = connection.Exec(ctx, `SET ROLE billing_service`); err != nil {
			t.Fatal(err)
		}
		loaded, err := migrations.Load("../../migrations/billing")
		if err != nil {
			t.Fatal(err)
		}
		var highest int64
		if err := r.pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&highest); err != nil || highest != 900031 {
			t.Fatal("fixture did not stop at exact direct-repair prefix", highest, err)
		}
		var prefix []migrations.Migration
		for _, migration := range loaded {
			if migration.Version <= 900031 {
				prefix = append(prefix, migration)
			}
		}
		marker := func() string {
			var value string
			if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('oid',oid,'owner',pg_get_userbyid(relowner),'acl',relacl)::text FROM pg_class WHERE oid='public.payment_order_repair_commands'::regclass`).Scan(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		beforeMarker := marker()
		if _, err := billingschema.VerifyApplied(ctx, connection, prefix); err != nil {
			t.Fatal("direct-repair prefix identity", err)
		}
		err = billingschema.DownOne(ctx, connection, prefix)
		var rejected *pgconn.PgError
		if !errors.As(err, &rejected) || rejected.Code != "55000" || rejected.Message != "direct payment repair commands are forward-only" {
			t.Fatal("direct-repair prefix lost its exact forward-only refusal", err)
		}
		var after string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&after); err != nil || after != ledger || marker() != beforeMarker || directRepairFacts(t, r, ctx) != before {
			t.Fatal("failed down changed command/financial/migration facts", err)
		}
		if _, err := billingschema.VerifyApplied(ctx, connection, prefix); err != nil {
			t.Fatal("rejected down changed prefix identity", err)
		}
		if err := billingschema.Up(ctx, connection, loaded); err != nil {
			t.Fatal("full current Up after exact direct prefix", err)
		}
		if _, err := billingschema.VerifyApplied(ctx, connection, loaded); err != nil {
			t.Fatal("full current migration identity", err)
		}
		var fullLedger string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&fullLedger); err != nil {
			t.Fatal(err)
		}
		if err := billingschema.Up(ctx, connection, loaded); err != nil {
			t.Fatal("full current Up replay", err)
		}
		if _, err := billingschema.VerifyApplied(ctx, connection, loaded); err != nil {
			t.Fatal("full replay identity", err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&after); err != nil || after != fullLedger || marker() != beforeMarker || directRepairFacts(t, r, ctx) != before {
			t.Fatal("full Up/replay changed migration ledger, marker/ACL or financial facts", err)
		}
	})
}

func directRepairFixture(t *testing.T, r *PostgresRepository, ctx context.Context) (*PaymentCoordinator, PaymentOrder, *directRepairProvider) {
	t.Helper()
	current := r.now
	r.now = func() time.Time { return current().Add(-2 * time.Hour) }
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	r.now = current
	provider := &directRepairProvider{retainedFinancialProvider: c.provider.(*retainedFinancialProvider)}
	paid := current()
	provider.result.PaidAt = &paid
	c.provider = provider
	review, err := c.SyncOrder(ctx, order.ID)
	if err != nil || review.Status != "MANUAL_REVIEW" || review.FulfillmentStatus != "MANUAL_REVIEW" {
		t.Fatal("real late payment producer did not require review", review, err)
	}
	return c, review, provider
}
func directRepairFacts(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	facts := map[string]json.RawMessage{}
	for _, table := range []string{"payment_orders", "payment_transactions", "payment_evidence", "payment_order_repairs", "payment_order_repair_commands", "idempotency_records", "wallets", "journal_entries", "ledger_postings", "entitlement_packages", "entitlement_components", "receivables", "manual_review_cases", "payment_anomaly_cases", "payment_anomaly_commands", "outbox_events"} {
		var raw []byte
		where := ""
		if table == "outbox_events" {
			// The original read-only provider call records its independent health
			// observation before entering the financial transaction.
			where = " WHERE event_type<>'billing.payment-provider.health-observed.v1'"
		}
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(item) ORDER BY to_jsonb(item)::text),'[]'::jsonb) FROM `+table+` item`+where).Scan(&raw); err != nil {
			t.Fatal(fmt.Errorf("snapshot %s: %w", table, err))
		}
		facts[table] = raw
	}
	body, _ := json.Marshal(facts)
	return string(body)
}
func directRepairHandler(t *testing.T, r *PostgresRepository, c *PaymentCoordinator, allowed *atomic.Bool) http.Handler {
	t.Helper()
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Subject    string `json:"subject"`
			Permission string `json:"permission"`
		}
		if request.Method != "POST" || request.URL.Path != "/internal/v1/authorization/check" || json.NewDecoder(request.Body).Decode(&body) != nil {
			t.Error("unexpected authority protocol")
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"allowed": allowed.Load() && body.Permission == "platform.billing.repairs.request", "user_id": body.Subject})
	}))
	t.Cleanup(authority.Close)
	authorizer, err := authorization.NewClient(authority.URL, "controlled-service")
	if err != nil {
		t.Fatal(err)
	}
	return NewService(r).WithPayments(c).WithAuthorizer(authorizer).Handler("controlled-service", func(_ context.Context, token string) (identity.Principal, error) {
		return identity.Principal{Issuer: "controlled", Subject: token, AuthenticatedAt: r.now()}, nil
	})
}
func directRepairHTTP(origin string, id, actor uuid.UUID, key, reason string) (*http.Response, error) {
	body, _ := json.Marshal(map[string]any{"reason": reason, "command_protocol": 1})
	request, _ := http.NewRequest("POST", origin+"/admin/v1/payment-orders/"+id.String()+"/repair", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+actor.String())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	return (&http.Client{Timeout: 15 * time.Second}).Do(request)
}
