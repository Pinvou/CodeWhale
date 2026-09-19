package billing

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	adminSnapshotTTL             = 15 * time.Minute
	adminSnapshotStatementLimit  = 10 * time.Second
	adminSnapshotMaxMembers      = 1_000_000
	adminSnapshotMaxGlobal       = 64
	adminSnapshotMaxGlobalMember = 20_000_000
	adminSnapshotSubjectPerMin   = 6
	adminSnapshotGlobalPerMin    = 60
)

var (
	ErrPaginationCursorInvalid       = errors.New("pagination cursor is invalid")
	ErrPaginationCursorExpired       = errors.New("pagination cursor is expired")
	ErrPaginationFilterRequired      = errors.New("pagination filter is required")
	ErrPaginationSnapshotLimited     = errors.New("pagination snapshot capacity is limited")
	ErrPaginationSnapshotUnavailable = errors.New("pagination snapshot storage is unavailable")
)

const (
	adminPageWallets             = "wallets"
	adminPageWalletLedger        = "wallet-ledger"
	adminPageReceivables         = "receivables"
	adminPagePaymentOrders       = "payment-orders"
	adminPageRefunds             = "refunds"
	adminPageAdjustments         = "adjustments"
	adminPageUsageHolds          = "usage-holds"
	adminPageLatePaymentReviews  = "late-payment-reviews"
	adminPagePaymentAnomalies    = "payment-anomalies"
	adminPageReconciliationRuns  = "reconciliation-runs"
	adminPageReconciliationCases = "reconciliation-cases"
)

type AdminPaginationRequest struct {
	SubjectID uuid.UUID
	Resource  string
	Scope     string
	Filters   map[string]string
	Cursor    string
	Limit     int
}

type AdminPaginationPage struct {
	ResourceIDs  []string
	NextCursor   string
	SnapshotAsOf time.Time
}

type adminPaginationRepository interface {
	AdminPaginationPage(context.Context, AdminPaginationRequest) (AdminPaginationPage, error)
}

func normalizedAdminPageRequest(w http.ResponseWriter, r *http.Request, resource, scope string) (AdminPaginationRequest, bool) {
	limit, err := parseAdminPageLimit(r.URL.Query().Get("limit"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", "limit must be between 1 and 200", httpserver.RequestID(r.Context()))
		return AdminPaginationRequest{}, false
	}
	request := AdminPaginationRequest{
		SubjectID: adminActor(adminPrincipal(r)),
		Resource:  resource,
		Scope:     strings.TrimSpace(scope),
		Filters:   map[string]string{},
		Cursor:    strings.TrimSpace(r.URL.Query().Get("cursor")),
		Limit:     limit,
	}
	if request.SubjectID == uuid.Nil {
		writeAdminPaginationError(w, r, ErrPaginationCursorInvalid)
		return AdminPaginationRequest{}, false
	}
	definitions, ok := adminPaginationFilters[resource]
	if !ok {
		writeAdminPaginationError(w, r, ErrPaginationCursorInvalid)
		return AdminPaginationRequest{}, false
	}
	for name, definition := range definitions {
		value := strings.TrimSpace(r.URL.Query().Get(name))
		if definition.upper {
			value = strings.ToUpper(value)
		}
		if len(value) > definition.maxLength {
			api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", name+" is too long", httpserver.RequestID(r.Context()))
			return AdminPaginationRequest{}, false
		}
		if value != "" && len(definition.values) > 0 && !definition.values[value] {
			api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", name+" is invalid", httpserver.RequestID(r.Context()))
			return AdminPaginationRequest{}, false
		}
		if value != "" && definition.uuid {
			parsed, err := uuid.Parse(value)
			if err != nil {
				api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", name+" is invalid", httpserver.RequestID(r.Context()))
				return AdminPaginationRequest{}, false
			}
			value = parsed.String()
		}
		if value != "" && definition.rfc3339 {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", name+" must use RFC3339", httpserver.RequestID(r.Context()))
				return AdminPaginationRequest{}, false
			}
			value = parsed.UTC().Format(time.RFC3339Nano)
		}
		if value != "" && definition.date {
			parsed, err := time.Parse("2006-01-02", value)
			if err != nil {
				api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", name+" must use YYYY-MM-DD", httpserver.RequestID(r.Context()))
				return AdminPaginationRequest{}, false
			}
			value = parsed.Format("2006-01-02")
		}
		if value != "" && definition.identifier && !adminFilterIdentifier.MatchString(value) {
			api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", name+" is invalid", httpserver.RequestID(r.Context()))
			return AdminPaginationRequest{}, false
		}
		request.Filters[name] = value
	}
	if resource == adminPageLatePaymentReviews {
		if request.Filters["created_from"] == "" {
			request.Filters["created_from"] = request.Filters["entered_from"]
		}
		if request.Filters["created_to"] == "" {
			request.Filters["created_to"] = request.Filters["entered_to"]
		}
	}
	if !validAdminTimeRange(request.Filters, "created_from", "created_to") ||
		!validAdminTimeRange(request.Filters, "entered_from", "entered_to") ||
		!validAdminDateRange(request.Filters, "business_date_from", "business_date_to") {
		api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", "filter range must be a non-empty half-open interval of at most 366 days", httpserver.RequestID(r.Context()))
		return AdminPaginationRequest{}, false
	}
	return request, true
}

