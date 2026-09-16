package account

import (
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	ApprovalSnapshotTTL              = 15 * time.Minute
	ApprovalSnapshotTombstoneTTL     = 20 * time.Minute
	ApprovalSnapshotKeyRetention     = ApprovalSnapshotTTL + ApprovalSnapshotTombstoneTTL
	ApprovalSnapshotBuildBudget      = 10 * time.Second
	ApprovalSnapshotReadBudget       = 3 * time.Second
	ApprovalSnapshotMaxMembers       = 1_000_000
	ApprovalSnapshotMaxGlobal        = 64
	ApprovalSnapshotMaxGlobalMembers = 20_000_000
	ApprovalSnapshotMaxStorageBytes  = int64(8 * 1024 * 1024 * 1024)
	ApprovalSnapshotSubjectPerMinute = 6
	ApprovalSnapshotGlobalPerMinute  = 60
	ApprovalReadSubjectPerMinute     = 60
	ApprovalReadIPPerMinute          = 120
	approvalPaginationResource       = "approval_requests"
	approvalPaginationSort           = "updated_at_desc_id_desc"
)

var (
	ErrApprovalPaginationDisabled            = errors.New("approval pagination is disabled")
	ErrApprovalPaginationLimitInvalid        = errors.New("approval pagination limit is invalid")
	ErrApprovalPaginationFilterInvalid       = errors.New("approval pagination filter is invalid")
	ErrApprovalPaginationCursorInvalid       = errors.New("approval pagination cursor is invalid")
	ErrApprovalPaginationCursorExpired       = errors.New("approval pagination cursor is expired")
	ErrApprovalPaginationFilterRequired      = errors.New("approval pagination filter is required")
	ErrApprovalPaginationSnapshotLimited     = errors.New("approval pagination snapshot is limited")
	ErrApprovalPaginationRateLimited         = errors.New("approval pagination reads are rate limited")
	ErrApprovalPaginationSnapshotUnavailable = errors.New("approval pagination snapshot is unavailable")
	ErrApprovalPaginationQueryTimeout        = errors.New("approval pagination query timed out")
)

type ApprovalPaginationConfig struct {
	Enabled             bool
	HardDisabled        bool
	CurrentKeyID        string
	CurrentKey          []byte
	PreviousKeyID       string
	PreviousKey         []byte
	PreviousRetainUntil time.Time
	MaximumStorageBytes int64
}

type approvalPaginationRuntime struct {
	enabled             bool
	hardDisabled        bool
	currentKeyID        string
	currentKey          []byte
	previousKeyID       string
	previousKey         []byte
	previousRetainUntil time.Time
	maximumStorageBytes int64
}

type ApprovalPaginationPage struct {
	ApprovalRequestIDs []uuid.UUID `json:"-"`
	NextCursor         *string     `json:"next_cursor"`
	HasMore            bool        `json:"has_more"`
	Limit              int         `json:"limit"`
	AsOf               time.Time   `json:"as_of"`
	SnapshotExpiresAt  time.Time   `json:"snapshot_expires_at"`
}

type ApprovalPaginationCapacity struct {
	ActiveSnapshots  int64
	ActiveMembers    int64
	MemberBytes      int64
	StorageBytes     int64
	CleanupBacklog   int64
	OldestCleanupLag time.Duration
	Warning          bool
	Critical         bool
}

type approvalPaginationCursor struct {
	Version              string    `json:"v"`
	KeyID                string    `json:"kid"`
	SnapshotID           uuid.UUID `json:"snapshot_id"`
	SubjectID            uuid.UUID `json:"subject_id"`
	AuthorizationVersion int64     `json:"authorization_version"`
	Resource             string    `json:"resource"`
	ScopeDigest          string    `json:"scope_digest"`
	FilterDigest         string    `json:"filter_digest"`
	Sort                 string    `json:"sort"`
	Limit                int       `json:"limit"`
	Position             int       `json:"position"`
	PositionKey          string    `json:"position_key"`
	AsOf                 time.Time `json:"as_of"`
	ExpiresAt            time.Time `json:"expires_at"`
}

type approvalPaginationRateError struct {
	cause      error
	retryAfter int
}

func (e approvalPaginationRateError) Error() string   { return e.cause.Error() }
func (e approvalPaginationRateError) Unwrap() error   { return e.cause }
func (e approvalPaginationRateError) RetryAfter() int { return e.retryAfter }

type ApprovalPaginationHTTPError struct {
	Status     int
	Code       string
	RetryAfter int
}

func ClassifyApprovalPaginationError(err error) ApprovalPaginationHTTPError {
	var rateError approvalPaginationRateError
	if errors.As(err, &rateError) {
		code := "RATE_LIMITED"
		if errors.Is(err, ErrApprovalPaginationSnapshotLimited) {
			code = "PAGINATION_SNAPSHOT_LIMITED"
		}
		return ApprovalPaginationHTTPError{Status: 429, Code: code, RetryAfter: max(1, min(60, rateError.RetryAfter()))}
	}
	switch {
	case errors.Is(err, ErrApprovalPaginationLimitInvalid):
		return ApprovalPaginationHTTPError{Status: 400, Code: "INVALID_LIMIT"}
	case errors.Is(err, ErrApprovalPaginationFilterInvalid):
		return ApprovalPaginationHTTPError{Status: 400, Code: "INVALID_FILTER"}
	case errors.Is(err, ErrApprovalPaginationCursorInvalid):
		return ApprovalPaginationHTTPError{Status: 400, Code: "INVALID_CURSOR"}
	case errors.Is(err, ErrApprovalPaginationCursorExpired):
		return ApprovalPaginationHTTPError{Status: 410, Code: "CURSOR_EXPIRED"}
	case errors.Is(err, ErrApprovalPaginationFilterRequired):
		return ApprovalPaginationHTTPError{Status: 422, Code: "PAGINATION_FILTER_REQUIRED"}
	case errors.Is(err, ErrApprovalPaginationSnapshotLimited):
		return ApprovalPaginationHTTPError{Status: 429, Code: "PAGINATION_SNAPSHOT_LIMITED", RetryAfter: 1}
	case errors.Is(err, ErrApprovalPaginationQueryTimeout):
		return ApprovalPaginationHTTPError{Status: 504, Code: "QUERY_TIMEOUT"}
	default:
		return ApprovalPaginationHTTPError{Status: 503, Code: "PAGINATION_SNAPSHOT_UNAVAILABLE", RetryAfter: 5}
	}
}

