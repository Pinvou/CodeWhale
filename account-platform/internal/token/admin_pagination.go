package token

import (
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	AdminSnapshotTTL              = 15 * time.Minute
	AdminSnapshotTombstoneTTL     = 20 * time.Minute
	AdminSnapshotBuildBudget      = 10 * time.Second
	AdminSnapshotReadBudget       = 3 * time.Second
	AdminPrecheckBudget           = 5 * time.Second
	AdminSnapshotMaxMembers       = 1_000_000
	AdminSnapshotMaxGlobal        = 64
	AdminSnapshotMaxGlobalMembers = 20_000_000
	AdminSnapshotMaxStorageBytes  = int64(8 * 1024 * 1024 * 1024)
	AdminSnapshotSubjectPerMinute = 6
	AdminSnapshotGlobalPerMinute  = 60
	adminSerializationAttempts    = 5
)

var (
	ErrAdminPaginationDisabled            = errors.New("admin pagination is disabled")
	ErrAdminPaginationLimitInvalid        = errors.New("admin pagination limit is invalid")
	ErrAdminPaginationCursorInvalid       = errors.New("admin pagination cursor is invalid")
	ErrAdminPaginationCursorExpired       = errors.New("admin pagination cursor is expired")
	ErrAdminPaginationFilterRequired      = errors.New("admin pagination filter is required")
	ErrAdminPaginationSnapshotLimited     = errors.New("admin pagination snapshot is limited")
	ErrAdminPaginationSnapshotUnavailable = errors.New("admin pagination snapshot is unavailable")
	ErrAdminPaginationQueryTimeout        = errors.New("admin pagination query timeout")
)

type AdminPaginationConfig struct {
	Enabled         bool
	CursorHMACKey   []byte
	MaxStorageBytes int64
}

type adminPaginationRuntime struct {
	enabled         bool
	cursorHMACKey   []byte
	maxStorageBytes int64
}

// ConfigureAdminPagination is startup-only configuration. Enabling requires a
// dedicated 256-bit key; disabling still permits already issued cursors to age
// out during a safe rollback when the key remains configured.
func (r *PostgresRepository) ConfigureAdminPagination(config AdminPaginationConfig) error {
	if r == nil {
		return ErrAdminPaginationSnapshotUnavailable
	}
	if len(config.CursorHMACKey) != 0 && len(config.CursorHMACKey) < 32 {
		return errors.New("admin pagination cursor HMAC key must contain at least 32 bytes")
	}
	if config.Enabled && len(config.CursorHMACKey) < 32 {
		return errors.New("admin pagination cursor HMAC key is required when enabled")
	}
	maximum := config.MaxStorageBytes
	if maximum <= 0 {
		maximum = AdminSnapshotMaxStorageBytes
	}
	r.adminPagination = adminPaginationRuntime{
		enabled:         config.Enabled,
		cursorHMACKey:   append([]byte(nil), config.CursorHMACKey...),
		maxStorageBytes: maximum,
	}
	return nil
}

// SetAdminPaginationGate serializes the persisted rollout gate with snapshot
// builders. Disabling waits for an in-flight build to finish and prevents every
// later first-page transaction while continuation reads remain available.
func (r *PostgresRepository) SetAdminPaginationGate(ctx context.Context, enabled bool) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('token-admin-pagination-capacity'))`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO token_admin_pagination_control(singleton,accepting_new_snapshots,updated_at) VALUES(TRUE,$1,transaction_timestamp())
		ON CONFLICT(singleton) DO UPDATE SET accepting_new_snapshots=EXCLUDED.accepting_new_snapshots,updated_at=EXCLUDED.updated_at`, enabled); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type AdminPaginationRequest struct {
	SubjectID            uuid.UUID
	AuthorizationVersion int64
	Resource             string
	Scope                string
	Filters              map[string]string
	Sort                 string
	Cursor               string
	Limit                int
}

type AdminPaginationPage struct {
	ResourceIDs       []string  `json:"-"`
	NextCursor        *string   `json:"next_cursor"`
	HasMore           bool      `json:"has_more"`
	Limit             int       `json:"limit"`
	AsOf              time.Time `json:"as_of"`
	SnapshotExpiresAt time.Time `json:"snapshot_expires_at"`
}

