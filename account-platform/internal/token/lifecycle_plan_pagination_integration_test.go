package token

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

// This regression uses the real pagination SQL and HTTP projection. The
// authorization server is the existing explicit fixture; no lifecycle writer,
// disabled constraint, or uncommitted activation helper is needed to seed history.
func TestLifecyclePlanPaginationPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, tokenPaginationIntegrationDatabaseURL(t))
	if err := repository.ConfigureAdminPagination(AdminPaginationConfig{Enabled: true, CursorHMACKey: bytes.Repeat([]byte{31}, 32)}); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetAdminPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}
	actor, modelID, otherModel := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, id := range []uuid.UUID{modelID, otherModel} {
		digest := strings.ReplaceAll(id.String(), "-", "")
		if _, err := repository.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'pagination-fixture','v1',$2,$3,$4,$5)`, id, id.String(), digest+digest, actor, now); err != nil {
			t.Fatal(err)
		}
	}
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'pagination fixture catalog',$2,$3,$3) RETURNING version`, uuid.New(), actor, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	seed := func(model uuid.UUID, version int64, status, target string) {
		t.Helper()
		id := uuid.New()
		digest := strings.ReplaceAll(id.String(), "-", "")
		var replacement any
		var cutoff any
		effective := now.Add(time.Hour)
		if target == "REMOVED" {
			replacement, cutoff = otherModel, effective
		}
		_, err := repository.pool.Exec(ctx, `INSERT INTO model_lifecycle_plans(id,model_id,plan_version,expected_catalog_version,current_lifecycle,target_lifecycle,effective_at,existing_session_cutoff_at,replacement_model_id,status,reason,request_digest,idempotency_key_digest,created_by,created_at,updated_at)
VALUES($1,$2,$3,$4,'PUBLISHED',$5,$6,$7,$8,$9,'pagination fixture historical plan',$10,$10,$11,$12,$12)`, id, model, version, catalogVersion, target, effective, cutoff, replacement, status, digest+digest, actor, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, state := range []string{"CANCELLED", "EXPIRED", "CANCELLED", "EXPIRED", "CANCELLED", "PENDING_APPROVAL"} {
		target := "DEPRECATED"
		if i == 3 {
			target = "REMOVED"
		}
		seed(modelID, int64(i+1), state, target)
	}
	seed(otherModel, 1, "CANCELLED", "DEPRECATED")
	handler := httpserver.CorrelationHandler(newLifecyclePlanPermissionIntegrationHandler(t, repository, actor, []string{"platform.models.read"}, "pagination-regression"))
	request := func(handler http.Handler, model uuid.UUID, query string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/admin/v1/models/"+model.String()+"/lifecycle-plans?"+query, nil).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer pagination-fixture")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	page := func(query string, expected ...int64) lifecyclePaginationTestPage {
		t.Helper()
		response := request(handler, modelID, query)
		var result lifecyclePaginationTestPage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatalf("lifecycle page: %d %s", response.Code, response.Body.String())
		}
		if len(result.Items) != len(expected) || result.Limit != 2 || result.AsOf.IsZero() || !result.SnapshotExpiresAt.After(result.AsOf) || result.RequestID == "" {
			t.Fatalf("invalid pagination envelope: %s", response.Body.String())
		}
		for i, version := range expected {
			if result.Items[i].ModelID != modelID || result.Items[i].PlanVersion != version || result.Items[i].ID == uuid.Nil {
				t.Fatalf("unexpected ordered member %d: %+v, expected version %d", i, result.Items[i], version)
			}
		}
		if result.HasMore != (result.NextCursor != nil) || (result.NextCursor != nil && *result.NextCursor == "") {
			t.Fatalf("inconsistent end cursor: %+v", result)
		}
		return result
	}
	cursorQuery := func(p lifecyclePaginationTestPage) string {
		t.Helper()
		if !p.HasMore || p.NextCursor == nil {
			t.Fatal("expected next page")
		}
		return "limit=2&cursor=" + url.QueryEscape(*p.NextCursor)
	}
	beforeFirst := lifecyclePaginationDomainEvidence(t, ctx, repository)
	first := page("limit=2", 6, 5)
	if after := lifecyclePaginationDomainEvidence(t, ctx, repository); after != beforeFirst {
		t.Fatal("first page mutated lifecycle/domain facts")
	}
	seed(modelID, 7, "CANCELLED", "DEPRECATED")
	beforeRemaining := lifecyclePaginationDomainEvidence(t, ctx, repository)
	second := page(cursorQuery(first), 4, 3)
	last := page(cursorQuery(second), 2, 1)
	if last.HasMore || last.NextCursor != nil {
		t.Fatal("last page has a cursor")
	}
	replay := page(cursorQuery(first), 4, 3)
	if !reflect.DeepEqual(second.Items, replay.Items) || !second.AsOf.Equal(replay.AsOf) || !second.SnapshotExpiresAt.Equal(replay.SnapshotExpiresAt) || !first.AsOf.Equal(last.AsOf) {
		t.Fatal("cursor replay did not retain frozen members and timestamps")
	}
	otherActor := httpserver.CorrelationHandler(newLifecyclePlanPermissionIntegrationHandler(t, repository, uuid.New(), []string{"platform.models.read"}, "other-pagination-actor"))
	for _, check := range []struct {
		name    string
		handler http.Handler
		model   uuid.UUID
		query   string
	}{
		{"filter binding", handler, modelID, cursorQuery(first) + "&status=CANCELLED"},
		{"model binding", handler, otherModel, cursorQuery(first)},
		{"actor binding", otherActor, modelID, cursorQuery(first)},
		{"malformed cursor", handler, modelID, "limit=2&cursor=invalid"},
	} {
		response := request(check.handler, check.model, check.query)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_CURSOR"`) {
			t.Fatalf("%s: %d %s", check.name, response.Code, response.Body.String())
		}
	}
	page("limit=2", 7, 6) // A new first page, unlike the old cursor, sees the inserted plan.
	filtered := page("limit=2&status=CANCELLED&target_lifecycle=DEPRECATED", 7, 5)
	filteredLast := page(cursorQuery(filtered)+"&status=CANCELLED&target_lifecycle=DEPRECATED", 3, 1)
	if filteredLast.HasMore {
		t.Fatal("filtered last page has more members")
	}
	removed := page("limit=2&status=EXPIRED&target_lifecycle=REMOVED", 4)
	if removed.HasMore {
		t.Fatal("exact status/target filter returned more members")
	}
	empty := page("limit=2&status=FAILED")
	if empty.Items == nil || empty.HasMore {
		t.Fatal("empty result must be an empty array without continuation")
	}
	for _, query := range []string{"limit=2&status=INVALID", "limit=2&status=CANCELLED&status=EXPIRED", "limit=2&unknown=1", "limit=2&target_lifecycle=DROP_TABLE"} {
		response := request(handler, modelID, query)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_FILTER"`) {
			t.Fatalf("invalid filter: %d %s", response.Code, response.Body.String())
		}
	}
	if after := lifecyclePaginationDomainEvidence(t, ctx, repository); after != beforeRemaining {
		t.Fatal("pagination/replays/rejected inputs mutated domain, audit, or outbox facts")
	}
}

type lifecyclePaginationTestPage struct {
	Items             []LifecyclePlan `json:"items"`
	NextCursor        *string         `json:"next_cursor"`
	HasMore           bool            `json:"has_more"`
	Limit             int             `json:"limit"`
	AsOf              time.Time       `json:"as_of"`
	SnapshotExpiresAt time.Time       `json:"snapshot_expires_at"`
	RequestID         string          `json:"request_id"`
}

func lifecyclePaginationDomainEvidence(t *testing.T, ctx context.Context, repository *PostgresRepository) string {
	t.Helper()
	var evidence string
	err := repository.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'plans',(SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY p.id),'[]'::jsonb) FROM model_lifecycle_plans p),
'events',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.id),'[]'::jsonb) FROM model_lifecycle_plan_events e),
'models',(SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY m.id),'[]'::jsonb) FROM model_identities m),
'catalog',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY c.version),'[]'::jsonb) FROM model_catalog_versions c),
'audits',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.id),'[]'::jsonb) FROM token_audit_logs a),
'outbox',(SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY o.event_id),'[]'::jsonb) FROM outbox_events o))::text`).Scan(&evidence)
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}
