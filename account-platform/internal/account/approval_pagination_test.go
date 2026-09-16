package account

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestNormalizeApprovalPaginationLimit(t *testing.T) {
	for input, expected := range map[int]int{0: 50, 1: 1, 50: 50, 200: 200} {
		actual, err := NormalizeApprovalPaginationLimit(input)
		if err != nil || actual != expected {
			t.Fatalf("limit %d: actual=%d err=%v", input, actual, err)
		}
	}
	for _, input := range []int{-1, 201} {
		if _, err := NormalizeApprovalPaginationLimit(input); !errors.Is(err, ErrApprovalPaginationLimitInvalid) {
			t.Fatalf("limit %d error=%v", input, err)
		}
	}
}

func TestApprovalPaginationCursorClosedSignatureAndRotation(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	current := []byte(strings.Repeat("c", 32))
	previous := []byte(strings.Repeat("p", 32))
	payload := approvalPaginationCursor{
		Version: "1", KeyID: "current", SnapshotID: uuid.New(), SubjectID: uuid.New(),
		AuthorizationVersion: 7, Resource: approvalPaginationResource,
		ScopeDigest: strings.Repeat("a", 64), FilterDigest: strings.Repeat("b", 64),
		Sort: approvalPaginationSort, Limit: 50, Position: 50,
		PositionKey: "2026-09-16T12:00:00.000000Z:" + uuid.NewString(),
		AsOf:        now, ExpiresAt: now.Add(ApprovalSnapshotTTL),
	}
	runtime := approvalPaginationRuntime{
		currentKeyID: "current", currentKey: current,
		previousKeyID: "previous", previousKey: previous,
		previousRetainUntil: now.Add(ApprovalSnapshotKeyRetention),
	}
	raw, err := signedApprovalPaginationCursor(payload, current)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseApprovalPaginationCursor(raw, runtime, now)
	if err != nil || parsed != payload {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}

	parts := strings.Split(raw, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	signature[0] ^= 0xff
	if _, err := parseApprovalPaginationCursor(parts[0]+"."+base64.RawURLEncoding.EncodeToString(signature), runtime, now); !errors.Is(err, ErrApprovalPaginationCursorInvalid) {
		t.Fatalf("tampered signature error=%v", err)
	}

	payload.KeyID = "previous"
	raw, err = signedApprovalPaginationCursor(payload, previous)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseApprovalPaginationCursor(raw, runtime, now); err != nil {
		t.Fatalf("retained previous key error=%v", err)
	}
	if _, err := parseApprovalPaginationCursor(raw, runtime, runtime.previousRetainUntil); !errors.Is(err, ErrApprovalPaginationCursorInvalid) {
		t.Fatalf("retired previous key error=%v", err)
	}
}

func TestApprovalPaginationCursorRejectsNonClosedPayload(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	runtime := approvalPaginationRuntime{currentKeyID: "key", currentKey: key}
	valid := approvalPaginationCursor{
		Version: "1", KeyID: "key", SnapshotID: uuid.New(), SubjectID: uuid.New(),
		AuthorizationVersion: 1, Resource: approvalPaginationResource,
		ScopeDigest: strings.Repeat("a", 64), FilterDigest: strings.Repeat("b", 64),
		Sort: approvalPaginationSort, Limit: 1, Position: 1, PositionKey: "position",
		AsOf: now, ExpiresAt: now.Add(time.Minute),
	}
	raw, err := signedApprovalPaginationCursor(valid, key)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(raw, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	closed := strings.TrimSuffix(string(payload), "}")
	for _, invalid := range []string{
		closed + `,"extra":true}`,
		closed + `,"v":"1"}`,
		strings.Replace(string(payload), strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
	} {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(invalid))
		// The signature is intentionally irrelevant: schema rejection must happen
		// before a cursor can reach snapshot-member SQL.
		if _, err := parseApprovalPaginationCursor(encoded+"."+strings.Split(raw, ".")[1], runtime, now); !errors.Is(err, ErrApprovalPaginationCursorInvalid) {
			t.Fatalf("invalid payload %q error=%v", invalid, err)
		}
	}
}

func TestConfigureApprovalPaginationFailsClosed(t *testing.T) {
	repository := &PostgresRepository{}
	if err := repository.ConfigureApprovalPagination(t.Context(), ApprovalPaginationConfig{Enabled: true, CurrentKeyID: "short", CurrentKey: []byte("short")}); err == nil {
		t.Fatal("short key was accepted")
	}
	if repository.approvalPagination.valid() || !repository.approvalPagination.hardDisabled {
		t.Fatal("rejected configuration did not fail closed")
	}
	if err := repository.ConfigureApprovalPagination(t.Context(), ApprovalPaginationConfig{HardDisabled: true, Enabled: true, CurrentKeyID: "ignored", CurrentKey: []byte("ignored")}); err != nil {
		t.Fatal(err)
	}
	if repository.approvalPagination.valid() || !repository.approvalPagination.hardDisabled {
		t.Fatal("hard disable was not retained")
	}
}

func TestClassifyApprovalPaginationError(t *testing.T) {
	tests := []struct {
		err  error
		want ApprovalPaginationHTTPError
	}{
		{ErrApprovalPaginationLimitInvalid, ApprovalPaginationHTTPError{Status: 400, Code: "INVALID_LIMIT"}},
		{ErrApprovalPaginationFilterInvalid, ApprovalPaginationHTTPError{Status: 400, Code: "INVALID_FILTER"}},
		{ErrApprovalPaginationCursorInvalid, ApprovalPaginationHTTPError{Status: 400, Code: "INVALID_CURSOR"}},
		{ErrApprovalPaginationCursorExpired, ApprovalPaginationHTTPError{Status: 410, Code: "CURSOR_EXPIRED"}},
		{ErrApprovalPaginationFilterRequired, ApprovalPaginationHTTPError{Status: 422, Code: "PAGINATION_FILTER_REQUIRED"}},
		{ErrApprovalPaginationSnapshotLimited, ApprovalPaginationHTTPError{Status: 429, Code: "PAGINATION_SNAPSHOT_LIMITED", RetryAfter: 1}},
		{approvalPaginationRateError{cause: ErrApprovalPaginationSnapshotLimited, retryAfter: 37}, ApprovalPaginationHTTPError{Status: 429, Code: "PAGINATION_SNAPSHOT_LIMITED", RetryAfter: 37}},
		{approvalPaginationRateError{cause: ErrApprovalPaginationRateLimited, retryAfter: 0}, ApprovalPaginationHTTPError{Status: 429, Code: "RATE_LIMITED", RetryAfter: 1}},
		{ErrApprovalPaginationQueryTimeout, ApprovalPaginationHTTPError{Status: 504, Code: "QUERY_TIMEOUT"}},
		{ErrApprovalPaginationDisabled, ApprovalPaginationHTTPError{Status: 503, Code: "PAGINATION_SNAPSHOT_UNAVAILABLE", RetryAfter: 5}},
	}
	for _, test := range tests {
		if actual := ClassifyApprovalPaginationError(test.err); actual != test.want {
			t.Fatalf("error %v: actual=%+v want=%+v", test.err, actual, test.want)
		}
	}
}

func TestApprovalPaginationRejectsMalformedCursorBeforeDatabase(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	repository := &PostgresRepository{
		now: func() time.Time { return now },
		approvalPagination: approvalPaginationRuntime{
			enabled: true, currentKeyID: "key", currentKey: []byte(strings.Repeat("k", 32)),
		},
	}
	principal := identity.Principal{Issuer: "issuer", Subject: "subject", SessionID: "session", AuthenticatedAt: now}
	// pool is deliberately nil: reaching any identity, authorization, snapshot,
	// or member SQL would panic and fail this test.
	if _, err := repository.ApprovalPaginationPage(t.Context(), principal, ApprovalFilter{}, "malformed", 50, "127.0.0.1"); !errors.Is(err, ErrApprovalPaginationCursorInvalid) {
		t.Fatalf("malformed cursor error=%v", err)
	}
}

func TestApprovalPaginationDeadlineCoversAuthorizationRead(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	key := []byte(strings.Repeat("k", 32))
	repository := &PostgresRepository{
		now: func() time.Time { return now },
		approvalPagination: approvalPaginationRuntime{
			enabled: true, currentKeyID: "key", currentKey: key,
		},
	}
	principal := identity.Principal{Issuer: "issuer", Subject: "subject", SessionID: "session", AuthenticatedAt: now}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := repository.ApprovalPaginationPage(cancelled, principal, ApprovalFilter{}, "", 50, "127.0.0.1"); !errors.Is(err, ErrApprovalPaginationSnapshotUnavailable) {
		t.Fatalf("first-page deadline error=%v", err)
	}
	payload := approvalPaginationCursor{
		Version: "1", KeyID: "key", SnapshotID: uuid.New(), SubjectID: uuid.New(),
		AuthorizationVersion: 1, Resource: approvalPaginationResource,
		ScopeDigest: strings.Repeat("a", 64), FilterDigest: approvalPaginationFilterDigest(ApprovalFilter{}),
		Sort: approvalPaginationSort, Limit: 50, Position: 50, PositionKey: "position",
		AsOf: now, ExpiresAt: now.Add(time.Minute),
	}
	raw, err := signedApprovalPaginationCursor(payload, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.ApprovalPaginationPage(cancelled, principal, ApprovalFilter{}, raw, 50, "127.0.0.1"); !errors.Is(err, ErrApprovalPaginationQueryTimeout) {
		t.Fatalf("continuation deadline error=%v", err)
	}
}

func TestApprovalPaginationPostgresStatementTimeoutClassification(t *testing.T) {
	if err := approvalPaginationReadError(&pgconn.PgError{Code: "57014"}); !errors.Is(err, ErrApprovalPaginationQueryTimeout) {
		t.Fatalf("PostgreSQL statement cancellation classification=%v", err)
	}
	if err := approvalPaginationOperationError(&pgconn.PgError{Code: "57014"}, true); !errors.Is(err, ErrApprovalPaginationQueryTimeout) {
		t.Fatalf("authorization lookup statement timeout classification=%v", err)
	}
	if err := approvalPaginationReadError(&pgconn.PgError{Code: "23505"}); errors.Is(err, ErrApprovalPaginationQueryTimeout) {
		t.Fatalf("unrelated PostgreSQL error was classified as timeout: %v", err)
	}
}
