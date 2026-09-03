package token

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestTokenAdminPaginationPostgresIntegration uses an isolated schema because
// SERIALIZABLE replacement, advisory locking, database time, and rollback of a
// million-member overflow cannot be established by mocks. CI supplies
// PINVOU_TEST_TOKEN_DATABASE_URL; local runs skip without weakening the gate.
func TestTokenAdminPaginationPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repository := newTokenPaginationIntegrationRepository(t, ctx, databaseURL)
	key := []byte("0123456789abcdef0123456789abcdef")
	if err := repository.ConfigureAdminPagination(AdminPaginationConfig{Enabled: true, CursorHMACKey: key}); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetAdminPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `CREATE TABLE pagination_source(id TEXT PRIMARY KEY,label TEXT NOT NULL,created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp())`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO pagination_source(id,label) SELECT 'resource-'||lpad(value::text,7,'0'),'label-'||lpad(value::text,7,'0') FROM generate_series(1,1501) value`); err != nil {
		t.Fatal(err)
	}
	source := func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		_, err := tx.Exec(ctx, `INSERT INTO token_admin_pagination_snapshot_members(snapshot_id,position,resource_id)
			SELECT $1,row_number() OVER(ORDER BY label,id)-1,id FROM (SELECT id,label FROM pagination_source WHERE created_at<=$2 ORDER BY label,id LIMIT $3) source`, snapshotID, highWater, maximum)
		return err
	}

	started := time.Now()
	request := AdminPaginationRequest{SubjectID: uuid.New(), AuthorizationVersion: 11, Resource: "models", Scope: "platform", Filters: map[string]string{}, Sort: "label ASC,id ASC", Limit: 50}
	first, err := repository.AdminPaginationPage(ctx, request, source)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > AdminSnapshotBuildBudget || first.NextCursor == nil || first.AsOf.IsZero() || !first.SnapshotExpiresAt.Equal(first.AsOf.Add(AdminSnapshotTTL)) {
		t.Fatalf("first snapshot violated budget/envelope: elapsed=%s page=%+v", time.Since(started), first)
	}
	// This ID sorts after the current cursor but must not enter an existing DB-time snapshot.
	if _, err := repository.pool.Exec(ctx, `INSERT INTO pagination_source(id,label) VALUES('zzzz-post-snapshot','zzzz-post-snapshot')`); err != nil {
		t.Fatal(err)
	}
	ids := append([]string{}, first.ResourceIDs...)
	cursor := *first.NextCursor
	for cursor != "" {
		continuation := request
		continuation.Cursor = cursor
		before := paginationRowCount(t, ctx, repository.pool)
		page, readErr := repository.AdminPaginationPage(ctx, continuation, func(context.Context, pgx.Tx, uuid.UUID, time.Time, int) error {
			t.Fatal("continuation queried the fact source")
			return nil
		})
		if readErr != nil {
			t.Fatal(readErr)
		}
		if after := paginationRowCount(t, ctx, repository.pool); after != before {
			t.Fatalf("continuation wrote state: before=%d after=%d", before, after)
		}
		ids = append(ids, page.ResourceIDs...)
		cursor = ""
		if page.NextCursor != nil {
			cursor = *page.NextCursor
		}
	}
	if len(ids) != 1501 {
		t.Fatalf("full traversal returned %d members", len(ids))
	}
	// Application wall-clock skew cannot expire or extend a DB-time cursor.
	continuationWithSkew := request
	continuationWithSkew.Cursor = *first.NextCursor
	for _, skew := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		repository.now = func() time.Time { return time.Now().UTC().Add(skew) }
		if _, err := repository.AdminPaginationPage(ctx, continuationWithSkew, nil); err != nil {
			t.Fatalf("application clock skew %s changed DB-time cursor validity: %v", skew, err)
		}
	}
	repository.now = func() time.Time { return time.Now().UTC() }

	// Gate-off prevents first pages but preserves an already committed cursor.
	if err := repository.SetAdminPaginationGate(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: uuid.New(), AuthorizationVersion: 1, Resource: "gate-off", Sort: "id ASC", Limit: 50}, source); !errors.Is(err, ErrAdminPaginationDisabled) {
		t.Fatalf("gate-off first page returned %v", err)
	}
	if _, err := repository.AdminPaginationPage(ctx, continuationWithSkew, nil); err != nil {
		t.Fatalf("gate-off invalidated a live cursor early: %v", err)
	}
	if err := repository.SetAdminPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}
	for index, id := range ids {
		want := fmt.Sprintf("resource-%07d", index+1)
		if id != want {
			t.Fatalf("position %d=%s want=%s", index, id, want)
		}
	}

	changedAuthorization := request
	changedAuthorization.Cursor = *first.NextCursor
	changedAuthorization.AuthorizationVersion++
	if _, err := repository.AdminPaginationPage(ctx, changedAuthorization, nil); !errors.Is(err, ErrAdminPaginationCursorExpired) {
		t.Fatalf("authorization change returned %v", err)
	}
	forged := request
	forged.Cursor = "A" + (*first.NextCursor)[1:]
	if _, err := repository.AdminPaginationPage(ctx, forged, nil); !errors.Is(err, ErrAdminPaginationCursorInvalid) {
		t.Fatalf("forged cursor returned %v", err)
	}

	// Concurrent replacement is serialized; the older valid cursor becomes a
	// source-local 410 tombstone and no half snapshot survives.
	concurrent := request
	concurrent.SubjectID = uuid.New()
	start := make(chan struct{})
	results := make(chan AdminPaginationPage, 2)
	errs := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			page, pageErr := repository.AdminPaginationPage(ctx, concurrent, source)
			results <- page
			errs <- pageErr
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errs)
	for workerErr := range errs {
		if workerErr != nil {
			t.Fatalf("concurrent replacement: %v", workerErr)
		}
	}
	pages := make([]AdminPaginationPage, 0, 2)
	for page := range results {
		pages = append(pages, page)
	}
	if reflect.DeepEqual(pages[0].NextCursor, pages[1].NextCursor) {
		t.Fatal("concurrent replacement reused a snapshot cursor")
	}
	var active int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_admin_pagination_snapshots WHERE subject_id=$1 AND resource_type=$2`, concurrent.SubjectID, concurrent.Resource).Scan(&active); err != nil || active != 1 {
		t.Fatalf("concurrent replacement left %d snapshots: %v", active, err)
	}
	old := concurrent
	old.Cursor = *pages[0].NextCursor
	_, firstErr := repository.AdminPaginationPage(ctx, old, nil)
	old.Cursor = *pages[1].NextCursor
	_, secondErr := repository.AdminPaginationPage(ctx, old, nil)
	if !errors.Is(firstErr, ErrAdminPaginationCursorExpired) && !errors.Is(secondErr, ErrAdminPaginationCursorExpired) {
		t.Fatalf("neither replaced cursor returned 410: %v / %v", firstErr, secondErr)
	}

	// A third resource transactionally recycles only this subject's oldest resource.
	recycleSubject := uuid.New()
	var oldest AdminPaginationPage
	for index, resource := range []string{"models", "protocols", "formulas"} {
		recycleRequest := request
		recycleRequest.SubjectID = recycleSubject
		recycleRequest.Resource = resource
		page, pageErr := repository.AdminPaginationPage(ctx, recycleRequest, source)
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		if index == 0 {
			oldest = page
		}
	}
	recycled := request
	recycled.SubjectID = recycleSubject
	recycled.Cursor = *oldest.NextCursor
	if _, err := repository.AdminPaginationPage(ctx, recycled, nil); !errors.Is(err, ErrAdminPaginationCursorExpired) {
		t.Fatalf("third-resource recycled cursor returned %v", err)
	}

	// Capacity rejection cannot leave a snapshot; the separately committed
	// creation-attempt quota remains consumed by design.
	repository.adminPagination.maxStorageBytes = 1
	limited := request
	limited.SubjectID = uuid.New()
	limited.Resource = "token-accounts"
	if _, err := repository.AdminPaginationPage(ctx, limited, source); !errors.Is(err, ErrAdminPaginationSnapshotLimited) {
		t.Fatalf("storage boundary returned %v", err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_admin_pagination_snapshots WHERE subject_id=$1`, limited.SubjectID).Scan(&active); err != nil || active != 0 {
		t.Fatalf("capacity refusal left a snapshot: %d, %v", active, err)
	}
	repository.adminPagination.maxStorageBytes = AdminSnapshotMaxStorageBytes

	// Cleanup is idempotent and retains a 20-minute tombstone.
	if _, err := repository.pool.Exec(ctx, `UPDATE token_admin_pagination_snapshots SET expires_at=transaction_timestamp()-interval '1 second' WHERE subject_id=$1`, request.SubjectID); err != nil {
		t.Fatal(err)
	}
	if err := repository.CleanupAdminPagination(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repository.CleanupAdminPagination(ctx); err != nil {
		t.Fatal(err)
	}
	continuation := request
	continuation.Cursor = *first.NextCursor
	if _, err := repository.AdminPaginationPage(ctx, continuation, nil); !errors.Is(err, ErrAdminPaginationCursorExpired) {
		t.Fatalf("cleaned cursor returned %v", err)
	}
	var tombstoneRemaining time.Duration
	firstCursorPayload, err := parseAdminPaginationCursor(*first.NextCursor, repository.adminPagination.cursorHMACKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT purge_after-transaction_timestamp() FROM token_admin_pagination_snapshot_tombstones WHERE snapshot_id=$1`, firstCursorPayload.SnapshotID).Scan(&tombstoneRemaining); err != nil {
		t.Fatal(err)
	}
	if tombstoneRemaining < AdminSnapshotTombstoneTTL-time.Minute {
		t.Fatalf("tombstone retention is shorter than 20 minutes: %s", tombstoneRemaining)
	}

	// Failed builders roll back header, members and tombstone while preserving
	// the separately committed creation-attempt quota.
	failed := request
	failed.SubjectID = uuid.New()
	failed.Resource = "failed-build"
	beforeFailureSnapshots := paginationSnapshotStateCount(t, ctx, repository.pool)
	if _, err := repository.AdminPaginationPage(ctx, failed, func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, _ time.Time, _ int) error {
		if _, insertErr := tx.Exec(ctx, `INSERT INTO token_admin_pagination_snapshot_members(snapshot_id,position,resource_id) VALUES($1,0,'partial')`, snapshotID); insertErr != nil {
			return insertErr
		}
		return errors.New("injected builder failure")
	}); !errors.Is(err, ErrAdminPaginationSnapshotUnavailable) {
		t.Fatalf("injected transaction failure returned %v", err)
	}
	if afterFailure := paginationSnapshotStateCount(t, ctx, repository.pool); afterFailure != beforeFailureSnapshots {
		t.Fatalf("failed build left partial snapshot state: before=%d after=%d", beforeFailureSnapshots, afterFailure)
	}
	interrupted := request
	interrupted.SubjectID = uuid.New()
	interrupted.Resource = "connection-interrupted"
	if _, err := repository.AdminPaginationPage(ctx, interrupted, func(ctx context.Context, tx pgx.Tx, _ uuid.UUID, _ time.Time, _ int) error {
		_, terminateErr := tx.Exec(ctx, `SELECT pg_terminate_backend(pg_backend_pid())`)
		return terminateErr
	}); !errors.Is(err, ErrAdminPaginationSnapshotUnavailable) {
		t.Fatalf("connection interruption returned %v", err)
	}
	if err := repository.pool.Ping(ctx); err != nil {
		t.Fatalf("pool did not recover after interrupted builder: %v", err)
	}
	if count := paginationSnapshotCountForSubject(t, ctx, repository.pool, interrupted.SubjectID); count != 0 {
		t.Fatalf("interrupted builder left %d snapshots", count)
	}
	clientInterrupted := request
	clientInterrupted.SubjectID = uuid.New()
	clientInterrupted.Resource = "client-process-interrupted"
	clientContext, cancelClient := context.WithCancel(ctx)
	if _, err := repository.AdminPaginationPage(clientContext, clientInterrupted, func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, _ time.Time, _ int) error {
		if _, insertErr := tx.Exec(ctx, `INSERT INTO token_admin_pagination_snapshot_members(snapshot_id,position,resource_id) VALUES($1,0,'partial-before-client-exit')`, snapshotID); insertErr != nil {
			return insertErr
		}
		cancelClient()
		return ctx.Err()
	}); !errors.Is(err, ErrAdminPaginationSnapshotUnavailable) {
		cancelClient()
		t.Fatalf("client process interruption returned %v", err)
	}
	cancelClient()
	if count := paginationSnapshotCountForSubject(t, ctx, repository.pool, clientInterrupted.SubjectID); count != 0 {
		t.Fatalf("client process interruption left %d snapshots", count)
	}
	var failedAttempts int
	if err := repository.pool.QueryRow(ctx, `SELECT creation_count FROM token_admin_pagination_creation_windows WHERE rate_key=$1 AND window_start=date_trunc('minute',transaction_timestamp())`, "subject:"+failed.SubjectID.String()).Scan(&failedAttempts); err != nil || failedAttempts != 1 {
		t.Fatalf("failed build did not consume exactly one attempt: count=%d err=%v", failedAttempts, err)
	}
	serializationRetried := request
	serializationRetried.SubjectID = uuid.New()
	serializationRetried.Resource = "serialization-retried"
	serializationCalls := 0
	if _, err := repository.AdminPaginationPage(ctx, serializationRetried, func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, highWater time.Time, maximum int) error {
		serializationCalls++
		if serializationCalls == 1 {
			return &pgconn.PgError{Code: "40001", Message: "injected serialization failure"}
		}
		return source(ctx, tx, snapshotID, highWater, maximum)
	}); err != nil || serializationCalls != 2 {
		t.Fatalf("serialization retry did not recover exactly once: calls=%d err=%v", serializationCalls, err)
	}
	var serializationAttempts int
	if err := repository.pool.QueryRow(ctx, `SELECT creation_count FROM token_admin_pagination_creation_windows WHERE rate_key=$1 AND window_start=date_trunc('minute',transaction_timestamp())`, "subject:"+serializationRetried.SubjectID.String()).Scan(&serializationAttempts); err != nil || serializationAttempts != 1 {
		t.Fatalf("serialization retry double-charged attempt: count=%d err=%v", serializationAttempts, err)
	}

	// Exact 70/85 percent snapshot-water marks are observable, and critical
	// state refuses a new non-replacement before its builder runs.
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_admin_pagination_snapshots; DELETE FROM token_admin_pagination_snapshot_tombstones; DELETE FROM token_admin_pagination_creation_windows`); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 45; index++ {
		insertTokenPaginationCapacityFixture(t, ctx, repository.pool, index, 0)
	}
	capacity, err := repository.AdminPaginationCapacity(ctx)
	if err != nil || !capacity.Warning || capacity.Critical {
		t.Fatalf("70%% capacity signal=%+v err=%v", capacity, err)
	}
	for index := 45; index < 55; index++ {
		insertTokenPaginationCapacityFixture(t, ctx, repository.pool, index, 0)
	}
	capacity, err = repository.AdminPaginationCapacity(ctx)
	if err != nil || !capacity.Critical {
		t.Fatalf("85%% capacity signal=%+v err=%v", capacity, err)
	}
	critical := request
	critical.SubjectID = uuid.New()
	critical.Resource = "critical-refusal"
	builderCalled := false
	if _, err := repository.AdminPaginationPage(ctx, critical, func(context.Context, pgx.Tx, uuid.UUID, time.Time, int) error { builderCalled = true; return nil }); !errors.Is(err, ErrAdminPaginationSnapshotLimited) || builderCalled {
		t.Fatalf("critical capacity did not reject before build: err=%v called=%v", err, builderCalled)
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_admin_pagination_snapshots`); err != nil {
		t.Fatal(err)
	}
	capacity, err = repository.AdminPaginationCapacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	baselineBytes := capacity.StorageBytes
	repository.adminPagination.maxStorageBytes = max(1, baselineBytes*100/70-1)
	capacity, err = repository.AdminPaginationCapacity(ctx)
	if err != nil || !capacity.Warning {
		t.Fatalf("byte 70%% water mark=%+v err=%v", capacity, err)
	}
	repository.adminPagination.maxStorageBytes = max(1, baselineBytes*100/85-1)
	capacity, err = repository.AdminPaginationCapacity(ctx)
	if err != nil || !capacity.Critical {
		t.Fatalf("byte 85%% water mark=%+v err=%v", capacity, err)
	}
	repository.adminPagination.maxStorageBytes = AdminSnapshotMaxStorageBytes

	// Member-count water marks use the same aggregate query as production
	// metrics without materializing tens of millions of member rows in CI.
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_admin_pagination_snapshots; DELETE FROM token_admin_pagination_snapshot_tombstones; DELETE FROM token_admin_pagination_creation_windows`); err != nil {
		t.Fatal(err)
	}
	replacementSnapshotID := uuid.New()
	replacementSubject := uuid.New()
	const replacementResource = "member-capacity-replacement"
	insertTokenPaginationCapacityFixtureFor(t, ctx, repository.pool, replacementSnapshotID, replacementSubject, replacementResource, AdminSnapshotMaxMembers)
	for index := 1; index < 14; index++ {
		insertTokenPaginationCapacityFixture(t, ctx, repository.pool, index, AdminSnapshotMaxMembers)
	}
	capacity, err = repository.AdminPaginationCapacity(ctx)
	if err != nil || capacity.ActiveMembers != 14_000_000 || !capacity.Warning || capacity.Critical {
		t.Fatalf("14-million member warning=%+v err=%v", capacity, err)
	}
	for index := 14; index < 17; index++ {
		insertTokenPaginationCapacityFixture(t, ctx, repository.pool, index, AdminSnapshotMaxMembers)
	}
	capacity, err = repository.AdminPaginationCapacity(ctx)
	if err != nil || capacity.ActiveMembers != 17_000_000 || !capacity.Critical {
		t.Fatalf("17-million member critical=%+v err=%v", capacity, err)
	}
	criticalMemberRequest := request
	criticalMemberRequest.SubjectID = uuid.New()
	criticalMemberRequest.Resource = "member-critical-non-replacement"
	criticalMemberBuilderCalled := false
	if _, err := repository.AdminPaginationPage(ctx, criticalMemberRequest, func(context.Context, pgx.Tx, uuid.UUID, time.Time, int) error {
		criticalMemberBuilderCalled = true
		return nil
	}); !errors.Is(err, ErrAdminPaginationSnapshotLimited) || criticalMemberBuilderCalled {
		t.Fatalf("17-million member critical did not reject a non-replacement: called=%v err=%v", criticalMemberBuilderCalled, err)
	}
	criticalReplacement := request
	criticalReplacement.SubjectID = replacementSubject
	criticalReplacement.Resource = replacementResource
	replacementBuilderCalled := false
	if _, err := repository.AdminPaginationPage(ctx, criticalReplacement, func(context.Context, pgx.Tx, uuid.UUID, time.Time, int) error {
		replacementBuilderCalled = true
		return errors.New("injected replacement failure")
	}); !errors.Is(err, ErrAdminPaginationSnapshotUnavailable) || !replacementBuilderCalled {
		t.Fatalf("critical same-resource replacement did not enter its transaction: called=%v err=%v", replacementBuilderCalled, err)
	}
	if !paginationSnapshotExists(t, ctx, repository.pool, replacementSnapshotID) {
		t.Fatal("failed critical replacement did not preserve the prior snapshot")
	}

	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_admin_pagination_snapshots; DELETE FROM token_admin_pagination_snapshot_tombstones; DELETE FROM token_admin_pagination_creation_windows`); err != nil {
		t.Fatal(err)
	}
	hardReplacementID := uuid.New()
	hardReplacementSubject := uuid.New()
	const hardReplacementResource = "hard-member-replacement"
	insertTokenPaginationCapacityFixtureFor(t, ctx, repository.pool, hardReplacementID, hardReplacementSubject, hardReplacementResource, 0)
	for index := 0; index < 20; index++ {
		insertTokenPaginationCapacityFixture(t, ctx, repository.pool, index, AdminSnapshotMaxMembers)
	}
	hardReplacement := request
	hardReplacement.SubjectID = hardReplacementSubject
	hardReplacement.Resource = hardReplacementResource
	hardReplacementBuilderCalled := false
	if _, err := repository.AdminPaginationPage(ctx, hardReplacement, func(context.Context, pgx.Tx, uuid.UUID, time.Time, int) error {
		hardReplacementBuilderCalled = true
		return nil
	}); !errors.Is(err, ErrAdminPaginationSnapshotLimited) || hardReplacementBuilderCalled {
		t.Fatalf("hard member limit did not reject replacement before build: called=%v err=%v", hardReplacementBuilderCalled, err)
	}
	if !paginationSnapshotExists(t, ctx, repository.pool, hardReplacementID) {
		t.Fatal("hard-limit replacement did not preserve the prior snapshot")
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_admin_pagination_snapshots; DELETE FROM token_admin_pagination_snapshot_tombstones; DELETE FROM token_admin_pagination_creation_windows`); err != nil {
		t.Fatal(err)
	}

	for index := 0; index < AdminSnapshotMaxGlobal; index++ {
		insertTokenPaginationCapacityFixture(t, ctx, repository.pool, index, 0)
	}
	hardSnapshots := request
	hardSnapshots.SubjectID = uuid.New()
	hardSnapshots.Resource = "hard-snapshot-limit"
	if _, err := repository.AdminPaginationPage(ctx, hardSnapshots, source); !errors.Is(err, ErrAdminPaginationSnapshotLimited) {
		t.Fatalf("64 snapshot hard boundary returned %v", err)
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_admin_pagination_snapshots; DELETE FROM token_admin_pagination_creation_windows`); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 20; index++ {
		insertTokenPaginationCapacityFixture(t, ctx, repository.pool, index, AdminSnapshotMaxMembers)
	}
	hardMembers := request
	hardMembers.SubjectID = uuid.New()
	hardMembers.Resource = "hard-member-limit"
	if _, err := repository.AdminPaginationPage(ctx, hardMembers, source); !errors.Is(err, ErrAdminPaginationSnapshotLimited) {
		t.Fatalf("20 million member hard boundary returned %v", err)
	}

	// Creation limits are transactionally exact. Rejected attempts do not leave
	// a refund when either the subject or global boundary is exceeded.
	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_admin_pagination_snapshots; DELETE FROM token_admin_pagination_snapshot_tombstones; DELETE FROM token_admin_pagination_creation_windows`); err != nil {
		t.Fatal(err)
	}
	subjectLimited := request
	subjectLimited.SubjectID = uuid.New()
	subjectLimited.Resource = "subject-rate"
	for attempt := 1; attempt <= 6; attempt++ {
		if _, err := repository.AdminPaginationPage(ctx, subjectLimited, source); err != nil {
			t.Fatalf("subject attempt %d/6 returned %v", attempt, err)
		}
	}
	if _, err := repository.AdminPaginationPage(ctx, subjectLimited, source); !errors.Is(err, ErrAdminPaginationSnapshotLimited) {
		t.Fatalf("subject attempt 7 returned %v", err)
	}
	var subjectAttempts int
	if err := repository.pool.QueryRow(ctx, `SELECT creation_count FROM token_admin_pagination_creation_windows WHERE rate_key=$1 AND window_start=date_trunc('minute',transaction_timestamp())`, "subject:"+subjectLimited.SubjectID.String()).Scan(&subjectAttempts); err != nil || subjectAttempts != 7 {
		t.Fatalf("subject 6/7 boundary=%d, %v", subjectAttempts, err)
	}

	if _, err := repository.pool.Exec(ctx, `DELETE FROM token_admin_pagination_snapshots; DELETE FROM token_admin_pagination_snapshot_tombstones; DELETE FROM token_admin_pagination_creation_windows;
		INSERT INTO token_admin_pagination_creation_windows(rate_key,window_start,creation_count) VALUES('global',date_trunc('minute',transaction_timestamp()),60)`); err != nil {
		t.Fatal(err)
	}
	rateLimited := request
	rateLimited.SubjectID = uuid.New()
	rateLimited.Resource = "rate-refusal"
	if _, err := repository.AdminPaginationPage(ctx, rateLimited, source); !errors.Is(err, ErrAdminPaginationSnapshotLimited) {
		t.Fatalf("global creation boundary returned %v", err)
	}
	var subjectWindows int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_admin_pagination_creation_windows WHERE rate_key=$1`, "subject:"+rateLimited.SubjectID.String()).Scan(&subjectWindows); err != nil || subjectWindows != 1 {
		t.Fatalf("rate refusal did not count the logical attempt once: %d, %v", subjectWindows, err)
	}
	var globalAttempts int
	if err := repository.pool.QueryRow(ctx, `SELECT creation_count FROM token_admin_pagination_creation_windows WHERE rate_key='global' AND window_start=date_trunc('minute',transaction_timestamp())`).Scan(&globalAttempts); err != nil || globalAttempts != 61 {
		t.Fatalf("global 60/61 boundary=%d, %v", globalAttempts, err)
	}
}

