package account

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
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

func (r *PostgresRepository) LookupUserByCode(ctx context.Context, principal identity.Principal, rawCode, source, device string) (AdminUser, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AdminUser{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return AdminUser{}, err
	}
	normalized, validationErr := NormalizeUserCode(rawCode)
	if strings.TrimSpace(source) == "" {
		source = "actor:" + actorID.String()
	}
	if strings.TrimSpace(device) == "" {
		device = "session:" + principal.SessionID
	}
	sourceHash := sha256.Sum256([]byte(strings.TrimSpace(source)))
	deviceHash := sha256.Sum256([]byte(strings.TrimSpace(device)))
	targetHash := sha256.Sum256([]byte(strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(rawCode), "-", ""))))

	var limited bool
	err = tx.QueryRow(ctx, `
SELECT
  (SELECT count(*) >= 30 FROM user_code_lookup_attempts WHERE actor_user_id=$1 AND occurred_at > now()-interval '1 minute')
  OR (SELECT count(*) >= 60 FROM user_code_lookup_attempts WHERE source_hash=$2 AND occurred_at > now()-interval '1 minute')
  OR (SELECT count(*) >= 30 FROM user_code_lookup_attempts WHERE device_hash=$3 AND occurred_at > now()-interval '1 minute')
  OR (SELECT count(*) >= 10 FROM user_code_lookup_attempts WHERE target_code_hash=$4 AND occurred_at > now()-interval '1 minute')`, actorID, sourceHash[:], deviceHash[:], targetHash[:]).Scan(&limited)
	if err != nil {
		return AdminUser{}, err
	}
	if limited {
		if err := recordUserCodeLookup(ctx, tx, principal, actorID, sourceHash, deviceHash, targetHash, "RATE_LIMITED"); err != nil {
			return AdminUser{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AdminUser{}, err
		}
		return AdminUser{}, ErrUserCodeLookupRateLimited
	}

	var allowed bool
	err = tx.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM role_assignments ra
  JOIN role_permissions rp ON rp.role_code=ra.role_code
  WHERE ra.user_id=$1 AND ra.organization_id IS NULL AND rp.permission_code=$2
    AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
)`, actorID, PermissionPlatformUsersRead).Scan(&allowed)
	if err != nil {
		return AdminUser{}, err
	}
	if !allowed || validationErr != nil {
		if err := recordUserCodeLookup(ctx, tx, principal, actorID, sourceHash, deviceHash, targetHash, "DENIED"); err != nil {
			return AdminUser{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return AdminUser{}, err
		}
		return AdminUser{}, ErrNotFound
	}

	var item AdminUser
	err = tx.QueryRow(ctx, `
SELECT u.id,u.user_code,u.username,u.display_name,u.email,u.status,
       (SELECT count(*) FROM memberships m WHERE m.user_id=u.id AND m.status='ACTIVE'),
       (SELECT max(s.last_seen_at) FROM user_sessions s WHERE s.user_id=u.id),
       u.created_at,u.updated_at
FROM users u WHERE u.user_code=$1`, normalized).Scan(
		&item.ID, &item.UserCode, &item.Username, &item.DisplayName, &item.Email, &item.Status,
		&item.OrganizationCount, &item.LastSeenAt, &item.CreatedAt, &item.UpdatedAt,
	)
	result := "SUCCEEDED"
	if errors.Is(err, pgx.ErrNoRows) {
		result = "NOT_FOUND"
	} else if err != nil {
		return AdminUser{}, err
	}
	if err := recordUserCodeLookup(ctx, tx, principal, actorID, sourceHash, deviceHash, targetHash, result); err != nil {
		return AdminUser{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AdminUser{}, err
	}
	if result == "NOT_FOUND" {
		return AdminUser{}, ErrNotFound
	}
	item.UserCode = FormatUserCode(item.UserCode)
	return item, nil
}

func recordUserCodeLookup(ctx context.Context, tx pgx.Tx, principal identity.Principal, actorID uuid.UUID, sourceHash, deviceHash, targetHash [32]byte, result string) error {
	if _, err := tx.Exec(ctx, `
INSERT INTO user_code_lookup_attempts(id,actor_user_id,source_hash,device_hash,target_code_hash,result,occurred_at)
VALUES($1,$2,$3,$4,$5,$6,now())`, uuid.Must(uuid.NewV7()), actorID, sourceHash[:], deviceHash[:], targetHash[:], result); err != nil {
		return err
	}
	auditResult := "DENIED"
	if result == "SUCCEEDED" {
		auditResult = "SUCCEEDED"
	}
	return appendAudit(ctx, tx, actorID, principal.SessionID, nil, "user.code.lookup", "user_code", hex.EncodeToString(targetHash[:]), auditResult, map[string]any{"lookup_result": result})
}

func (s *Service) adminUserByCode(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(userCodeLookupRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "account administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.LookupUserByCode(r.Context(), mustPrincipal(r.Context()), r.PathValue("user_code"), s.requestSource(r), r.Header.Get("X-Device-ID"))
	if errors.Is(err, ErrUserCodeLookupRateLimited) {
		api.WriteError(w, http.StatusTooManyRequests, "LOOKUP_RATE_LIMITED", "the user lookup could not be completed", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
		api.WriteError(w, http.StatusNotFound, "USER_NOT_FOUND", "the user lookup could not be completed", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) requestSource(r *http.Request) string {
	proof := r.Header.Get("X-Pinvou-Client-Source-Authorization")
	trusted := s.serviceToken != "" && subtle.ConstantTimeCompare([]byte(proof), []byte(s.serviceToken)) == 1
	if source := strings.TrimSpace(r.Header.Get("X-Pinvou-Client-Source")); trusted && net.ParseIP(source) != nil {
		return source
	}
	if host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr)); err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}