// AdminSnapshotMemberSource is implemented by the nine APT-121 resource
// adapters. It must insert a deterministic, zero-based membership ordering and
// may insert at most maxMembers+1 rows, where the final row is an overflow
// sentinel. The callback runs inside the snapshot transaction and receives the
// database-authoritative high-water mark.
type AdminSnapshotMemberSource func(context.Context, pgx.Tx, uuid.UUID, time.Time, int) error

type adminCursorEnvelope struct {
	Version    int       `json:"v"`
	SnapshotID uuid.UUID `json:"sid"`
	Position   int       `json:"pos"`
	// PositionKey binds the signed cursor to the exact last member tuple of
	// the preceding page. It is optional only for cursors issued before this
	// backward-compatible envelope extension.
	PositionKey          string    `json:"position_key,omitempty"`
	SubjectID            uuid.UUID `json:"sub"`
	AuthorizationVersion int64     `json:"av"`
	Resource             string    `json:"res"`
	ScopeDigest          string    `json:"scope"`
	FilterDigest         string    `json:"filter"`
	Sort                 string    `json:"sort"`
	Limit                int       `json:"limit"`
	HighWaterUnixNano    int64     `json:"high_water"`
	ExpiresUnixNano      int64     `json:"expires"`
}

const adminPaginationStorageQuery = `SELECT
	pg_total_relation_size('token_admin_pagination_snapshots')+
	pg_total_relation_size('token_admin_pagination_snapshot_members')+
	pg_total_relation_size('token_admin_pagination_snapshot_tombstones')`

func NormalizeAdminPaginationLimit(limit, defaultLimit, maximum int) (int, error) {
	if defaultLimit < 1 || maximum < defaultLimit || maximum > 200 {
		return 0, ErrAdminPaginationLimitInvalid
	}
	if limit == 0 {
		return defaultLimit, nil
	}
	if limit < 1 || limit > maximum {
		return 0, ErrAdminPaginationLimitInvalid
	}
	return limit, nil
}

type AdminPaginationHTTPError struct {
	Status     int
	Code       string
	RetryAfter int
}

type adminPaginationSnapshotRateLimitError struct {
	retryAfter int
}

func (e adminPaginationSnapshotRateLimitError) Error() string {
	return ErrAdminPaginationSnapshotLimited.Error()
}

func (e adminPaginationSnapshotRateLimitError) Unwrap() error {
	return ErrAdminPaginationSnapshotLimited
}

func ClassifyAdminPaginationError(err error) AdminPaginationHTTPError {
	var rateLimitError adminPaginationSnapshotRateLimitError
	if errors.As(err, &rateLimitError) {
		return AdminPaginationHTTPError{
			Status:     429,
			Code:       "PAGINATION_SNAPSHOT_LIMITED",
			RetryAfter: max(1, min(60, rateLimitError.retryAfter)),
		}
	}
	switch {
	case errors.Is(err, ErrAdminPaginationLimitInvalid), errors.Is(err, ErrAdminPaginationCursorInvalid):
		code := "INVALID_CURSOR"
		if errors.Is(err, ErrAdminPaginationLimitInvalid) {
			code = "INVALID_LIMIT"
		}
		return AdminPaginationHTTPError{Status: 400, Code: code}
	case errors.Is(err, ErrAdminPaginationCursorExpired):
		return AdminPaginationHTTPError{Status: 410, Code: "CURSOR_EXPIRED"}
	case errors.Is(err, ErrAdminPaginationFilterRequired):
		return AdminPaginationHTTPError{Status: 422, Code: "PAGINATION_FILTER_REQUIRED"}
	case errors.Is(err, ErrAdminPaginationSnapshotLimited):
		return AdminPaginationHTTPError{Status: 429, Code: "PAGINATION_SNAPSHOT_LIMITED", RetryAfter: 1}
	case errors.Is(err, ErrAdminPaginationQueryTimeout):
		return AdminPaginationHTTPError{Status: 504, Code: "QUERY_TIMEOUT"}
	default:
		return AdminPaginationHTTPError{Status: 503, Code: "PAGINATION_SNAPSHOT_UNAVAILABLE", RetryAfter: 5}
	}
}

