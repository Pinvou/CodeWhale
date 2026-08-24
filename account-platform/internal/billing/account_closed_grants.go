package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AccountClosureGrantIsolation struct {
	ClosureRequestID        uuid.UUID `json:"closure_request_id"`
	MemberUserID            uuid.UUID `json:"member_user_id"`
	SubjectLifecycleVersion int64     `json:"subject_lifecycle_version"`
	IsolatedGrantCount      int       `json:"isolated_grant_count"`
	Replayed                bool      `json:"replayed"`
}

type AccountClosedGrantReclaim struct {
	Grant                   MemberQuotaGrant `json:"grant"`
	AmountMicrocredits      int64            `json:"amount_microcredits"`
	SubjectLifecycleVersion int64            `json:"subject_lifecycle_version"`
	Replayed                bool             `json:"replayed"`
}

type AccountClosureStateVerifier interface {
	RequireAnonymized(context.Context, uuid.UUID, int64) error
}

type HTTPAccountClosureStateVerifier struct {
	baseURL      *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPAccountClosureStateVerifier(rawURL, serviceToken string) (*HTTPAccountClosureStateVerifier, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("account closure state verifier requires an absolute Account URL and service token")
	}
	return &HTTPAccountClosureStateVerifier{baseURL: parsed, serviceToken: strings.TrimSpace(serviceToken), client: observability.NewHTTPClient(5 * time.Second)}, nil
}