type adminFilterDefinition struct {
	upper      bool
	uuid       bool
	rfc3339    bool
	date       bool
	identifier bool
	maxLength  int
	values     map[string]bool
}

var adminFilterIdentifier = regexp.MustCompile(`^[A-Z0-9_]{1,32}$`)

func enumFilter(values ...string) adminFilterDefinition {
	allowed := make(map[string]bool, len(values))
	for _, value := range values {
		allowed[value] = true
	}
	return adminFilterDefinition{upper: true, maxLength: 80, values: allowed}
}

var adminPaginationFilters = map[string]map[string]adminFilterDefinition{
	adminPageWallets: {
		"owner_type": enumFilter("PERSONAL", "ORGANIZATION", "ORG_UNIT"),
		"status":     enumFilter("ACTIVE", "FROZEN", "CLOSED"),
		"q":          {maxLength: 160},
	},
	adminPageWalletLedger: {
		"entry_type":         enumFilter("RECHARGE", "USAGE", "REFUND", "REVERSAL", "ADJUSTMENT", "EXPIRY", "RECEIVABLE", "CHARGEBACK", "REPAIR_CREDIT", "FORCED_REFUND"),
		"business_reference": {maxLength: 160},
		"created_from":       {rfc3339: true, maxLength: 64},
		"created_to":         {rfc3339: true, maxLength: 64},
	},
	adminPageReceivables: {
		"wallet_id":    {uuid: true, maxLength: 64},
		"status":       enumFilter("OPEN", "PARTIALLY_SETTLED", "SETTLED", "WRITTEN_OFF"),
		"source_type":  enumFilter("USAGE_OVERAGE", "CHARGEBACK", "FORCED_REFUND"),
		"created_from": {rfc3339: true, maxLength: 64},
		"created_to":   {rfc3339: true, maxLength: 64},
	},
	adminPagePaymentOrders: {
		"q":            {maxLength: 160},
		"status":       enumFilter("CREATED", "PENDING", "PAID", "PAID_LATE", "FAILED", "CLOSED", "EXPIRED", "MANUAL_REVIEW"),
		"provider_id":  {maxLength: 120},
		"channel":      {upper: true, identifier: true, maxLength: 32},
		"created_from": {rfc3339: true, maxLength: 64},
		"created_to":   {rfc3339: true, maxLength: 64},
	},
	adminPageRefunds: {
		"q":            {maxLength: 160},
		"status":       enumFilter("REQUESTED", "SUCCEEDED", "FAILED", "MANUAL_REVIEW", "REVIEW_REQUIRED", "RESOLVED"),
		"provider_id":  {maxLength: 120},
		"created_from": {rfc3339: true, maxLength: 64},
		"created_to":   {rfc3339: true, maxLength: 64},
	},
	adminPageAdjustments: {
		"wallet_id":    {uuid: true, maxLength: 64},
		"status":       enumFilter("PENDING_APPROVAL", "APPROVED", "REJECTED", "POSTED"),
		"requested_by": {uuid: true, maxLength: 64},
		"created_from": {rfc3339: true, maxLength: 64},
		"created_to":   {rfc3339: true, maxLength: 64},
	},
	adminPageUsageHolds: {
		"wallet_id":    {uuid: true, maxLength: 64},
		"status":       enumFilter("ACTIVE", "COMMITTED", "RELEASED", "EXPIRED", "REVIEW"),
		"model_id":     {maxLength: 160},
		"created_from": {rfc3339: true, maxLength: 64},
		"created_to":   {rfc3339: true, maxLength: 64},
	},
	adminPageLatePaymentReviews: {
		"q":                  {maxLength: 160},
		"status":             enumFilter("REVIEW_REQUIRED_LATE_PAYMENT", "PROVIDER_BLIND", "ARCHIVED_CONFIRMED_UNPAID", "RESOLVED_PAID"),
		"severity":           enumFilter("P0", "P1", "P2"),
		"owner_id":           {uuid: true, maxLength: 64},
		"provider_id":        {maxLength: 120},
		"created_from":       {rfc3339: true, maxLength: 64},
		"created_to":         {rfc3339: true, maxLength: 64},
		"entered_from":       {rfc3339: true, maxLength: 64},
		"entered_to":         {rfc3339: true, maxLength: 64},
		"next_review_before": {rfc3339: true, maxLength: 64},
		"overdue":            {maxLength: 5, values: map[string]bool{"true": true, "false": true}},
	},
	adminPagePaymentAnomalies: {
		"q":            {maxLength: 160},
		"anomaly_type": enumFilter("CHARGEBACK", "FORCED_REFUND", "DUPLICATE_PAYMENT", "LATE_PAYMENT_CONFLICT", "REFUND_FAILURE", "PAYMENT_FACT_CONFLICT", "ILLEGAL_TRANSITION"),
		"status":       enumFilter("OPEN", "RESOLVED"),
		"severity":     enumFilter("P0", "P1", "P2"),
		"owner_id":     {uuid: true, maxLength: 64},
		"provider_id":  {maxLength: 120},
		"created_from": {rfc3339: true, maxLength: 64},
		"created_to":   {rfc3339: true, maxLength: 64},
	},
	adminPageReconciliationRuns: {
		"provider_id":        {maxLength: 120},
		"channel":            {upper: true, identifier: true, maxLength: 32},
		"status":             enumFilter("RUNNING", "COMPLETED", "FAILED"),
		"business_date_from": {date: true, maxLength: 16},
		"business_date_to":   {date: true, maxLength: 16},
	},
	adminPageReconciliationCases: {
		"status":   enumFilter("OPEN", "IN_REVIEW", "RESOLVED", "IGNORED"),
		"severity": enumFilter("P0", "P1", "P2"),
		"owner_id": {uuid: true, maxLength: 64},
		"case_type": enumFilter(
			"PROVIDER_PAID_INTERNAL_MISSING", "INTERNAL_PAID_PROVIDER_MISSING", "AMOUNT_MISMATCH", "DUPLICATE_TRANSACTION",
			"PROVIDER_REFUND_INTERNAL_MISSING", "INTERNAL_REFUND_PROVIDER_MISSING", "REFUND_AMOUNT_MISMATCH",
			"PROVIDER_CHARGEBACK_INTERNAL_MISSING", "INTERNAL_CHARGEBACK_PROVIDER_MISSING",
			"PAYMENT_LEDGER_MISSING", "REFUND_LEDGER_MISSING", "CHARGEBACK_LEDGER_MISSING", "UNSUPPORTED_PROVIDER_ENTRY",
		),
		"created_from": {rfc3339: true, maxLength: 64},
		"created_to":   {rfc3339: true, maxLength: 64},
	},
}

