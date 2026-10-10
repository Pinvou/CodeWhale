package billing

import (
	"context"
	"encoding/json"
	"errors"
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
		return r.commitAccountClosureIsolation(ctx, tx, result)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AccountClosureGrantIsolation{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "account-closed-grants:"+userID.String()); err != nil {
		return AccountClosureGrantIsolation{}, err
	}
	walletRows, err := tx.Query(ctx, `SELECT DISTINCT wallet.id FROM wallets wallet JOIN member_quota_grants quota ON quota.organization_id=wallet.owner_id WHERE wallet.owner_type='ORGANIZATION' AND quota.member_user_id=$1 AND quota.status IN ('ACTIVE','SUSPENDED') ORDER BY wallet.id`, userID)
	if err != nil {
		return AccountClosureGrantIsolation{}, err
	}
	walletIDs := make([]uuid.UUID, 0)
	for walletRows.Next() {
		var id uuid.UUID
		if err := walletRows.Scan(&id); err != nil {
			walletRows.Close()
			return AccountClosureGrantIsolation{}, err
		}
		walletIDs = append(walletIDs, id)
	}
	err = walletRows.Err()
	walletRows.Close()
	if err != nil {
		return AccountClosureGrantIsolation{}, err
	}
	for _, id := range walletIDs {
		if _, err := tx.Exec(ctx, `SELECT id FROM wallets WHERE id=$1 FOR UPDATE`, id); err != nil {
			return AccountClosureGrantIsolation{}, err
		}
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
	if err := rows.Err(); err != nil {
		rows.Close()
		return AccountClosureGrantIsolation{}, err
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
	return r.commitAccountClosureIsolation(ctx, tx, result)
}

func (r *PostgresRepository) commitAccountClosureIsolation(ctx context.Context, tx pgx.Tx, result AccountClosureGrantIsolation) (AccountClosureGrantIsolation, error) {
	if err := tx.Commit(ctx); err != nil {
		return AccountClosureGrantIsolation{}, err
	}
	// Scrub contact metadata outside the financial wallet/row-lock transaction:
	// payer orders can belong to other wallets. The immutable isolation receipt
	// is durable first; a missing scrub ACK retries it without re-isolating money.
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET mobile_e164=NULL WHERE payer_user_id=$1 AND mobile_e164 IS NOT NULL`, result.MemberUserID); err != nil {
		return AccountClosureGrantIsolation{}, err
	}
	return result, nil
}
