package token

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestAdminPaginationLimitsAndStableErrors(t *testing.T) {
	for _, item := range []struct {
		limit, defaultLimit, maximum, want int
		err                                bool
	}{{0, 50, 200, 50, false}, {1, 50, 200, 1, false}, {200, 50, 200, 200, false}, {201, 50, 200, 0, true}, {51, 20, 50, 0, true}} {
		got, err := NormalizeAdminPaginationLimit(item.limit, item.defaultLimit, item.maximum)
		if got != item.want || item.err != (err != nil) {
			t.Fatalf("NormalizeAdminPaginationLimit(%d,%d,%d)=(%d,%v)", item.limit, item.defaultLimit, item.maximum, got, err)
		}
	}
	for err, want := range map[error]AdminPaginationHTTPError{
		ErrAdminPaginationLimitInvalid:        {Status: 400, Code: "INVALID_LIMIT"},
		ErrAdminPaginationCursorInvalid:       {Status: 400, Code: "INVALID_CURSOR"},
		ErrAdminPaginationCursorExpired:       {Status: 410, Code: "CURSOR_EXPIRED"},
		ErrAdminPaginationFilterRequired:      {Status: 422, Code: "PAGINATION_FILTER_REQUIRED"},
		ErrAdminPaginationSnapshotLimited:     {Status: 429, Code: "PAGINATION_SNAPSHOT_LIMITED", RetryAfter: 1},
		ErrAdminPaginationQueryTimeout:        {Status: 504, Code: "QUERY_TIMEOUT"},
		ErrAdminPaginationSnapshotUnavailable: {Status: 503, Code: "PAGINATION_SNAPSHOT_UNAVAILABLE", RetryAfter: 5},
	} {
		if got := ClassifyAdminPaginationError(err); got != want {
			t.Fatalf("ClassifyAdminPaginationError(%v)=%+v want=%+v", err, got, want)
		}
	}
}

func TestAdminPaginationCursorBindsCompleteSnapshotIdentity(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	payload := adminCursorEnvelope{
		Version: 1, SnapshotID: uuid.New(), Position: 50, SubjectID: uuid.New(), AuthorizationVersion: 7,
		Resource: "models", ScopeDigest: adminPaginationDigest("platform"), FilterDigest: adminPaginationDigest(map[string]string{"status": "PUBLISHED"}),
		Sort: "display_name_normalized ASC,id ASC", Limit: 50,
		HighWaterUnixNano: time.Now().UTC().UnixNano(), ExpiresUnixNano: time.Now().UTC().Add(AdminSnapshotTTL).UnixNano(),
	}
	raw, err := signedAdminPaginationCursor(payload, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseAdminPaginationCursor(raw, key)
	if err != nil || parsed != payload {
		t.Fatalf("cursor round trip=%+v, %v", parsed, err)
	}
	for index := 0; index < len(raw); index += max(1, len(raw)/7) {
		forged := []byte(raw)
		if forged[index] == 'A' {
			forged[index] = 'B'
		} else {
			forged[index] = 'A'
		}
		if _, err := parseAdminPaginationCursor(string(forged), key); !errors.Is(err, ErrAdminPaginationCursorInvalid) {
			t.Fatalf("tampered cursor byte %d returned %v", index, err)
		}
	}
}

func TestAdminPaginationFeatureGateFailsClosedWithoutState(t *testing.T) {
	repository := &PostgresRepository{}
	request := AdminPaginationRequest{SubjectID: uuid.New(), AuthorizationVersion: 1, Resource: "models", Sort: "id ASC", Limit: 50}
	if _, err := repository.AdminPaginationPage(t.Context(), request, func(_ context.Context, _ pgx.Tx, _ uuid.UUID, _ time.Time, _ int) error { return nil }); !errors.Is(err, ErrAdminPaginationDisabled) {
		t.Fatalf("disabled gate returned %v", err)
	}
}
