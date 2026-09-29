package billing

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrRegistrationBonusInvalid      = errors.New("registration bonus request is invalid")
	ErrRegistrationBonusConflict     = errors.New("registration bonus policy changed")
	ErrRegistrationBonusKeyReused    = errors.New("registration bonus idempotency key was reused")
	ErrRegistrationBonusReplayChange = errors.New("registration bonus provisioning replay changed")
)

const registrationBonusMicrocreditsPerPoint int64 = 10000

type RegistrationBonusPolicy struct {
	Revision                       int64     `json:"revision"`
	Enabled                        bool      `json:"enabled"`
	AmountMicrocredits             int64     `json:"amount_microcredits"`
	ValidityDays                   int       `json:"validity_days"`
	UpdatedAt                      time.Time `json:"updated_at"`
	EligibilityStatus              string    `json:"eligibility_status"`
	RecoverableRegistrationCount   int64     `json:"recoverable_registration_count"`
	UnrecoverableRegistrationCount int64     `json:"unrecoverable_registration_count"`
}

type RegistrationBonusProvisionRequest struct {
	UserID                  uuid.UUID `json:"user_id"`
	RegistrationOperationID uuid.UUID `json:"registration_operation_id"`
	FingerprintKeyID        string    `json:"fingerprint_key_id"`
	MobileFingerprint       string    `json:"mobile_fingerprint"`
}

type RegistrationBonusReceipt struct {
	RegistrationOperationID uuid.UUID `json:"registration_operation_id"`
	UserID                  uuid.UUID `json:"user_id"`
	WalletID                uuid.UUID `json:"wallet_id"`
	ClaimID                 uuid.UUID `json:"claim_id"`
	PolicyRevision          int64     `json:"policy_revision"`
	Outcome                 string    `json:"outcome"`
	GrantedMicrocredits     int64     `json:"granted_microcredits"`
	CreatedAt               time.Time `json:"created_at"`
}

type registrationBonusRepository interface {
	ActiveRegistrationBonusPolicy(context.Context) (RegistrationBonusPolicy, error)
	ReplayRegistrationBonusPolicy(context.Context, uuid.UUID, bool, int64, int, int64, string, string) (RegistrationBonusPolicy, bool, error)
	UpdateRegistrationBonusPolicy(context.Context, uuid.UUID, bool, int64, int, int64, string, string) (RegistrationBonusPolicy, bool, error)
	ReplayRegistrationBonusProvision(context.Context, RegistrationBonusProvisionRequest) (RegistrationBonusReceipt, bool, error)
	ProvisionRegistrationBonus(context.Context, RegistrationBonusProvisionRequest) (RegistrationBonusReceipt, bool, error)
}

func (r *PostgresRepository) ReplayRegistrationBonusPolicy(ctx context.Context, actor uuid.UUID, enabled bool, amount int64, validityDays int, expectedRevision int64, reason, idempotencyKey string) (RegistrationBonusPolicy, bool, error) {
	reason, idempotencyKey = strings.TrimSpace(reason), strings.TrimSpace(idempotencyKey)
	if actor == uuid.Nil || amount < 0 || amount%registrationBonusMicrocreditsPerPoint != 0 || validityDays < 1 || validityDays > 3650 || expectedRevision < 1 || len(reason) < 10 || len(reason) > 500 || len(idempotencyKey) < 16 || len(idempotencyKey) > 128 {
		return RegistrationBonusPolicy{}, false, ErrRegistrationBonusInvalid
	}
	hash := registrationBonusPolicyHash(enabled, amount, validityDays, expectedRevision, reason)
	var replayHash []byte
	var replayRevision int64
	err := r.pool.QueryRow(ctx, `SELECT request_hash,resulting_revision FROM registration_bonus_policy_commands WHERE actor_user_id=$1 AND idempotency_key=$2`, actor, idempotencyKey).Scan(&replayHash, &replayRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegistrationBonusPolicy{}, false, nil
	}
	if err != nil {
		return RegistrationBonusPolicy{}, false, err
	}
	if subtle.ConstantTimeCompare(replayHash, hash[:]) != 1 {
		return RegistrationBonusPolicy{}, false, ErrRegistrationBonusKeyReused
	}
	policy, err := scanRegistrationBonusPolicy(r.pool.QueryRow(ctx, `SELECT revision,enabled,amount_microcredits,validity_days,created_at FROM registration_bonus_policy_versions WHERE revision=$1`, replayRevision))
	return policy, err == nil, err
}