// ConfigureApprovalPagination is startup-only configuration. The persisted key
// usage ledger prevents an operator from retiring a verification key while a
// cursor or its replacement tombstone can still reference that key.
func (r *PostgresRepository) ConfigureApprovalPagination(ctx context.Context, config ApprovalPaginationConfig) error {
	if r == nil {
		return ErrApprovalPaginationSnapshotUnavailable
	}
	// Any rejected reconfiguration fails closed instead of retaining a stale,
	// apparently usable runtime configuration.
	r.approvalPagination = approvalPaginationRuntime{hardDisabled: true}
	config.CurrentKeyID = strings.TrimSpace(config.CurrentKeyID)
	config.PreviousKeyID = strings.TrimSpace(config.PreviousKeyID)
	if config.HardDisabled {
		return nil
	}
	if len(config.CurrentKey) != 0 && (config.CurrentKeyID == "" || len(config.CurrentKey) < 32) {
		return errors.New("approval pagination current key requires kid and at least 32 bytes")
	}
	if len(config.CurrentKeyID) > 128 || len(config.PreviousKeyID) > 128 {
		return errors.New("approval pagination kid cannot exceed 128 bytes")
	}
	if config.Enabled && len(config.CurrentKey) < 32 {
		return errors.New("approval pagination current key is required when enabled")
	}
	previousConfigured := config.PreviousKeyID != "" || len(config.PreviousKey) != 0 || !config.PreviousRetainUntil.IsZero()
	if previousConfigured && (config.PreviousKeyID == "" || len(config.PreviousKey) < 32 || config.PreviousRetainUntil.IsZero()) {
		return errors.New("approval pagination previous key configuration is incomplete")
	}
	if config.CurrentKeyID != "" && config.CurrentKeyID == config.PreviousKeyID {
		return errors.New("approval pagination current and previous kid must differ")
	}
	rows, err := r.pool.Query(ctx, `SELECT kid,last_signed_at,key_digest FROM approval_pagination_signing_keys WHERE last_signed_at+make_interval(secs=>$1)>transaction_timestamp() ORDER BY kid`, int64(ApprovalSnapshotKeyRetention/time.Second))
	if err != nil {
		return fmtApprovalPaginationConfiguration(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kid string
		var lastSignedAt time.Time
		var storedDigest *string
		if err = rows.Scan(&kid, &lastSignedAt, &storedDigest); err != nil {
			return fmtApprovalPaginationConfiguration(err)
		}
		var configuredKey []byte
		switch kid {
		case config.CurrentKeyID:
			configuredKey = config.CurrentKey
		case config.PreviousKeyID:
			configuredKey = config.PreviousKey
			if config.PreviousRetainUntil.Before(lastSignedAt.Add(ApprovalSnapshotKeyRetention)) {
				return errors.New("approval pagination previous key retention is shorter than the persisted cursor lifetime")
			}
		default:
			return errors.New("approval pagination configuration omits a key that still has cursor references")
		}
		if storedDigest == nil || *storedDigest != approvalPaginationKeyDigest(configuredKey) {
			return errors.New("approval pagination kid is bound to different or unknown signing material")
		}
	}
	if err := rows.Err(); err != nil {
		return fmtApprovalPaginationConfiguration(err)
	}
	maximum := config.MaximumStorageBytes
	if maximum <= 0 {
		maximum = ApprovalSnapshotMaxStorageBytes
	}
	r.approvalPagination = approvalPaginationRuntime{
		enabled: config.Enabled, hardDisabled: config.HardDisabled,
		currentKeyID: config.CurrentKeyID, currentKey: append([]byte(nil), config.CurrentKey...),
		previousKeyID: config.PreviousKeyID, previousKey: append([]byte(nil), config.PreviousKey...),
		previousRetainUntil: config.PreviousRetainUntil.UTC(), maximumStorageBytes: maximum,
	}
	return nil
}

func fmtApprovalPaginationConfiguration(err error) error {
	return errors.Join(ErrApprovalPaginationSnapshotUnavailable, err)
}

func approvalPaginationKeyDigest(key []byte) string {
	digest := sha256.Sum256(key)
	return hex.EncodeToString(digest[:])
}

func (r *PostgresRepository) SetApprovalPaginationGate(ctx context.Context, enabled bool) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('account-approval-pagination-capacity'))`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE approval_pagination_control SET accepting_new_snapshots=$1,updated_at=transaction_timestamp() WHERE singleton=TRUE`, enabled); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func NormalizeApprovalPaginationLimit(limit int) (int, error) {
	if limit == 0 {
		return 50, nil
	}
	if limit < 1 || limit > 200 {
		return 0, ErrApprovalPaginationLimitInvalid
	}
	return limit, nil
}