func validAdminTimeRange(filters map[string]string, fromKey, toKey string) bool {
	from, to := filters[fromKey], filters[toKey]
	if from == "" || to == "" {
		return true
	}
	start, startErr := time.Parse(time.RFC3339Nano, from)
	end, endErr := time.Parse(time.RFC3339Nano, to)
	return startErr == nil && endErr == nil && start.Before(end) && end.Sub(start) <= 366*24*time.Hour
}

func validAdminDateRange(filters map[string]string, fromKey, toKey string) bool {
	from, to := filters[fromKey], filters[toKey]
	if from == "" || to == "" {
		return true
	}
	start, startErr := time.Parse("2006-01-02", from)
	end, endErr := time.Parse("2006-01-02", to)
	return startErr == nil && endErr == nil && start.Before(end) && end.Sub(start) <= 366*24*time.Hour
}

func adminPaginationDigest(value any) string {
	payload, _ := json.Marshal(value)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

type adminCursorPayload struct {
	SnapshotID uuid.UUID
	Position   int
	Signature  []byte
}

func parseAdminCursor(cursor string) (adminCursorPayload, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) != 56 {
		return adminCursorPayload{}, ErrPaginationCursorInvalid
	}
	position := binary.BigEndian.Uint64(raw[16:24])
	if position > adminSnapshotMaxMembers {
		return adminCursorPayload{}, ErrPaginationCursorInvalid
	}
	return adminCursorPayload{SnapshotID: uuid.UUID(raw[:16]), Position: int(position), Signature: raw[24:]}, nil
}