func (r *PostgresRepository) AdminPaginationPage(ctx context.Context, request AdminPaginationRequest, source AdminSnapshotMemberSource) (AdminPaginationPage, error) {
	request.Resource = strings.TrimSpace(request.Resource)
	request.Scope = strings.TrimSpace(request.Scope)
	request.Sort = strings.TrimSpace(request.Sort)
	request.Cursor = strings.TrimSpace(request.Cursor)
	if request.SubjectID == uuid.Nil || request.AuthorizationVersion < 0 || request.Resource == "" || request.Sort == "" || request.Limit < 1 || request.Limit > 200 {
		return AdminPaginationPage{}, ErrAdminPaginationCursorInvalid
	}
	scopeDigest := adminPaginationDigest(request.Scope)
	filterDigest := adminPaginationDigest(normalizedAdminPaginationFilters(request.Filters))
	if request.Cursor != "" {
		return r.readAdminPaginationPage(ctx, request, scopeDigest, filterDigest)
	}
	if !r.adminPagination.enabled {
		return AdminPaginationPage{}, ErrAdminPaginationDisabled
	}
	if source == nil || len(r.adminPagination.cursorHMACKey) < 32 {
		return AdminPaginationPage{}, ErrAdminPaginationSnapshotUnavailable
	}
	page, err := r.createAdminPaginationPage(ctx, request, scopeDigest, filterDigest, source)
	r.recordAdminPaginationCreation(request.Resource, err)
	return page, err
}

func normalizedAdminPaginationFilters(filters map[string]string) map[string]string {
	normalized := make(map[string]string, len(filters))
	for key, value := range filters {
		key = strings.TrimSpace(key)
		if key != "" {
			normalized[key] = strings.TrimSpace(value)
		}
	}
	return normalized
}

