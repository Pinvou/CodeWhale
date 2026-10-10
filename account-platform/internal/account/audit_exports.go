package account

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	auditExportMaximumRange = 31 * 24 * time.Hour
	auditExportMaximumRows  = 100_000
	auditExportMaximumBytes = 500 * 1024 * 1024
	auditExportRecentAuth   = 15 * time.Minute
	auditExportTicketTTL    = 15 * time.Minute
	auditExportFileTTL      = 24 * time.Hour
)

var ErrAuditExportAuthorizationUnavailable = errors.New("audit export authorization unavailable")
var ErrAuditExportQuota = errors.New("audit export quota exceeded")
var ErrInvalidAuditExportRequest = errors.New("audit export request is invalid")

type AuditExportFilter struct {
	Actor    string    `json:"actor,omitempty"`
	Action   string    `json:"action,omitempty"`
	Resource string    `json:"resource,omitempty"`
	Result   string    `json:"result,omitempty"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
}

type AuditExportRequest struct {
	Filter         AuditExportFilter `json:"filter"`
	Reason         string            `json:"reason"`
	IncludePII     bool              `json:"include_pii"`
	IdempotencyKey string            `json:"idempotency_key"`
}

type AuditExportJob struct {
	ID                  uuid.UUID  `json:"export_id"`
	Status              string     `json:"status"`
	IncludePII          bool       `json:"include_pii"`
	Filter              any        `json:"filter"`
	Reason              string     `json:"reason"`
	RowCount            *int       `json:"row_count,omitempty"`
	SizeBytes           *int64     `json:"size_bytes,omitempty"`
	Filename            *string    `json:"filename,omitempty"`
	ErrorCode           *string    `json:"error_code,omitempty"`
	DownloadTicketAt    *time.Time `json:"download_ticket_issued_at,omitempty"`
	DownloadExpiresAt   *time.Time `json:"download_expires_at,omitempty"`
	DownloadedAt        *time.Time `json:"downloaded_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	CompletedAt         *time.Time `json:"completed_at,omitempty"`
	DecryptionKey       string     `json:"decryption_key,omitempty"`
	DecryptionKeyFormat string     `json:"decryption_key_format,omitempty"`
	Replayed            bool       `json:"replayed"`
}

