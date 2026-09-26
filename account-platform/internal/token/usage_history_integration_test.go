package token

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestUsageHistoryPostgresPaginationKeepsLogicalTurnsWhole(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)

	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, ownerID, walletID := uuid.New(), uuid.New(), uuid.New()
	accountID, credentialID, actorID := uuid.New(), uuid.New(), uuid.New()
	if _, err := repository.pool.Exec(ctx, `
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode)
VALUES($1,'USER',$2,$3,'Usage history account','ACTIVE',$4,$5,$5,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status)
VALUES($6,$1,'provider','https://provider.invalid','x'::bytea,'key','usage-history-fingerprint',1,'ACTIVE',ARRAY['model-one'],$5,$4,$5,'VALID');`,
		pgx.QueryExecModeSimpleProtocol, accountID, ownerID, walletID, actorID, now, credentialID); err != nil {
		t.Fatal(err)
	}

	insertUsage := func(id uuid.UUID, logicalTurnID string, startedAt time.Time, organizationID *uuid.UUID, status, settlement string, actual *int64) {
		t.Helper()
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(id.String())))
		if _, err := repository.pool.Exec(ctx, `INSERT INTO usage_requests(
id,user_id,session_id,organization_id,token_account_id,credential_id,wallet_id,model,status,execution_status,delivery_status,settlement_status,
maximum_microcredits,actual_microcredits,request_snapshot,started_at,terminal_at,updated_at,idempotency_key_digest,request_fingerprint)
VALUES($1,$2,'usage-history-session',$3,$4,$5,$6,'model-one',$7,'SUCCEEDED','DELIVERED',$8,100,$9,
       CASE WHEN $10='' THEN '{}'::jsonb ELSE jsonb_build_object('logical_turn_id',$10::text) END,$11,$11,$11,$12,$12)`,
			id, userID, organizationID, accountID, credentialID, walletID, status, settlement, actual, logicalTurnID, startedAt, digest); err != nil {
			t.Fatal(err)
		}
	}

	firstID := uuid.New()
	for index := 0; index < 19; index++ {
		actual := int64(index + 1)
		id := uuid.New()
		if index == 0 {
			id = firstID
		}
		insertUsage(id, fmt.Sprintf("turn-%02d", index+1), now.Add(-time.Duration(index)*time.Minute), nil, "COMMITTED", "SETTLED", &actual)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE usage_requests
SET usage_snapshot=jsonb_build_object('usage',jsonb_build_object('input_tokens',101,'output_tokens',11,'cached_input_tokens',7))
WHERE id=$1`, firstID); err != nil {
		t.Fatal(err)
	}
	boundaryHeadID, boundaryTailID := uuid.New(), uuid.New()
	settledActual, releasedActual := int64(200), int64(900)
	insertUsage(boundaryHeadID, "turn-20", now.Add(-19*time.Minute), nil, "COMMITTED", "SETTLED", &settledActual)
	insertUsage(boundaryTailID, "turn-20", now.Add(-20*time.Minute), nil, "RELEASED", "RELEASED", &releasedActual)
	nextID := uuid.New()
	insertUsage(nextID, "turn-21", now.Add(-21*time.Minute), nil, "COMMITTED", "SETTLED", &settledActual)
	legacyID := uuid.New()
	insertUsage(legacyID, "", now.Add(-22*time.Minute), nil, "RELEASED", "RELEASED", &releasedActual)
	organizationID := uuid.New()
	insertUsage(uuid.New(), "organization-turn", now.Add(time.Minute), &organizationID, "COMMITTED", "SETTLED", &settledActual)

	// Simulate the expand phase before the background worker has visited any
	// historical rows, then prove an old binary's unchanged INSERT is still
	// captured by the database trigger while bounded batches converge history.
	if _, err := repository.pool.Exec(ctx, `DELETE FROM usage_history_groups; DELETE FROM usage_history_members;
UPDATE usage_history_projection_state SET last_usage_request_id=NULL,projected_rows=0,completed_at=NULL,updated_at=now()`); err != nil {
		t.Fatal(err)
	}
	rollingUserID, rollingID := uuid.New(), uuid.New()
	userIDBeforeRolling := userID
	userID = rollingUserID
	insertUsage(rollingID, "rolling-old-writer-turn", now.Add(-time.Hour), nil, "RELEASED", "RELEASED", &releasedActual)
	userID = userIDBeforeRolling
	var rollingWriteProjected bool
	if err := repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_history_members WHERE usage_request_id=$1)`, rollingID).Scan(&rollingWriteProjected); err != nil || !rollingWriteProjected {
		t.Fatalf("old Gateway write was not captured during backfill: projected=%v err=%v", rollingWriteProjected, err)
	}
	complete := false
	for batches := 0; batches < 100 && !complete; batches++ {
		_, batchComplete, backfillErr := repository.BackfillUsageHistoryProjection(ctx, 5)
		if backfillErr != nil {
			t.Fatal(backfillErr)
		}
		complete = batchComplete
	}
	if !complete {
		t.Fatal("bounded usage history backfill did not converge")
	}
	if ready, err := repository.UsageHistoryProjectionReady(ctx); err != nil || !ready {
		t.Fatalf("usage history projection readiness = %v, err=%v", ready, err)
	}

	personal := AuthorizationContext{UserID: userID, ContextType: "PERSONAL"}
	firstFetch, err := repository.ListUsageTurnsForContext(ctx, personal, 21, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstFetch) == 0 {
		t.Fatal("usage turn repository returned no rows")
	}
	assertMappedUsageTurn(t, firstFetch[0], firstID, now)
	assertUsageTurnHTTPProjection(t, repository, personal, firstID, now)
	firstPage, cursor := paginateUsageHistoryRecords(firstFetch, 20)
	if len(firstPage) != 20 {
		t.Fatalf("first page has %d rows, want one bounded projection for each of 20 turns", len(firstPage))
	}
	boundary := firstPage[len(firstPage)-1]
	if boundary.UsageRequestID != boundaryHeadID || boundary.UsageRequestCount != 2 {
		t.Fatalf("logical turn was not aggregated at the boundary: %#v", boundary)
	}
	if boundary.ChargedMicrocredits == nil || *boundary.ChargedMicrocredits != settledActual {
		t.Fatalf("turn charge = %v, want %d", boundary.ChargedMicrocredits, settledActual)
	}
	if boundary.ActualMicrocredits == nil || *boundary.ActualMicrocredits != settledActual+releasedActual {
		t.Fatalf("turn Provider actual = %v, want %d", boundary.ActualMicrocredits, settledActual+releasedActual)
	}
	if boundary.InputTokens != nil || boundary.OutputTokens != nil || boundary.CachedInputTokens != nil {
		t.Fatalf("released Provider execution with missing Tokens must keep the turn unknown: %#v", boundary)
	}
	var oldWriterProjected bool
	if err := repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM usage_history_members WHERE usage_request_id=$1)`, boundaryHeadID).Scan(&oldWriterProjected); err != nil || !oldWriterProjected {
		t.Fatalf("old-writer-compatible insert was not projected by the database trigger: projected=%v err=%v", oldWriterProjected, err)
	}
	if cursor == nil || cursor.ID != boundaryHeadID || !cursor.StartedAt.Equal(now.Add(-19*time.Minute)) {
		t.Fatalf("first page cursor = %#v, want boundary head %s", cursor, boundaryHeadID)
	}

	secondFetch, err := repository.ListUsageTurnsForContext(ctx, personal, 21, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondFetch) != 2 || secondFetch[0].UsageRequestID != nextID || secondFetch[1].UsageRequestID != legacyID {
		t.Fatalf("second page = %#v, want next logical turn then independent legacy request", secondFetch)
	}
	organizationFetch, err := repository.ListUsageTurnsForContext(ctx, AuthorizationContext{UserID: userID, ContextType: "ORGANIZATION", OrganizationID: &organizationID}, 20, nil)
	if err != nil || len(organizationFetch) != 1 || organizationFetch[0].LogicalTurnID != "organization-turn" {
		t.Fatalf("organization-scoped history = %#v, err=%v", organizationFetch, err)
	}

	largeUserID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO usage_requests(
id,user_id,session_id,token_account_id,credential_id,wallet_id,model,status,execution_status,delivery_status,settlement_status,
maximum_microcredits,actual_microcredits,request_snapshot,started_at,terminal_at,updated_at,idempotency_key_digest,request_fingerprint)
SELECT gen_random_uuid(),$1,'large-turn-session',$2,$3,$4,'model-one','RELEASED','FAILED','DELIVERED','RELEASED',
       100,1,jsonb_build_object('logical_turn_id','large-turn-00000001'),$5::timestamptz-i*interval '1 microsecond',$5,$5,
       md5(i::text)||md5('idempotency-'||i::text),md5(i::text)||md5('fingerprint-'||i::text)
FROM generate_series(1,10000) AS series(i)`, largeUserID, accountID, credentialID, walletID, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	largePage, err := repository.ListUsageTurnsForContext(ctx, AuthorizationContext{UserID: largeUserID, ContextType: "PERSONAL"}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(largePage) != 1 || largePage[0].UsageRequestCount != 10_000 {
		t.Fatalf("limit=1 large turn response = %#v, want one row summarizing 10000 requests", largePage)
	}
	if largePage[0].ActualMicrocredits == nil || *largePage[0].ActualMicrocredits != 10_000 || largePage[0].ChargedMicrocredits == nil || *largePage[0].ChargedMicrocredits != 0 {
		t.Fatalf("large turn cost/charge projection = %#v", largePage[0])
	}

	otherUserID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO usage_requests(
id,user_id,session_id,token_account_id,credential_id,wallet_id,model,status,execution_status,delivery_status,settlement_status,
maximum_microcredits,actual_microcredits,request_snapshot,started_at,terminal_at,updated_at,idempotency_key_digest,request_fingerprint)
SELECT gen_random_uuid(),$1,'plan-scale-session',$2,$3,$4,'model-one','RELEASED','FAILED','DELIVERED','RELEASED',
       100,1,jsonb_build_object('logical_turn_id','plan-scale-'||lpad(i::text,8,'0')),$5::timestamptz-i*interval '1 microsecond',$5,$5,
       md5('plan-idempotency-'||i::text)||md5(i::text),md5('plan-fingerprint-'||i::text)||md5(i::text)
FROM generate_series(1,40000) AS series(i)`, otherUserID, accountID, credentialID, walletID, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `ANALYZE usage_requests; ANALYZE usage_history_members; ANALYZE usage_history_groups`); err != nil {
		t.Fatal(err)
	}
	var rawPlan []byte
	if err := repository.pool.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+usageTurnProjectionQuery,
		largeUserID, uuid.Nil, (*time.Time)(nil), (*uuid.UUID)(nil), 1).Scan(&rawPlan); err != nil {
		t.Fatal(err)
	}
	assertUsageTurnProductionPlan(t, rawPlan)
}

func assertMappedUsageTurn(t *testing.T, item UserUsageTurnRecord, requestID uuid.UUID, startedAt time.Time) {
	t.Helper()
	if item.UsageRequestID != requestID || item.LogicalTurnID != "turn-01" || item.UsageRequestCount != 1 || item.CancelUsageRequestID != nil {
		t.Fatalf("usage turn identity mapping = %#v", item)
	}
	if item.Model != "model-one" || item.DisplayStatus != "SETTLED" || item.ExecutionStatus != "SUCCEEDED" || item.DeliveryStatus != "DELIVERED" || item.CancelStatus != "NOT_REQUESTED" || item.SettlementStatus != "SETTLED" {
		t.Fatalf("usage turn state mapping = %#v", item)
	}
	if item.ActualMicrocredits == nil || *item.ActualMicrocredits != 1 || item.ChargedMicrocredits == nil || *item.ChargedMicrocredits != 1 {
		t.Fatalf("usage turn cost mapping = %#v", item)
	}
	if item.InputTokens == nil || *item.InputTokens != 101 || item.OutputTokens == nil || *item.OutputTokens != 11 || item.CachedInputTokens == nil || *item.CachedInputTokens != 7 {
		t.Fatalf("usage turn Token mapping = %#v", item)
	}
	if !item.StartedAt.Equal(startedAt) || item.CompletedAt == nil || !item.CompletedAt.Equal(startedAt) || !item.UpdatedAt.Equal(startedAt) {
		t.Fatalf("usage turn timestamp mapping = %#v, want %s", item, startedAt)
	}
}

func assertUsageTurnHTTPProjection(t *testing.T, repository *PostgresRepository, resolved AuthorizationContext, requestID uuid.UUID, startedAt time.Time) {
	t.Helper()
	authorizationRecorder := &authorizationDecisionRecorder{allowed: true, userID: resolved.UserID}
	authorizationServer := httptest.NewServer(authorizationRecorder)
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "usage-history-service-token")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, fixedBillingContextResolver{resolved: resolved}, nil, nil, ProviderURLPolicy{}, nil, nil, 0).WithAuthorizer(authorizer)
	request := httptest.NewRequest(http.MethodGet, "/v1/me/usage-turns?limit=1", nil)
	request.Header.Set("Authorization", "Bearer usage-history-user")
	response := httptest.NewRecorder()
	service.Handler("usage-history-service-token", successfulBillingVerify).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("usage turn HTTP status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Items       []UserUsageTurnRecord `json:"items"`
		NextCursor  *string               `json:"next_cursor"`
		ViewVersion string                `json:"view_version"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.ViewVersion != "logical-turns-v1" || body.NextCursor == nil || !strings.HasPrefix(*body.NextCursor, usageTurnCursorPrefix) {
		t.Fatalf("usage turn HTTP envelope = %#v", body)
	}
	assertMappedUsageTurn(t, body.Items[0], requestID, startedAt)
	if len(authorizationRecorder.permissions) != 1 || authorizationRecorder.permissions[0] != "personal.usage.read" {
		t.Fatalf("usage turn HTTP permissions=%v", authorizationRecorder.permissions)
	}
}