func (r *PostgresRepository) ApprovalPaginationPage(ctx context.Context, principal identity.Principal, filter ApprovalFilter, cursor string, limit int, sourceIP string) (ApprovalPaginationPage, error) {
	if !humanApprovalPrincipal(principal) {
		return ApprovalPaginationPage{}, ErrForbidden
	}
	limit, err := NormalizeApprovalPaginationLimit(limit)
	if err != nil {
		return ApprovalPaginationPage{}, err
	}
	filter.Limit = 0
	if err := validateApprovalPaginationFilter(filter); err != nil {
		return ApprovalPaginationPage{}, err
	}
	parsedIP := net.ParseIP(strings.TrimSpace(sourceIP))
	if parsedIP == nil {
		return ApprovalPaginationPage{}, ErrApprovalPaginationFilterInvalid
	}
	sourceIP = parsedIP.String()
	filterDigest := approvalPaginationFilterDigest(filter)
	cursor = strings.TrimSpace(cursor)
	isContinuation := cursor != ""
	if !r.approvalPagination.valid() {
		return ApprovalPaginationPage{}, ErrApprovalPaginationSnapshotUnavailable
	}
	var parsedCursor approvalPaginationCursor
	if isContinuation {
		parsedCursor, err = parseApprovalPaginationCursor(cursor, r.approvalPagination, r.now())
		if err != nil {
			return ApprovalPaginationPage{}, err
		}
		if parsedCursor.Resource != approvalPaginationResource || parsedCursor.Sort != approvalPaginationSort || parsedCursor.Limit != limit || parsedCursor.FilterDigest != filterDigest || parsedCursor.Position < 1 || parsedCursor.PositionKey == "" {
			return ApprovalPaginationPage{}, ErrApprovalPaginationCursorInvalid
		}
		if !parsedCursor.ExpiresAt.After(r.now()) {
			return ApprovalPaginationPage{}, ErrApprovalPaginationCursorExpired
		}
	} else if !r.approvalPagination.enabled {
		return ApprovalPaginationPage{}, ErrApprovalPaginationDisabled
	}
	budget := ApprovalSnapshotBuildBudget
	if isContinuation {
		budget = ApprovalSnapshotReadBudget
	}
	operationCtx, cancel, err := approvalPaginationContext(ctx, budget)
	if err != nil {
		return ApprovalPaginationPage{}, approvalPaginationOperationError(err, isContinuation)
	}
	defer cancel()
	actorID, err := r.userIDByPrincipal(operationCtx, principal)
	if err != nil {
		return ApprovalPaginationPage{}, approvalPaginationOperationError(err, isContinuation)
	}
	version, permissions, err := r.approvalDirectoryAuthorization(operationCtx, principal)
	if err != nil {
		return ApprovalPaginationPage{}, approvalPaginationOperationError(err, isContinuation)
	}
	scopeDigest := approvalPaginationScopeDigest(actorID, permissions)
	if isContinuation {
		if parsedCursor.SubjectID != actorID {
			return ApprovalPaginationPage{}, ErrApprovalPaginationCursorInvalid
		}
		if parsedCursor.AuthorizationVersion != version || parsedCursor.ScopeDigest != scopeDigest {
			return ApprovalPaginationPage{}, ErrApprovalPaginationCursorExpired
		}
		return r.readApprovalPaginationPage(operationCtx, actorID, version, scopeDigest, filterDigest, parsedCursor, limit, sourceIP)
	}
	return r.createApprovalPaginationPage(operationCtx, actorID, version, scopeDigest, filterDigest, filter, limit, sourceIP)
}

func validateApprovalPaginationFilter(filter ApprovalFilter) error {
	if filter.Requester != nil && *filter.Requester == uuid.Nil {
		return ErrApprovalPaginationFilterInvalid
	}
	if filter.SubmittedFrom != nil && filter.SubmittedTo != nil && !filter.SubmittedFrom.Before(*filter.SubmittedTo) {
		return ErrApprovalPaginationFilterInvalid
	}
	if filter.ExpiresFrom != nil && filter.ExpiresTo != nil && !filter.ExpiresFrom.Before(*filter.ExpiresTo) {
		return ErrApprovalPaginationFilterInvalid
	}
	command := strings.ToUpper(strings.TrimSpace(filter.CommandType))
	if command != "" {
		policy, err := approvalpolicy.Lookup(command)
		if err != nil || policy.Mode != approvalpolicy.ApprovalRequired {
			return ErrApprovalPaginationFilterInvalid
		}
	}
	status := strings.ToUpper(strings.TrimSpace(filter.Status))
	if status != "" {
		valid := map[string]bool{"DRAFT": true, "SUBMITTED": true, "APPROVED": true, "REJECTED": true, "WITHDRAWN": true, "EXPIRED": true, "INVALIDATED": true, "EXECUTING": true, "EXECUTED": true, "EXECUTION_FAILED": true}
		if !valid[status] {
			return ErrApprovalPaginationFilterInvalid
		}
	}
	risk := strings.ToUpper(strings.TrimSpace(filter.RiskClass))
	if risk != "" {
		valid := false
		for _, policy := range approvalpolicy.All() {
			if policy.RiskClass == risk {
				valid = true
				break
			}
		}
		if !valid {
			return ErrApprovalPaginationFilterInvalid
		}
	}
	return nil
}

func approvalPaginationScopeDigest(actorID uuid.UUID, permissions map[string]struct{}) string {
	type commandScope struct {
		CommandType string `json:"command_type"`
		Own         bool   `json:"own"`
		Approve     bool   `json:"approve"`
	}
	items := make([]commandScope, 0)
	for _, policy := range approvalpolicy.All() {
		if policy.Mode != approvalpolicy.ApprovalRequired {
			continue
		}
		_, own := permissions[policy.RequesterPermission]
		_, approve := permissions[policy.ApproverPermission]
		if own || approve {
			items = append(items, commandScope{policy.CommandType, own, approve})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CommandType < items[j].CommandType })
	return approvalPaginationDigest(struct {
		SubjectID uuid.UUID      `json:"subject_id"`
		Resource  string         `json:"resource"`
		View      string         `json:"view"`
		Commands  []commandScope `json:"commands"`
	}{actorID, approvalPaginationResource, "own_or_current_approver", items})
}

