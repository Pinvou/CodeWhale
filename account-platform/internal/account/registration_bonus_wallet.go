package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

// RegistrationBonusWalletProvisioner extends ordinary wallet provisioning only for
// the immutable EVALUATE job facts committed by the registration transaction.
type RegistrationBonusWalletProvisioner interface {
	ProvisionRegistrationWallet(context.Context, RegistrationWalletProvisionRequest) error
}

// These are the mature registration worker's existing request fields; no raw
// mobile or new wallet-read capability is added to the internal contract.
type RegistrationWalletProvisionRequest struct {
	UserID                  uuid.UUID `json:"user_id"`
	RegistrationOperationID uuid.UUID `json:"registration_operation_id"`
	FingerprintKeyID        string    `json:"fingerprint_key_id"`
	MobileFingerprint       string    `json:"mobile_fingerprint"`
}

type registrationWalletReceipt struct {
	RegistrationOperationID uuid.UUID  `json:"registration_operation_id"`
	UserID                  uuid.UUID  `json:"user_id"`
	WalletID                uuid.UUID  `json:"wallet_id"`
	ClaimID                 uuid.UUID  `json:"claim_id"`
	PolicyRevision          *int64     `json:"policy_revision"`
	Outcome                 string     `json:"outcome"`
	GrantedMicrocredits     *int64     `json:"granted_microcredits"`
	CreatedAt               *time.Time `json:"created_at"`
}

func (p *HTTPWalletProvisioner) ProvisionRegistrationWallet(ctx context.Context, input RegistrationWalletProvisionRequest) error {
	input.FingerprintKeyID = strings.TrimSpace(input.FingerprintKeyID)
	fingerprint, decodeErr := hex.DecodeString(strings.TrimSpace(input.MobileFingerprint))
	if p == nil || p.baseURL == nil || p.client == nil || input.UserID == uuid.Nil || input.RegistrationOperationID == uuid.Nil || input.FingerprintKeyID == "" || len(input.FingerprintKeyID) > 100 || !utf8.ValidString(input.FingerprintKeyID) || strings.ContainsRune(input.FingerprintKeyID, 0) || decodeErr != nil || len(fingerprint) != sha256.Size {
		return ErrWalletProvisioning
	}
	input.MobileFingerprint = hex.EncodeToString(fingerprint)
	body, err := json.Marshal(input)
	if err != nil {
		return ErrWalletProvisioning
	}
	endpoint := *p.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/registration-bonus-grants"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return ErrWalletProvisioning
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "registration-bonus:"+input.RegistrationOperationID.String())
	if serviceauth.SetAuthorization(request, p.serviceToken) != nil {
		return ErrWalletProvisioning
	}
	response, err := p.client.Do(request)
	if err != nil {
		return ErrWalletProvisioning
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return ErrWalletProvisioning
	}
	var receipt registrationWalletReceipt
	if decodeAuthorityResponse(response.Body, &receipt) != nil || !receipt.matches(input.UserID, input.RegistrationOperationID) {
		return ErrWalletProvisioning
	}
	// The gift receipt has no wallet owner/status/version fields. Confirm those
	// facts with the existing ensure authority rather than treating HTTP 2xx or
	// an operation receipt alone as a READY wallet acknowledgement.
	walletID, err := p.personalWalletAuthority(ctx, input.UserID)
	if err != nil || walletID != receipt.WalletID {
		return ErrWalletProvisioning
	}
	return nil
}

func (receipt registrationWalletReceipt) matches(userID, operationID uuid.UUID) bool {
	if receipt.RegistrationOperationID != operationID || receipt.UserID != userID || receipt.WalletID == uuid.Nil || receipt.ClaimID == uuid.Nil || receipt.PolicyRevision == nil || *receipt.PolicyRevision < 1 || receipt.GrantedMicrocredits == nil || receipt.CreatedAt == nil || receipt.CreatedAt.IsZero() {
		return false
	}
	switch receipt.Outcome {
	case "GRANTED":
		return *receipt.GrantedMicrocredits > 0 && *receipt.GrantedMicrocredits%10000 == 0
	case "NOT_GRANTED_DISABLED", "NOT_GRANTED_ZERO", "ALREADY_CLAIMED":
		return *receipt.GrantedMicrocredits == 0
	default:
		return false
	}
}

// personalWalletAuthority retains the already-delivered personal wallet ACK
// facts. The existing error-only EnsurePersonalWallet interface can delegate to
// this helper when the reviewed predecessor is assembled; ordinary callers do
// not acquire registration-bonus capability or a new wallet-read permission.
func (p *HTTPWalletProvisioner) personalWalletAuthority(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	if p == nil || p.baseURL == nil || p.client == nil || userID == uuid.Nil {
		return uuid.Nil, ErrWalletProvisioning
	}
	body, err := json.Marshal(struct {
		OwnerType string    `json:"owner_type"`
		OwnerID   uuid.UUID `json:"owner_id"`
	}{"PERSONAL", userID})
	if err != nil {
		return uuid.Nil, ErrWalletProvisioning
	}
	endpoint := *p.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/wallets/ensure"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return uuid.Nil, ErrWalletProvisioning
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "personal-wallet:"+userID.String())
	if serviceauth.SetAuthorization(request, p.serviceToken) != nil {
		return uuid.Nil, ErrWalletProvisioning
	}
	response, err := p.client.Do(request)
	if err != nil {
		return uuid.Nil, ErrWalletProvisioning
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return uuid.Nil, ErrWalletProvisioning
	}
	var wallet struct {
		ID        uuid.UUID `json:"wallet_id"`
		OwnerType string    `json:"owner_type"`
		OwnerID   uuid.UUID `json:"owner_id"`
		Status    string    `json:"status"`
		Version   int64     `json:"version"`
	}
	if decodeAuthorityResponse(response.Body, &wallet) != nil || wallet.ID == uuid.Nil || wallet.OwnerType != "PERSONAL" || wallet.OwnerID != userID || wallet.Status != "ACTIVE" || wallet.Version < 1 {
		return uuid.Nil, ErrWalletProvisioning
	}
	return wallet.ID, nil
}