func (r *PostgresRepository) ActiveRegistrationBonusPolicy(ctx context.Context) (RegistrationBonusPolicy, error) {
	var policy RegistrationBonusPolicy
	err := r.pool.QueryRow(ctx, `SELECT version.revision,version.enabled,version.amount_microcredits,version.validity_days,state.updated_at
FROM registration_bonus_policy_state state JOIN registration_bonus_policy_versions version ON version.revision=state.active_revision
WHERE state.singleton=TRUE`).Scan(&policy.Revision, &policy.Enabled, &policy.AmountMicrocredits, &policy.ValidityDays, &policy.UpdatedAt)
	return policy, err
}

func registrationBonusPolicyHash(enabled bool, amount int64, validityDays int, expectedRevision int64, reason string) [32]byte {
	raw, _ := json.Marshal(struct {
		Enabled          bool   `json:"enabled"`
		Amount           int64  `json:"amount_microcredits"`
		ValidityDays     int    `json:"validity_days"`
		ExpectedRevision int64  `json:"expected_revision"`
		Reason           string `json:"reason"`
	}{enabled, amount, validityDays, expectedRevision, reason})
	return sha256.Sum256(raw)
}

func (r *PostgresRepository) UpdateRegistrationBonusPolicy(ctx context.Context, actor uuid.UUID, enabled bool, amount int64, validityDays int, expectedRevision int64, reason, idempotencyKey string) (RegistrationBonusPolicy, bool, error) {
	reason, idempotencyKey = strings.TrimSpace(reason), strings.TrimSpace(idempotencyKey)
	if actor == uuid.Nil || amount < 0 || amount%registrationBonusMicrocreditsPerPoint != 0 || validityDays < 1 || validityDays > 3650 || expectedRevision < 1 || len(reason) < 10 || len(reason) > 500 || len(idempotencyKey) < 16 || len(idempotencyKey) > 128 {
		return RegistrationBonusPolicy{}, false, ErrRegistrationBonusInvalid
	}
	hash := registrationBonusPolicyHash(enabled, amount, validityDays, expectedRevision, reason)
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RegistrationBonusPolicy{}, false, err
	}
	defer tx.Rollback(ctx)
	// The idempotency lock must be acquired before reading the command row. At
	// READ COMMITTED, a concurrent replay observes the committed command after
	// it wakes up instead of keeping a stale transaction snapshot.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "registration-bonus-policy:"+actor.String()+":"+idempotencyKey); err != nil {
		return RegistrationBonusPolicy{}, false, err
	}
	var replayHash []byte
	var replayRevision int64
	err = tx.QueryRow(ctx, `SELECT request_hash,resulting_revision FROM registration_bonus_policy_commands WHERE actor_user_id=$1 AND idempotency_key=$2`, actor, idempotencyKey).Scan(&replayHash, &replayRevision)
	if err == nil {
		if subtle.ConstantTimeCompare(replayHash, hash[:]) != 1 {
			return RegistrationBonusPolicy{}, false, ErrRegistrationBonusKeyReused
		}
		policy, loadErr := scanRegistrationBonusPolicy(tx.QueryRow(ctx, `SELECT revision,enabled,amount_microcredits,validity_days,created_at FROM registration_bonus_policy_versions WHERE revision=$1`, replayRevision))
		if loadErr != nil {
			return RegistrationBonusPolicy{}, false, loadErr
		}
		return policy, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RegistrationBonusPolicy{}, false, err
	}
	var current RegistrationBonusPolicy
	if err = tx.QueryRow(ctx, `SELECT version.revision,version.enabled,version.amount_microcredits,version.validity_days,state.updated_at
FROM registration_bonus_policy_state state JOIN registration_bonus_policy_versions version ON version.revision=state.active_revision
WHERE state.singleton=TRUE FOR UPDATE OF state`).Scan(&current.Revision, &current.Enabled, &current.AmountMicrocredits, &current.ValidityDays, &current.UpdatedAt); err != nil {
		return RegistrationBonusPolicy{}, false, err
	}
	if current.Revision != expectedRevision {
		return RegistrationBonusPolicy{}, false, ErrRegistrationBonusConflict
	}
	now, next := r.now(), expectedRevision+1
	if _, err = tx.Exec(ctx, `INSERT INTO registration_bonus_policy_versions(revision,enabled,amount_microcredits,validity_days,actor_user_id,reason,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, next, enabled, amount, validityDays, actor, reason, now); err != nil {
		return RegistrationBonusPolicy{}, false, err
	}
	command, err := tx.Exec(ctx, `UPDATE registration_bonus_policy_state SET revision=$1,active_revision=$1,updated_at=$2 WHERE singleton=TRUE AND revision=$3`, next, now, expectedRevision)
	if err != nil {
		return RegistrationBonusPolicy{}, false, err
	}
	if command.RowsAffected() != 1 {
		return RegistrationBonusPolicy{}, false, ErrRegistrationBonusConflict
	}
	if _, err = tx.Exec(ctx, `INSERT INTO registration_bonus_policy_commands(actor_user_id,idempotency_key,request_hash,resulting_revision,created_at) VALUES($1,$2,$3,$4,$5)`, actor, idempotencyKey, hash[:], next, now); err != nil {
		return RegistrationBonusPolicy{}, false, err
	}
	before, _ := json.Marshal(current)
	updated := RegistrationBonusPolicy{Revision: next, Enabled: enabled, AmountMicrocredits: amount, ValidityDays: validityDays, UpdatedAt: now}
	after, _ := json.Marshal(updated)
	if _, err = tx.Exec(ctx, `INSERT INTO registration_bonus_policy_audit(id,actor_user_id,action,reason,before_snapshot,after_snapshot,occurred_at) VALUES($1,$2,'POLICY_UPDATED',$3,$4,$5,$6)`, uuid.Must(uuid.NewV7()), actor, reason, before, after, now); err != nil {
		return RegistrationBonusPolicy{}, false, err
	}
	return updated, false, tx.Commit(ctx)
}

func scanRegistrationBonusPolicy(row pgx.Row) (RegistrationBonusPolicy, error) {
	var policy RegistrationBonusPolicy
	err := row.Scan(&policy.Revision, &policy.Enabled, &policy.AmountMicrocredits, &policy.ValidityDays, &policy.UpdatedAt)
	return policy, err
}

func registrationBonusProvisionHash(request RegistrationBonusProvisionRequest, fingerprint []byte) [32]byte {
	return sha256.Sum256([]byte(request.UserID.String() + "\n" + request.RegistrationOperationID.String() + "\n" + request.FingerprintKeyID + "\n" + hex.EncodeToString(fingerprint)))
}

func (r *PostgresRepository) ReplayRegistrationBonusProvision(ctx context.Context, request RegistrationBonusProvisionRequest) (RegistrationBonusReceipt, bool, error) {
	request.FingerprintKeyID, request.MobileFingerprint = strings.TrimSpace(request.FingerprintKeyID), strings.TrimSpace(request.MobileFingerprint)
	fingerprint, err := hex.DecodeString(request.MobileFingerprint)
	if request.UserID == uuid.Nil || request.RegistrationOperationID == uuid.Nil || request.FingerprintKeyID == "" || len(request.FingerprintKeyID) > 100 || len(fingerprint) != sha256.Size || err != nil {
		return RegistrationBonusReceipt{}, false, ErrRegistrationBonusInvalid
	}
	request.MobileFingerprint = hex.EncodeToString(fingerprint)
	hash := registrationBonusProvisionHash(request, fingerprint)
	receipt, replayHash, err := scanRegistrationBonusReceipt(r.pool.QueryRow(ctx, `SELECT registration_operation_id,user_id,wallet_id,claim_id,policy_revision,outcome,granted_microcredits,created_at,request_hash FROM registration_bonus_receipts WHERE registration_operation_id=$1`, request.RegistrationOperationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RegistrationBonusReceipt{}, false, nil
	}
	if err != nil {
		return RegistrationBonusReceipt{}, false, err
	}
	if subtle.ConstantTimeCompare(replayHash, hash[:]) != 1 {
		return RegistrationBonusReceipt{}, false, ErrRegistrationBonusReplayChange
	}
	return receipt, true, nil
}

func (r *PostgresRepository) ProvisionRegistrationBonus(ctx context.Context, request RegistrationBonusProvisionRequest) (RegistrationBonusReceipt, bool, error) {
	request.FingerprintKeyID, request.MobileFingerprint = strings.TrimSpace(request.FingerprintKeyID), strings.TrimSpace(request.MobileFingerprint)
	fingerprint, err := hex.DecodeString(request.MobileFingerprint)
	if request.UserID == uuid.Nil || request.RegistrationOperationID == uuid.Nil || request.FingerprintKeyID == "" || len(request.FingerprintKeyID) > 100 || len(fingerprint) != sha256.Size || err != nil {
		return RegistrationBonusReceipt{}, false, ErrRegistrationBonusInvalid
	}
	request.MobileFingerprint = hex.EncodeToString(fingerprint)
	hash := registrationBonusProvisionHash(request, fingerprint)
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RegistrationBonusReceipt{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "registration-bonus-operation:"+request.RegistrationOperationID.String()); err != nil {
		return RegistrationBonusReceipt{}, false, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "registration-bonus:"+request.MobileFingerprint); err != nil {
		return RegistrationBonusReceipt{}, false, err
	}
	receipt, replayHash, loadErr := scanRegistrationBonusReceipt(tx.QueryRow(ctx, `SELECT registration_operation_id,user_id,wallet_id,claim_id,policy_revision,outcome,granted_microcredits,created_at,request_hash FROM registration_bonus_receipts WHERE registration_operation_id=$1`, request.RegistrationOperationID))
	if loadErr == nil {
		if subtle.ConstantTimeCompare(replayHash, hash[:]) != 1 {
			return RegistrationBonusReceipt{}, false, ErrRegistrationBonusReplayChange
		}
		return receipt, false, tx.Commit(ctx)
	}
	if !errors.Is(loadErr, pgx.ErrNoRows) {
		return RegistrationBonusReceipt{}, false, loadErr
	}
	wallet, _, err := r.ensureWalletWithinTransaction(ctx, tx, "PERSONAL", request.UserID)
	if err != nil || wallet.Status != "ACTIVE" {
		if err == nil {
			err = ErrWalletUnavailable
		}
		return RegistrationBonusReceipt{}, false, err
	}
	var policy RegistrationBonusPolicy
	if err = tx.QueryRow(ctx, `SELECT version.revision,version.enabled,version.amount_microcredits,version.validity_days,state.updated_at
FROM registration_bonus_policy_state state JOIN registration_bonus_policy_versions version ON version.revision=state.active_revision
WHERE state.singleton=TRUE FOR SHARE OF state`).Scan(&policy.Revision, &policy.Enabled, &policy.AmountMicrocredits, &policy.ValidityDays, &policy.UpdatedAt); err != nil {
		return RegistrationBonusReceipt{}, false, err
	}
	now, claimID := r.now(), uuid.Must(uuid.NewV7())
	outcome, granted := "NOT_GRANTED_DISABLED", int64(0)
	if policy.Enabled {
		outcome = "NOT_GRANTED_ZERO"
		if policy.AmountMicrocredits > 0 {
			outcome, granted = "GRANTED", policy.AmountMicrocredits
		}
	}
	var entitlementPackageID *uuid.UUID
	if granted > 0 {
		value := uuid.Must(uuid.NewV7())
		entitlementPackageID = &value
	}
	var insertedClaim uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO registration_bonus_claims(id,fingerprint_key_id,mobile_fingerprint,first_registration_operation_id,first_user_id,policy_revision,outcome,granted_microcredits,entitlement_package_id,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(mobile_fingerprint) DO NOTHING RETURNING id`, claimID, request.FingerprintKeyID, fingerprint, request.RegistrationOperationID, request.UserID, policy.Revision, outcome, granted, entitlementPackageID, now).Scan(&insertedClaim)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = tx.QueryRow(ctx, `SELECT id,policy_revision FROM registration_bonus_claims WHERE mobile_fingerprint=$1`, fingerprint).Scan(&claimID, &policy.Revision); err != nil {
			return RegistrationBonusReceipt{}, false, err
		}
		outcome, granted = "ALREADY_CLAIMED", 0
	} else if err != nil {
		return RegistrationBonusReceipt{}, false, err
	} else if granted > 0 {
		grantErr := r.writeRegistrationBonusGrant(ctx, tx, wallet, claimID, *entitlementPackageID, granted, policy.ValidityDays, now)
		if grantErr != nil {
			return RegistrationBonusReceipt{}, false, grantErr
		}
	}
	receipt = RegistrationBonusReceipt{RegistrationOperationID: request.RegistrationOperationID, UserID: request.UserID, WalletID: wallet.ID, ClaimID: claimID, PolicyRevision: policy.Revision, Outcome: outcome, GrantedMicrocredits: granted, CreatedAt: now}
	if _, err = tx.Exec(ctx, `INSERT INTO registration_bonus_receipts(registration_operation_id,user_id,request_hash,wallet_id,claim_id,policy_revision,outcome,granted_microcredits,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, receipt.RegistrationOperationID, receipt.UserID, hash[:], receipt.WalletID, receipt.ClaimID, receipt.PolicyRevision, receipt.Outcome, receipt.GrantedMicrocredits, receipt.CreatedAt); err != nil {
		return RegistrationBonusReceipt{}, false, err
	}
	return receipt, true, tx.Commit(ctx)
}

func (r *PostgresRepository) writeRegistrationBonusGrant(ctx context.Context, tx pgx.Tx, wallet Wallet, claimID, packageID uuid.UUID, amount int64, validityDays int, now time.Time) error {
	if wallet.PostedMicrocredits > math.MaxInt64-amount {
		return ErrConflict
	}
	entryID, lotID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	reference, expiresAt := "registration-bonus:"+claimID.String(), now.AddDate(0, 0, validityDays)
	if err := insertJournal(ctx, tx, entryID, "ADJUSTMENT", reference, "new registration bonus", now); err != nil {
		return err
	}
	walletAccount, err := walletLedgerAccount(ctx, tx, wallet.ID)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'PROMOTION',$3,$4,$4,FALSE,$5,$6)`, lotID, wallet.ID, reference, amount, expiresAt, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,NULL,$4,$9,$9,'',0,$5,$5,$6,$6,$7,'ACTIVE',$6,$6,'SYSTEM',$8)`, packageID, wallet.OwnerID, wallet.ID, lotID, amount, now, expiresAt, reference, registrationBonusProductID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BONUS',$1,$3,$3,$4,$5,'ACTIVE',$4,$4)`, lotID, packageID, amount, now, expiresAt); err != nil {
		return err
	}
	if err = insertPosting(ctx, tx, entryID, walletAccount, amount, &lotID, now); err != nil {
		return err
	}
	if err = insertPosting(ctx, tx, entryID, adjustmentAccount, -amount, nil, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits+$2,version=version+1,updated_at=$3 WHERE id=$1`, wallet.ID, amount, now); err != nil {
		return err
	}
	return nil
}

func scanRegistrationBonusReceipt(row pgx.Row) (RegistrationBonusReceipt, []byte, error) {
	var receipt RegistrationBonusReceipt
	var hash []byte
	err := row.Scan(&receipt.RegistrationOperationID, &receipt.UserID, &receipt.WalletID, &receipt.ClaimID, &receipt.PolicyRevision, &receipt.Outcome, &receipt.GrantedMicrocredits, &receipt.CreatedAt, &hash)
	return receipt, hash, err
}

func (s *Service) internalProvisionRegistrationBonus(w http.ResponseWriter, r *http.Request) {
	var request RegistrationBonusProvisionRequest
	if s.registrationBonuses == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "REGISTRATION_BONUS_UNAVAILABLE", "registration bonus provisioning is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if err := api.DecodeJSON(w, r, &request); err != nil {
		return
	}
	if receipt, replayed, err := s.registrationBonuses.ReplayRegistrationBonusProvision(r.Context(), request); err != nil {
		s.writeRegistrationBonusError(w, r, err)
		return
	} else if replayed {
		api.WriteJSON(w, http.StatusOK, receipt)
		return
	}
	if s.registrationBonusEligibility == nil {
		s.writeRegistrationBonusError(w, r, ErrRegistrationBonusEligibilityUnavailable)
		return
	}
	if readinessErr := s.registrationBonusEligibility.RequireReady(r.Context()); readinessErr != nil {
		s.writeRegistrationBonusError(w, r, readinessErr)
		return
	}
	receipt, created, err := s.registrationBonuses.ProvisionRegistrationBonus(r.Context(), request)
	if err != nil {
		s.writeRegistrationBonusError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	api.WriteJSON(w, status, receipt)
}

func (s *Service) adminRegistrationBonusPolicy(w http.ResponseWriter, r *http.Request) {
	if s.registrationBonuses == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "REGISTRATION_BONUS_UNAVAILABLE", "registration bonus policy is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	policy, err := s.registrationBonuses.ActiveRegistrationBonusPolicy(r.Context())
	if err != nil {
		s.writeRegistrationBonusError(w, r, err)
		return
	}
	if s.registrationBonusEligibility == nil {
		s.writeRegistrationBonusError(w, r, ErrRegistrationBonusEligibilityUnavailable)
		return
	}
	readiness, err := s.registrationBonusEligibility.Readiness(r.Context())
	if err != nil {
		s.writeRegistrationBonusError(w, r, err)
		return
	}
	policy.EligibilityStatus = readiness.Status
	policy.RecoverableRegistrationCount = readiness.RecoverableRegistrationCount
	policy.UnrecoverableRegistrationCount = readiness.UnrecoverableRegistrationCount
	api.WriteJSON(w, http.StatusOK, policy)
}

func (s *Service) adminUpdateRegistrationBonusPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled          bool   `json:"enabled"`
		AmountPoints     int64  `json:"amount_points"`
		ValidityDays     int    `json:"validity_days"`
		ExpectedRevision int64  `json:"expected_revision"`
		Reason           string `json:"reason"`
	}
	if s.registrationBonuses == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "REGISTRATION_BONUS_UNAVAILABLE", "registration bonus policy is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	principal, actor, ok := catalogActor(r)
	if !ok || principal.Subject == "" || body.AmountPoints < 0 || body.AmountPoints > math.MaxInt64/registrationBonusMicrocreditsPerPoint {
		s.writeRegistrationBonusError(w, r, ErrRegistrationBonusInvalid)
		return
	}
	amountMicrocredits := body.AmountPoints * registrationBonusMicrocreditsPerPoint
	replayedPolicy, replayed, replayErr := s.registrationBonuses.ReplayRegistrationBonusPolicy(r.Context(), actor, body.Enabled, amountMicrocredits, body.ValidityDays, body.ExpectedRevision, body.Reason, r.Header.Get("Idempotency-Key"))
	if replayErr != nil {
		s.writeRegistrationBonusError(w, r, replayErr)
		return
	}
	if replayed {
		s.enrichRegistrationBonusPolicyReadiness(r.Context(), &replayedPolicy)
		api.WriteJSON(w, http.StatusOK, replayedPolicy)
		return
	}
	if body.Enabled {
		if s.registrationBonusEligibility == nil {
			s.writeRegistrationBonusError(w, r, ErrRegistrationBonusEligibilityUnavailable)
			return
		}
		if readinessErr := s.registrationBonusEligibility.RequireReady(r.Context()); readinessErr != nil {
			s.writeRegistrationBonusError(w, r, readinessErr)
			return
		}
	}
	policy, _, err := s.registrationBonuses.UpdateRegistrationBonusPolicy(r.Context(), actor, body.Enabled, amountMicrocredits, body.ValidityDays, body.ExpectedRevision, body.Reason, r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.writeRegistrationBonusError(w, r, err)
		return
	}
	s.enrichRegistrationBonusPolicyReadiness(r.Context(), &policy)
	api.WriteJSON(w, http.StatusOK, policy)
}

func (s *Service) enrichRegistrationBonusPolicyReadiness(ctx context.Context, policy *RegistrationBonusPolicy) {
	policy.EligibilityStatus = "UNAVAILABLE"
	if s.registrationBonusEligibility == nil {
		return
	}
	if readiness, err := s.registrationBonusEligibility.Readiness(ctx); err == nil {
		policy.EligibilityStatus = readiness.Status
		policy.RecoverableRegistrationCount = readiness.RecoverableRegistrationCount
		policy.UnrecoverableRegistrationCount = readiness.UnrecoverableRegistrationCount
	}
}

func (s *Service) writeRegistrationBonusError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrRegistrationBonusInvalid):
		api.WriteError(w, http.StatusBadRequest, "REGISTRATION_BONUS_INVALID", "registration bonus request is invalid", httpserver.RequestID(r.Context()))
	case errors.Is(err, ErrRegistrationBonusConflict):
		api.WriteError(w, http.StatusConflict, "REGISTRATION_BONUS_CONFLICT", "registration bonus policy changed; reload and retry", httpserver.RequestID(r.Context()))
	case errors.Is(err, ErrRegistrationBonusKeyReused), errors.Is(err, ErrRegistrationBonusReplayChange):
		api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "idempotent request payload changed", httpserver.RequestID(r.Context()))
	case errors.Is(err, ErrRegistrationBonusEligibilityNotReady):
		api.WriteError(w, http.StatusConflict, "REGISTRATION_BONUS_ELIGIBILITY_NOT_READY", "historical mobile eligibility backfill is not ready; policy activation is blocked", httpserver.RequestID(r.Context()))
	case errors.Is(err, ErrRegistrationBonusEligibilityUnavailable):
		api.WriteError(w, http.StatusServiceUnavailable, "REGISTRATION_BONUS_ELIGIBILITY_UNAVAILABLE", "registration bonus eligibility authority is unavailable", httpserver.RequestID(r.Context()))
	default:
		s.writeError(w, r, err)
	}
}