func approvalPaginationFilterDigest(filter ApprovalFilter) string {
	timestamp := func(value *time.Time) string {
		if value == nil {
			return ""
		}
		return value.UTC().Format(time.RFC3339Nano)
	}
	requester := ""
	if filter.Requester != nil {
		requester = filter.Requester.String()
	}
	return approvalPaginationDigest(struct {
		CommandType   string `json:"command_type"`
		Status        string `json:"status"`
		RiskClass     string `json:"risk_class"`
		Requester     string `json:"requester_user_id"`
		Target        string `json:"target"`
		SubmittedFrom string `json:"submitted_from"`
		SubmittedTo   string `json:"submitted_to"`
		ExpiresFrom   string `json:"expires_from"`
		ExpiresTo     string `json:"expires_to"`
	}{strings.ToUpper(strings.TrimSpace(filter.CommandType)), strings.ToUpper(strings.TrimSpace(filter.Status)), strings.ToUpper(strings.TrimSpace(filter.RiskClass)), requester, strings.TrimSpace(filter.Target), timestamp(filter.SubmittedFrom), timestamp(filter.SubmittedTo), timestamp(filter.ExpiresFrom), timestamp(filter.ExpiresTo)})
}

func approvalPaginationDigest(value any) string {
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (r *PostgresRepository) createApprovalPaginationPage(ctx context.Context, actorID uuid.UUID, version int64, scopeDigest, filterDigest string, filter ApprovalFilter, limit int, sourceIP string) (ApprovalPaginationPage, error) {
	var finalErr error
	defer func() { r.recordApprovalPaginationCreation(finalErr) }()
	deadlineCtx, cancel, err := approvalPaginationContext(ctx, ApprovalSnapshotBuildBudget)
	if err != nil {
		finalErr = err
		return ApprovalPaginationPage{}, err
	}
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		page, createErr := r.createApprovalPaginationPageOnce(deadlineCtx, actorID, version, scopeDigest, filterDigest, filter, limit, sourceIP)
		if !approvalPaginationSerializationFailure(createErr) {
			finalErr = approvalPaginationBuildError(createErr)
			return page, finalErr
		}
		if err := approvalPaginationRetry(deadlineCtx, attempt); err != nil {
			finalErr = approvalPaginationBuildError(err)
			return ApprovalPaginationPage{}, finalErr
		}
	}
	finalErr = ErrApprovalPaginationSnapshotUnavailable
	return ApprovalPaginationPage{}, finalErr
}