func TestTokenAdminPaginationPostgresMillionMemberBoundary(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	repository := newTokenPaginationIntegrationRepository(t, ctx, databaseURL)
	if err := repository.ConfigureAdminPagination(AdminPaginationConfig{Enabled: true, CursorHMACKey: []byte("0123456789abcdef0123456789abcdef")}); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetAdminPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}
	request := AdminPaginationRequest{SubjectID: uuid.New(), AuthorizationVersion: 1, Resource: "million-boundary", Sort: "id ASC", Limit: 200}
	sourceWithCount := func(count int) AdminSnapshotMemberSource {
		return func(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, _ time.Time, maximum int) error {
			if count > maximum {
				count = maximum
			}
			_, err := tx.Exec(ctx, `INSERT INTO token_admin_pagination_snapshot_members(snapshot_id,position,resource_id)
				SELECT $1,value-1,'member-'||lpad(value::text,7,'0') FROM generate_series(1,$2) value`, snapshotID, count)
			return err
		}
	}
	started := time.Now()
	if _, err := repository.AdminPaginationPage(ctx, request, sourceWithCount(AdminSnapshotMaxMembers)); err != nil {
		t.Fatalf("one-million snapshot: %v", err)
	}
	if elapsed := time.Since(started); elapsed > AdminSnapshotBuildBudget {
		t.Fatalf("one-million snapshot exceeded 10-second budget: %s", elapsed)
	}
	var memberCount int
	if err := repository.pool.QueryRow(ctx, `SELECT member_count FROM token_admin_pagination_snapshots WHERE subject_id=$1 AND resource_type=$2`, request.SubjectID, request.Resource).Scan(&memberCount); err != nil || memberCount != AdminSnapshotMaxMembers {
		t.Fatalf("one-million snapshot stored %d members: %v", memberCount, err)
	}
	if _, err := repository.AdminPaginationPage(ctx, request, sourceWithCount(AdminSnapshotMaxMembers+1)); !errors.Is(err, ErrAdminPaginationFilterRequired) {
		t.Fatalf("one-million-plus-one returned %v", err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT member_count FROM token_admin_pagination_snapshots WHERE subject_id=$1 AND resource_type=$2`, request.SubjectID, request.Resource).Scan(&memberCount); err != nil || memberCount != AdminSnapshotMaxMembers {
		t.Fatalf("overflow replaced the prior valid snapshot: count=%d err=%v", memberCount, err)
	}
}

func TestTokenAdminPaginationMigrationFailsClosedAndIsIdempotent(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repository := newTokenPaginationIntegrationRepository(t, ctx, databaseURL)
	up, err := os.ReadFile(filepath.Join("..", "..", "migrations", "token", "000023_admin_pagination_snapshots.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile(filepath.Join("..", "..", "migrations", "token", "000023_admin_pagination_snapshots.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, string(up)); err != nil {
		t.Fatalf("repeated up must be idempotent: %v", err)
	}
	if err := repository.ConfigureAdminPagination(AdminPaginationConfig{Enabled: true, CursorHMACKey: []byte("0123456789abcdef0123456789abcdef")}); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetAdminPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "TOKEN_ADMIN_PAGINATION_GATE_ENABLED") {
		t.Fatalf("down did not fail while gate was enabled: %v", err)
	}
	if err := repository.SetAdminPaginationGate(ctx, false); err != nil {
		t.Fatal(err)
	}
	snapshotID, subjectID := uuid.New(), uuid.New()
	highWater := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	expiresAt := highWater.Add(AdminSnapshotTTL)
	if _, err := repository.pool.Exec(ctx, `INSERT INTO token_admin_pagination_snapshots(id,subject_id,authorization_version,resource_type,scope_digest,filter_digest,sort_spec,page_limit,high_water,expires_at,member_count,member_bytes,created_at)
		VALUES($1,$2,1,'models',$3,$4,'id ASC',50,$5,$6,0,0,$5)`, snapshotID, subjectID, adminPaginationDigest("platform"), adminPaginationDigest(map[string]string{}), highWater, expiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "TOKEN_ADMIN_PAGINATION_CURSOR_ACTIVE") {
		t.Fatalf("down did not fail while an unexpired cursor existed: %v", err)
	}
	// Advance only the fixture's DB facts; the signed expired cursor and row use
	// the exact same timestamp, so the service returns 410 instead of a mismatch.
	expiresAt = time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	highWater = expiresAt.Add(-AdminSnapshotTTL)
	if _, err := repository.pool.Exec(ctx, `UPDATE token_admin_pagination_snapshots SET high_water=$2,created_at=$2,expires_at=$3 WHERE id=$1`, snapshotID, highWater, expiresAt); err != nil {
		t.Fatal(err)
	}
	cursor, err := signedAdminPaginationCursor(adminCursorEnvelope{Version: 1, SnapshotID: snapshotID, Position: 0, SubjectID: subjectID, AuthorizationVersion: 1, Resource: "models", ScopeDigest: adminPaginationDigest("platform"), FilterDigest: adminPaginationDigest(map[string]string{}), Sort: "id ASC", Limit: 50, HighWaterUnixNano: highWater.UnixNano(), ExpiresUnixNano: expiresAt.UnixNano()}, repository.adminPagination.cursorHMACKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.AdminPaginationPage(ctx, AdminPaginationRequest{SubjectID: subjectID, AuthorizationVersion: 1, Resource: "models", Scope: "platform", Filters: map[string]string{}, Sort: "id ASC", Cursor: cursor, Limit: 50}, nil); !errors.Is(err, ErrAdminPaginationCursorExpired) {
		t.Fatalf("naturally expired cursor returned %v", err)
	}
	if _, err := repository.pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down after gate-off and cursor expiry: %v", err)
	}
	if _, err := repository.pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("repeated down must be idempotent: %v", err)
	}
	if _, err := repository.pool.Exec(ctx, string(up)); err != nil {
		t.Fatalf("up after down: %v", err)
	}
	if _, err := repository.pool.Exec(ctx, string(up)); err != nil {
		t.Fatalf("second up after down: %v", err)
	}
}

func tokenPaginationIntegrationDatabaseURL(t *testing.T) string {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_TOKEN_DATABASE_URL is not configured")
	}
	if testing.Short() && os.Getenv("PINVOU_TEST_TOKEN_PAGINATION_GATE") != "1" {
		t.Skip("Token pagination PostgreSQL gate is run explicitly before the short race suite")
	}
	return databaseURL
}

func newTokenPaginationIntegrationRepository(t *testing.T, ctx context.Context, databaseURL string) *PostgresRepository {
	t.Helper()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "token_pagination_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = connection.Exec(cleanupCtx, `DROP SCHEMA `+identifier+` CASCADE`)
	})
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile(filepath.Join("..", "..", "migrations", "token", "000023_admin_pagination_snapshots.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, string(up)); err != nil {
		t.Fatal(err)
	}
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.MaxConns = 20
	if configuration.ConnConfig.RuntimeParams == nil {
		configuration.ConnConfig.RuntimeParams = map[string]string{}
	}
	configuration.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }, adminPaginationMetrics: newAdminPaginationMetrics()}
}

func paginationRowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var count int64
	err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM token_admin_pagination_snapshots)+
		(SELECT count(*) FROM token_admin_pagination_snapshot_members)+
		(SELECT count(*) FROM token_admin_pagination_snapshot_tombstones)+
		(SELECT count(*) FROM token_admin_pagination_creation_windows)`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func paginationSnapshotStateCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_admin_pagination_snapshots)+(SELECT count(*) FROM token_admin_pagination_snapshot_members)+(SELECT count(*) FROM token_admin_pagination_snapshot_tombstones)`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func paginationSnapshotCountForSubject(t *testing.T, ctx context.Context, pool *pgxpool.Pool, subjectID uuid.UUID) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM token_admin_pagination_snapshots WHERE subject_id=$1`, subjectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func insertTokenPaginationCapacityFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, index int, members int) {
	t.Helper()
	insertTokenPaginationCapacityFixtureFor(t, ctx, pool, uuid.New(), uuid.New(), fmt.Sprintf("capacity-%03d", index), members)
}

func insertTokenPaginationCapacityFixtureFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, snapshotID, subjectID uuid.UUID, resource string, members int) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO token_admin_pagination_snapshots(id,subject_id,authorization_version,resource_type,scope_digest,filter_digest,sort_spec,page_limit,high_water,expires_at,member_count,member_bytes,created_at)
		VALUES($1,$2,1,$3,$4,$5,'id ASC',50,transaction_timestamp(),transaction_timestamp()+interval '15 minutes',$6,0,transaction_timestamp())`,
		snapshotID, subjectID, resource, adminPaginationDigest(""), adminPaginationDigest(map[string]string{}), members)
	if err != nil {
		t.Fatal(err)
	}
}

func paginationSnapshotExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, snapshotID uuid.UUID) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM token_admin_pagination_snapshots WHERE id=$1)`, snapshotID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}
