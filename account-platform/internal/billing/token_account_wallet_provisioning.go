package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type TokenAccountWalletProvisioningReceipt struct {
	CommandReference uuid.UUID  `json:"command_reference"`
	OwnerType        string     `json:"owner_type"`
	OwnerID          uuid.UUID  `json:"owner_id"`
	RequestDigest    string     `json:"request_digest"`
	Status           string     `json:"status"`
	WalletID         *uuid.UUID `json:"wallet_id"`
	StableErrorCode  *string    `json:"stable_error_code"`
	AttemptCount     int        `json:"attempt_count"`
	CreatedAt        time.Time  `json:"created_at"`
	CompletedAt      *time.Time `json:"completed_at"`
}

type tokenAccountWalletProvisioningStore interface {
	ProvisionTokenAccountWallet(context.Context, uuid.UUID, string, uuid.UUID, string) (TokenAccountWalletProvisioningReceipt, bool, error)
	TokenAccountWalletProvisioningReceipt(context.Context, uuid.UUID) (TokenAccountWalletProvisioningReceipt, error)
}

func tokenAccountWalletRequestDigest(ownerType string, ownerID uuid.UUID) string {
	digest := sha256.Sum256([]byte(ownerType + "\n" + ownerID.String() + "\n"))
	return hex.EncodeToString(digest[:])
}

