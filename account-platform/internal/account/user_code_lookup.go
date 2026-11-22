package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrUserCodeLookupRateLimited = errors.New("user code lookup rate limited")

type userCodeLookupRepository interface {
	LookupUserByCode(context.Context, identity.Principal, string, string, string) (AdminUser, error)
}

// LookupUserByCode owns authorization so denied, invalid and absent codes have
// the same response and durable digest-only attempt/audit. A middleware denial
// must not bypass this transaction or record the raw code in a denial URL.
func (r *PostgresRepository) LookupUserByCode(ctx context.Context, principal identity.Principal, rawCode, source, device string) (AdminUser, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return AdminUser{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return AdminUser{}, err
	}
	if strings.TrimSpace(source) == "" {
		source = "actor:" + actorID.String()
	}
	if strings.TrimSpace(device) == "" {
		device = "session:" + principal.SessionID
	}
	sourceHash := sha256.Sum256([]byte(strings.TrimSpace(source)))
	deviceHash := sha256.Sum256([]byte(strings.TrimSpace(device)))
	targetHash := sha256.Sum256([]byte(strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(rawCode), "-", ""))))
	// All lookups acquire the same globally ordered dimension locks before USER.
	// ReadCommitted then samples each count after lock acquisition; a snapshot
	// taken before waiting cannot admit a burst using stale minute counters.
	locks := []string{"actor:" + actorID.String(), "source:" + hex.EncodeToString(sourceHash[:]), "device:" + hex.EncodeToString(deviceHash[:]), "target:" + hex.EncodeToString(targetHash[:])}
	sort.Strings(locks)
	for _, key := range locks {
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('user-code-lookup:' || $1,0))`, key); err != nil {
			return AdminUser{}, err
		}
	}
	var lockedID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT u.id FROM users u JOIN identities i ON i.user_id=u.id WHERE u.id=$1 AND i.issuer=$2 AND i.subject=$3 AND u.status='ACTIVE' FOR UPDATE OF u`, actorID, principal.Issuer, principal.Subject).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminUser{}, ErrForbidden
	}
	if err != nil {
		return AdminUser{}, err
	}
	var limited bool
	err = tx.QueryRow(ctx, `SELECT
 (SELECT count(*)>=30 FROM user_code_lookup_attempts WHERE actor_user_id=$1 AND occurred_at>clock_timestamp()-interval '1 minute')
 OR (SELECT count(*)>=60 FROM user_code_lookup_attempts WHERE source_hash=$2 AND occurred_at>clock_timestamp()-interval '1 minute')
 OR (SELECT count(*)>=30 FROM user_code_lookup_attempts WHERE device_hash=$3 AND occurred_at>clock_timestamp()-interval '1 minute')
 OR (SELECT count(*)>=10 FROM user_code_lookup_attempts WHERE target_code_hash=$4 AND occurred_at>clock_timestamp()-interval '1 minute')`, actorID, sourceHash[:], deviceHash[:], targetHash[:]).Scan(&limited)
	if err != nil {
		return AdminUser{}, err
	}
	finish := func(result string) error {
		if e := recordUserCodeLookup(ctx, tx, principal, actorID, sourceHash, deviceHash, targetHash, result); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	if limited {
		if err = finish("RATE_LIMITED"); err != nil {
			return AdminUser{}, err
		}
		return AdminUser{}, ErrUserCodeLookupRateLimited
	}
	normalized, invalid := NormalizeUserCode(rawCode)
	decision, authErr := authorizePermission(ctx, tx, principal, PermissionPlatformUsersRead, nil, nil, true)
	if authErr != nil && !errors.Is(authErr, ErrForbidden) {
		return AdminUser{}, authErr
	}
	if invalid != nil || authErr != nil || !decision.Allowed {
		if err = finish("DENIED"); err != nil {
			return AdminUser{}, err
		}
		return AdminUser{}, ErrNotFound
	}
	var item AdminUser
	err = tx.QueryRow(ctx, `SELECT id,user_code,username,display_name,status,
 (SELECT max(last_seen_at) FROM user_sessions WHERE user_id=u.id),created_at,updated_at
 FROM users u WHERE user_code=$1`, normalized).Scan(&item.ID, &item.UserCode, &item.Username, &item.DisplayName, &item.Status, &item.LastSeenAt, &item.CreatedAt, &item.UpdatedAt)
	result := "SUCCEEDED"
	if errors.Is(err, pgx.ErrNoRows) {
		result = "NOT_FOUND"
	} else if err != nil {
		return AdminUser{}, err
	}
	if err = finish(result); err != nil {
		return AdminUser{}, err
	}
	if result == "NOT_FOUND" {
		return AdminUser{}, ErrNotFound
	}
	item.UserCode = FormatUserCode(item.UserCode)
	return item, nil
}

func recordUserCodeLookup(ctx context.Context, tx pgx.Tx, principal identity.Principal, actorID uuid.UUID, sourceHash, deviceHash, targetHash [32]byte, result string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO user_code_lookup_attempts(id,actor_user_id,source_hash,device_hash,target_code_hash,result,occurred_at)
 VALUES($1,$2,$3,$4,$5,$6,clock_timestamp())`, uuid.Must(uuid.NewV7()), actorID, sourceHash[:], deviceHash[:], targetHash[:], result); err != nil {
		return err
	}
	auditResult := "DENIED"
	if result == "SUCCEEDED" {
		auditResult = "SUCCEEDED"
	}
	return appendAudit(ctx, tx, actorID, principal.SessionID, nil, "user.code.lookup", "user_code", hex.EncodeToString(targetHash[:]), auditResult, map[string]any{"lookup_result": result})
}

func (s *Service) adminUserByCode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	repository, ok := s.repository.(userCodeLookupRepository)
	if !ok {
		api.WriteError(w, 503, "ADMIN_UNAVAILABLE", "account administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.LookupUserByCode(r.Context(), mustPrincipal(r.Context()), r.PathValue("user_code"), requestSource(r), r.Header.Get("X-Device-ID"))
	if errors.Is(err, ErrUserCodeLookupRateLimited) {
		api.WriteError(w, 429, "LOOKUP_RATE_LIMITED", "the user lookup could not be completed", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
		api.WriteError(w, 404, "USER_NOT_FOUND", "the user lookup could not be completed", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, 200, item)
}