func signedAdminCursor(snapshotID uuid.UUID, position int, secret []byte) (string, error) {
	if snapshotID == uuid.Nil || position < 0 || position > adminSnapshotMaxMembers || len(secret) != 32 {
		return "", ErrPaginationCursorInvalid
	}
	payload := make([]byte, 24, 56)
	copy(payload[:16], snapshotID[:])
	binary.BigEndian.PutUint64(payload[16:24], uint64(position))
	signer := hmac.New(sha256.New, secret)
	_, _ = signer.Write(payload)
	payload = append(payload, signer.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func validAdminCursorSignature(cursor adminCursorPayload, secret []byte) bool {
	if len(secret) != 32 || len(cursor.Signature) != sha256.Size {
		return false
	}
	payload := make([]byte, 24)
	copy(payload[:16], cursor.SnapshotID[:])
	binary.BigEndian.PutUint64(payload[16:24], uint64(cursor.Position))
	signer := hmac.New(sha256.New, secret)
	_, _ = signer.Write(payload)
	return hmac.Equal(cursor.Signature, signer.Sum(nil))
}

func (r *PostgresRepository) AdminPaginationPage(ctx context.Context, request AdminPaginationRequest) (AdminPaginationPage, error) {
	if request.SubjectID == uuid.Nil || request.Limit < 1 || request.Limit > 200 {
		return AdminPaginationPage{}, ErrPaginationCursorInvalid
	}
	scopeDigest := adminPaginationDigest(request.Scope)
	filterDigest := adminPaginationDigest(request.Filters)
	if request.Cursor != "" {
		return r.readAdminPaginationPage(ctx, request, scopeDigest, filterDigest)
	}
	if err := r.reserveAdminSnapshotCreation(ctx, request.SubjectID); err != nil {
		r.paginationMetrics.recordCreation(request.Resource, err)
		return AdminPaginationPage{}, err
	}
	page, err := r.createAdminPaginationPage(ctx, request, scopeDigest, filterDigest)
	r.paginationMetrics.recordCreation(request.Resource, err)
	return page, err
}

func (r *PostgresRepository) reserveAdminSnapshotCreation(ctx context.Context, subjectID uuid.UUID) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		lastErr = r.reserveAdminSnapshotCreationOnce(ctx, subjectID)
		if !isAdminPaginationSerializationFailure(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

func (r *PostgresRepository) reserveAdminSnapshotCreationOnce(ctx context.Context, subjectID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPaginationSnapshotUnavailable, err)
	}
	defer tx.Rollback(ctx)
	now := r.now().UTC()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('billing-admin-pagination-rate'))`); err != nil {
		return fmt.Errorf("%w: %w", ErrPaginationSnapshotUnavailable, err)
	}
	_, _ = tx.Exec(ctx, `DELETE FROM admin_pagination_creation_windows WHERE window_start<$1`, now.Add(-2*time.Minute).Truncate(time.Minute))
	window := now.Truncate(time.Minute)
	for _, quota := range []struct {
		key string
		max int
	}{{"subject:" + subjectID.String(), adminSnapshotSubjectPerMin}, {"global", adminSnapshotGlobalPerMin}} {
		var count int
		err = tx.QueryRow(ctx, `INSERT INTO admin_pagination_creation_windows(rate_key,window_start,creation_count) VALUES($1,$2,1)
			ON CONFLICT(rate_key,window_start) DO UPDATE SET creation_count=admin_pagination_creation_windows.creation_count+1
			RETURNING creation_count`, quota.key, window).Scan(&count)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrPaginationSnapshotUnavailable, err)
		}
		if count > quota.max {
			return ErrPaginationSnapshotLimited
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrPaginationSnapshotUnavailable, err)
	}
	return nil
}

func (r *PostgresRepository) createAdminPaginationPage(ctx context.Context, request AdminPaginationRequest, scopeDigest, filterDigest string) (AdminPaginationPage, error) {
	var page AdminPaginationPage
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		page, lastErr = r.createAdminPaginationPageOnce(ctx, request, scopeDigest, filterDigest)
		if !isAdminPaginationSerializationFailure(lastErr) {
			return page, lastErr
		}
	}
	return AdminPaginationPage{}, lastErr
}

func (r *PostgresRepository) createAdminPaginationPageOnce(ctx context.Context, request AdminPaginationRequest, scopeDigest, filterDigest string) (AdminPaginationPage, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AdminPaginationPage{}, fmt.Errorf("%w: %w", ErrPaginationSnapshotUnavailable, err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL statement_timeout='10s'`); err != nil {
		return AdminPaginationPage{}, fmt.Errorf("%w: %w", ErrPaginationSnapshotUnavailable, err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('billing-admin-pagination-capacity'))`); err != nil {
		return AdminPaginationPage{}, fmt.Errorf("%w: %w", ErrPaginationSnapshotUnavailable, err)
	}
	now := r.now().UTC()
	if err = cleanupAdminPaginationTx(ctx, tx, now); err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	var replacedID uuid.UUID
	var replacedSecret []byte
	err = tx.QueryRow(ctx, `SELECT id,cursor_secret FROM admin_pagination_snapshots WHERE subject_id=$1 AND resource_type=$2`, request.SubjectID, request.Resource).Scan(&replacedID, &replacedSecret)
	if err == nil {
		if err = expireAdminPaginationSnapshot(ctx, tx, replacedID, replacedSecret, now); err != nil {
			return AdminPaginationPage{}, paginationStorageError(err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	var subjectSnapshots int64
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM admin_pagination_snapshots WHERE subject_id=$1 AND expires_at>$2`, request.SubjectID, now).Scan(&subjectSnapshots); err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	if subjectSnapshots >= 2 {
		var oldestID uuid.UUID
		var oldestSecret []byte
		err = tx.QueryRow(ctx, `SELECT id,cursor_secret FROM admin_pagination_snapshots WHERE subject_id=$1 AND expires_at>$2 ORDER BY created_at,id LIMIT 1`, request.SubjectID, now).Scan(&oldestID, &oldestSecret)
		if err != nil {
			return AdminPaginationPage{}, paginationStorageError(err)
		}
		if err = expireAdminPaginationSnapshot(ctx, tx, oldestID, oldestSecret, now); err != nil {
			return AdminPaginationPage{}, paginationStorageError(err)
		}
	}
	var activeSnapshots, activeMembers int64
	if err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(member_count),0) FROM admin_pagination_snapshots WHERE expires_at>$1`, now).Scan(&activeSnapshots, &activeMembers); err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	if activeSnapshots >= adminSnapshotMaxGlobal {
		return AdminPaginationPage{}, ErrPaginationSnapshotLimited
	}
	var storageBytes int64
	if err = tx.QueryRow(ctx, adminPaginationStorageQuery).Scan(&storageBytes); err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	if storageBytes >= r.adminPaginationMaxStorageBytes() {
		return AdminPaginationPage{}, ErrPaginationSnapshotLimited
	}
	snapshotID := uuid.New()
	cursorSecret := make([]byte, 32)
	if _, err = rand.Read(cursorSecret); err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	expiresAt := now.Add(adminSnapshotTTL)
	if _, err = tx.Exec(ctx, `INSERT INTO admin_pagination_snapshots(id,subject_id,resource_type,scope_digest,filter_digest,cursor_secret,snapshot_as_of,expires_at,member_count,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,0,$7)`, snapshotID, request.SubjectID, request.Resource, scopeDigest, filterDigest, cursorSecret, now, expiresAt); err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	resourceSQL, args, queryErr := adminSnapshotResourceQuery(request)
	if queryErr != nil {
		return AdminPaginationPage{}, queryErr
	}
	insertSQL := `INSERT INTO admin_pagination_snapshot_members(snapshot_id,position,resource_id)
		SELECT $1,row_number() OVER (ORDER BY sort_at DESC,sort_id DESC,resource_id DESC)-1,resource_id FROM (` + resourceSQL + ` LIMIT 1000001) snapshot_members`
	if _, err = tx.Exec(ctx, insertSQL, append([]any{snapshotID}, args...)...); err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	var memberCount int64
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM admin_pagination_snapshot_members WHERE snapshot_id=$1`, snapshotID).Scan(&memberCount); err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	if memberCount > adminSnapshotMaxMembers {
		return AdminPaginationPage{}, ErrPaginationFilterRequired
	}
	if activeMembers+memberCount > adminSnapshotMaxGlobalMember {
		return AdminPaginationPage{}, ErrPaginationSnapshotLimited
	}
	if err = tx.QueryRow(ctx, adminPaginationStorageQuery).Scan(&storageBytes); err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	if storageBytes > r.adminPaginationMaxStorageBytes() {
		return AdminPaginationPage{}, ErrPaginationSnapshotLimited
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_pagination_snapshots SET member_count=$2 WHERE id=$1`, snapshotID, memberCount); err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	page, err := readAdminSnapshotMembers(ctx, tx, snapshotID, cursorSecret, 0, request.Limit, now)
	if err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	return page, nil
}

func expireAdminPaginationSnapshot(ctx context.Context, tx pgx.Tx, snapshotID uuid.UUID, cursorSecret []byte, now time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO admin_pagination_snapshot_tombstones(snapshot_id,cursor_secret,purge_after) VALUES($1,$2,$3)
		ON CONFLICT(snapshot_id) DO UPDATE SET cursor_secret=EXCLUDED.cursor_secret,purge_after=EXCLUDED.purge_after`, snapshotID, cursorSecret, now.Add(24*time.Hour)); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM admin_pagination_snapshots WHERE id=$1`, snapshotID)
	return err
}

func (r *PostgresRepository) adminPaginationMaxStorageBytes() int64 {
	if r.paginationMaxStorageBytes > 0 {
		return r.paginationMaxStorageBytes
	}
	return adminSnapshotMaxStorageBytes
}

func (r *PostgresRepository) readAdminPaginationPage(ctx context.Context, request AdminPaginationRequest, scopeDigest, filterDigest string) (AdminPaginationPage, error) {
	cursor, err := parseAdminCursor(request.Cursor)
	if err != nil {
		return AdminPaginationPage{}, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	defer tx.Rollback(ctx)
	var subjectID uuid.UUID
	var resource, storedScope, storedFilter string
	var cursorSecret []byte
	var snapshotAsOf, expiresAt time.Time
	err = tx.QueryRow(ctx, `SELECT subject_id,resource_type,scope_digest,filter_digest,cursor_secret,snapshot_as_of,expires_at
		FROM admin_pagination_snapshots WHERE id=$1`, cursor.SnapshotID).
		Scan(&subjectID, &resource, &storedScope, &storedFilter, &cursorSecret, &snapshotAsOf, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var tombstoneSecret []byte
		tombstoneErr := tx.QueryRow(ctx, `SELECT cursor_secret FROM admin_pagination_snapshot_tombstones WHERE snapshot_id=$1 AND purge_after>$2`, cursor.SnapshotID, r.now().UTC()).Scan(&tombstoneSecret)
		if tombstoneErr == nil && validAdminCursorSignature(cursor, tombstoneSecret) {
			return AdminPaginationPage{}, ErrPaginationCursorExpired
		}
		if tombstoneErr != nil && !errors.Is(tombstoneErr, pgx.ErrNoRows) {
			return AdminPaginationPage{}, paginationStorageError(tombstoneErr)
		}
		return AdminPaginationPage{}, ErrPaginationCursorInvalid
	}
	if err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	if !validAdminCursorSignature(cursor, cursorSecret) {
		return AdminPaginationPage{}, ErrPaginationCursorInvalid
	}
	if !expiresAt.After(r.now().UTC()) {
		return AdminPaginationPage{}, ErrPaginationCursorExpired
	}
	if subjectID != request.SubjectID || resource != request.Resource || storedScope != scopeDigest || storedFilter != filterDigest {
		return AdminPaginationPage{}, ErrPaginationCursorInvalid
	}
	page, err := readAdminSnapshotMembers(ctx, tx, cursor.SnapshotID, cursorSecret, cursor.Position, request.Limit, snapshotAsOf)
	if err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return AdminPaginationPage{}, paginationStorageError(err)
	}
	return page, nil
}

type adminPageTx interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readAdminSnapshotMembers(ctx context.Context, tx adminPageTx, snapshotID uuid.UUID, cursorSecret []byte, position, limit int, snapshotAsOf time.Time) (AdminPaginationPage, error) {
	rows, err := tx.Query(ctx, `SELECT resource_id FROM admin_pagination_snapshot_members WHERE snapshot_id=$1 AND position>=$2 ORDER BY position LIMIT $3`, snapshotID, position, limit+1)
	if err != nil {
		return AdminPaginationPage{}, err
	}
	defer rows.Close()
	ids := make([]string, 0, limit+1)
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
	page := AdminPaginationPage{SnapshotAsOf: snapshotAsOf, ResourceIDs: ids}
	if len(ids) > limit {
		page.ResourceIDs = ids[:limit]
		cursor, err := signedAdminCursor(snapshotID, position+limit, cursorSecret)
		if err != nil {
			return AdminPaginationPage{}, err
		}
		page.NextCursor = cursor
	}
	return page, nil
}

func paginationStorageError(err error) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "57014" {
		return fmt.Errorf("%w: query timeout", ErrPaginationSnapshotUnavailable)
	}
	return fmt.Errorf("%w: %w", ErrPaginationSnapshotUnavailable, err)
}

func isAdminPaginationSerializationFailure(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "40001"
}

func cleanupAdminPaginationTx(ctx context.Context, tx pgx.Tx, now time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO admin_pagination_snapshot_tombstones(snapshot_id,cursor_secret,purge_after)
		SELECT id,cursor_secret,$2 FROM admin_pagination_snapshots WHERE expires_at<=$1
		ON CONFLICT(snapshot_id) DO UPDATE SET cursor_secret=EXCLUDED.cursor_secret,purge_after=EXCLUDED.purge_after`, now, now.Add(24*time.Hour)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM admin_pagination_snapshots WHERE expires_at<=$1`, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM admin_pagination_snapshot_tombstones WHERE purge_after<=$1`, now)
	return err
}