type AuditExportDownloadTicket struct {
	ExportID    uuid.UUID `json:"export_id"`
	DownloadURL string    `json:"download_url"`
	Token       string    `json:"download_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type AuditExportDownload struct {
	Filename string
	Payload  []byte
}

type AuditExportService struct {
	repository  *PostgresRepository
	masterKey   []byte
	environment string
	now         func() time.Time
}

func (s *Service) createAuditExport(w http.ResponseWriter, r *http.Request) {
	var request AuditExportRequest
	if err := api.DecodeJSON(w, r, &request); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid audit export request", httpserver.RequestID(r.Context()))
		return
	}
	job, err := s.auditExports.Create(r.Context(), mustPrincipal(r.Context()), request)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusAccepted, job)
}

func (s *Service) getAuditExport(w http.ResponseWriter, r *http.Request) {
	exportID, ok := pathUUID(w, r, "export_id")
	if !ok {
		return
	}
	job, err := s.auditExports.Get(r.Context(), mustPrincipal(r.Context()), exportID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, job)
}

func (s *Service) issueAuditExportDownloadTicket(w http.ResponseWriter, r *http.Request) {
	exportID, ok := pathUUID(w, r, "export_id")
	if !ok {
		return
	}
	ticket, err := s.auditExports.IssueDownloadTicket(r.Context(), mustPrincipal(r.Context()), exportID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusCreated, ticket)
}

func (s *Service) downloadAuditExport(w http.ResponseWriter, r *http.Request) {
	// HEAD cannot consume a single-use payload that it will not deliver.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		api.WriteError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "audit export download requires GET", httpserver.RequestID(r.Context()))
		return
	}
	exportID, ok := pathUUID(w, r, "export_id")
	if !ok {
		return
	}
	download, err := s.auditExports.Download(r.Context(), mustPrincipal(r.Context()), exportID, r.Header.Get("X-Pinvou-Download-Token"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/vnd.pinvou.audit-envelope+json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", download.Filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(download.Payload)
	clear(download.Payload)
}

func NewAuditExportService(repository *PostgresRepository, masterKey []byte, environment string) (*AuditExportService, error) {
	if repository == nil || len(masterKey) != 32 {
		return nil, errors.New("audit export master key must contain exactly 32 bytes")
	}
	environment = strings.TrimSpace(environment)
	if environment == "" {
		return nil, errors.New("audit export environment is required")
	}
	return &AuditExportService{repository: repository, masterKey: append([]byte(nil), masterKey...), environment: environment, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *AuditExportService) Create(ctx context.Context, principal identity.Principal, request AuditExportRequest) (AuditExportJob, error) {
	now := s.now()
	if !auditExportRecentlyAuthenticated(principal.AuthenticatedAt, now) {
		return AuditExportJob{}, ErrRecentAuthenticationRequired
	}
	request.Reason = strings.TrimSpace(request.Reason)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.Filter.Actor = strings.TrimSpace(request.Filter.Actor)
	request.Filter.Action = strings.TrimSpace(request.Filter.Action)
	request.Filter.Resource = strings.TrimSpace(request.Filter.Resource)
	request.Filter.Result = strings.ToUpper(strings.TrimSpace(request.Filter.Result))
	request.Filter.From = request.Filter.From.UTC()
	request.Filter.To = request.Filter.To.UTC()
	if !utf8.ValidString(request.Reason) || utf8.RuneCountInString(request.Reason) < 10 || utf8.RuneCountInString(request.Reason) > 500 || len(request.IdempotencyKey) < 8 || len(request.IdempotencyKey) > 120 || request.Filter.From.IsZero() || request.Filter.To.IsZero() || !request.Filter.To.After(request.Filter.From) || request.Filter.To.Sub(request.Filter.From) > auditExportMaximumRange || request.Filter.To.After(now.Add(time.Minute)) {
		return AuditExportJob{}, ErrInvalidAuditExportRequest
	}
	if len(request.Filter.Actor) > 320 || len(request.Filter.Action) > 160 || len(request.Filter.Resource) > 320 || (request.Filter.Result != "" && request.Filter.Result != "SUCCEEDED" && request.Filter.Result != "DENIED" && request.Filter.Result != "FAILED") {
		return AuditExportJob{}, ErrInvalidAuditExportRequest
	}
	if request.IncludePII {
		decision, err := s.repository.Authorize(ctx, principal, PermissionPlatformAuditPIIExport, nil, nil)
		if err != nil {
			if errors.Is(err, ErrForbidden) {
				return AuditExportJob{}, ErrForbidden
			}
			return AuditExportJob{}, fmt.Errorf("%w: %w", ErrAuditExportAuthorizationUnavailable, err)
		}
		if !decision.Allowed {
			return AuditExportJob{}, ErrForbidden
		}
	}
	requestPayload, _ := json.Marshal(struct {
		Filter     AuditExportFilter `json:"filter"`
		Reason     string            `json:"reason"`
		IncludePII bool              `json:"include_pii"`
	}{request.Filter, request.Reason, request.IncludePII})
	requestDigest := sha256Hex(requestPayload)
	keyDigest := sha256Hex([]byte(request.IdempotencyKey))
	filterPayload, _ := json.Marshal(request.Filter)
	tx, err := s.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AuditExportJob{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := requiredAuditExportActor(ctx, tx, principal)
	if err != nil {
		return AuditExportJob{}, err
	}
	var lockedActorID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, actorID).Scan(&lockedActorID); err != nil {
		return AuditExportJob{}, err
	}
	var existingID uuid.UUID
	var existingDigest, existingStatus string
	var wrappedNonce, wrappedKey []byte
	err = tx.QueryRow(ctx, `SELECT id,request_digest,status,wrapped_file_key_nonce,wrapped_file_key FROM audit_export_jobs WHERE actor_user_id=$1 AND idempotency_key_digest=$2`, actorID, keyDigest).Scan(&existingID, &existingDigest, &existingStatus, &wrappedNonce, &wrappedKey)
	if err == nil {
		if existingDigest != requestDigest {
			return AuditExportJob{}, ErrConflict
		}
		job, err := auditExportJobByID(ctx, tx, existingID, actorID)
		if err != nil {
			return AuditExportJob{}, err
		}
		if len(wrappedKey) > 0 {
			fileKey, unwrapErr := decryptAEAD(s.masterKey, wrappedNonce, wrappedKey, []byte(existingID.String()))
			if unwrapErr != nil {
				return AuditExportJob{}, unwrapErr
			}
			job.DecryptionKey = base64.RawURLEncoding.EncodeToString(fileKey)
			job.DecryptionKeyFormat = "BASE64URL_AES_256_GCM_KEY"
			clear(fileKey)
		}
		job.Replayed = true
		return job, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AuditExportJob{}, err
	}
	var active, daily int
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status IN ('PENDING','GENERATING')),count(*) FILTER (WHERE created_at>=date_trunc('day',$2::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') FROM audit_export_jobs WHERE actor_user_id=$1`, actorID, now).Scan(&active, &daily); err != nil {
		return AuditExportJob{}, err
	}
	if active >= 2 || daily >= 10 {
		return AuditExportJob{}, ErrAuditExportQuota
	}
	exportID := uuid.Must(uuid.NewV7())
	fileKey := make([]byte, 32)
	if _, err := rand.Read(fileKey); err != nil {
		return AuditExportJob{}, err
	}
	defer clear(fileKey)
	wrappedNonce, wrappedKey, err = encryptAEAD(s.masterKey, fileKey, []byte(exportID.String()))
	if err != nil {
		return AuditExportJob{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_export_jobs(id,actor_user_id,request_digest,idempotency_key_digest,filter_snapshot,reason,include_pii,status,wrapped_file_key_nonce,wrapped_file_key,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,'PENDING',$8,$9,$10,$10)`, exportID, actorID, requestDigest, keyDigest, filterPayload, request.Reason, request.IncludePII, wrappedNonce, wrappedKey, now); err != nil {
		return AuditExportJob{}, err
	}
	if err := appendAuditExportEvent(ctx, tx, exportID, "CREATED", &actorID, map[string]any{"include_pii": request.IncludePII, "filter_digest": sha256Hex(filterPayload), "limits": map[string]any{"days": 31, "rows": auditExportMaximumRows, "bytes": auditExportMaximumBytes}}, now); err != nil {
		return AuditExportJob{}, err
	}
	if err := appendAuditExportAudit(ctx, tx, actorID, "admin.audit.export.created", exportID, "SUCCEEDED", map[string]any{"include_pii": request.IncludePII, "filter_digest": sha256Hex(filterPayload)}, now); err != nil {
		return AuditExportJob{}, err
	}
	job, err := auditExportJobByID(ctx, tx, exportID, actorID)
	if err != nil {
		return AuditExportJob{}, err
	}
	job.DecryptionKey = base64.RawURLEncoding.EncodeToString(fileKey)
	job.DecryptionKeyFormat = "BASE64URL_AES_256_GCM_KEY"
	return job, tx.Commit(ctx)
}

func auditExportRecentlyAuthenticated(authenticatedAt, now time.Time) bool {
	return !authenticatedAt.IsZero() && !authenticatedAt.After(now.Add(time.Minute)) && now.Sub(authenticatedAt) <= auditExportRecentAuth
}

func (s *AuditExportService) Get(ctx context.Context, principal identity.Principal, exportID uuid.UUID) (AuditExportJob, error) {
	actorID, err := s.actorID(ctx, principal)
	if err != nil {
		return AuditExportJob{}, err
	}
	return auditExportJobByID(ctx, s.repository.pool, exportID, actorID)
}

func (s *AuditExportService) IssueDownloadTicket(ctx context.Context, principal identity.Principal, exportID uuid.UUID) (AuditExportDownloadTicket, error) {
	now := s.now()
	if !auditExportRecentlyAuthenticated(principal.AuthenticatedAt, now) {
		return AuditExportDownloadTicket{}, ErrRecentAuthenticationRequired
	}
	tx, err := s.repository.pool.Begin(ctx)
	if err != nil {
		return AuditExportDownloadTicket{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := requiredAuditExportActor(ctx, tx, principal)
	if err != nil {
		return AuditExportDownloadTicket{}, err
	}
	var status string
	var fileExpiresAt *time.Time
	var issuedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT status,file_expires_at,download_ticket_issued_at FROM audit_export_jobs WHERE id=$1 AND actor_user_id=$2 FOR UPDATE`, exportID, actorID).Scan(&status, &fileExpiresAt, &issuedAt); errors.Is(err, pgx.ErrNoRows) {
		return AuditExportDownloadTicket{}, ErrNotFound
	} else if err != nil {
		return AuditExportDownloadTicket{}, err
	}
	// The row lock may wait across the file or recent-authentication deadline.
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return AuditExportDownloadTicket{}, err
	}
	if !auditExportRecentlyAuthenticated(principal.AuthenticatedAt, now) {
		return AuditExportDownloadTicket{}, ErrRecentAuthenticationRequired
	}
	if status != "READY" || issuedAt != nil || fileExpiresAt == nil || !fileExpiresAt.After(now) {
		return AuditExportDownloadTicket{}, ErrConflict
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return AuditExportDownloadTicket{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	clear(tokenBytes)
	expiresAt := now.Add(auditExportTicketTTL)
	if fileExpiresAt.Before(expiresAt) {
		expiresAt = *fileExpiresAt
	}
	if _, err := tx.Exec(ctx, `UPDATE audit_export_jobs SET download_token_digest=$2,download_ticket_issued_at=$3,download_expires_at=$4,updated_at=$3 WHERE id=$1`, exportID, sha256Hex([]byte(token)), now, expiresAt); err != nil {
		return AuditExportDownloadTicket{}, err
	}
	if err := appendAuditExportEvent(ctx, tx, exportID, "DOWNLOAD_TICKET_ISSUED", &actorID, map[string]any{"expires_at": expiresAt, "maximum_successes": 1}, now); err != nil {
		return AuditExportDownloadTicket{}, err
	}
	if err := appendAuditExportAudit(ctx, tx, actorID, "admin.audit.export.download-ticket", exportID, "SUCCEEDED", map[string]any{"expires_at": expiresAt}, now); err != nil {
		return AuditExportDownloadTicket{}, err
	}
	return AuditExportDownloadTicket{ExportID: exportID, DownloadURL: "/admin/v1/audit-exports/" + exportID.String() + "/download", Token: token, ExpiresAt: expiresAt}, tx.Commit(ctx)
}

func (s *AuditExportService) Download(ctx context.Context, principal identity.Principal, exportID uuid.UUID, token string) (AuditExportDownload, error) {
	token = strings.TrimSpace(token)
	if len(token) < 40 || len(token) > 100 {
		return AuditExportDownload{}, ErrNotFound
	}
	tx, err := s.repository.pool.Begin(ctx)
	if err != nil {
		return AuditExportDownload{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := requiredAuditExportActor(ctx, tx, principal)
	if err != nil {
		return AuditExportDownload{}, err
	}
	var status, digest string
	var filename *string
	var expiresAt *time.Time
	var payload []byte
	if err := tx.QueryRow(ctx, `SELECT status,COALESCE(download_token_digest,''),download_expires_at,filename,file_payload FROM audit_export_jobs WHERE id=$1 AND actor_user_id=$2 FOR UPDATE`, exportID, actorID).Scan(&status, &digest, &expiresAt, &filename, &payload); errors.Is(err, pgx.ErrNoRows) {
		return AuditExportDownload{}, ErrNotFound
	} else if err != nil {
		return AuditExportDownload{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return AuditExportDownload{}, err
	}
	if status != "READY" || expiresAt == nil || !expiresAt.After(now) || filename == nil || *filename == "" || !hmac.Equal([]byte(digest), []byte(sha256Hex([]byte(token)))) || len(payload) == 0 {
		if err := appendAuditExportEvent(ctx, tx, exportID, "DOWNLOAD_FAILED", &actorID, map[string]any{"reason_code": "INVALID_OR_EXPIRED_TICKET"}, now); err != nil {
			return AuditExportDownload{}, err
		}
		if err := appendAuditExportAudit(ctx, tx, actorID, "admin.audit.export.downloaded", exportID, "FAILED", map[string]any{"reason_code": "INVALID_OR_EXPIRED_TICKET"}, now); err != nil {
			return AuditExportDownload{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AuditExportDownload{}, err
		}
		return AuditExportDownload{}, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE audit_export_jobs SET status='DOWNLOADED',file_payload=NULL,download_token_digest=NULL,wrapped_file_key_nonce=NULL,wrapped_file_key=NULL,downloaded_at=$2,updated_at=$2 WHERE id=$1`, exportID, now); err != nil {
		return AuditExportDownload{}, err
	}
	if err := appendAuditExportEvent(ctx, tx, exportID, "DOWNLOADED", &actorID, map[string]any{"size_bytes": len(payload), "single_use_consumed": true}, now); err != nil {
		return AuditExportDownload{}, err
	}
	if err := appendAuditExportAudit(ctx, tx, actorID, "admin.audit.export.downloaded", exportID, "SUCCEEDED", map[string]any{"size_bytes": len(payload), "single_use_consumed": true}, now); err != nil {
		return AuditExportDownload{}, err
	}
	return AuditExportDownload{Filename: *filename, Payload: payload}, tx.Commit(ctx)
}

func (s *AuditExportService) actorID(ctx context.Context, principal identity.Principal) (uuid.UUID, error) {
	return requiredAuditExportActor(ctx, s.repository.pool, principal)
}

type auditExportQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func requiredAuditExportActor(ctx context.Context, queryer auditExportQueryer, principal identity.Principal) (uuid.UUID, error) {
	var actorID uuid.UUID
	err := queryer.QueryRow(ctx, `SELECT u.id FROM identities identity JOIN users u ON u.id=identity.user_id WHERE identity.issuer=$1 AND identity.subject=$2 AND u.status='ACTIVE'`, principal.Issuer, principal.Subject).Scan(&actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrForbidden
	}
	return actorID, err
}

func auditExportJobByID(ctx context.Context, queryer auditExportQueryer, exportID, actorID uuid.UUID) (AuditExportJob, error) {
	var job AuditExportJob
	var filter []byte
	err := queryer.QueryRow(ctx, `SELECT id,status,include_pii,filter_snapshot,reason,row_count,size_bytes,filename,error_code,download_ticket_issued_at,download_expires_at,downloaded_at,created_at,updated_at,completed_at FROM audit_export_jobs WHERE id=$1 AND actor_user_id=$2`, exportID, actorID).Scan(&job.ID, &job.Status, &job.IncludePII, &filter, &job.Reason, &job.RowCount, &job.SizeBytes, &job.Filename, &job.ErrorCode, &job.DownloadTicketAt, &job.DownloadExpiresAt, &job.DownloadedAt, &job.CreatedAt, &job.UpdatedAt, &job.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuditExportJob{}, ErrNotFound
	}
	if err == nil {
		var decoded any
		_ = json.Unmarshal(filter, &decoded)
		job.Filter = decoded
	}
	return job, err
}

func (s *AuditExportService) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		_ = s.expire(ctx)
		for index := 0; index < 4; index++ {
			job, found, err := s.claim(ctx)
			if err != nil || !found {
				break
			}
			if err := s.generate(ctx, job); err != nil {
				_ = s.fail(ctx, job, stableAuditExportError(err))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type claimedAuditExport struct {
	Attempt        int
	ID             uuid.UUID
	ActorUserID    uuid.UUID
	Filter         AuditExportFilter
	IncludePII     bool
	WrappedNonce   []byte
	WrappedFileKey []byte
}

func (s *AuditExportService) claim(ctx context.Context) (claimedAuditExport, bool, error) {
	now := s.now()
	var job claimedAuditExport
	var filter []byte
	err := s.repository.pool.QueryRow(ctx, `WITH candidate AS (
    SELECT id FROM audit_export_jobs WHERE status IN ('PENDING','GENERATING') AND (lease_until IS NULL OR lease_until<$1)
    ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1
), claimed AS (
    UPDATE audit_export_jobs job SET status='GENERATING',attempts=attempts+1,lease_until=$1::timestamptz+interval '5 minutes',updated_at=$1::timestamptz
    FROM candidate WHERE job.id=candidate.id RETURNING job.id,job.actor_user_id,job.filter_snapshot,job.include_pii,job.wrapped_file_key_nonce,job.wrapped_file_key,job.attempts
)
SELECT id,actor_user_id,filter_snapshot,include_pii,wrapped_file_key_nonce,wrapped_file_key,attempts FROM claimed`, now).Scan(&job.ID, &job.ActorUserID, &filter, &job.IncludePII, &job.WrappedNonce, &job.WrappedFileKey, &job.Attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return claimedAuditExport{}, false, nil
	}
	if err != nil {
		return claimedAuditExport{}, false, err
	}
	if err := json.Unmarshal(filter, &job.Filter); err != nil {
		return job, true, err
	}
	return job, true, nil
}

func (s *AuditExportService) generate(ctx context.Context, job claimedAuditExport) error {
	fileKey, err := decryptAEAD(s.masterKey, job.WrappedNonce, job.WrappedFileKey, []byte(job.ID.String()))
	if err != nil {
		return err
	}
	defer clear(fileKey)
	rows, err := s.repository.pool.Query(ctx, adminAuditSelect+`
WHERE ($1='' OR COALESCE(log.actor_user_id::text,'') ILIKE '%'||$1||'%' OR COALESCE(actor.display_name,'') ILIKE '%'||$1||'%' OR COALESCE(actor.email,'') ILIKE '%'||$1||'%' OR COALESCE(log.change_summary->>'actor_subject','') ILIKE '%'||$1||'%')
  AND ($2='' OR log.action ILIKE '%'||$2||'%')
  AND ($3='' OR log.resource_type ILIKE '%'||$3||'%' OR log.resource_id ILIKE '%'||$3||'%')
  AND ($4='' OR log.result=$4) AND log.occurred_at >= $5 AND log.occurred_at <= $6
ORDER BY log.occurred_at,log.id LIMIT $7`, job.Filter.Actor, job.Filter.Action, job.Filter.Resource, job.Filter.Result, job.Filter.From, job.Filter.To, auditExportMaximumRows+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	filterPayload, _ := json.Marshal(job.Filter)
	scopeDigest := sha256Hex(filterPayload)[:12]
	watermark := fmt.Sprintf("export_id=%s;operator_id=%s;generated_at=%s;environment=%s;scope=%s", job.ID, job.ActorUserID, s.now().Format(time.RFC3339Nano), safeFilenamePart(s.environment), scopeDigest)
	var plaintext bytes.Buffer
	_, _ = fmt.Fprintf(&plaintext, "# PINVOU AUDIT EXPORT\n# WATERMARK %s\n", watermark)
	writer := csv.NewWriter(&plaintext)
	header := []string{"audit_id", "actor_user_id", "action", "resource_type", "resource_reference", "result", "request_id", "change_summary_sha256", "occurred_at"}
	if job.IncludePII {
		header = append(header[:2], append([]string{"actor_display_name", "actor_email"}, header[2:]...)...)
	}
	if err := writer.Write(header); err != nil {
		return err
	}
	count := 0
	for rows.Next() {
		var item AdminAuditLog
		if err := rows.Scan(&item.ID, &item.ActorUserID, &item.ActorDisplayName, &item.ActorEmail, &item.ActiveOrganizationID, &item.Action, &item.ResourceType, &item.ResourceID, &item.Result, &item.RequestID, &item.ChangeSummary, &item.OccurredAt); err != nil {
			return err
		}
		count++
		if count > auditExportMaximumRows {
			return ErrAuditExportQuota
		}
		actorID := ""
		if item.ActorUserID != nil {
			actorID = "subject_" + hmacHex(s.masterKey, []byte(item.ActorUserID.String()))[:24]
		}
		resource := maskAuditReference(s.masterKey, item.ResourceID)
		record := []string{item.ID.String(), actorID, item.Action, item.ResourceType, resource, item.Result, item.RequestID, sha256Hex(item.ChangeSummary), item.OccurredAt.UTC().Format(time.RFC3339Nano)}
		if job.IncludePII {
			if item.ActorUserID != nil {
				actorID = item.ActorUserID.String()
			}
			resource = item.ResourceID
			display, email := "", ""
			if item.ActorDisplayName != nil {
				display = *item.ActorDisplayName
			}
			if item.ActorEmail != nil {
				email = *item.ActorEmail
			}
			record = []string{item.ID.String(), actorID, display, email, item.Action, item.ResourceType, resource, item.Result, item.RequestID, sha256Hex(item.ChangeSummary), item.OccurredAt.UTC().Format(time.RFC3339Nano)}
		}
		for index := range record {
			record[index] = safeAuditCSVCell(record[index])
		}
		if err := writer.Write(record); err != nil {
			return err
		}
		writer.Flush()
		if writer.Error() != nil {
			return writer.Error()
		}
		if plaintext.Len() > auditExportMaximumBytes {
			return ErrAuditExportQuota
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return err
	}
	nonce, ciphertext, err := encryptAEAD(fileKey, plaintext.Bytes(), []byte(watermark))
	if err != nil {
		return err
	}
	signature := hmacHex(fileKey, []byte(job.ID.String()), nonce, ciphertext, []byte(watermark))
	envelope, err := json.Marshal(map[string]any{"format": "PINVOU_AUDIT_ENVELOPE_V1", "encryption": "AES-256-GCM", "signature": "HMAC-SHA-256", "export_id": job.ID, "watermark": watermark, "nonce": base64.RawURLEncoding.EncodeToString(nonce), "ciphertext": base64.RawURLEncoding.EncodeToString(ciphertext), "signature_value": signature})
	if err != nil {
		return err
	}
	if len(envelope) > auditExportMaximumBytes {
		return ErrAuditExportQuota
	}
	now := s.now()
	filename := fmt.Sprintf("pinvou-audit-%s-%s-%s-%s-%s.pae", job.ID, job.ActorUserID, now.Format("20060102T150405Z"), safeFilenamePart(s.environment), scopeDigest)
	tx, err := s.repository.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now, err = requireAuditExportLease(ctx, tx, job)
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE audit_export_jobs SET status='READY',file_payload=$2,file_signature=$3,filename=$4,row_count=$5,size_bytes=$6,lease_until=NULL,file_expires_at=$7,completed_at=$8,updated_at=$8,error_code=NULL WHERE id=$1 AND status='GENERATING'`, job.ID, envelope, signature, filename, count, len(envelope), now.Add(auditExportFileTTL), now)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return err
	}
	if err := appendAuditExportEvent(ctx, tx, job.ID, "READY", nil, map[string]any{"row_count": count, "size_bytes": len(envelope), "file_signature": signature, "watermark": watermark}, now); err != nil {
		return err
	}
	if err := appendAuditExportAudit(ctx, tx, job.ActorUserID, "admin.audit.export.ready", job.ID, "SUCCEEDED", map[string]any{"row_count": count, "size_bytes": len(envelope), "file_signature": signature}, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// requireAuditExportLease serializes a terminal write with reclaim and checks
// the persisted attempt and live database clock after acquiring the row lock.
// A delayed worker may neither publish its payload nor fail a newer attempt.
func requireAuditExportLease(ctx context.Context, tx pgx.Tx, job claimedAuditExport) (time.Time, error) {
	if job.Attempt < 1 || job.ID == uuid.Nil || job.ActorUserID == uuid.Nil {
		return time.Time{}, ErrConflict
	}
	var attempt int
	var actorID uuid.UUID
	var status string
	var until *time.Time
	if err := tx.QueryRow(ctx, `SELECT attempts,actor_user_id,status,lease_until FROM audit_export_jobs WHERE id=$1 FOR UPDATE`, job.ID).Scan(&attempt, &actorID, &status, &until); err != nil {
		return time.Time{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, err
	}
	if attempt != job.Attempt || actorID != job.ActorUserID || status != "GENERATING" || until == nil || !until.After(now) {
		return time.Time{}, ErrConflict
	}
	return now, nil
}

func (s *AuditExportService) fail(ctx context.Context, job claimedAuditExport, code string) error {
	tx, err := s.repository.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now, err := requireAuditExportLease(ctx, tx, job)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE audit_export_jobs SET status='FAILED',wrapped_file_key_nonce=NULL,wrapped_file_key=NULL,file_payload=NULL,lease_until=NULL,error_code=$2,completed_at=$3,updated_at=$3 WHERE id=$1`, job.ID, code, now); err != nil {
		return err
	}
	if err := appendAuditExportEvent(ctx, tx, job.ID, "FAILED", nil, map[string]any{"error_code": code}, now); err != nil {
		return err
	}
	if err := appendAuditExportAudit(ctx, tx, job.ActorUserID, "admin.audit.export.failed", job.ID, "FAILED", map[string]any{"error_code": code}, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *AuditExportService) expire(ctx context.Context) error {
	now := s.now()
	rows, err := s.repository.pool.Query(ctx, `SELECT id,actor_user_id FROM audit_export_jobs WHERE status='READY' AND (file_expires_at<=$1 OR (download_ticket_issued_at IS NOT NULL AND download_expires_at<=$1)) ORDER BY updated_at,id LIMIT 100`, now)
	if err != nil {
		return err
	}
	type expired struct{ id, actor uuid.UUID }
	items := make([]expired, 0)
	for rows.Next() {
		var item expired
		if err := rows.Scan(&item.id, &item.actor); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	rows.Close()
	for _, item := range items {
		tx, err := s.repository.pool.Begin(ctx)
		if err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE audit_export_jobs SET status='EXPIRED',wrapped_file_key_nonce=NULL,wrapped_file_key=NULL,file_payload=NULL,download_token_digest=NULL,error_code='DOWNLOAD_EXPIRED',completed_at=COALESCE(completed_at,$2),updated_at=$2 WHERE id=$1 AND status='READY' AND (file_expires_at<=$2 OR (download_ticket_issued_at IS NOT NULL AND download_expires_at<=$2))`, item.id, now)
		if err == nil && command.RowsAffected() == 1 {
			err = appendAuditExportEvent(ctx, tx, item.id, "EXPIRED", nil, map[string]any{"file_deleted": true}, now)
		}
		if err == nil && command.RowsAffected() == 1 {
			err = appendAuditExportAudit(ctx, tx, item.actor, "admin.audit.export.expired", item.id, "FAILED", map[string]any{"file_deleted": true}, now)
		}
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func appendAuditExportEvent(ctx context.Context, tx pgx.Tx, exportID uuid.UUID, eventType string, actorID *uuid.UUID, snapshot any, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_export_events(id,export_id,event_type,actor_user_id,snapshot,occurred_at) VALUES($1,$2,$3,$4,$5,$6)`, uuid.Must(uuid.NewV7()), exportID, eventType, actorID, snapshot, now)
	return err
}

func appendAuditExportAudit(ctx context.Context, tx pgx.Tx, actorID uuid.UUID, action string, exportID uuid.UUID, result string, summary any, now time.Time) error {
	requestID := "audit-export-" + exportID.String()
	_, err := tx.Exec(ctx, `INSERT INTO audit_logs(id,actor_user_id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at) VALUES($1,$2,$3,'audit_export',$4,$5,$6,$6,$7,$8)`, uuid.Must(uuid.NewV7()), actorID, action, exportID.String(), result, requestID, summary, now)
	return err
}

func stableAuditExportError(err error) string {
	if errors.Is(err, ErrAuditExportQuota) {
		return "LIMIT_EXCEEDED"
	}
	if errors.Is(err, context.Canceled) {
		return "WORKER_CANCELLED"
	}
	return "GENERATION_FAILED"
}

func encryptAEAD(key, plaintext, additionalData []byte) ([]byte, []byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, aead.Seal(nil, nonce, plaintext, additionalData), nil
}

func decryptAEAD(key, nonce, ciphertext, additionalData []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, errors.New("invalid encrypted audit key nonce")
	}
	return aead.Open(nil, nonce, ciphertext, additionalData)
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func hmacHex(key []byte, values ...[]byte) string {
	mac := hmac.New(sha256.New, key)
	for _, value := range values {
		_, _ = mac.Write(value)
		_, _ = mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func maskAuditReference(key []byte, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return "ref_" + hmacHex(key, []byte(value))[:24]
}

func safeFilenamePart(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			builder.WriteRune(character)
		}
	}
	if builder.Len() == 0 {
		return "unknown"
	}
	return builder.String()
}

func safeAuditCSVCell(value string) string {
	first, _ := utf8.DecodeRuneInString(value)
	if value != "" && strings.ContainsRune("=+-@\t\r\n＝＋－＠", first) {
		return "'" + value
	}
	return value
}