func (v *HTTPAccountClosureStateVerifier) RequireAnonymized(ctx context.Context, userID uuid.UUID, lifecycleVersion int64) error {
	endpoint := *v.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/users/" + userID.String() + "/account-closure-state"
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err := serviceauth.SetAuthorization(request, v.serviceToken); err != nil {
		return err
	}
	response, err := v.client.Do(request)
	if err != nil {
		return fmt.Errorf("account closure state request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return ErrConflict
	}
	var state struct {
		Stage                   string `json:"stage"`
		SubjectLifecycleVersion int64  `json:"subject_lifecycle_version"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&state) != nil || (state.Stage != "ANONYMIZED" && state.Stage != "COMPLETED") || state.SubjectLifecycleVersion != lifecycleVersion {
		return ErrConflict
	}
	return nil
}

func (r *PostgresRepository) IsolateAccountClosedGrants(ctx context.Context, closureID, userID uuid.UUID, lifecycleVersion int64) (AccountClosureGrantIsolation, error) {
	if closureID == uuid.Nil || userID == uuid.Nil || lifecycleVersion < 2 {
		return AccountClosureGrantIsolation{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AccountClosureGrantIsolation{}, err
	}
	defer tx.Rollback(ctx)
	var snapshot []byte
	err = tx.QueryRow(ctx, `SELECT response_snapshot FROM account_closure_grant_isolations WHERE closure_request_id=$1`, closureID).Scan(&snapshot)
	if err == nil {
		var result AccountClosureGrantIsolation
		if json.Unmarshal(snapshot, &result) != nil || result.MemberUserID != userID || result.SubjectLifecycleVersion != lifecycleVersion {
			return AccountClosureGrantIsolation{}, ErrIdempotencyConflict
		}
		result.Replayed = true
		return result, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AccountClosureGrantIsolation{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "account-closed-grants:"+userID.String()); err != nil {
		return AccountClosureGrantIsolation{}, err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM member_quota_grants WHERE member_user_id=$1 AND status IN ('ACTIVE','SUSPENDED') ORDER BY component_id,id FOR UPDATE`, userID)
	if err != nil {
		return AccountClosureGrantIsolation{}, err
	}
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return AccountClosureGrantIsolation{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	now := r.now()
	for _, id := range ids {
		grant, _, err := loadMemberQuotaGrant(ctx, tx, id, false)
		if err != nil {
			return AccountClosureGrantIsolation{}, err
		}
		if grant.ReservedMicrocredits != 0 {
			return AccountClosureGrantIsolation{}, ErrConflict
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM entitlement_components WHERE id=$1 FOR UPDATE`, grant.ComponentID); err != nil {
			return AccountClosureGrantIsolation{}, err
		}
		if grant.Status == "ACTIVE" && grant.AvailableMicrocredits > 0 {
			command, err := tx.Exec(ctx, `UPDATE entitlement_components SET active_grant_microcredits=active_grant_microcredits-$2,suspended_grant_microcredits=suspended_grant_microcredits+$2,updated_at=$3 WHERE id=$1 AND active_grant_microcredits>=$2`, grant.ComponentID, grant.AvailableMicrocredits, now)
			if err != nil || command.RowsAffected() != 1 {
				if err == nil {
					err = ErrConflict
				}
				return AccountClosureGrantIsolation{}, err
			}
		}
		reason := "ACCOUNT_CLOSED"
		if _, err := tx.Exec(ctx, `UPDATE member_quota_grants SET status='SUSPENDED',suspension_reason=$2,allocation_version=allocation_version+1,updated_at=$3 WHERE id=$1`, grant.ID, reason, now); err != nil {
			return AccountClosureGrantIsolation{}, err
		}
		grant.Status, grant.SuspensionReason, grant.AllocationVersion, grant.UpdatedAt = "SUSPENDED", &reason, grant.AllocationVersion+1, now
		if err := appendMemberQuotaAudit(ctx, tx, grant, "ACCOUNT_CLOSED_ISOLATED", nil, grant.AvailableMicrocredits, now); err != nil {
			return AccountClosureGrantIsolation{}, err
		}
		if err := appendMemberQuotaOutbox(ctx, tx, grant, "billing.member-quota.account-closed.v1", now); err != nil {
			return AccountClosureGrantIsolation{}, err
		}
	}
	result := AccountClosureGrantIsolation{ClosureRequestID: closureID, MemberUserID: userID, SubjectLifecycleVersion: lifecycleVersion, IsolatedGrantCount: len(ids)}
	snapshot, _ = json.Marshal(result)
	if _, err := tx.Exec(ctx, `INSERT INTO account_closure_grant_isolations(closure_request_id,member_user_id,subject_lifecycle_version,isolated_grant_count,response_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6)`, closureID, userID, lifecycleVersion, len(ids), snapshot, now); err != nil {
		return AccountClosureGrantIsolation{}, err
	}
	return result, tx.Commit(ctx)
}

func (r *PostgresRepository) ReclaimAccountClosedGrant(ctx context.Context, organizationID, grantID, actorID uuid.UUID, lifecycleVersion int64, reason, idempotencyKey string) (AccountClosedGrantReclaim, error) {
	reason, idempotencyKey = strings.TrimSpace(reason), strings.TrimSpace(idempotencyKey)
	if organizationID == uuid.Nil || grantID == uuid.Nil || actorID == uuid.Nil || lifecycleVersion < 2 || reason == "" || len(reason) > 500 || len(idempotencyKey) < 16 || len(idempotencyKey) > 128 {
		return AccountClosedGrantReclaim{}, ErrConflict
	}
	digest := sha256.Sum256([]byte("account-closed-reclaim-v1\n" + organizationID.String() + "\n" + grantID.String() + "\n" + reason))
	fingerprint := hex.EncodeToString(digest[:])
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AccountClosedGrantReclaim{}, err
	}
	defer tx.Rollback(ctx)
	var storedFingerprint string
	var snapshot []byte
	err = tx.QueryRow(ctx, `SELECT request_fingerprint,response_snapshot FROM account_closed_grant_reclaim_commands WHERE organization_id=$1 AND idempotency_key=$2`, organizationID, idempotencyKey).Scan(&storedFingerprint, &snapshot)
	if err == nil {
		if storedFingerprint != fingerprint {
			return AccountClosedGrantReclaim{}, ErrIdempotencyConflict
		}
		var result AccountClosedGrantReclaim
		if json.Unmarshal(snapshot, &result) != nil {
			return AccountClosedGrantReclaim{}, ErrConflict
		}
		result.Replayed = true
		return result, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AccountClosedGrantReclaim{}, err
	}
	grant, _, err := loadMemberQuotaGrant(ctx, tx, grantID, true)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && grant.OrganizationID != organizationID) {
		return AccountClosedGrantReclaim{}, ErrNotFound
	}
	if err != nil {
		return AccountClosedGrantReclaim{}, err
	}
	if grant.Status != "SUSPENDED" || grant.SuspensionReason == nil || *grant.SuspensionReason != "ACCOUNT_CLOSED" || !grant.ExpiresAt.After(r.now()) || grant.ReservedMicrocredits != 0 || grant.AvailableMicrocredits == 0 {
		return AccountClosedGrantReclaim{}, ErrConflict
	}
	var frozen bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
SELECT 1 FROM member_quota_hold_allocations a JOIN usage_holds h ON h.id=a.hold_id WHERE a.grant_id=$1 AND h.status IN ('ACTIVE','REVIEW')
UNION ALL SELECT 1 FROM refunds WHERE entitlement_package_id=$2 AND status IN ('REQUESTED','MANUAL_REVIEW')
UNION ALL SELECT 1 FROM chargeback_cases WHERE entitlement_package_id=$2 AND status='REVIEW_REQUIRED'
UNION ALL SELECT 1 FROM reconciliation_cases c JOIN payment_orders p ON p.id=c.payment_order_id WHERE p.entitlement_package_id=$2 AND c.status IN ('OPEN','IN_REVIEW')
UNION ALL SELECT 1 FROM payment_anomaly_cases anomaly JOIN payment_orders payment ON payment.id=anomaly.payment_order_id WHERE payment.entitlement_package_id=$2 AND anomaly.status='OPEN'
)`, grant.ID, grant.PackageID).Scan(&frozen); err != nil {
		return AccountClosedGrantReclaim{}, err
	}
	if frozen {
		return AccountClosedGrantReclaim{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM entitlement_components WHERE id=$1 FOR UPDATE`, grant.ComponentID); err != nil {
		return AccountClosedGrantReclaim{}, err
	}
	now, amount := r.now(), grant.AvailableMicrocredits
	command, err := tx.Exec(ctx, `UPDATE entitlement_components SET suspended_grant_microcredits=suspended_grant_microcredits-$2,available_microcredits=available_microcredits+$2,updated_at=$3 WHERE id=$1 AND suspended_grant_microcredits>=$2`, grant.ComponentID, amount, now)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return AccountClosedGrantReclaim{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE member_quota_grants SET available_microcredits=0,revoked_microcredits=revoked_microcredits+$2,status='RECLAIMED_ACCOUNT_CLOSED',suspension_reason=NULL,allocation_version=allocation_version+1,updated_at=$3 WHERE id=$1`, grant.ID, amount, now); err != nil {
		return AccountClosedGrantReclaim{}, err
	}
	grant.AvailableMicrocredits, grant.RevokedMicrocredits, grant.Status, grant.SuspensionReason, grant.AllocationVersion, grant.UpdatedAt = 0, grant.RevokedMicrocredits+amount, "RECLAIMED_ACCOUNT_CLOSED", nil, grant.AllocationVersion+1, now
	if err := appendMemberQuotaAudit(ctx, tx, grant, "RECLAIMED_ACCOUNT_CLOSED", &actorID, amount, now); err != nil {
		return AccountClosedGrantReclaim{}, err
	}
	if err := appendMemberQuotaOutbox(ctx, tx, grant, "billing.member-quota.account-closed-reclaimed.v1", now); err != nil {
		return AccountClosedGrantReclaim{}, err
	}
	result := AccountClosedGrantReclaim{Grant: grant, AmountMicrocredits: amount, SubjectLifecycleVersion: lifecycleVersion}
	snapshot, _ = json.Marshal(result)
	if _, err := tx.Exec(ctx, `INSERT INTO account_closed_grant_reclaim_commands(organization_id,idempotency_key,request_fingerprint,grant_id,actor_user_id,reason,response_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, organizationID, idempotencyKey, fingerprint, grant.ID, actorID, reason, snapshot, now); err != nil {
		return AccountClosedGrantReclaim{}, err
	}
	return result, tx.Commit(ctx)
}

func (r *PostgresRepository) AccountClosedGrantCandidate(ctx context.Context, organizationID, grantID uuid.UUID) (MemberQuotaGrant, int64, error) {
	var grant MemberQuotaGrant
	var lifecycleVersion int64
	err := scanMemberQuotaGrant(r.pool.QueryRow(ctx, `SELECT grant.id,grant.organization_id,grant.member_user_id,grant.package_id,grant.component_id,grant.component_type,grant.original_microcredits,grant.available_microcredits,grant.reserved_microcredits,grant.used_microcredits,grant.expired_microcredits,grant.revoked_microcredits,grant.status,grant.suspension_reason,grant.membership_version,grant.allocation_version,grant.created_by,grant.expires_at,grant.created_at,grant.updated_at FROM member_quota_grants grant WHERE grant.id=$1 AND grant.organization_id=$2 AND EXISTS(SELECT 1 FROM account_closure_grant_isolations isolation WHERE isolation.member_user_id=grant.member_user_id)`, grantID, organizationID), &grant)
	if err == nil {
		// scanMemberQuotaGrant intentionally owns the stable grant projection; fetch
		// the closure version separately to avoid changing that shared contract.
		err = r.pool.QueryRow(ctx, `SELECT isolation.subject_lifecycle_version FROM account_closure_grant_isolations isolation JOIN member_quota_grants grant ON grant.member_user_id=isolation.member_user_id WHERE grant.id=$1 AND grant.organization_id=$2`, grantID, organizationID).Scan(&lifecycleVersion)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return MemberQuotaGrant{}, 0, ErrNotFound
	}
	return grant, lifecycleVersion, err
}

func (s *Service) isolateAccountClosedGrants(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		IsolateAccountClosedGrants(context.Context, uuid.UUID, uuid.UUID, int64) (AccountClosureGrantIsolation, error)
	})
	userID, err := uuid.Parse(r.PathValue("user_id"))
	var body struct {
		ClosureRequestID        uuid.UUID `json:"closure_request_id"`
		SubjectLifecycleVersion int64     `json:"subject_lifecycle_version"`
	}
	if !ok || err != nil || api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid account closure grant isolation", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.IsolateAccountClosedGrants(r.Context(), body.ClosureRequestID, userID, body.SubjectLifecycleVersion)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) reclaimAccountClosedGrantInternal(w http.ResponseWriter, r *http.Request) {
	organizationID, organizationErr := uuid.Parse(r.PathValue("organization_id"))
	grantID, grantErr := uuid.Parse(r.PathValue("grant_id"))
	var body struct {
		ActorUserID uuid.UUID `json:"actor_user_id"`
		Reason      string    `json:"reason"`
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if organizationErr != nil || grantErr != nil || api.DecodeJSON(w, r, &body) != nil || body.ActorUserID == uuid.Nil || len(key) < 16 || len(key) > 128 || strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid account-closed grant reclaim", httpserver.RequestID(r.Context()))
		return
	}
	if s.closureStates == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_CLOSURE_UNAVAILABLE", "account closure verification is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		AccountClosedGrantCandidate(context.Context, uuid.UUID, uuid.UUID) (MemberQuotaGrant, int64, error)
		ReclaimAccountClosedGrant(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64, string, string) (AccountClosedGrantReclaim, error)
	})
	if !ok {
		s.writeError(w, r, ErrConflict)
		return
	}
	grant, lifecycleVersion, err := repository.AccountClosedGrantCandidate(r.Context(), organizationID, grantID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.closureStates.RequireAnonymized(r.Context(), grant.MemberUserID, lifecycleVersion); err != nil {
		s.writeError(w, r, err)
		return
	}
	result, err := repository.ReclaimAccountClosedGrant(r.Context(), organizationID, grantID, body.ActorUserID, lifecycleVersion, body.Reason, key)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}