func (r *PostgresRepository) createApprovalPaginationPageOnce(ctx context.Context, actorID uuid.UUID, version int64, scopeDigest, filterDigest string, filter ApprovalFilter, limit int, sourceIP string) (ApprovalPaginationPage, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ApprovalPaginationPage{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('account-approval-pagination-capacity'))`); err != nil {
		return ApprovalPaginationPage{}, err
	}
	var now time.Time
	var accepting bool
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp(),accepting_new_snapshots FROM approval_pagination_control WHERE singleton=TRUE FOR UPDATE`).Scan(&now, &accepting); err != nil || !accepting {
		return ApprovalPaginationPage{}, ErrApprovalPaginationSnapshotUnavailable
	}
	if err = cleanupApprovalPaginationTx(ctx, tx, now); err != nil {
		return ApprovalPaginationPage{}, err
	}
	if err = reserveApprovalPaginationRate(ctx, tx, "snapshot:subject:"+actorID.String(), ApprovalSnapshotSubjectPerMinute, now, ErrApprovalPaginationSnapshotLimited); err != nil {
		return ApprovalPaginationPage{}, err
	}
	if err = reserveApprovalPaginationRate(ctx, tx, "snapshot:global", ApprovalSnapshotGlobalPerMinute, now, ErrApprovalPaginationSnapshotLimited); err != nil {
		return ApprovalPaginationPage{}, err
	}
	if err = reserveApprovalPaginationReads(ctx, tx, actorID, sourceIP, now); err != nil {
		return ApprovalPaginationPage{}, err
	}
	var replacing bool
	var oldID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM approval_pagination_snapshots WHERE subject_id=$1 AND resource_type=$2 FOR UPDATE`, actorID, approvalPaginationResource).Scan(&oldID)
	if err == nil {
		replacing = true
		if err = expireApprovalPaginationSnapshot(ctx, tx, oldID, now); err != nil {
			return ApprovalPaginationPage{}, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ApprovalPaginationPage{}, err
	}
	var activeSnapshots, activeMembers, storageBytes int64
	err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(member_count),0),pg_total_relation_size('approval_pagination_snapshots')+pg_total_relation_size('approval_pagination_snapshot_members')+pg_total_relation_size('approval_pagination_snapshot_tombstones') FROM approval_pagination_snapshots WHERE expires_at>$1`, now).Scan(&activeSnapshots, &activeMembers, &storageBytes)
	if err != nil {
		return ApprovalPaginationPage{}, err
	}
	critical := activeSnapshots*100 >= ApprovalSnapshotMaxGlobal*85 || activeMembers*100 >= ApprovalSnapshotMaxGlobalMembers*85 || storageBytes*100 >= r.approvalPagination.maximumStorageBytes*85
	if activeSnapshots >= ApprovalSnapshotMaxGlobal || activeMembers >= ApprovalSnapshotMaxGlobalMembers || storageBytes >= r.approvalPagination.maximumStorageBytes || critical && !replacing {
		return ApprovalPaginationPage{}, ErrApprovalPaginationSnapshotLimited
	}
	snapshotID := uuid.Must(uuid.NewV7())
	expiresAt := now.Add(ApprovalSnapshotTTL)
	if _, err = tx.Exec(ctx, `INSERT INTO approval_pagination_snapshots(id,subject_id,authorization_version,resource_type,scope_digest,filter_digest,sort_spec,page_limit,as_of,expires_at,member_count,member_bytes,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,0,0,$9)`, snapshotID, actorID, version, approvalPaginationResource, scopeDigest, filterDigest, approvalPaginationSort, limit, now, expiresAt); err != nil {
		return ApprovalPaginationPage{}, err
	}
	command, status, risk := strings.ToUpper(strings.TrimSpace(filter.CommandType)), strings.ToUpper(strings.TrimSpace(filter.Status)), strings.ToUpper(strings.TrimSpace(filter.RiskClass))
	_, err = tx.Exec(ctx, `INSERT INTO approval_pagination_snapshot_members(snapshot_id,position,approval_request_id,position_key)
SELECT $1,(row_number() OVER(ORDER BY ar.updated_at DESC,ar.id DESC)-1)::integer,ar.id,
       to_char(ar.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')||':'||ar.id::text
FROM approval_requests ar JOIN approval_command_policies p ON p.command_type=ar.command_type AND p.policy_version=ar.policy_version
WHERE ar.updated_at<=$2 AND ($3='' OR ar.command_type=$3) AND ($4='' OR ar.status=$4) AND ($5='' OR p.risk_class=$5)
  AND ($6::uuid IS NULL OR ar.requester_user_id=$6) AND ($7='' OR ar.target_resource_id=$7)
  AND ($8::timestamptz IS NULL OR ar.submitted_at>=$8) AND ($9::timestamptz IS NULL OR ar.submitted_at<$9)
  AND ($10::timestamptz IS NULL OR ar.expires_at>=$10) AND ($11::timestamptz IS NULL OR ar.expires_at<$11)
  AND (ar.requester_user_id=$12 OR EXISTS(SELECT 1 FROM role_assignments ra JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=p.approver_permission WHERE ra.user_id=$12 AND ra.organization_id IS NULL AND ra.revoked_at IS NULL AND ra.starts_at<=$2 AND (ra.expires_at IS NULL OR ra.expires_at>$2)))
ORDER BY ar.updated_at DESC,ar.id DESC LIMIT $13`, snapshotID, now, command, status, risk, filter.Requester, strings.TrimSpace(filter.Target), filter.SubmittedFrom, filter.SubmittedTo, filter.ExpiresFrom, filter.ExpiresTo, actorID, ApprovalSnapshotMaxMembers+1)
	if err != nil {
		return ApprovalPaginationPage{}, err
	}
	var memberCount int64
	var memberBytes int64
	if err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(pg_column_size(m)),0) FROM approval_pagination_snapshot_members m WHERE snapshot_id=$1`, snapshotID).Scan(&memberCount, &memberBytes); err != nil {
		return ApprovalPaginationPage{}, err
	}
	if memberCount > ApprovalSnapshotMaxMembers {
		return ApprovalPaginationPage{}, ErrApprovalPaginationFilterRequired
	}
	if activeMembers+memberCount > ApprovalSnapshotMaxGlobalMembers || storageBytes+memberBytes > r.approvalPagination.maximumStorageBytes {
		return ApprovalPaginationPage{}, ErrApprovalPaginationSnapshotLimited
	}
	// pg_column_size does not include B-tree or page overhead. The physical
	// post-insert check is the hard cap; a rejected transaction rolls back all
	// member rows and cannot deliver a truncated first page.
	var physicalBytes int64
	if err = tx.QueryRow(ctx, `SELECT pg_total_relation_size('approval_pagination_snapshots')+pg_total_relation_size('approval_pagination_snapshot_members')+pg_total_relation_size('approval_pagination_snapshot_tombstones')`).Scan(&physicalBytes); err != nil {
		return ApprovalPaginationPage{}, err
	}
	if physicalBytes > r.approvalPagination.maximumStorageBytes {
		return ApprovalPaginationPage{}, ErrApprovalPaginationSnapshotLimited
	}
	if _, err = tx.Exec(ctx, `UPDATE approval_pagination_snapshots SET member_count=$2,member_bytes=$3 WHERE id=$1`, snapshotID, memberCount, memberBytes); err != nil {
		return ApprovalPaginationPage{}, err
	}
	page, err := r.readApprovalSnapshotMembers(ctx, tx, actorID, version, scopeDigest, filterDigest, snapshotID, 0, "", limit, now, expiresAt)
	if err != nil {
		return ApprovalPaginationPage{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ApprovalPaginationPage{}, err
	}
	if err = ensureApprovalAuthorizationVersion(ctx, r.pool, actorID, version); err != nil {
		if errors.Is(err, ErrApprovalAuthorizationChanged) {
			return ApprovalPaginationPage{}, ErrApprovalPaginationCursorExpired
		}
		return ApprovalPaginationPage{}, approvalPaginationBuildError(err)
	}
	return page, nil
}

func (r *PostgresRepository) readApprovalPaginationPage(ctx context.Context, actorID uuid.UUID, version int64, scopeDigest, filterDigest string, cursor approvalPaginationCursor, limit int, sourceIP string) (ApprovalPaginationPage, error) {
	if !cursor.ExpiresAt.After(r.now()) {
		return ApprovalPaginationPage{}, ErrApprovalPaginationCursorExpired
	}
	deadlineCtx, cancel, err := approvalPaginationContext(ctx, ApprovalSnapshotReadBudget)
	if err != nil {
		return ApprovalPaginationPage{}, err
	}
	defer cancel()
	tx, err := r.pool.BeginTx(deadlineCtx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return ApprovalPaginationPage{}, approvalPaginationReadError(err)
	}
	defer tx.Rollback(deadlineCtx)
	var now time.Time
	if err = tx.QueryRow(deadlineCtx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return ApprovalPaginationPage{}, approvalPaginationReadError(err)
	}
	if err = reserveApprovalPaginationReads(deadlineCtx, tx, actorID, sourceIP, now); err != nil {
		return ApprovalPaginationPage{}, approvalPaginationReadError(err)
	}
	var storedVersion int64
	var storedScope, storedFilter, storedSort string
	var storedLimit int
	var asOf, expiresAt time.Time
	err = tx.QueryRow(deadlineCtx, `SELECT authorization_version,scope_digest,filter_digest,sort_spec,page_limit,as_of,expires_at FROM approval_pagination_snapshots WHERE id=$1 AND subject_id=$2`, cursor.SnapshotID, actorID).Scan(&storedVersion, &storedScope, &storedFilter, &storedSort, &storedLimit, &asOf, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var tombstone bool
		_ = tx.QueryRow(deadlineCtx, `SELECT EXISTS(SELECT 1 FROM approval_pagination_snapshot_tombstones WHERE snapshot_id=$1)`, cursor.SnapshotID).Scan(&tombstone)
		if tombstone {
			return ApprovalPaginationPage{}, ErrApprovalPaginationCursorExpired
		}
		return ApprovalPaginationPage{}, ErrApprovalPaginationCursorInvalid
	}
	if err != nil {
		return ApprovalPaginationPage{}, approvalPaginationReadError(err)
	}
	if !expiresAt.After(now) {
		return ApprovalPaginationPage{}, ErrApprovalPaginationCursorExpired
	}
	if storedVersion != version || storedScope != scopeDigest || storedFilter != filterDigest || storedSort != approvalPaginationSort || storedLimit != limit || !asOf.Equal(cursor.AsOf) || !expiresAt.Equal(cursor.ExpiresAt) {
		return ApprovalPaginationPage{}, ErrApprovalPaginationCursorExpired
	}
	var previousKey string
	if err = tx.QueryRow(deadlineCtx, `SELECT position_key FROM approval_pagination_snapshot_members WHERE snapshot_id=$1 AND position=$2`, cursor.SnapshotID, cursor.Position-1).Scan(&previousKey); err != nil || previousKey != cursor.PositionKey {
		return ApprovalPaginationPage{}, ErrApprovalPaginationCursorInvalid
	}
	page, err := r.readApprovalSnapshotMembers(deadlineCtx, tx, actorID, version, scopeDigest, filterDigest, cursor.SnapshotID, cursor.Position, cursor.PositionKey, limit, asOf, expiresAt)
	if err != nil {
		return ApprovalPaginationPage{}, approvalPaginationReadError(err)
	}
	if err = tx.Commit(deadlineCtx); err != nil {
		return ApprovalPaginationPage{}, approvalPaginationReadError(err)
	}
	if err = ensureApprovalAuthorizationVersion(ctx, r.pool, actorID, version); err != nil {
		if errors.Is(err, ErrApprovalAuthorizationChanged) {
			return ApprovalPaginationPage{}, ErrApprovalPaginationCursorExpired
		}
		return ApprovalPaginationPage{}, approvalPaginationReadError(err)
	}
	return page, nil
}

func (runtime approvalPaginationRuntime) valid() bool {
	if runtime.hardDisabled || runtime.currentKeyID == "" || len(runtime.currentKey) < 32 {
		return false
	}
	previousConfigured := runtime.previousKeyID != "" || len(runtime.previousKey) != 0 || !runtime.previousRetainUntil.IsZero()
	return !previousConfigured || runtime.previousKeyID != "" && len(runtime.previousKey) >= 32 && !runtime.previousRetainUntil.IsZero() && runtime.previousKeyID != runtime.currentKeyID
}

func (r *PostgresRepository) readApprovalSnapshotMembers(ctx context.Context, queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, actorID uuid.UUID, version int64, scopeDigest, filterDigest string, snapshotID uuid.UUID, position int, _ string, limit int, asOf, expiresAt time.Time) (ApprovalPaginationPage, error) {
	rows, err := queryer.Query(ctx, `SELECT m.approval_request_id,m.position,m.position_key