func adminPaginationDigest(value any) string {
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (r *PostgresRepository) createAdminPaginationPage(ctx context.Context, request AdminPaginationRequest, scopeDigest, filterDigest string, source AdminSnapshotMemberSource) (AdminPaginationPage, error) {
	deadlineCtx, cancel, err := adminPaginationContext(ctx, AdminSnapshotBuildBudget)
	if err != nil {
		return AdminPaginationPage{}, fmt.Errorf("%w: %w", ErrAdminPaginationSnapshotUnavailable, err)
	}
	defer cancel()
	// One logical first-page attempt consumes quota in its own committed
	// transaction. Builder failure and serialization retries cannot refund or
	// double-charge the attempt.
	if err := r.reserveAdminPaginationCreation(deadlineCtx, request.SubjectID); err != nil {
		return AdminPaginationPage{}, err
	}
	var page AdminPaginationPage
	createErr := retryAdminSerialization(deadlineCtx, func() error {
		var attemptErr error
		page, attemptErr = r.createAdminPaginationPageOnce(deadlineCtx, request, scopeDigest, filterDigest, source)
		return attemptErr
	})
	if createErr == nil {
		return page, nil
	}
	if adminSerializationFailure(createErr) {
		return AdminPaginationPage{}, fmt.Errorf("%w: serialization retry exhausted: %w", ErrAdminPaginationSnapshotUnavailable, createErr)
	}
	if errors.Is(createErr, context.Canceled) || errors.Is(createErr, context.DeadlineExceeded) {
		return AdminPaginationPage{}, fmt.Errorf("%w: %w", ErrAdminPaginationSnapshotUnavailable, createErr)
	}
	return AdminPaginationPage{}, createErr
}

func (r *PostgresRepository) createAdminPaginationPageOnce(ctx context.Context, request AdminPaginationRequest, scopeDigest, filterDigest string, source AdminSnapshotMemberSource) (AdminPaginationPage, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	defer tx.Rollback(ctx)
	remaining, ok := remainingAdminPaginationBudget(ctx)
	if !ok {
		return AdminPaginationPage{}, fmt.Errorf("%w: %w", ErrAdminPaginationSnapshotUnavailable, context.DeadlineExceeded)
	}
	if _, err = tx.Exec(ctx, `SELECT set_config('statement_timeout',$1,true)`, strconv.FormatInt(max(1, remaining.Milliseconds()), 10)+"ms"); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('token-admin-pagination-capacity'))`); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	now = now.UTC()
	var accepting bool
	if err = tx.QueryRow(ctx, `SELECT accepting_new_snapshots FROM token_admin_pagination_control WHERE singleton=TRUE FOR SHARE`).Scan(&accepting); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	if !accepting {
		return AdminPaginationPage{}, ErrAdminPaginationDisabled
	}
	if err = cleanupAdminPaginationTx(ctx, tx, now); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}

	var replacedID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM token_admin_pagination_snapshots WHERE subject_id=$1 AND resource_type=$2 FOR UPDATE`, request.SubjectID, request.Resource).Scan(&replacedID)
	replacing := err == nil
	if err == nil {
		if err = expireAdminPaginationSnapshot(ctx, tx, replacedID, now); err != nil {
			return AdminPaginationPage{}, adminPaginationBuildError(err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}

	var subjectCount int64
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM token_admin_pagination_snapshots WHERE subject_id=$1 AND expires_at>$2`, request.SubjectID, now).Scan(&subjectCount); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	if subjectCount >= 2 {
		var oldest uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM token_admin_pagination_snapshots WHERE subject_id=$1 AND expires_at>$2 ORDER BY created_at,id LIMIT 1 FOR UPDATE`, request.SubjectID, now).Scan(&oldest); err != nil {
			return AdminPaginationPage{}, adminPaginationBuildError(err)
		}
		if err = expireAdminPaginationSnapshot(ctx, tx, oldest, now); err != nil {
			return AdminPaginationPage{}, adminPaginationBuildError(err)
		}
	}

	var activeSnapshots, activeMembers, storageBytes int64
	if err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(member_count),0) FROM token_admin_pagination_snapshots WHERE expires_at>$1`, now).Scan(&activeSnapshots, &activeMembers); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	if err = tx.QueryRow(ctx, adminPaginationStorageQuery).Scan(&storageBytes); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	maxStorage := r.adminPagination.maxStorageBytes
	if maxStorage <= 0 {
		maxStorage = AdminSnapshotMaxStorageBytes
	}
	if activeSnapshots >= AdminSnapshotMaxGlobal || activeMembers >= AdminSnapshotMaxGlobalMembers || storageBytes >= maxStorage ||
		(!replacing && (activeSnapshots*100 >= AdminSnapshotMaxGlobal*85 || activeMembers*100 >= AdminSnapshotMaxGlobalMembers*85 || storageBytes*100 >= maxStorage*85)) {
		return AdminPaginationPage{}, ErrAdminPaginationSnapshotLimited
	}

	snapshotID := uuid.New()
	expiresAt := now.Add(AdminSnapshotTTL)
	if _, err = tx.Exec(ctx, `INSERT INTO token_admin_pagination_snapshots(id,subject_id,authorization_version,resource_type,scope_digest,filter_digest,sort_spec,page_limit,high_water,expires_at,member_count,member_bytes,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,0,0,$9)`, snapshotID, request.SubjectID, request.AuthorizationVersion, request.Resource, scopeDigest, filterDigest, request.Sort, request.Limit, now, expiresAt); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	if err = source(ctx, tx, snapshotID, now, AdminSnapshotMaxMembers+1); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	var memberCount, memberBytes int64
	if err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(octet_length(resource_id)),0) FROM token_admin_pagination_snapshot_members WHERE snapshot_id=$1`, snapshotID).Scan(&memberCount, &memberBytes); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	if memberCount > AdminSnapshotMaxMembers {
		return AdminPaginationPage{}, ErrAdminPaginationFilterRequired
	}
	if activeMembers+memberCount > AdminSnapshotMaxGlobalMembers {
		return AdminPaginationPage{}, ErrAdminPaginationSnapshotLimited
	}
	if err = tx.QueryRow(ctx, adminPaginationStorageQuery).Scan(&storageBytes); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	if storageBytes > maxStorage {
		return AdminPaginationPage{}, ErrAdminPaginationSnapshotLimited
	}
	if _, err = tx.Exec(ctx, `UPDATE token_admin_pagination_snapshots SET member_count=$2,member_bytes=$3 WHERE id=$1`, snapshotID, memberCount, memberBytes); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	page, err := r.readAdminSnapshotMembers(ctx, tx, request, snapshotID, scopeDigest, filterDigest, 0, now, expiresAt)
	if err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return AdminPaginationPage{}, adminPaginationBuildError(err)
	}
	return page, nil
}

