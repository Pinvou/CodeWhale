package billing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestAdminPaginationPostgresIntegration intentionally uses a real PostgreSQL
// instance because snapshot isolation, advisory locks, statement_timeout, and
// transaction rollback cannot be proved by a mock. CI supplies a dedicated
// database through PINVOU_TEST_BILLING_DATABASE_URL; local runs skip safely.
func TestAdminPaginationPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, schema := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	now := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	repository.now = func() time.Time { return now }

	originalIDs := seedAdminPaginationWallets(t, ctx, repository.pool, now, 6)
	subjectID := uuid.New()
	request := AdminPaginationRequest{SubjectID: subjectID, Resource: adminPageWallets, Filters: map[string]string{}, Limit: 2}
	first, err := repository.AdminPaginationPage(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" || !first.SnapshotAsOf.Equal(now) {
		t.Fatalf("first page did not return a continuation bound to the snapshot: %+v", first)
	}

	// Mutations overlap cursor traversal after the snapshot is committed.
	// Membership and order must remain frozen while later item hydration is free
	// to expose current state.
	insertedID := uuid.New()
	mutationReady := make(chan struct{})
	allowMutationCommit := make(chan struct{})
	mutationResult := make(chan error, 1)
	go func() {
		tx, beginErr := repository.pool.Begin(ctx)
		if beginErr != nil {
			mutationResult <- beginErr
			close(mutationReady)
			return
		}
		defer tx.Rollback(ctx)
		_, insertErr := tx.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',$3,$3)`, insertedID, uuid.New(), now.Add(time.Minute))
		if insertErr == nil {
			_, insertErr = tx.Exec(ctx, `UPDATE wallets SET status='FROZEN',updated_at=$2 WHERE id=$1`, originalIDs[len(originalIDs)-1], now.Add(2*time.Minute))
		}
		close(mutationReady)
		if insertErr != nil {
			mutationResult <- insertErr
			return
		}
		<-allowMutationCommit
		mutationResult <- tx.Commit(ctx)
	}()
	<-mutationReady
	gotIDs := append([]string{}, first.ResourceIDs...)
	cursor := first.NextCursor
	var allowCommitOnce sync.Once
	for cursor != "" {
		before := countAdminPaginationRows(t, ctx, repository.pool)
		continuation := request
		continuation.Cursor = cursor
		page, readErr := repository.AdminPaginationPage(ctx, continuation)
		if readErr != nil {
			t.Fatal(readErr)
		}
		replayed, replayErr := repository.AdminPaginationPage(ctx, continuation)
		if replayErr != nil {
			t.Fatal(replayErr)
		}
		if !reflect.DeepEqual(page, replayed) {
			t.Fatalf("cursor replay changed the page: first=%+v replay=%+v", page, replayed)
		}
		if after := countAdminPaginationRows(t, ctx, repository.pool); after != before {
			t.Fatalf("cursor replay wrote pagination state: before=%d after=%d", before, after)
		}
		gotIDs = append(gotIDs, page.ResourceIDs...)
		cursor = page.NextCursor
		allowCommitOnce.Do(func() { close(allowMutationCommit) })
	}
	allowCommitOnce.Do(func() { close(allowMutationCommit) })
	if mutationErr := <-mutationResult; mutationErr != nil {
		t.Fatal(mutationErr)
	}
	if !reflect.DeepEqual(gotIDs, originalIDs) {
		t.Fatalf("snapshot membership changed after concurrent mutations: got=%v want=%v", gotIDs, originalIDs)
	}
	for _, id := range gotIDs {
		if id == insertedID.String() {
			t.Fatal("post-snapshot wallet leaked into frozen membership")
		}
	}

	continuation := request
	continuation.Cursor = first.NextCursor
	for name, mutate := range map[string]func(*AdminPaginationRequest){
		"subject":  func(value *AdminPaginationRequest) { value.SubjectID = uuid.New() },
		"resource": func(value *AdminPaginationRequest) { value.Resource = adminPageReceivables },
		"scope":    func(value *AdminPaginationRequest) { value.Scope = uuid.NewString() },
		"filter":   func(value *AdminPaginationRequest) { value.Filters = map[string]string{"status": "ACTIVE"} },
	} {
		misused := continuation
		mutate(&misused)
		if _, err := repository.AdminPaginationPage(ctx, misused); !errors.Is(err, ErrPaginationCursorInvalid) {
			t.Fatalf("cross-%s cursor misuse returned %v", name, err)
		}
	}
	forged := continuation
	replacement := "A"
	if strings.HasPrefix(forged.Cursor, replacement) {
		replacement = "B"
	}
	forged.Cursor = replacement + forged.Cursor[1:]
	if _, err := repository.AdminPaginationPage(ctx, forged); !errors.Is(err, ErrPaginationCursorInvalid) {
		t.Fatalf("forged cursor returned %v", err)
	}

	// Concurrent replacement for the same subject/resource must serialize and
	// leave only one active snapshot rather than violating the unique invariant.
	concurrentSubject := uuid.New()
	concurrentRequest := AdminPaginationRequest{SubjectID: concurrentSubject, Resource: adminPageWallets, Filters: map[string]string{}, Limit: 2}
	start := make(chan struct{})
	errorsByWorker := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, workerErr := repository.AdminPaginationPage(ctx, concurrentRequest)
			errorsByWorker <- workerErr
		}()
	}
	close(start)
	workers.Wait()
	close(errorsByWorker)
	for workerErr := range errorsByWorker {
		if workerErr != nil {
			t.Fatalf("serialized snapshot replacement failed: %v", workerErr)
		}
	}
	var active int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM admin_pagination_snapshots WHERE subject_id=$1 AND resource_type=$2`, concurrentSubject, adminPageWallets).Scan(&active); err != nil || active != 1 {
		t.Fatalf("same-resource concurrency left %d snapshots: %v", active, err)
	}
	capacitySubject := uuid.New()
	for _, resource := range []string{adminPageReceivables, adminPagePaymentOrders} {
		if _, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: capacitySubject, Resource: resource, Filters: map[string]string{}, Limit: 1}); err != nil {
			t.Fatalf("create subject-capacity prerequisite: %v", err)
		}
	}
	var oldestSnapshotID uuid.UUID
	var oldestSnapshotSecret []byte
	if err := repository.pool.QueryRow(ctx, `SELECT id,cursor_secret FROM admin_pagination_snapshots WHERE subject_id=$1 ORDER BY created_at,id LIMIT 1`, capacitySubject).Scan(&oldestSnapshotID, &oldestSnapshotSecret); err != nil {
		t.Fatalf("read oldest subject snapshot: %v", err)
	}
	oldestCursor, err := signedAdminCursor(oldestSnapshotID, 0, oldestSnapshotSecret)
	if err != nil {
		t.Fatalf("sign oldest subject cursor: %v", err)
	}
	if _, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: capacitySubject, Resource: adminPageRefunds, Filters: map[string]string{}, Limit: 1}); err != nil {
		t.Fatalf("third resource should recycle the oldest subject snapshot: %v", err)
	}
	if _, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: capacitySubject, Resource: adminPageReceivables, Filters: map[string]string{}, Cursor: oldestCursor, Limit: 1}); !errors.Is(err, ErrPaginationCursorExpired) {
		t.Fatalf("recycled subject snapshot cursor returned %v", err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM admin_pagination_snapshots WHERE subject_id=$1 AND expires_at>$2`, capacitySubject, now).Scan(&active); err != nil || active != 2 {
		t.Fatalf("subject snapshot recycling left %d active snapshots: %v", active, err)
	}
	rateSubject := uuid.New()
	for attempt := 0; attempt < adminSnapshotSubjectPerMin; attempt++ {
		if _, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: rateSubject, Resource: adminPageReceivables, Filters: map[string]string{}, Limit: 1}); err != nil {
			t.Fatalf("allowed subject snapshot %d returned %v", attempt+1, err)
		}
	}
	if _, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: rateSubject, Resource: adminPageReceivables, Filters: map[string]string{}, Limit: 1}); !errors.Is(err, ErrPaginationSnapshotLimited) {
		t.Fatalf("seventh subject snapshot in one minute returned %v", err)
	}

	// Every production resource query is parsed and executed by PostgreSQL. Most
	// tables are intentionally empty; this catches stale columns and invalid joins.
	for _, resource := range []string{
		adminPageWallets, adminPageWalletLedger, adminPageReceivables, adminPagePaymentOrders,
		adminPageRefunds, adminPageAdjustments, adminPageUsageHolds, adminPageLatePaymentReviews,
		adminPagePaymentAnomalies, adminPageReconciliationRuns, adminPageReconciliationCases,
	} {
		scope := ""
		if resource == adminPageWalletLedger {
			scope = originalIDs[0]
		} else if resource == adminPageReconciliationCases {
			scope = uuid.NewString()
		}
		_, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: uuid.New(), Resource: resource, Scope: scope, Filters: map[string]string{}, Limit: 1})
		if err != nil {
			t.Fatalf("PostgreSQL rejected %s snapshot query: %v", resource, err)
		}
	}

	// Global runtime limits are exercised with transactionally inserted fixtures
	// so the test reaches exact boundaries without materializing 20 million rows.
	var activeSnapshots int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM admin_pagination_snapshots WHERE expires_at>$1`, now).Scan(&activeSnapshots); err != nil {
		t.Fatal(err)
	}
	for index := activeSnapshots; index < adminSnapshotMaxGlobal; index++ {
		insertAdminPaginationFixture(t, ctx, repository.pool, now, "snapshot", index, 0)
	}
	globalSnapshotSubject := uuid.New()
	if _, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: globalSnapshotSubject, Resource: adminPageReceivables, Filters: map[string]string{}, Limit: 1}); !errors.Is(err, ErrPaginationSnapshotLimited) {
		t.Fatalf("global snapshot boundary returned %v", err)
	}
	assertNoAdminPaginationSnapshot(t, ctx, repository.pool, globalSnapshotSubject)
	if _, err := repository.pool.Exec(ctx, `DELETE FROM admin_pagination_snapshots WHERE resource_type LIKE 'fixture-snapshot-%'`); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < adminSnapshotMaxGlobalMember/adminSnapshotMaxMembers; index++ {
		insertAdminPaginationFixture(t, ctx, repository.pool, now, "members", index, adminSnapshotMaxMembers)
	}
	globalMemberSubject := uuid.New()
	if _, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: globalMemberSubject, Resource: adminPageReceivables, Filters: map[string]string{}, Limit: 1}); !errors.Is(err, ErrPaginationSnapshotLimited) {
		t.Fatalf("global member boundary returned %v", err)
	}
	assertNoAdminPaginationSnapshot(t, ctx, repository.pool, globalMemberSubject)
	if _, err := repository.pool.Exec(ctx, `DELETE FROM admin_pagination_snapshots WHERE resource_type LIKE 'fixture-members-%'`); err != nil {
		t.Fatal(err)
	}
	window := now.Truncate(time.Minute)
	if _, err := repository.pool.Exec(ctx, `INSERT INTO admin_pagination_creation_windows(rate_key,window_start,creation_count) VALUES('global',$1,$2) ON CONFLICT(rate_key,window_start) DO UPDATE SET creation_count=EXCLUDED.creation_count`, window, adminSnapshotGlobalPerMin); err != nil {
		t.Fatal(err)
	}
	globalRateSubject := uuid.New()
	if _, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: globalRateSubject, Resource: adminPageReceivables, Filters: map[string]string{}, Limit: 1}); !errors.Is(err, ErrPaginationSnapshotLimited) {
		t.Fatalf("global creation-rate boundary returned %v", err)
	}
	assertNoAdminPaginationSnapshot(t, ctx, repository.pool, globalRateSubject)
	var globalCreations, subjectRateRows int
	if err := repository.pool.QueryRow(ctx, `SELECT creation_count,(SELECT count(*) FROM admin_pagination_creation_windows WHERE rate_key=$2 AND window_start=$1) FROM admin_pagination_creation_windows WHERE rate_key='global' AND window_start=$1`, window, "subject:"+globalRateSubject.String()).Scan(&globalCreations, &subjectRateRows); err != nil {
		t.Fatal(err)
	}
	if globalCreations != adminSnapshotGlobalPerMin || subjectRateRows != 0 {
		t.Fatalf("rate-limit rejection left partial quota state: global=%d subject_rows=%d", globalCreations, subjectRateRows)
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM admin_pagination_creation_windows WHERE window_start=$1 AND (rate_key='global' OR rate_key=$2)`, window, "subject:"+globalRateSubject.String()); err != nil {
		t.Fatal(err)
	}
	storageSubject := uuid.New()
	repository.paginationMaxStorageBytes = 1
	if _, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: storageSubject, Resource: adminPageReceivables, Filters: map[string]string{}, Limit: 1}); !errors.Is(err, ErrPaginationSnapshotLimited) {
		t.Fatalf("storage boundary returned %v", err)
	}
	assertNoAdminPaginationSnapshot(t, ctx, repository.pool, storageSubject)
	repository.paginationMaxStorageBytes = adminSnapshotMaxStorageBytes

	// Expiry is observable before and after cleanup. The tombstone keeps only the
	// per-snapshot verification secret, so a valid old cursor remains 410 while a
	// fabricated token remains 400.
	now = now.Add(16 * time.Minute)
	if _, err := repository.AdminPaginationPage(ctx, continuation); !errors.Is(err, ErrPaginationCursorExpired) {
		t.Fatalf("expired live snapshot returned %v", err)
	}
	if err := repository.CleanupAdminPagination(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.AdminPaginationPage(ctx, continuation); !errors.Is(err, ErrPaginationCursorExpired) {
		t.Fatalf("expired tombstoned snapshot returned %v", err)
	}
	var tombstones int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM admin_pagination_snapshot_tombstones`).Scan(&tombstones); err != nil || tombstones == 0 {
		t.Fatalf("cleanup did not retain expiry evidence: count=%d err=%v", tombstones, err)
	}

	// A lock-induced statement timeout must roll back the partially created
	// snapshot. This subcase intentionally takes approximately ten seconds.
	lockConnection, err := repository.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockConnection.Release()
	lockTx, err := lockConnection.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `LOCK TABLE wallets IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	timeoutSubject := uuid.New()
	_, timeoutErr := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: timeoutSubject, Resource: adminPageWallets, Filters: map[string]string{}, Limit: 1})
	_ = lockTx.Rollback(ctx)
	if !errors.Is(timeoutErr, ErrPaginationSnapshotUnavailable) {
		t.Fatalf("statement timeout returned %v", timeoutErr)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM admin_pagination_snapshots WHERE subject_id=$1`, timeoutSubject).Scan(&active); err != nil || active != 0 {
		t.Fatalf("timed-out snapshot did not roll back: count=%d err=%v", active, err)
	}

	t.Logf("validated PostgreSQL pagination lifecycle in schema %s", schema)
}

func newAdminPaginationIntegrationRepository(t *testing.T, ctx context.Context, databaseURL string) (*PostgresRepository, string) {
	t.Helper()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect integration database: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "billing_pagination_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatalf("create isolated integration schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = connection.Exec(cleanupCtx, `DROP SCHEMA `+identifier+` CASCADE`)
	})
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatalf("select isolated integration schema: %v", err)
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatalf("migrate isolated integration schema: %v", err)
	}
	migrations.Unlock(ctx, connection)

	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.MaxConns = 8
	if configuration.ConnConfig.RuntimeParams == nil {
		configuration.ConnConfig.RuntimeParams = make(map[string]string)
	}
	configuration.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	return &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }, paginationMetrics: newAdminPaginationMetrics(), paginationMaxStorageBytes: adminSnapshotMaxStorageBytes}, schema
}

func seedAdminPaginationWallets(t *testing.T, ctx context.Context, pool *pgxpool.Pool, now time.Time, count int) []string {
	t.Helper()
	for index := 0; index < count; index++ {
		createdAt := now.Add(-time.Duration(index) * time.Minute)
		if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',$3,$3)`, uuid.New(), uuid.New(), createdAt); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT id::text FROM wallets ORDER BY created_at DESC,id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := make([]string, 0, count)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != count {
		t.Fatalf("seeded %d wallets, read %d", count, len(ids))
	}
	return ids
}

func countAdminPaginationRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var count int64
	err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM admin_pagination_snapshots)+
		(SELECT count(*) FROM admin_pagination_snapshot_members)+
		(SELECT count(*) FROM admin_pagination_snapshot_tombstones)+
		(SELECT count(*) FROM admin_pagination_creation_windows)`).Scan(&count)
	if err != nil {
		t.Fatal(fmt.Errorf("count pagination rows: %w", err))
	}
	return count
}

func insertAdminPaginationFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, now time.Time, group string, index, memberCount int) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO admin_pagination_snapshots(id,subject_id,resource_type,scope_digest,filter_digest,cursor_secret,snapshot_as_of,expires_at,member_count,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$7)`,
		uuid.New(), uuid.New(), fmt.Sprintf("fixture-%s-%d", group, index), adminPaginationDigest(""), adminPaginationDigest(map[string]string{}), []byte("0123456789abcdef0123456789abcdef"), now, now.Add(adminSnapshotTTL), memberCount)
	if err != nil {
		t.Fatal(err)
	}
}

func assertNoAdminPaginationSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, subjectID uuid.UUID) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_pagination_snapshots WHERE subject_id=$1`, subjectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("limited snapshot left %d partial rows for subject %s", count, subjectID)
	}
}
