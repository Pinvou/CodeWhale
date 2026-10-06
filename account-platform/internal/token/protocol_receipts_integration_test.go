package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func governanceFixture(t *testing.T) *PostgresRepository {
	t.Helper()
	return &PostgresRepository{pool: testdb.Open(t, "token"), now: func() time.Time { return time.Now().UTC() }}
}

func TestPostgresGovernanceLogicalCommandsPreserveFacts(t *testing.T) {
	ctx := context.Background()
	repo := governanceFixture(t)
	actor, key := uuid.New(), uuid.New()
	first, err := repo.CreateProtocolDraft(ctx, actor, key, "CHAT_NEW", json.RawMessage(`{"parameters":["request_count"]}`))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := repo.CreateProtocolDraft(ctx, actor, key, " chat_new ", json.RawMessage(` { "parameters": ["request_count"] } `))
	if err != nil || first.ID != replay.ID || first.Version != replay.Version {
		t.Fatalf("draft replay: %+v %v", replay, err)
	}
	if _, err := repo.CreateProtocolDraft(ctx, actor, key, "OTHER", json.RawMessage(`{}`)); !errors.Is(err, ErrGovernanceConflict) {
		t.Fatalf("different facts: %v", err)
	}
	publicationKey := uuid.New()
	published, err := repo.PublishProtocol(ctx, actor, publicationKey, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	publicationReplay, err := repo.PublishProtocol(ctx, actor, publicationKey, first.ID)
	if err != nil || published.PublishedAt == nil || publicationReplay.PublishedAt == nil || !publicationReplay.PublishedAt.Equal(*published.PublishedAt) {
		t.Fatalf("publication replay: %+v %v", publicationReplay, err)
	}
	var drafts, audits int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM protocol_definitions WHERE protocol_code='CHAT_NEW'`).Scan(&drafts); err != nil {
		t.Fatal(err)
	}
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM token_audit_logs WHERE resource_id=$1`, first.ID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if drafts != 1 || audits != 2 {
		t.Fatalf("duplicates: drafts=%d audits=%d", drafts, audits)
	}
}

func TestPostgresGovernanceConcurrentDraftAndAuditRollback(t *testing.T) {
	ctx := context.Background()
	repo := governanceFixture(t)
	actor, key := uuid.New(), uuid.New()
	var group sync.WaitGroup
	results := make(chan ProtocolDefinition, 8)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 20 {
				item, err := repo.CreateProtocolDraft(ctx, actor, key, "CONCURRENT", json.RawMessage(`{}`))
				if err == nil {
					results <- item
					return
				}
				var pgError *pgconn.PgError
				if !errors.As(err, &pgError) || pgError.Code != "40001" {
					t.Errorf("unexpected draft error: %v", err)
					return
				}
			}
			t.Error("serializable draft did not converge")
		}()
	}
	group.Wait()
	close(results)
	var id uuid.UUID
	count := 0
	for item := range results {
		count++
		if id == uuid.Nil {
			id = item.ID
		}
		if item.ID != id {
			t.Error("multiple draft identities")
		}
	}
	if count != 8 {
		t.Fatalf("completed %d callers", count)
	}
	if _, err := repo.pool.Exec(ctx, `CREATE FUNCTION reject_governance_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled audit rejection'; END $$; CREATE TRIGGER reject_governance_audit BEFORE INSERT ON token_audit_logs FOR EACH ROW EXECUTE FUNCTION reject_governance_audit()`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateProtocolDraft(ctx, actor, uuid.New(), "ROLLBACK", json.RawMessage(`{}`)); err == nil {
		t.Fatal("audit rejection was ignored")
	}
	var rows int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM protocol_definitions WHERE protocol_code='ROLLBACK'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("draft escaped audit transaction: %d %v", rows, err)
	}
}

