package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

func newApprovalPaginationIntegrationDatabase(t *testing.T) (*approvalAuthorizationIntegrationDatabase, *PostgresRepository, []migrations.Migration) {
	t.Helper()
	database, repository, _, _ := newApprovalDeliveryIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err = approvalPaginationMigrations(loaded)
	if err != nil {
		t.Fatal(err)
	}
	connection := authorizationTestConnection(t, ctx, database.migratorPool)
	if _, err = connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	defer migrations.Unlock(ctx, connection.Conn())
	if err = migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection.Conn())
	if err = migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatalf("repeat pagination migration: %v", err)
	}
	return database, repository, loaded
}

func approvalPaginationMigrations(loaded []migrations.Migration) ([]migrations.Migration, error) {
	for index, migration := range loaded {
		if migration.Version == 61 {
			return loaded[:index+1], nil
		}
	}
	return nil, errors.New("pagination integration fixture requires Account migration 61")
}

func TestApprovalPaginationMigrationBoundary(t *testing.T) {
	for _, versions := range [][]int64{{60, 61}, {60, 61, 62}} {
		loaded := make([]migrations.Migration, len(versions))
		for index, version := range versions {
			loaded[index] = migrations.Migration{Version: version, UpSQL: "up", DownSQL: "down", Checksum: "checksum"}
		}
		current, err := approvalPaginationMigrations(loaded)
		if err != nil || current[len(current)-1] != loaded[1] || current[len(current)-1].Version != 61 {
			t.Fatalf("pagination boundary changed: %+v err=%v", current, err)
		}
	}
	if _, err := approvalPaginationMigrations([]migrations.Migration{{Version: 60}, {Version: 62}}); err == nil {
		t.Fatal("missing pagination migration was accepted")
	}
}

func TestApprovalPaginationPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	database, repository, loaded := newApprovalPaginationIntegrationDatabase(t)
	currentKey := []byte(strings.Repeat("a", 32))
	if err := repository.ConfigureApprovalPagination(ctx, ApprovalPaginationConfig{Enabled: true, CurrentKeyID: "key-a", CurrentKey: currentKey}); err != nil {
		t.Fatal(err)
	}
	if err := repository.SetApprovalPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}
	requester := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PLATFORM_BILLING_OPERATOR", "pagination-requester-"+uuid.NewString())
	policy, err := approvalpolicy.Lookup("WALLET_ADJUSTMENT")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	want := make([]uuid.UUID, 5)
	for index := range want {
		want[index] = seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
		if _, err = database.adminPool.Exec(ctx, `UPDATE approval_requests SET updated_at=$2 WHERE id=$1`, want[index], base.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	for left, right := 0, len(want)-1; left < right; left, right = left+1, right-1 {
		want[left], want[right] = want[right], want[left]
	}
	principal := approvalIntegrationPrincipal(requester)
	first, err := repository.ApprovalPaginationPage(ctx, principal, ApprovalFilter{}, "", 2, "127.0.0.1")
	if err != nil || first.NextCursor == nil || !first.HasMore || !sameUUIDs(first.ApprovalRequestIDs, want[:2]) {
		t.Fatalf("first page=%+v err=%v want=%v", first, err, want[:2])
	}

	// Mutate current order and add a request after the snapshot. Continuations
	// must still enumerate exactly the frozen membership once.
	if _, err = database.adminPool.Exec(ctx, `UPDATE approval_requests SET updated_at=transaction_timestamp() WHERE id=$1`, want[len(want)-1]); err != nil {
		t.Fatal(err)
	}
	newRequest := seedAdvancementRequest(t, ctx, repository, requester.ID, policy, "SUBMITTED")
	all := append([]uuid.UUID(nil), first.ApprovalRequestIDs...)
	cursor := *first.NextCursor
	for {
		page, pageErr := repository.ApprovalPaginationPage(ctx, principal, ApprovalFilter{}, cursor, 2, "127.0.0.1")
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		all = append(all, page.ApprovalRequestIDs...)
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if !sameUUIDs(all, want) || containsUUID(all, newRequest) {
		t.Fatalf("frozen membership=%v want=%v new=%s", all, want, newRequest)
	}

	// Replacing the one active snapshot for this resource leaves a tombstone;
	// the old cursor is expired rather than silently restarting from page one.
	replaced, err := repository.ApprovalPaginationPage(ctx, principal, ApprovalFilter{}, "", 2, "127.0.0.1")
	if err != nil || replaced.NextCursor == nil {
		t.Fatalf("replacement page=%+v err=%v", replaced, err)
	}
	if _, err = repository.ApprovalPaginationPage(ctx, principal, ApprovalFilter{}, *first.NextCursor, 2, "127.0.0.1"); !errors.Is(err, ErrApprovalPaginationCursorExpired) {
		t.Fatalf("replaced cursor error=%v", err)
	}

	// Safe rollback closes only new snapshot creation. An already signed cursor
	// remains readable until its original expiry without rebuilding membership.
	if err = repository.SetApprovalPaginationGate(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.ApprovalPaginationPage(ctx, principal, ApprovalFilter{}, "", 2, "127.0.0.1"); !errors.Is(err, ErrApprovalPaginationSnapshotUnavailable) {
		t.Fatalf("closed gate first-page error=%v", err)
	}
	if _, err = repository.ApprovalPaginationPage(ctx, principal, ApprovalFilter{}, *replaced.NextCursor, 2, "127.0.0.1"); err != nil {
		t.Fatalf("closed gate continuation error=%v", err)
	}
	if err = repository.SetApprovalPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}

	// Rotation verifies the old cursor with previous and signs the next cursor
	// only with the new current key. Persisted usage blocks premature retirement.
	rotationFirst, err := repository.ApprovalPaginationPage(ctx, principal, ApprovalFilter{}, "", 2, "127.0.0.1")
	if err != nil || rotationFirst.NextCursor == nil {
		t.Fatalf("rotation first page=%+v err=%v", rotationFirst, err)
	}
	keyB := []byte(strings.Repeat("b", 32))
	rotation := ApprovalPaginationConfig{Enabled: true, CurrentKeyID: "key-b", CurrentKey: keyB, PreviousKeyID: "key-a", PreviousKey: currentKey, PreviousRetainUntil: time.Now().UTC().Add(36 * time.Minute)}
	if err = repository.ConfigureApprovalPagination(ctx, rotation); err != nil {
		t.Fatal(err)
	}
	rotatedPage, err := repository.ApprovalPaginationPage(ctx, principal, ApprovalFilter{}, *rotationFirst.NextCursor, 2, "127.0.0.1")
	if err != nil || rotatedPage.NextCursor == nil {
		t.Fatalf("rotated continuation=%+v err=%v", rotatedPage, err)
	}
	rotatedCursor, err := parseApprovalPaginationCursor(*rotatedPage.NextCursor, repository.approvalPagination, time.Now().UTC())
	if err != nil || rotatedCursor.KeyID != "key-b" {
		t.Fatalf("rotated cursor=%+v err=%v", rotatedCursor, err)
	}
	if err = repository.ConfigureApprovalPagination(ctx, ApprovalPaginationConfig{Enabled: true, CurrentKeyID: "key-b", CurrentKey: keyB}); err == nil || repository.approvalPagination.valid() {
		t.Fatal("premature previous-key retirement was accepted or did not fail closed")
	}
	if err = repository.ConfigureApprovalPagination(ctx, rotation); err != nil {
		t.Fatalf("restore rotation configuration: %v", err)
	}

	// A signed cursor is invalidated before member reads as soon as the current
	// authorization version advances.
	if _, err = database.adminPool.Exec(ctx, `UPDATE authorization_versions SET version=version+1,updated_at=transaction_timestamp() WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid`, requester.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.ApprovalPaginationPage(ctx, principal, ApprovalFilter{}, *rotatedPage.NextCursor, 2, "127.0.0.1"); !errors.Is(err, ErrApprovalPaginationCursorExpired) {
		t.Fatalf("authorization-changed cursor error=%v", err)
	}

	capacity, err := repository.ApprovalPaginationCapacity(ctx)
	if err != nil || capacity.ActiveSnapshots != 1 || capacity.ActiveMembers < 5 || capacity.StorageBytes <= 0 || capacity.Warning || capacity.Critical {
		t.Fatalf("capacity=%+v err=%v", capacity, err)
	}
	repository.approvalPagination.maximumStorageBytes = capacity.StorageBytes * 100 / 75
	warning, err := repository.ApprovalPaginationCapacity(ctx)
	if err != nil || !warning.Warning || warning.Critical {
		t.Fatalf("warning capacity=%+v err=%v", warning, err)
	}
	repository.approvalPagination.maximumStorageBytes = capacity.StorageBytes * 100 / 90
	critical, err := repository.ApprovalPaginationCapacity(ctx)
	if err != nil || !critical.Warning || !critical.Critical {
		t.Fatalf("critical capacity=%+v err=%v", critical, err)
	}
	criticalActor := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PLATFORM_BILLING_OPERATOR", "pagination-critical-"+uuid.NewString())
	seedAdvancementRequest(t, ctx, repository, criticalActor.ID, policy, "SUBMITTED")
	if _, err = repository.ApprovalPaginationPage(ctx, approvalIntegrationPrincipal(criticalActor), ApprovalFilter{}, "", 1, "127.0.0.2"); !errors.Is(err, ErrApprovalPaginationSnapshotLimited) {
		t.Fatalf("critical non-replacement error=%v", err)
	}
	repository.approvalPagination.maximumStorageBytes = ApprovalSnapshotMaxStorageBytes

	// A retained snapshot cannot disclose a request to an approver whose
	// current role binding was revoked, even when a faulty external mutation
	// omitted the authorization-version bump. Ownership remains visible.
	approver := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PLATFORM_BILLING_APPROVER", "pagination-approver-"+uuid.NewString())
	approverPrincipal := approvalIntegrationPrincipal(approver)
	approverFirst, err := repository.ApprovalPaginationPage(ctx, approverPrincipal, ApprovalFilter{}, "", 2, "127.0.0.4")
	if err != nil || approverFirst.NextCursor == nil || len(approverFirst.ApprovalRequestIDs) != 2 {
		t.Fatalf("approver first page=%+v err=%v", approverFirst, err)
	}
	if _, err = database.adminPool.Exec(ctx, `UPDATE role_assignments SET revoked_at=transaction_timestamp() WHERE user_id=$1 AND role_code='PLATFORM_BILLING_APPROVER' AND revoked_at IS NULL`, approver.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.adminPool.Exec(ctx, `UPDATE authorization_versions SET version=version-1,updated_at=transaction_timestamp() WHERE user_id=$1 AND scope='PLATFORM' AND scope_id='00000000-0000-0000-0000-000000000000'::uuid AND version>1`, approver.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.ApprovalPaginationPage(ctx, approverPrincipal, ApprovalFilter{}, *approverFirst.NextCursor, 2, "127.0.0.4"); !errors.Is(err, ErrApprovalPaginationCursorExpired) {
		t.Fatalf("revoked approver cursor error=%v", err)
	}
	parsedApprover, err := parseApprovalPaginationCursor(*approverFirst.NextCursor, repository.approvalPagination, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// Also exercise the member SQL directly against the current role facts:
	// even a non-bumping role mutation cannot return a frozen non-owned ID.
	visibilityPage, err := repository.readApprovalSnapshotMembers(ctx, database.adminPool, approver.ID, parsedApprover.AuthorizationVersion, parsedApprover.ScopeDigest, parsedApprover.FilterDigest, parsedApprover.SnapshotID, parsedApprover.Position, parsedApprover.PositionKey, 2, parsedApprover.AsOf, parsedApprover.ExpiresAt)
	if err != nil || len(visibilityPage.ApprovalRequestIDs) != 0 || visibilityPage.NextCursor != nil {
		t.Fatalf("revoked member SQL page=%+v err=%v", visibilityPage, err)
	}

	// Snapshot creation rate is database-authoritative and replacement does not
	// bypass the six-per-subject minute window.
	rateActor := seedTokenOwnerCandidateActor(t, ctx, database.adminPool, "PLATFORM_BILLING_OPERATOR", "pagination-rate-"+uuid.NewString())
	seedAdvancementRequest(t, ctx, repository, rateActor.ID, policy, "SUBMITTED")
	ratePrincipal := approvalIntegrationPrincipal(rateActor)
	for attempt := 0; attempt < ApprovalSnapshotSubjectPerMinute; attempt++ {
		if _, err = repository.ApprovalPaginationPage(ctx, ratePrincipal, ApprovalFilter{}, "", 1, "127.0.0.3"); err != nil {
			t.Fatalf("rate attempt %d: %v", attempt+1, err)
		}
	}
	if _, err = repository.ApprovalPaginationPage(ctx, ratePrincipal, ApprovalFilter{}, "", 1, "127.0.0.3"); !errors.Is(err, ErrApprovalPaginationSnapshotLimited) {
		t.Fatalf("seventh snapshot error=%v", err)
	} else {
		var rateError approvalPaginationRateError
		if !errors.As(err, &rateError) || rateError.RetryAfter() < 1 || rateError.RetryAfter() > 60 {
			t.Fatalf("snapshot retry-after error=%v", err)
		}
	}

	// Down is conservative: after closing the gate it still refuses retained
	// cursors/tombstones and only succeeds once cleanup proves both windows over.
	if err = repository.SetApprovalPaginationGate(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err = database.adminPool.Exec(ctx, `UPDATE approval_pagination_snapshots SET created_at=transaction_timestamp()-interval '2 seconds',expires_at=transaction_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err = repository.CleanupApprovalPagination(ctx); err != nil {
		t.Fatal(err)
	}
	connection := authorizationTestConnection(t, ctx, database.migratorPool)
	if _, err = connection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	if err = migrations.DownOne(ctx, connection.Conn(), loaded); err == nil {
		t.Fatal("down accepted retained cursor references")
	}
	migrations.Unlock(ctx, connection.Conn())
	if _, err = database.adminPool.Exec(ctx, `UPDATE approval_pagination_snapshot_tombstones SET purge_after=transaction_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err = database.adminPool.Exec(ctx, `UPDATE approval_pagination_signing_keys SET last_signed_at=transaction_timestamp()-interval '36 minutes'`); err != nil {
		t.Fatal(err)
	}
	if err = repository.CleanupApprovalPagination(ctx); err != nil {
		t.Fatal(err)
	}
	if err = migrations.DownOne(ctx, connection.Conn(), loaded); err != nil {
		t.Fatalf("drained down: %v", err)
	}
	migrations.Unlock(ctx, connection.Conn())
	if err = migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatalf("pagination migration reentry: %v", err)
	}
	migrations.Unlock(ctx, connection.Conn())
}

func sameUUIDs(actual, expected []uuid.UUID) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

func containsUUID(values []uuid.UUID, candidate uuid.UUID) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