func (s *Service) provisionTokenAccountWallet(w http.ResponseWriter, r *http.Request) {
	store, ok := s.repository.(tokenAccountWalletProvisioningStore)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "WALLET_PROVISIONING_UNAVAILABLE", "wallet provisioning is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		CommandReference uuid.UUID `json:"command_reference"`
		OwnerType        string    `json:"owner_type"`
		OwnerID          uuid.UUID `json:"owner_id"`
		RequestDigest    string    `json:"request_digest"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	body.OwnerType = strings.ToUpper(strings.TrimSpace(body.OwnerType))
	body.RequestDigest = strings.TrimSpace(body.RequestDigest)
	if body.CommandReference == uuid.Nil || body.OwnerID == uuid.Nil || (body.OwnerType != "PERSONAL" && body.OwnerType != "ORGANIZATION" && body.OwnerType != "ORG_UNIT") || body.RequestDigest != tokenAccountWalletRequestDigest(body.OwnerType, body.OwnerID) {
		api.WriteError(w, http.StatusUnprocessableEntity, "INVALID_WALLET_PROVISIONING_REQUEST", "wallet provisioning request is invalid", httpserver.RequestID(r.Context()))
		return
	}
	receipt, created, err := store.ProvisionTokenAccountWallet(r.Context(), body.CommandReference, body.OwnerType, body.OwnerID, body.RequestDigest)
	if errors.Is(err, ErrConflict) {
		api.WriteError(w, http.StatusConflict, "WALLET_PROVISIONING_RECEIPT_MISMATCH", "wallet provisioning receipt does not match", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "WALLET_PROVISIONING_UNAVAILABLE", "wallet provisioning is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	api.WriteJSON(w, status, receipt)
}

func (s *Service) getTokenAccountWalletProvisioningReceipt(w http.ResponseWriter, r *http.Request) {
	store, ok := s.repository.(tokenAccountWalletProvisioningStore)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "WALLET_PROVISIONING_UNAVAILABLE", "wallet provisioning is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	reference, err := uuid.Parse(r.PathValue("command_reference"))
	if err != nil || reference == uuid.Nil {
		api.WriteError(w, http.StatusUnprocessableEntity, "INVALID_WALLET_PROVISIONING_REQUEST", "wallet provisioning reference is invalid", httpserver.RequestID(r.Context()))
		return
	}
	receipt, err := store.TokenAccountWalletProvisioningReceipt(r.Context(), reference)
	if errors.Is(err, ErrNotFound) {
		api.WriteError(w, http.StatusNotFound, "WALLET_PROVISIONING_RECEIPT_NOT_FOUND", "wallet provisioning receipt was not found", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "WALLET_PROVISIONING_UNAVAILABLE", "wallet provisioning is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, receipt)
}

const tokenAccountWalletReceiptSelect = `SELECT command_reference,owner_type,owner_id,request_digest,status,wallet_id,stable_error_code,attempt_count,created_at,completed_at FROM token_account_wallet_provisioning_receipts`

func scanTokenAccountWalletReceipt(row pgx.Row) (TokenAccountWalletProvisioningReceipt, error) {
	var v TokenAccountWalletProvisioningReceipt
	err := row.Scan(&v.CommandReference, &v.OwnerType, &v.OwnerID, &v.RequestDigest, &v.Status, &v.WalletID, &v.StableErrorCode, &v.AttemptCount, &v.CreatedAt, &v.CompletedAt)
	return v, err
}

func (r *PostgresRepository) TokenAccountWalletProvisioningReceipt(ctx context.Context, reference uuid.UUID) (TokenAccountWalletProvisioningReceipt, error) {
	v, err := scanTokenAccountWalletReceipt(r.pool.QueryRow(ctx, tokenAccountWalletReceiptSelect+` WHERE command_reference=$1`, reference))
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

// ProvisionTokenAccountWallet commits only the original command receipt and a
// zero-balance wallet if needed. The shared owner lock and wallet-row lock retain
// current lifecycle fences; replay returns the immutable historical result first.
func (r *PostgresRepository) ProvisionTokenAccountWallet(ctx context.Context, reference uuid.UUID, ownerType string, ownerID uuid.UUID, requestDigest string) (TokenAccountWalletProvisioningReceipt, bool, error) {
	if reference == uuid.Nil || ownerID == uuid.Nil || (ownerType != "PERSONAL" && ownerType != "ORGANIZATION" && ownerType != "ORG_UNIT") || requestDigest != tokenAccountWalletRequestDigest(ownerType, ownerID) {
		return TokenAccountWalletProvisioningReceipt{}, false, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return TokenAccountWalletProvisioningReceipt{}, false, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "token-account-wallet-reference", reference.String()); err != nil {
		return TokenAccountWalletProvisioningReceipt{}, false, err
	}
	if existing, err := scanTokenAccountWalletReceipt(tx.QueryRow(ctx, tokenAccountWalletReceiptSelect+` WHERE command_reference=$1`, reference)); err == nil {
		if existing.OwnerType != ownerType || existing.OwnerID != ownerID || existing.RequestDigest != requestDigest {
			return TokenAccountWalletProvisioningReceipt{}, false, ErrConflict
		}
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return TokenAccountWalletProvisioningReceipt{}, false, err
	}
	wallet, _, err := r.ensureWalletWithinTransaction(ctx, tx, ownerType, ownerID)
	if err != nil {
		return TokenAccountWalletProvisioningReceipt{}, false, err
	}
	// Lifecycle projections and EnsureWallet share the owner advisory lock.
	// Financial and closure writers use the wallet row; retain that lock too.
	wallet, err = getWallet(ctx, tx, wallet.ID, true)
	if err != nil {
		return TokenAccountWalletProvisioningReceipt{}, false, err
	}
	if wallet.Status != "ACTIVE" {
		return TokenAccountWalletProvisioningReceipt{}, false, ErrWalletUnavailable
	}
	if ownerType == "ORG_UNIT" {
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_unit_lifecycle_states WHERE unit_id=$1 AND status<>'ACTIVE')`, ownerID).Scan(&pending); err != nil {
			return TokenAccountWalletProvisioningReceipt{}, false, err
		}
		if pending {
			return TokenAccountWalletProvisioningReceipt{}, false, ErrWalletUnavailable
		}
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `INSERT INTO token_account_wallet_provisioning_receipts(command_reference,owner_type,owner_id,request_digest,status,wallet_id,attempt_count,created_at,completed_at) VALUES($1,$2,$3,$4,'SUCCEEDED',$5,1,$6,$6)`, reference, ownerType, ownerID, requestDigest, wallet.ID, now); err != nil {
		return TokenAccountWalletProvisioningReceipt{}, false, err
	}
	receipt, err := scanTokenAccountWalletReceipt(tx.QueryRow(ctx, tokenAccountWalletReceiptSelect+` WHERE command_reference=$1`, reference))
	if err != nil {
		return TokenAccountWalletProvisioningReceipt{}, false, err
	}
	return receipt, true, tx.Commit(ctx)
}