FROM approval_pagination_snapshot_members m
JOIN approval_requests ar ON ar.id=m.approval_request_id
JOIN approval_command_policies p ON p.command_type=ar.command_type AND p.policy_version=ar.policy_version
WHERE m.snapshot_id=$1 AND m.position>=$2
  AND (ar.requester_user_id=$4 OR EXISTS(
    SELECT 1 FROM role_assignments ra JOIN role_permissions rp
      ON rp.role_code=ra.role_code AND rp.permission_code=p.approver_permission
    WHERE ra.user_id=$4 AND ra.organization_id IS NULL AND ra.revoked_at IS NULL
      AND ra.starts_at<=transaction_timestamp()
      AND (ra.expires_at IS NULL OR ra.expires_at>transaction_timestamp())))
ORDER BY m.position LIMIT $3`, snapshotID, position, limit+1, actorID)
	if err != nil {
		return ApprovalPaginationPage{}, err
	}
	defer rows.Close()
	ids := make([]uuid.UUID, 0, limit+1)
	lastPosition := position - 1
	lastKey := ""
	for rows.Next() {
		var id uuid.UUID
		var p int
		var key string
		if err = rows.Scan(&id, &p, &key); err != nil {
			return ApprovalPaginationPage{}, err
		}
		ids = append(ids, id)
		if len(ids) <= limit {
			lastPosition, lastKey = p, key
		}
	}
	if err = rows.Err(); err != nil {
		return ApprovalPaginationPage{}, err
	}
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	page := ApprovalPaginationPage{ApprovalRequestIDs: ids, HasMore: hasMore, Limit: limit, AsOf: asOf, SnapshotExpiresAt: expiresAt}
	if hasMore {
		cursor, signErr := signedApprovalPaginationCursor(approvalPaginationCursor{Version: "1", KeyID: r.approvalPagination.currentKeyID, SnapshotID: snapshotID, SubjectID: actorID, AuthorizationVersion: version, Resource: approvalPaginationResource, ScopeDigest: scopeDigest, FilterDigest: filterDigest, Sort: approvalPaginationSort, Limit: limit, Position: lastPosition + 1, PositionKey: lastKey, AsOf: asOf, ExpiresAt: expiresAt}, r.approvalPagination.currentKey)
		if signErr != nil {
			return ApprovalPaginationPage{}, signErr
		}
		command, signErr := queryer.Exec(ctx, `INSERT INTO approval_pagination_signing_keys(kid,last_signed_at,key_digest) VALUES($1,transaction_timestamp(),$2) ON CONFLICT(kid) DO UPDATE SET last_signed_at=GREATEST(approval_pagination_signing_keys.last_signed_at,EXCLUDED.last_signed_at) WHERE approval_pagination_signing_keys.key_digest=EXCLUDED.key_digest`, r.approvalPagination.currentKeyID, approvalPaginationKeyDigest(r.approvalPagination.currentKey))
		if signErr != nil || command.RowsAffected() != 1 {
			if signErr == nil {
				signErr = ErrApprovalPaginationSnapshotUnavailable
			}
			return ApprovalPaginationPage{}, signErr
		}
		page.NextCursor = &cursor
	}
	return page, nil
}

func signedApprovalPaginationCursor(payload approvalPaginationCursor, key []byte) (string, error) {
	if len(key) < 32 {
		return "", ErrApprovalPaginationSnapshotUnavailable
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(encoded)
	return base64.RawURLEncoding.EncodeToString(encoded) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func parseApprovalPaginationCursor(raw string, runtime approvalPaginationRuntime, now time.Time) (approvalPaginationCursor, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return approvalPaginationCursor{}, ErrApprovalPaginationCursorInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(payload) > 4096 {
		return approvalPaginationCursor{}, ErrApprovalPaginationCursorInvalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return approvalPaginationCursor{}, ErrApprovalPaginationCursorInvalid
	}
	if err = validateApprovalCursorJSON(payload); err != nil {
		return approvalPaginationCursor{}, ErrApprovalPaginationCursorInvalid
	}
	var cursor approvalPaginationCursor
	if json.Unmarshal(payload, &cursor) != nil {
		return approvalPaginationCursor{}, ErrApprovalPaginationCursorInvalid
	}
	var key []byte
	switch cursor.KeyID {
	case runtime.currentKeyID:
		key = runtime.currentKey
	case runtime.previousKeyID:
		if runtime.previousKeyID != "" && now.Before(runtime.previousRetainUntil) {
			key = runtime.previousKey
		}
	}
	if len(key) < 32 {
		return approvalPaginationCursor{}, ErrApprovalPaginationCursorInvalid
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return approvalPaginationCursor{}, ErrApprovalPaginationCursorInvalid
	}
	if cursor.Version != "1" || cursor.KeyID == "" || cursor.SnapshotID == uuid.Nil || cursor.SubjectID == uuid.Nil || cursor.AuthorizationVersion < 1 || cursor.Resource != approvalPaginationResource || cursor.Sort != approvalPaginationSort || cursor.Limit < 1 || cursor.Limit > 200 || cursor.Position < 1 || cursor.PositionKey == "" || !approvalPaginationHexDigest(cursor.ScopeDigest) || !approvalPaginationHexDigest(cursor.FilterDigest) || cursor.AsOf.IsZero() || cursor.ExpiresAt.IsZero() || !cursor.ExpiresAt.After(cursor.AsOf) {
		return approvalPaginationCursor{}, ErrApprovalPaginationCursorInvalid
	}
	return cursor, nil
}

func approvalPaginationHexDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validateApprovalCursorJSON(payload []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrApprovalPaginationCursorInvalid
	}
	want := map[string]bool{"v": true, "kid": true, "snapshot_id": true, "subject_id": true, "authorization_version": true, "resource": true, "scope_digest": true, "filter_digest": true, "sort": true, "limit": true, "position": true, "position_key": true, "as_of": true, "expires_at": true}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, tokenErr := decoder.Token()
		key, ok := keyToken.(string)
		if tokenErr != nil || !ok || !want[key] || seen[key] {
			return ErrApprovalPaginationCursorInvalid
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return ErrApprovalPaginationCursorInvalid
		}
	}
	end, endErr := decoder.Token()
	if endErr != nil || end != json.Delim('}') || len(seen) != len(want) {
		return ErrApprovalPaginationCursorInvalid
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return ErrApprovalPaginationCursorInvalid
	}
	return nil
}

func reserveApprovalPaginationReads(ctx context.Context, tx pgx.Tx, actorID uuid.UUID, sourceIP string, now time.Time) error {
	if err := reserveApprovalPaginationRate(ctx, tx, "read:subject:"+actorID.String()+":"+approvalPaginationResource, ApprovalReadSubjectPerMinute, now, ErrApprovalPaginationRateLimited); err != nil {
		return err
	}
	ip := net.ParseIP(strings.TrimSpace(sourceIP))
	if ip == nil {
		return ErrApprovalPaginationFilterInvalid
	}
	return reserveApprovalPaginationRate(ctx, tx, "read:ip:"+ip.String(), ApprovalReadIPPerMinute, now, ErrApprovalPaginationRateLimited)
}

func reserveApprovalPaginationRate(ctx context.Context, tx pgx.Tx, key string, limit int, now time.Time, cause error) error {
	window := now.UTC().Truncate(time.Minute)
	var count int
	err := tx.QueryRow(ctx, `INSERT INTO approval_pagination_rate_windows(rate_key,window_start,request_count) VALUES($1,$2,1) ON CONFLICT(rate_key,window_start) DO UPDATE SET request_count=approval_pagination_rate_windows.request_count+1 RETURNING request_count`, key, window).Scan(&count)
	if err != nil {
		return err
	}
	if count > limit {
		retry := int(window.Add(time.Minute).Sub(now).Seconds()) + 1
		if retry < 1 {
			retry = 1
		}
		if retry > 60 {
			retry = 60
		}
		return approvalPaginationRateError{cause, retry}
	}
	return nil
}

func expireApprovalPaginationSnapshot(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, now time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO approval_pagination_snapshot_tombstones(snapshot_id,purge_after) VALUES($1,$2) ON CONFLICT(snapshot_id) DO UPDATE SET purge_after=GREATEST(approval_pagination_snapshot_tombstones.purge_after,EXCLUDED.purge_after)`, snapshotID, now.Add(ApprovalSnapshotTombstoneTTL)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM approval_pagination_snapshots WHERE id=$1`, snapshotID)
	return err
}

func cleanupApprovalPaginationTx(ctx context.Context, tx pgx.Tx, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id FROM approval_pagination_snapshots WHERE expires_at<=$1 ORDER BY expires_at,id FOR UPDATE`, now)
	if err != nil {
		return err
	}
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			return scanErr
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		if err = expireApprovalPaginationSnapshot(ctx, tx, id, now); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM approval_pagination_snapshot_tombstones WHERE purge_after<=$1`, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM approval_pagination_signing_keys WHERE last_signed_at+make_interval(secs=>$2)<=$1`, now, int64(ApprovalSnapshotKeyRetention/time.Second)); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM approval_pagination_rate_windows WHERE window_start<$1`, now.Add(-2*time.Minute))
	return err
}

func (r *PostgresRepository) CleanupApprovalPagination(ctx context.Context) error {
	defer func() {
		metricsContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = r.refreshApprovalPaginationMetrics(metricsContext)
	}()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('account-approval-pagination-capacity'))`); err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if err = cleanupApprovalPaginationTx(ctx, tx, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) ApprovalPaginationCapacity(ctx context.Context) (ApprovalPaginationCapacity, error) {
	var result ApprovalPaginationCapacity
	var lagSeconds float64
	err := r.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE expires_at>transaction_timestamp()),COALESCE(sum(member_count) FILTER(WHERE expires_at>transaction_timestamp()),0),COALESCE(sum(member_bytes) FILTER(WHERE expires_at>transaction_timestamp()),0),count(*) FILTER(WHERE expires_at<=transaction_timestamp()),COALESCE(EXTRACT(EPOCH FROM transaction_timestamp()-min(expires_at) FILTER(WHERE expires_at<=transaction_timestamp())),0) FROM approval_pagination_snapshots`).Scan(&result.ActiveSnapshots, &result.ActiveMembers, &result.MemberBytes, &result.CleanupBacklog, &lagSeconds)
	if err != nil {
		return result, err
	}
	if err = r.pool.QueryRow(ctx, `SELECT pg_total_relation_size('approval_pagination_snapshots')+pg_total_relation_size('approval_pagination_snapshot_members')+pg_total_relation_size('approval_pagination_snapshot_tombstones')`).Scan(&result.StorageBytes); err != nil {
		return result, err
	}
	result.OldestCleanupLag = time.Duration(lagSeconds * float64(time.Second))
	maximum := r.approvalPagination.maximumStorageBytes
	if maximum <= 0 {
		maximum = ApprovalSnapshotMaxStorageBytes
	}
	peak := max(result.ActiveSnapshots*100/ApprovalSnapshotMaxGlobal, result.ActiveMembers*100/ApprovalSnapshotMaxGlobalMembers, result.StorageBytes*100/maximum)
	result.Warning = peak >= 70 || result.OldestCleanupLag > 5*time.Minute
	result.Critical = peak >= 85
	return result, nil
}

type ApprovalPaginationCleanupWorker struct {
	repository *PostgresRepository
	interval   time.Duration
}

func NewApprovalPaginationCleanupWorker(repository *PostgresRepository, interval time.Duration) *ApprovalPaginationCleanupWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	return &ApprovalPaginationCleanupWorker{repository: repository, interval: interval}
}

func (worker *ApprovalPaginationCleanupWorker) Run(ctx context.Context) {
	if worker == nil || worker.repository == nil {
		return
	}
	ticker := time.NewTicker(worker.interval)
	defer ticker.Stop()
	for {
		if err := worker.repository.CleanupApprovalPagination(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "Account approval pagination cleanup failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func approvalPaginationContext(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc, error) {
	if err := parent.Err(); err != nil {
		return nil, nil, err
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

func approvalPaginationRetry(ctx context.Context, attempt int) error {
	var random [1]byte
	if _, err := cryptorand.Read(random[:]); err != nil {
		return err
	}
	delay := time.Duration(random[0]%byte(10*(attempt+1)+1)) * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func approvalPaginationSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}
func approvalPaginationBuildError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrApprovalPaginationSnapshotUnavailable
	}
	return err
}
func approvalPaginationReadError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrApprovalPaginationQueryTimeout
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return ErrApprovalPaginationQueryTimeout
	}
	return err
}

func approvalPaginationOperationError(err error, continuation bool) error {
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return err
	}
	if continuation {
		return ErrApprovalPaginationQueryTimeout
	}
	return ErrApprovalPaginationSnapshotUnavailable
}