func (r *PostgresRepository) reserveAdminPaginationCreation(ctx context.Context, subjectID uuid.UUID) error {
	err := retryAdminSerialization(ctx, func() error {
		return r.reserveAdminPaginationCreationOnce(ctx, subjectID)
	})
	if adminSerializationFailure(err) {
		return fmt.Errorf("%w: quota serialization retry exhausted: %w", ErrAdminPaginationSnapshotUnavailable, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrAdminPaginationSnapshotUnavailable, err)
	}
	return err
}

type adminSerializationRetryWaiter func(context.Context, int) error

func waitAdminSerializationRetry(ctx context.Context, attempt int) error {
	var entropy [8]byte
	if _, err := cryptorand.Read(entropy[:]); err != nil {
		binary.LittleEndian.PutUint64(entropy[:], uint64(time.Now().UnixNano()))
	}
	delay := adminSerializationRetryDelay(attempt, binary.LittleEndian.Uint64(entropy[:]))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func adminSerializationRetryDelay(attempt int, jitter uint64) time.Duration {
	base := 10 * time.Millisecond * time.Duration(1<<min(max(attempt, 0), 4))
	return base/2 + time.Duration(jitter%uint64(base))
}

func retryAdminSerialization(ctx context.Context, operation func() error) error {
	return retryAdminSerializationWithWait(ctx, operation, waitAdminSerializationRetry)
}

func retryAdminSerializationWithWait(ctx context.Context, operation func() error, wait adminSerializationRetryWaiter) error {
	var lastErr error
	for attempt := 0; attempt < adminSerializationAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = operation()
		if !adminSerializationFailure(lastErr) {
			return lastErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt+1 < adminSerializationAttempts {
			if err := wait(ctx, attempt); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return lastErr
}

func (r *PostgresRepository) reserveAdminPaginationCreationOnce(ctx context.Context, subjectID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return adminPaginationBuildError(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('token-admin-pagination-rate'))`); err != nil {
		return adminPaginationBuildError(err)
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return adminPaginationBuildError(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM token_admin_pagination_creation_windows WHERE window_start<$1`, now.Add(-2*time.Minute).Truncate(time.Minute)); err != nil {
		return adminPaginationBuildError(err)
	}
	window := now.Truncate(time.Minute)
	limited := false
	for _, quota := range []struct {
		key string
		max int
	}{{"subject:" + subjectID.String(), AdminSnapshotSubjectPerMinute}, {"global", AdminSnapshotGlobalPerMinute}} {
		var count int
		err = tx.QueryRow(ctx, `INSERT INTO token_admin_pagination_creation_windows(rate_key,window_start,creation_count) VALUES($1,$2,1)
			ON CONFLICT(rate_key,window_start) DO UPDATE SET creation_count=token_admin_pagination_creation_windows.creation_count+1
			RETURNING creation_count`, quota.key, window).Scan(&count)
		if err != nil {
			return adminPaginationBuildError(err)
		}
		limited = limited || count > quota.max
	}
	if err = tx.Commit(ctx); err != nil {
		return adminPaginationBuildError(err)
	}
	if limited {
		remaining := window.Add(time.Minute).Sub(now)
		retryAfter := int((remaining + time.Second - 1) / time.Second)
		return adminPaginationSnapshotRateLimitError{retryAfter: max(1, min(60, retryAfter))}
	}
	return nil
}

func (r *PostgresRepository) readAdminPaginationPage(ctx context.Context, request AdminPaginationRequest, scopeDigest, filterDigest string) (AdminPaginationPage, error) {
	deadlineCtx, cancel, err := adminPaginationContext(ctx, AdminSnapshotReadBudget)
	if err != nil {
		return AdminPaginationPage{}, fmt.Errorf("%w: %w", ErrAdminPaginationQueryTimeout, err)
	}
	defer cancel()
	cursor, err := parseAdminPaginationCursor(request.Cursor, r.adminPagination.cursorHMACKey)
	if err != nil {
		return AdminPaginationPage{}, err
	}
	if cursor.SubjectID != request.SubjectID || cursor.Resource != request.Resource || cursor.ScopeDigest != scopeDigest || cursor.FilterDigest != filterDigest || cursor.Sort != request.Sort || cursor.Limit != request.Limit {
		return AdminPaginationPage{}, ErrAdminPaginationCursorInvalid
	}
	if cursor.AuthorizationVersion != request.AuthorizationVersion {
		return AdminPaginationPage{}, ErrAdminPaginationCursorExpired
	}
	tx, err := r.pool.BeginTx(deadlineCtx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AdminPaginationPage{}, adminPaginationReadError(err)
	}
	defer tx.Rollback(deadlineCtx)
	var now time.Time
	if err = tx.QueryRow(deadlineCtx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return AdminPaginationPage{}, adminPaginationReadError(err)
	}
	now = now.UTC()
	var subjectID uuid.UUID
	var authVersion int64
	var resource, storedScope, storedFilter, storedSort string
	var storedLimit int
	var highWater, expiresAt time.Time
	err = tx.QueryRow(deadlineCtx, `SELECT subject_id,authorization_version,resource_type,scope_digest,filter_digest,sort_spec,page_limit,high_water,expires_at FROM token_admin_pagination_snapshots WHERE id=$1`, cursor.SnapshotID).
		Scan(&subjectID, &authVersion, &resource, &storedScope, &storedFilter, &storedSort, &storedLimit, &highWater, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		tombstoneErr := tx.QueryRow(deadlineCtx, `SELECT true FROM token_admin_pagination_snapshot_tombstones WHERE snapshot_id=$1 AND purge_after>$2`, cursor.SnapshotID, now).Scan(&exists)
		if tombstoneErr == nil && exists {
			return AdminPaginationPage{}, ErrAdminPaginationCursorExpired
		}
		if tombstoneErr != nil && !errors.Is(tombstoneErr, pgx.ErrNoRows) {
			return AdminPaginationPage{}, adminPaginationReadError(tombstoneErr)
		}
		return AdminPaginationPage{}, ErrAdminPaginationCursorInvalid
	}
	if err != nil {
		return AdminPaginationPage{}, adminPaginationReadError(err)
	}
	if subjectID != cursor.SubjectID || authVersion != cursor.AuthorizationVersion || resource != cursor.Resource || storedScope != cursor.ScopeDigest || storedFilter != cursor.FilterDigest || storedSort != cursor.Sort || storedLimit != cursor.Limit || highWater.UnixNano() != cursor.HighWaterUnixNano || expiresAt.UnixNano() != cursor.ExpiresUnixNano {
		return AdminPaginationPage{}, ErrAdminPaginationCursorInvalid
	}
	if cursor.ExpiresUnixNano <= now.UnixNano() || !expiresAt.After(now) {
		return AdminPaginationPage{}, ErrAdminPaginationCursorExpired
	}
	if cursor.PositionKey != "" && cursor.Position > 0 {
		var storedPositionKey string
		if err = tx.QueryRow(deadlineCtx, `SELECT resource_id FROM token_admin_pagination_snapshot_members WHERE snapshot_id=$1 AND position=$2`, cursor.SnapshotID, cursor.Position-1).Scan(&storedPositionKey); errors.Is(err, pgx.ErrNoRows) {
			return AdminPaginationPage{}, ErrAdminPaginationCursorExpired
		} else if err != nil {
			return AdminPaginationPage{}, adminPaginationReadError(err)
		}
		if storedPositionKey != cursor.PositionKey {
			return AdminPaginationPage{}, ErrAdminPaginationCursorInvalid
		}
	}
	page, err := r.readAdminSnapshotMembers(deadlineCtx, tx, request, cursor.SnapshotID, scopeDigest, filterDigest, cursor.Position, highWater, expiresAt)
	if err != nil {
		return AdminPaginationPage{}, adminPaginationReadError(err)
	}
	if err = tx.Commit(deadlineCtx); err != nil {
		return AdminPaginationPage{}, adminPaginationReadError(err)
	}
	return page, nil
}

type adminPaginationQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (r *PostgresRepository) readAdminSnapshotMembers(ctx context.Context, queryer adminPaginationQueryer, request AdminPaginationRequest, snapshotID uuid.UUID, scopeDigest, filterDigest string, position int, highWater, expiresAt time.Time) (AdminPaginationPage, error) {
	rows, err := queryer.Query(ctx, `SELECT resource_id FROM token_admin_pagination_snapshot_members WHERE snapshot_id=$1 AND position>=$2 ORDER BY position LIMIT $3`, snapshotID, position, request.Limit+1)
	if err != nil {
		return AdminPaginationPage{}, err
	}
	defer rows.Close()
	ids := make([]string, 0, request.Limit+1)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return AdminPaginationPage{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return AdminPaginationPage{}, err
	}
	page := AdminPaginationPage{ResourceIDs: ids, Limit: request.Limit, AsOf: highWater, SnapshotExpiresAt: expiresAt}
	if len(ids) > request.Limit {
		page.ResourceIDs = ids[:request.Limit]
		page.HasMore = true
		nextCursor, cursorErr := signedAdminPaginationCursor(adminCursorEnvelope{
			Version: 1, SnapshotID: snapshotID, Position: position + request.Limit, SubjectID: request.SubjectID,
			PositionKey:          page.ResourceIDs[len(page.ResourceIDs)-1],
			AuthorizationVersion: request.AuthorizationVersion, Resource: request.Resource, ScopeDigest: scopeDigest,
			FilterDigest: filterDigest, Sort: request.Sort, Limit: request.Limit,
			HighWaterUnixNano: highWater.UnixNano(), ExpiresUnixNano: expiresAt.UnixNano(),
		}, r.adminPagination.cursorHMACKey)
		if cursorErr != nil {
			return AdminPaginationPage{}, cursorErr
		}
		page.NextCursor = &nextCursor
	}
	return page, nil
}

func signedAdminPaginationCursor(payload adminCursorEnvelope, key []byte) (string, error) {
	if payload.Version != 1 || payload.SnapshotID == uuid.Nil || payload.SubjectID == uuid.Nil || payload.Position < 0 || payload.Position > AdminSnapshotMaxMembers || len(key) < 32 {
		return "", ErrAdminPaginationCursorInvalid
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(encoded)
	return base64.RawURLEncoding.EncodeToString(encoded) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func parseAdminPaginationCursor(raw string, key []byte) (adminCursorEnvelope, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || len(key) < 32 {
		return adminCursorEnvelope{}, ErrAdminPaginationCursorInvalid
	}
	payloadBytes, payloadErr := base64.RawURLEncoding.DecodeString(parts[0])
	signature, signatureErr := base64.RawURLEncoding.DecodeString(parts[1])
	if payloadErr != nil || signatureErr != nil || len(signature) != sha256.Size || len(payloadBytes) > 4096 {
		return adminCursorEnvelope{}, ErrAdminPaginationCursorInvalid
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payloadBytes)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return adminCursorEnvelope{}, ErrAdminPaginationCursorInvalid
	}
	var payload adminCursorEnvelope
	decoder := json.NewDecoder(strings.NewReader(string(payloadBytes)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || payload.Version != 1 || payload.SnapshotID == uuid.Nil || payload.SubjectID == uuid.Nil || payload.Position < 0 || payload.Position > AdminSnapshotMaxMembers || payload.Resource == "" || payload.Sort == "" || payload.Limit < 1 || payload.Limit > 200 || payload.ExpiresUnixNano <= payload.HighWaterUnixNano {
		return adminCursorEnvelope{}, ErrAdminPaginationCursorInvalid
	}
	return payload, nil
}

func expireAdminPaginationSnapshot(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, now time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO token_admin_pagination_snapshot_tombstones(snapshot_id,purge_after) VALUES($1,$2)
		ON CONFLICT(snapshot_id) DO UPDATE SET purge_after=GREATEST(token_admin_pagination_snapshot_tombstones.purge_after,EXCLUDED.purge_after)`, snapshotID, now.Add(AdminSnapshotTombstoneTTL)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM token_admin_pagination_snapshots WHERE id=$1`, snapshotID)
	return err
}

func cleanupAdminPaginationTx(ctx context.Context, tx pgx.Tx, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id FROM token_admin_pagination_snapshots WHERE expires_at<=$1 ORDER BY expires_at,id FOR UPDATE`, now)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		if err := expireAdminPaginationSnapshot(ctx, tx, id, now); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM token_admin_pagination_snapshot_tombstones WHERE purge_after<=$1`, now); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM token_admin_pagination_creation_windows WHERE window_start<$1`, now.Add(-2*time.Minute).Truncate(time.Minute))
	return err
}

func (r *PostgresRepository) CleanupAdminPagination(ctx context.Context) error {
	defer func() {
		metricsContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = r.refreshAdminPaginationMetrics(metricsContext)
	}()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('token-admin-pagination-capacity'))`); err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if err = cleanupAdminPaginationTx(ctx, tx, now.UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type AdminPaginationCapacity struct {
	ActiveSnapshots  int64
	ActiveMembers    int64
	MemberBytes      int64
	StorageBytes     int64
	OldestCleanupLag time.Duration
	Warning          bool
	Critical         bool
}

func (r *PostgresRepository) AdminPaginationCapacity(ctx context.Context) (AdminPaginationCapacity, error) {
	var result AdminPaginationCapacity
	var lagSeconds float64
	err := r.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE expires_at>now()),
		COALESCE(sum(member_count) FILTER (WHERE expires_at>now()),0),
		COALESCE(sum(member_bytes) FILTER (WHERE expires_at>now()),0),
		COALESCE(EXTRACT(EPOCH FROM now()-min(expires_at) FILTER (WHERE expires_at<=now())),0)
		FROM token_admin_pagination_snapshots`).Scan(&result.ActiveSnapshots, &result.ActiveMembers, &result.MemberBytes, &lagSeconds)
	if err != nil {
		return result, err
	}
	if err = r.pool.QueryRow(ctx, adminPaginationStorageQuery).Scan(&result.StorageBytes); err != nil {
		return result, err
	}
	result.OldestCleanupLag = time.Duration(lagSeconds * float64(time.Second))
	maximum := r.adminPagination.maxStorageBytes
	if maximum <= 0 {
		maximum = AdminSnapshotMaxStorageBytes
	}
	percentages := []int64{result.ActiveSnapshots * 100 / AdminSnapshotMaxGlobal, result.ActiveMembers * 100 / AdminSnapshotMaxGlobalMembers, result.StorageBytes * 100 / maximum}
	sort.Slice(percentages, func(i, j int) bool { return percentages[i] > percentages[j] })
	result.Warning = percentages[0] >= 70 || result.OldestCleanupLag > 5*time.Minute
	result.Critical = percentages[0] >= 85
	return result, nil
}

type AdminPaginationCleanupWorker struct {
	repository *PostgresRepository
	interval   time.Duration
}

func NewAdminPaginationCleanupWorker(repository *PostgresRepository, interval time.Duration) *AdminPaginationCleanupWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	return &AdminPaginationCleanupWorker{repository: repository, interval: interval}
}

func (worker *AdminPaginationCleanupWorker) Run(ctx context.Context) {
	if worker == nil || worker.repository == nil {
		return
	}
	ticker := time.NewTicker(worker.interval)
	defer ticker.Stop()
	for {
		if err := worker.repository.CleanupAdminPagination(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "Token admin pagination cleanup failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func adminPaginationContext(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc, error) {
	if budget <= 0 {
		return nil, nil, context.DeadlineExceeded
	}
	deadline := time.Now().Add(budget)
	if existing, ok := parent.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	if !deadline.After(time.Now()) {
		return nil, nil, context.DeadlineExceeded
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	return ctx, cancel, nil
}

func remainingAdminPaginationBudget(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	remaining := time.Until(deadline)
	return remaining, remaining > 0
}

func adminSerializationFailure(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	code := adminPostgresSQLState(err)
	return code == "40001" || code == "40P01"
}

func adminPostgresSQLState(err error) string {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return postgresError.Code
	}
	return ""
}

func adminPaginationBuildError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "statement timeout") || strings.Contains(strings.ToLower(err.Error()), "canceling statement") {
		return fmt.Errorf("%w: %w", ErrAdminPaginationSnapshotUnavailable, err)
	}
	return fmt.Errorf("%w: %w", ErrAdminPaginationSnapshotUnavailable, err)
}

func adminPaginationReadError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "statement timeout") || strings.Contains(strings.ToLower(err.Error()), "canceling statement") {
		return fmt.Errorf("%w: %w", ErrAdminPaginationQueryTimeout, err)
	}
	return fmt.Errorf("%w: %w", ErrAdminPaginationSnapshotUnavailable, err)
}