type usageTurnExplain struct {
	Plan          usageTurnPlan `json:"Plan"`
	ExecutionTime float64       `json:"Execution Time"`
}

type usageTurnPlan struct {
	NodeType      string          `json:"Node Type"`
	RelationName  string          `json:"Relation Name"`
	IndexName     string          `json:"Index Name"`
	ActualRows    float64         `json:"Actual Rows"`
	ActualLoops   float64         `json:"Actual Loops"`
	SortMethod    string          `json:"Sort Method"`
	SortSpaceType string          `json:"Sort Space Type"`
	TempRead      int64           `json:"Temp Read Blocks"`
	TempWritten   int64           `json:"Temp Written Blocks"`
	Plans         []usageTurnPlan `json:"Plans"`
}

func assertUsageTurnProductionPlan(t *testing.T, raw []byte) {
	t.Helper()
	var documents []usageTurnExplain
	if err := json.Unmarshal(raw, &documents); err != nil || len(documents) != 1 {
		t.Fatalf("decode production usage turn plan: %v, %s", err, raw)
	}
	if documents[0].ExecutionTime > 10_000 {
		t.Fatalf("usage turn query exceeded the non-flaky 10s ceiling: %.2fms", documents[0].ExecutionTime)
	}
	indexes := map[string]float64{}
	var inspect func(usageTurnPlan)
	inspect = func(plan usageTurnPlan) {
		if plan.IndexName != "" {
			indexes[plan.IndexName] += plan.ActualRows * plan.ActualLoops
		}
		if plan.SortSpaceType == "Disk" || plan.SortMethod == "external merge" || plan.TempRead > 0 || plan.TempWritten > 0 {
			t.Fatalf("usage turn plan spilled a sort to disk: %#v", plan)
		}
		if plan.NodeType == "Seq Scan" && (plan.RelationName == "usage_history_groups" || plan.RelationName == "usage_history_members") {
			t.Fatalf("usage turn plan scanned a projection sidecar sequentially: %#v", plan)
		}
		for _, child := range plan.Plans {
			inspect(child)
		}
	}
	inspect(documents[0].Plan)
	if rows, ok := indexes["usage_history_groups_page_idx"]; !ok || rows < 1 || rows > 2 {
		t.Fatalf("page index missing or scanned too many rows: %#v", indexes)
	}
	if rows, ok := indexes["usage_history_members_group_idx"]; !ok || rows < 1 || rows > 10_001 {
		t.Fatalf("member index missing or scanned beyond the selected 10000-request turn: %#v", indexes)
	}
}