func TestPostgresFormulaPublicationSurvivesBillingACKLossAndRestart(t *testing.T) {
	ctx := context.Background()
	repo := governanceFixture(t)
	billingPool := testdb.Open(t, "billing")
	billingRepo, err := billingdomain.NewPostgresRepository(ctx, billingPool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer billingRepo.Close()
	actor, key := uuid.New(), uuid.New()
	protocolID := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	required := map[string]uuid.UUID{"INPUT_TOKEN": uuid.MustParse("00000000-0000-7000-8100-000000000001")}
	draft, err := repo.CreateFormulaDraft(ctx, actor, uuid.New(), protocolID, required, map[string]string{"INPUT_TOKEN": "estimated_input_tokens"})
	if err != nil {
		t.Fatal(err)
	}
	const serviceToken = "controlled-governance-service-token"
	actual := billingdomain.NewService(billingRepo).Handler(serviceToken)
	var calls atomic.Int32
	var publishedAt time.Time
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := httptest.NewRecorder()
		actual.ServeHTTP(response, r)
		if response.Code != http.StatusOK {
			t.Errorf("Billing status %d", response.Code)
		}
		if calls.Add(1) == 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}))
	defer peer.Close()
	client, err := NewHTTPBillingClient(peer.URL, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repo, nil, nil, nil, ProviderURLPolicy{}, client, nil, 10)
	invoke := func(s *Service) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/admin/v1/formulas/"+draft.ID.String()+"/publish", strings.NewReader(""))
		r.SetPathValue("formula_version_id", draft.ID.String())
		r.Header.Set("Idempotency-Key", key.String())
		w := httptest.NewRecorder()
		s.adminPublishFormula(w, r)
		return w
	}
	if response := invoke(service); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("first ACK loss status %d", response.Code)
	}
	if err := repo.pool.QueryRow(ctx, `SELECT started_at FROM token_formula_publication_intents WHERE formula_version_id=$1`, draft.ID).Scan(&publishedAt); err != nil {
		t.Fatal(err)
	}
	// A new repository and service share only durable PG state, not an in-memory receipt.
	restarted := &PostgresRepository{pool: repo.pool, now: func() time.Time { return time.Now().UTC().Add(time.Hour) }}
	newService := NewService(restarted, nil, nil, nil, ProviderURLPolicy{}, client, nil, 10)
	for range 2 {
		response := invoke(newService)
		var result ProtocolFormulaVersion
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.ID != draft.ID || result.Status != "PUBLISHED" || result.PublishedAt == nil || !result.PublishedAt.Equal(publishedAt) {
			t.Fatalf("publication did not recover: status=%d", response.Code)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("completed replay resynchronized Billing: %d", calls.Load())
	}
	var rows, audits int
	var billingPublishedAt time.Time
	if err := billingPool.QueryRow(ctx, `SELECT count(*),min(published_at) FROM billing_protocol_formula_bindings WHERE formula_version_id=$1`, draft.ID).Scan(&rows, &billingPublishedAt); err != nil {
		t.Fatal(err)
	}
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM token_audit_logs WHERE resource_id=$1`, draft.ID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || audits != 2 || !billingPublishedAt.Equal(publishedAt) {
		t.Fatalf("publication facts: rows=%d audits=%d same_time=%v", rows, audits, billingPublishedAt.Equal(publishedAt))
	}
	_ = actor // The direct HTTP boundary uses tokenAdminActor's controlled principal projection.
}

// Exercise the actual new migration around the previous token schema with a
// persisted model call. A populated zero reservation cannot be represented by
// the old >0 constraint: Down must fail atomically, never discard that call.
func TestPostgresTokenMeteringMigrationPreservesCallsAndFailsUnsafeDowngrade(t *testing.T) {
	ctx := context.Background()
	repo, usage := settlementFixture(t)
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := repo.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	defer connection.Conn().Close(ctx)
	snapshot := func() string {
		t.Helper()
		var result string
		if err := connection.QueryRow(ctx, `SELECT to_jsonb(u)::text FROM usage_requests u WHERE id=$1`, usage.ID).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := snapshot()
	for round := 0; round < 2; round++ {
		if err := migrations.DownOne(ctx, connection.Conn(), loaded); err != nil {
			t.Fatal(err)
		}
		var version int64
		if err := connection.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 9 || snapshot() != before {
			t.Fatal("old schema round lost a persisted model call")
		}
		if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
			t.Fatal(err)
		}
		if err := connection.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 10 || snapshot() != before {
			t.Fatal("new schema round changed original model facts")
		}
	}
	if _, err := connection.Exec(ctx, `UPDATE usage_requests SET maximum_microcredits=0 WHERE id=$1`, usage.ID); err != nil {
		t.Fatal(err)
	}
	zero := snapshot()
	if err := migrations.DownOne(ctx, connection.Conn(), loaded); err == nil {
		t.Fatal("unsafe downgrade accepted data that the old schema cannot represent")
	}
	var version int64
	var commandsExist bool
	if err := connection.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 10 {
		t.Fatal("failed downgrade changed the migration ledger")
	}
	if err := connection.QueryRow(ctx, `SELECT to_regclass('token_governance_commands') IS NOT NULL`).Scan(&commandsExist); err != nil || !commandsExist || snapshot() != zero {
		t.Fatal("failed downgrade lost the new schema or persisted zero-budget call")
	}
}