func (r *PostgresRepository) CleanupAdminPagination(ctx context.Context) error {
	defer r.markAdminPaginationMetricsStale()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('billing-admin-pagination-capacity'))`); err != nil {
		return err
	}
	if err = cleanupAdminPaginationTx(ctx, tx, r.now().UTC()); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

type AdminPaginationCleanupWorker struct {
	repository interface{ CleanupAdminPagination(context.Context) error }
	interval   time.Duration
}

func NewAdminPaginationCleanupWorker(repository interface{ CleanupAdminPagination(context.Context) error }, interval time.Duration) *AdminPaginationCleanupWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	return &AdminPaginationCleanupWorker{repository: repository, interval: interval}
}

func (worker *AdminPaginationCleanupWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(worker.interval)
	defer ticker.Stop()
	for {
		if err := worker.repository.CleanupAdminPagination(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "admin pagination cleanup failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func filterArg(filters map[string]string, name string) any {
	value := filters[name]
	if value == "" {
		return ""
	}
	return value
}

func adminSnapshotResourceQuery(request AdminPaginationRequest) (string, []any, error) {
	f := request.Filters
	// $1 belongs to snapshot_id in the outer INSERT, so resource arguments start at $2.
	switch request.Resource {
	case adminPageWallets:
		return `SELECT id::text resource_id,created_at sort_at,id sort_id FROM wallets WHERE ($2='' OR owner_type=$2) AND ($3='' OR status=$3) AND ($4='' OR id::text ILIKE '%'||$4||'%' OR owner_id::text ILIKE '%'||$4||'%') ORDER BY created_at DESC,id DESC`, []any{filterArg(f, "owner_type"), filterArg(f, "status"), filterArg(f, "q")}, nil
	case adminPageWalletLedger:
		walletID, err := uuid.Parse(request.Scope)
		if err != nil {
			return "", nil, ErrPaginationCursorInvalid
		}
		return `SELECT posting.id::text resource_id,posting.created_at sort_at,posting.id sort_id FROM ledger_postings posting JOIN ledger_accounts account ON account.id=posting.ledger_account_id JOIN journal_entries journal ON journal.id=posting.journal_entry_id WHERE account.wallet_id=$2 AND ($3='' OR journal.entry_type=$3) AND ($4='' OR journal.business_reference=$4) AND ($5='' OR posting.created_at>=NULLIF($5,'')::timestamptz) AND ($6='' OR posting.created_at<NULLIF($6,'')::timestamptz) ORDER BY posting.created_at DESC,posting.id DESC`, []any{walletID, filterArg(f, "entry_type"), filterArg(f, "business_reference"), filterArg(f, "created_from"), filterArg(f, "created_to")}, nil
	case adminPageReceivables:
		return `SELECT id::text resource_id,created_at sort_at,id sort_id FROM receivables WHERE ($2='' OR wallet_id=NULLIF($2,'')::uuid) AND ($3='' OR status=$3) AND ($4='' OR source_type=$4) AND ($5='' OR created_at>=NULLIF($5,'')::timestamptz) AND ($6='' OR created_at<NULLIF($6,'')::timestamptz) ORDER BY created_at DESC,id DESC`, []any{filterArg(f, "wallet_id"), filterArg(f, "status"), filterArg(f, "source_type"), filterArg(f, "created_from"), filterArg(f, "created_to")}, nil
	case adminPagePaymentOrders:
		return `SELECT payment.id::text resource_id,payment.created_at sort_at,payment.id sort_id FROM payment_orders payment WHERE ($2='' OR payment.id::text ILIKE '%'||$2||'%' OR payment.merchant_order_no ILIKE '%'||$2||'%' OR COALESCE(payment.primary_provider_transaction_id,'') ILIKE '%'||$2||'%' OR COALESCE(payment.mobile_e164,'') ILIKE '%'||$2||'%' OR COALESCE(payment.provider_sku,'') ILIKE '%'||$2||'%' OR EXISTS(SELECT 1 FROM payment_transactions transaction WHERE transaction.payment_order_id=payment.id AND transaction.provider_transaction_id ILIKE '%'||$2||'%')) AND ($3='' OR payment.status=$3) AND ($4='' OR payment.provider_id=$4) AND ($5='' OR payment.channel=$5) AND ($6='' OR payment.created_at>=NULLIF($6,'')::timestamptz) AND ($7='' OR payment.created_at<NULLIF($7,'')::timestamptz) ORDER BY payment.created_at DESC,payment.id DESC`, []any{filterArg(f, "q"), filterArg(f, "status"), filterArg(f, "provider_id"), filterArg(f, "channel"), filterArg(f, "created_from"), filterArg(f, "created_to")}, nil
	case adminPageRefunds:
		return `SELECT resource_id,created_at sort_at,id sort_id FROM (SELECT 'NORMAL:'||refund.id::text resource_id,refund.created_at,refund.id,payment.provider_id,refund.status,refund.payment_order_id::text order_id,COALESCE(refund.channel_refund_id,'') provider_refund_id FROM refunds refund JOIN payment_orders payment ON payment.id=refund.payment_order_id UNION ALL SELECT 'FORCED:'||forced.id::text,forced.created_at,forced.id,payment.provider_id,forced.status,forced.payment_order_id::text,COALESCE(forced.provider_refund_id,'') FROM forced_refund_cases forced JOIN payment_orders payment ON payment.id=forced.payment_order_id) operation WHERE ($2='' OR operation.resource_id ILIKE '%'||$2||'%' OR operation.order_id ILIKE '%'||$2||'%' OR operation.provider_refund_id ILIKE '%'||$2||'%') AND ($3='' OR operation.status=$3) AND ($4='' OR operation.provider_id=$4) AND ($5='' OR operation.created_at>=NULLIF($5,'')::timestamptz) AND ($6='' OR operation.created_at<NULLIF($6,'')::timestamptz) ORDER BY operation.created_at DESC,operation.id DESC,operation.resource_id DESC`, []any{filterArg(f, "q"), filterArg(f, "status"), filterArg(f, "provider_id"), filterArg(f, "created_from"), filterArg(f, "created_to")}, nil
	case adminPageAdjustments:
		return `SELECT id::text resource_id,created_at sort_at,id sort_id FROM manual_adjustments WHERE ($2='' OR wallet_id=NULLIF($2,'')::uuid) AND ($3='' OR status=$3) AND ($4='' OR requested_by=NULLIF($4,'')::uuid) AND ($5='' OR created_at>=NULLIF($5,'')::timestamptz) AND ($6='' OR created_at<NULLIF($6,'')::timestamptz) ORDER BY created_at DESC,id DESC`, []any{filterArg(f, "wallet_id"), filterArg(f, "status"), filterArg(f, "requested_by"), filterArg(f, "created_from"), filterArg(f, "created_to")}, nil
	case adminPageUsageHolds:
		return `SELECT id::text resource_id,created_at sort_at,id sort_id FROM usage_holds WHERE ($2='' OR wallet_id=NULLIF($2,'')::uuid) AND ($3='' OR status=$3) AND ($4='' OR model_id=$4) AND ($5='' OR created_at>=NULLIF($5,'')::timestamptz) AND ($6='' OR created_at<NULLIF($6,'')::timestamptz) ORDER BY created_at DESC,id DESC`, []any{filterArg(f, "wallet_id"), filterArg(f, "status"), filterArg(f, "model_id"), filterArg(f, "created_from"), filterArg(f, "created_to")}, nil
	case adminPageLatePaymentReviews:
		return `SELECT late.id::text resource_id,late.created_at sort_at,late.id sort_id FROM late_payment_review_cases late JOIN manual_review_cases review ON review.id=late.id WHERE ($2='' OR late.id::text ILIKE '%'||$2||'%' OR late.payment_order_id::text ILIKE '%'||$2||'%' OR late.merchant_order_no ILIKE '%'||$2||'%') AND ($3='' OR late.review_status=$3) AND ($4='' OR review.severity=$4) AND ($5='' OR review.owner_id=NULLIF($5,'')::uuid) AND ($6='' OR late.provider_id=$6) AND ($7='' OR late.created_at>=NULLIF($7,'')::timestamptz) AND ($8='' OR late.created_at<NULLIF($8,'')::timestamptz) AND ($9='' OR late.next_review_at<=NULLIF($9,'')::timestamptz) AND ($10<>'true' OR review.status='OPEN' AND (late.next_review_at<=now() OR review.claim_due_at<=now() AND review.claimed_at IS NULL OR review.first_response_due_at<=now() AND review.first_response_at IS NULL OR review.mitigation_due_at<=now() AND review.mitigation_at IS NULL OR review.resolution_due_at<=now())) ORDER BY late.created_at DESC,late.id DESC`, []any{filterArg(f, "q"), filterArg(f, "status"), filterArg(f, "severity"), filterArg(f, "owner_id"), filterArg(f, "provider_id"), filterArg(f, "created_from"), filterArg(f, "created_to"), filterArg(f, "next_review_before"), filterArg(f, "overdue")}, nil
	case adminPagePaymentAnomalies:
		return `SELECT anomaly.id::text resource_id,anomaly.created_at sort_at,anomaly.id sort_id FROM payment_anomaly_cases anomaly JOIN manual_review_cases review ON review.id=anomaly.id LEFT JOIN payment_orders payment ON payment.id=anomaly.payment_order_id WHERE ($2='' OR anomaly.id::text ILIKE '%'||$2||'%' OR anomaly.payment_order_id::text ILIKE '%'||$2||'%' OR COALESCE(payment.merchant_order_no,'') ILIKE '%'||$2||'%') AND ($3='' OR anomaly.anomaly_type=$3) AND ($4='' OR anomaly.status=$4) AND ($5='' OR review.severity=$5) AND ($6='' OR review.owner_id=NULLIF($6,'')::uuid) AND ($7='' OR payment.provider_id=$7) AND ($8='' OR anomaly.created_at>=NULLIF($8,'')::timestamptz) AND ($9='' OR anomaly.created_at<NULLIF($9,'')::timestamptz) ORDER BY anomaly.created_at DESC,anomaly.id DESC`, []any{filterArg(f, "q"), filterArg(f, "anomaly_type"), filterArg(f, "status"), filterArg(f, "severity"), filterArg(f, "owner_id"), filterArg(f, "provider_id"), filterArg(f, "created_from"), filterArg(f, "created_to")}, nil
	case adminPageReconciliationRuns:
		return `SELECT id::text resource_id,started_at sort_at,id sort_id FROM reconciliation_runs WHERE ($2='' OR provider_id=$2) AND ($3='' OR channel=$3) AND ($4='' OR status=$4) AND ($5='' OR business_date>=NULLIF($5,'')::date) AND ($6='' OR business_date<NULLIF($6,'')::date) ORDER BY started_at DESC,id DESC`, []any{filterArg(f, "provider_id"), filterArg(f, "channel"), filterArg(f, "status"), filterArg(f, "business_date_from"), filterArg(f, "business_date_to")}, nil
	case adminPageReconciliationCases:
		return `SELECT id::text resource_id,created_at sort_at,id sort_id FROM reconciliation_cases WHERE ($2='' OR reconciliation_run_id=NULLIF($2,'')::uuid) AND ($3='' OR status=$3) AND ($4='' OR severity=$4) AND ($5='' OR owner_id=NULLIF($5,'')::uuid) AND ($6='' OR case_type=$6) AND ($7='' OR created_at>=NULLIF($7,'')::timestamptz) AND ($8='' OR created_at<NULLIF($8,'')::timestamptz) ORDER BY created_at DESC,id DESC`, []any{request.Scope, filterArg(f, "status"), filterArg(f, "severity"), filterArg(f, "owner_id"), filterArg(f, "case_type"), filterArg(f, "created_from"), filterArg(f, "created_to")}, nil
	default:
		return "", nil, ErrPaginationCursorInvalid
	}
}

func writeAdminPaginationError(w http.ResponseWriter, r *http.Request, err error) {
	requestID := httpserver.RequestID(r.Context())
	switch {
	case errors.Is(err, ErrPaginationCursorExpired):
		api.WriteError(w, http.StatusGone, "PAGINATION_CURSOR_EXPIRED", "the pagination snapshot has expired", requestID)
	case errors.Is(err, ErrPaginationFilterRequired):
		api.WriteError(w, http.StatusUnprocessableEntity, "PAGINATION_FILTER_REQUIRED", "the result exceeds the snapshot capacity; narrow the filters", requestID)
	case errors.Is(err, ErrPaginationSnapshotLimited):
		w.Header().Set("Retry-After", "10")
		api.WriteError(w, http.StatusTooManyRequests, "PAGINATION_SNAPSHOT_LIMITED", "pagination snapshot capacity is temporarily exhausted", requestID)
	case errors.Is(err, ErrPaginationSnapshotUnavailable):
		w.Header().Set("Retry-After", "5")
		api.WriteError(w, http.StatusServiceUnavailable, "PAGINATION_SNAPSHOT_UNAVAILABLE", "pagination snapshot storage is temporarily unavailable", requestID)
	default:
		api.WriteError(w, http.StatusBadRequest, "PAGINATION_CURSOR_INVALID", "the pagination cursor does not match this request", requestID)
	}
}

func writeAdminPageResponse(w http.ResponseWriter, page AdminPaginationPage, items any) {
	api.WriteJSON(w, http.StatusOK, map[string]any{
		"items":          items,
		"next_cursor":    page.NextCursor,
		"snapshot_as_of": page.SnapshotAsOf,
	})
}

func (s *Service) writeAdminSnapshotCollection(w http.ResponseWriter, r *http.Request, resource, scope string, load func(adminPaginationItemsRepository, []string) (any, error)) {
	repository, err := ensureAdminPaginationItems(s.repository)
	if err != nil {
		writeAdminPaginationError(w, r, ErrPaginationSnapshotUnavailable)
		return
	}
	request, ok := normalizedAdminPageRequest(w, r, resource, scope)
	if !ok {
		return
	}
	page, err := repository.AdminPaginationPage(r.Context(), request)
	if err != nil {
		writeAdminPaginationError(w, r, err)
		return
	}
	items, err := load(repository, page.ResourceIDs)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminPageResponse(w, page, items)
}

func adminUUIDs(ids []string) ([]uuid.UUID, error) {
	result := make([]uuid.UUID, 0, len(ids))
	for _, raw := range ids {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid snapshot member %q: %w", raw, err)
		}
		result = append(result, id)
	}
	return result, nil
}

func parseAdminPageLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 50, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 200 {
		return 0, ErrPaginationCursorInvalid
	}
	return value, nil
}
