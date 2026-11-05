package token

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokenauthority"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type WalletProvisioningReceipt struct {
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

type WalletProvisioningClient interface {
	ProvisionTokenAccountWallet(context.Context, uuid.UUID, string, uuid.UUID, string) (WalletProvisioningReceipt, error)
	TokenAccountWalletProvisioningReceipt(context.Context, uuid.UUID) (WalletProvisioningReceipt, error)
}

func validWalletProvisioningReceipt(receipt WalletProvisioningReceipt) bool {
	if receipt.CommandReference == uuid.Nil || receipt.OwnerID == uuid.Nil || receipt.RequestDigest == "" || receipt.AttemptCount < 1 || receipt.CreatedAt.IsZero() || !slices.Contains([]string{"PERSONAL", "ORGANIZATION", "ORG_UNIT"}, receipt.OwnerType) {
		return false
	}
	switch receipt.Status {
	case "PENDING":
		return receipt.WalletID == nil && receipt.StableErrorCode == nil && receipt.CompletedAt == nil
	case "SUCCEEDED":
		return receipt.WalletID != nil && *receipt.WalletID != uuid.Nil && receipt.StableErrorCode == nil && receipt.CompletedAt != nil && !receipt.CompletedAt.Before(receipt.CreatedAt)
	case "FAILED", "UNKNOWN":
		return receipt.WalletID == nil && receipt.StableErrorCode != nil && validStableTokenErrorCode(*receipt.StableErrorCode) && receipt.CompletedAt != nil && !receipt.CompletedAt.Before(receipt.CreatedAt)
	default:
		return false
	}
}

type OwnerAuthorityCheck = tokenauthority.Request
type OwnerAuthorityDecision = tokenauthority.Response

type OwnerAuthorityClient interface {
	CheckTokenOwnerAuthority(context.Context, OwnerAuthorityCheck) (OwnerAuthorityDecision, error)
}

func walletProvisioningDigest(ownerType string, ownerID uuid.UUID) string {
	d := sha256.Sum256([]byte(ownerType + "\n" + ownerID.String() + "\n"))
	return hex.EncodeToString(d[:])
}
func (r *HTTPWalletResolver) ProvisionTokenAccountWallet(ctx context.Context, reference uuid.UUID, ownerType string, ownerID uuid.UUID, digest string) (WalletProvisioningReceipt, error) {
	var result WalletProvisioningReceipt
	err := r.callProvisioning(ctx, http.MethodPost, "/internal/v1/token-account-wallet-provisioning-receipts", map[string]any{"command_reference": reference, "owner_type": ownerType, "owner_id": ownerID, "request_digest": digest}, &result)
	return result, err
}
func (r *HTTPWalletResolver) TokenAccountWalletProvisioningReceipt(ctx context.Context, reference uuid.UUID) (WalletProvisioningReceipt, error) {
	var result WalletProvisioningReceipt
	err := r.callProvisioning(ctx, http.MethodGet, "/internal/v1/token-account-wallet-provisioning-receipts/"+reference.String(), nil, &result)
	return result, err
}
func (r *HTTPWalletResolver) callProvisioning(ctx context.Context, method, path string, input any, output any) error {
	var body io.Reader
	if input != nil {
		raw, _ := json.Marshal(input)
		body = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, r.baseURL.String()+path, body)
	if err != nil {
		return err
	}
	if err = serviceauth.SetAuthorization(request, r.serviceToken); err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return &BillingHTTPError{Status: response.StatusCode}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	decoder.DisallowUnknownFields()
	return decoder.Decode(output)
}

type HTTPOwnerAuthorityClient struct {
	endpoint     *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPOwnerAuthorityClient(rawURL, serviceToken string) (*HTTPOwnerAuthorityClient, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("invalid owner authority configuration")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/internal/v1/token-account-authority/snapshot"
	return &HTTPOwnerAuthorityClient{endpoint: parsed, serviceToken: serviceToken, client: observability.NewHTTPClient(5 * time.Second)}, nil
}
func (c *HTTPOwnerAuthorityClient) CheckTokenOwnerAuthority(ctx context.Context, input OwnerAuthorityCheck) (OwnerAuthorityDecision, error) {
	input.AuthorityRequestSHA256 = input.Digest()
	if err := input.Validate(); err != nil {
		return OwnerAuthorityDecision{}, err
	}
	raw, _ := json.Marshal(input)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		return OwnerAuthorityDecision{}, err
	}
	if err = serviceauth.SetAuthorization(request, c.serviceToken); err != nil {
		return OwnerAuthorityDecision{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return OwnerAuthorityDecision{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return OwnerAuthorityDecision{}, errors.New("owner authority unavailable")
	}
	var result OwnerAuthorityDecision
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&result); err != nil {
		return result, err
	}
	if result.AuthoritySchemaVersion != tokenauthority.SchemaVersion || result.Mode != input.Mode || result.CommandReference != input.CommandReference || result.AuthorityRequestSHA256 != input.AuthorityRequestSHA256 || result.ObservedAt.IsZero() || !equalAuthorityString(result.CommandRequestDigest, input.CommandRequestDigest) || !equalAuthorityString(result.ExpectedPlatformAuthorizationVersion, input.ExpectedPlatformAuthorizationVersion) || !equalAuthorityString(result.ExpectedActorVersion, input.ExpectedActorVersion) || !equalAuthorityString(result.ExpectedActorIdentityDigest, input.ExpectedActorIdentityDigest) || !equalAuthorityString(result.ExpectedOwnerVersion, input.ExpectedOwnerVersion) || !equalAuthorityString(result.ExpectedOwnerIdentityDigest, input.ExpectedOwnerIdentityDigest) {
		return result, errors.New("owner authority response mismatch")
	}
	actorID, actorErr := uuid.Parse(result.ActorUserID)
	actorVersion, actorVersionErr := strconv.ParseInt(result.ObservedActorVersion, 10, 64)
	platformVersion, platformVersionErr := strconv.ParseInt(result.ObservedPlatformAuthorizationVersion, 10, 64)
	if actorErr != nil || actorVersionErr != nil || platformVersionErr != nil || actorVersion < 1 || platformVersion < 1 || result.ActorIdentityDigest != tokenauthority.ActorIdentityDigest(actorID, result.ActorStatus, actorVersion) {
		return result, errors.New("owner authority actor evidence mismatch")
	}
	if input.Mode == tokenauthority.ModeOperatorAuthorize {
		if result.ObservedOwnerVersion != nil || result.OwnerStatus != nil || result.OwnerIdentityDigest != nil || result.OwnerOrganizationID != nil {
			return result, errors.New("operator authority leaked owner evidence")
		}
	} else {
		ownerID, ownerErr := uuid.Parse(*input.OwnerID)
		ownerVersion, ownerVersionErr := parseAuthorityVersion(result.ObservedOwnerVersion)
		if ownerErr != nil || ownerVersionErr != nil || result.OwnerStatus == nil || result.OwnerIdentityDigest == nil || !validAuthorityOwnerOrganization(*input.OwnerType, ownerID, result.OwnerOrganizationID) || *result.OwnerIdentityDigest != authorityOwnerDigest(*input.OwnerType, ownerID, result.OwnerOrganizationID, *result.OwnerStatus, ownerVersion) {
			return result, errors.New("owner authority owner evidence mismatch")
		}
	}
	validAllow := result.Decision == "ALLOW" && result.ReasonCode == nil && result.ActorStatus == "ACTIVE" && (input.Mode == tokenauthority.ModeOperatorAuthorize || result.OwnerStatus != nil && *result.OwnerStatus == "ACTIVE")
	validDeny := result.Decision == "DENY" && result.ReasonCode != nil && slices.Contains([]string{"ACTOR_NOT_FOUND", "ACTOR_INACTIVE", "AUTHORIZATION_REVOKED", "OWNER_NOT_FOUND", "OWNER_INACTIVE"}, *result.ReasonCode)
	if !validAllow && !validDeny {
		return result, errors.New("owner authority decision is not authoritative")
	}
	return result, nil
}

func equalAuthorityString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func parseAuthorityVersion(value *string) (int64, error) {
	if value == nil {
		return 0, errors.New("authority version is missing")
	}
	parsed, err := strconv.ParseInt(*value, 10, 64)
	if err != nil || parsed < 1 {
		return 0, errors.New("authority version is invalid")
	}
	return parsed, nil
}

type TokenAccountProvisioningReservation struct {
	CommandReference             uuid.UUID
	ActorUserID                  uuid.UUID
	OwnerType                    string
	OwnerID                      uuid.UUID
	OwnerOrganization            *uuid.UUID
	OwnerUnit                    *uuid.UUID
	AccountName                  string
	ShareScope                   string
	DefaultPriority              int
	CanonicalRequestDigest       string
	PlatformAuthorizationVersion int64
	ActorVersion                 int64
	ActorIdentityDigest          string
	OwnerVersion                 int64
	OwnerIdentityDigest          string
	PolicyVersion                int64
	ActivationEpoch              int64
	RecoveryEpoch                int64
	ReservationVersion           int64
	AttemptCount                 int
	ExpiresAt                    time.Time
	LeaseOwner                   string
	LeaseUntil                   time.Time
	LeaseFencingToken            uuid.UUID
}

type TokenAccountProvisioningWorker struct {
	repository *PostgresRepository
	wallets    WalletProvisioningClient
	authority  OwnerAuthorityClient
	workerID   string
	activation TokenCommandGovernanceActivation
}

func NewTokenAccountProvisioningWorker(repository *PostgresRepository, wallets WalletProvisioningClient, authority OwnerAuthorityClient, workerID string, activation TokenCommandGovernanceActivation) (*TokenAccountProvisioningWorker, error) {
	if repository == nil || wallets == nil || authority == nil || strings.TrimSpace(workerID) == "" || activation.State != "ACTIVE" || activation.Epoch < 1 {
		return nil, errors.New("invalid Token Account provisioning worker configuration")
	}
	return &TokenAccountProvisioningWorker{repository: repository, wallets: wallets, authority: authority, workerID: workerID, activation: activation}, nil
}

func (w *TokenAccountProvisioningWorker) ProcessOne(ctx context.Context) (bool, error) {
	item, err := w.repository.claimTokenAccountProvisioning(ctx, w.workerID, w.activation)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ownerType := map[string]string{"USER": "PERSONAL", "ORG": "ORGANIZATION", "ORG_UNIT": "ORG_UNIT"}[item.OwnerType]
	digest := walletProvisioningDigest(ownerType, item.OwnerID)
	receipt, err := w.wallets.ProvisionTokenAccountWallet(callCtx, item.CommandReference, ownerType, item.OwnerID, digest)
	cancel()
	if err != nil {
		lookupCtx, lookupCancel := context.WithTimeout(ctx, 5*time.Second)
		defer lookupCancel()
		receipt, err = w.wallets.TokenAccountWalletProvisioningReceipt(lookupCtx, item.CommandReference)
		if err != nil {
			return true, w.repository.releaseTokenAccountProvisioning(ctx, item, "")
		}
	}
	if !validWalletProvisioningReceipt(receipt) || receipt.CommandReference != item.CommandReference || receipt.OwnerType != ownerType || receipt.OwnerID != item.OwnerID || receipt.RequestDigest != digest {
		return true, w.repository.releaseTokenAccountProvisioning(ctx, item, "")
	}
	if receipt.Status == "FAILED" && receipt.StableErrorCode != nil {
		return true, w.repository.failTokenAccountProvisioning(ctx, item, "ACCOUNT_WALLET_PROVISIONING_FAILED")
	}
	if receipt.Status != "SUCCEEDED" || receipt.WalletID == nil || receipt.StableErrorCode != nil || receipt.CompletedAt == nil {
		return true, w.repository.releaseTokenAccountProvisioning(ctx, item, "")
	}
	check := creatorAuthorityRequest(item)
	authorityCtx, authorityCancel := context.WithTimeout(ctx, 5*time.Second)
	decision, err := w.authority.CheckTokenOwnerAuthority(authorityCtx, check)
	authorityCancel()
	if err != nil {
		return true, w.repository.releaseTokenAccountProvisioning(ctx, item, "")
	}
	observedPlatform, platformErr := strconv.ParseInt(decision.ObservedPlatformAuthorizationVersion, 10, 64)
	observedActor, actorErr := strconv.ParseInt(decision.ObservedActorVersion, 10, 64)
	observedOwner, ownerErr := parseAuthorityVersion(decision.ObservedOwnerVersion)
	if platformErr != nil || actorErr != nil || ownerErr != nil {
		return true, w.repository.releaseTokenAccountProvisioning(ctx, item, "")
	}
	if decision.Decision == "DENY" && decision.ReasonCode != nil {
		if (*decision.ReasonCode == "ACTOR_NOT_FOUND" || *decision.ReasonCode == "ACTOR_INACTIVE") && observedActor > item.ActorVersion {
			return true, w.repository.failTokenAccountProvisioning(ctx, item, "ACCOUNT_CREATOR_UNAVAILABLE")
		}
		if *decision.ReasonCode == "AUTHORIZATION_REVOKED" && observedPlatform > item.PlatformAuthorizationVersion {
			return true, w.repository.failTokenAccountProvisioning(ctx, item, "ACCOUNT_AUTHORIZATION_REVOKED")
		}
		if (*decision.ReasonCode == "OWNER_NOT_FOUND" || *decision.ReasonCode == "OWNER_INACTIVE") && observedOwner > item.OwnerVersion {
			return true, w.repository.failTokenAccountProvisioning(ctx, item, "ACCOUNT_OWNER_UNAVAILABLE")
		}
		return true, w.repository.releaseTokenAccountProvisioning(ctx, item, "")
	}
	return true, w.repository.finalizeTokenAccountProvisioning(ctx, item, receipt, decision)
}

func authorityOwnerType(ownerType string) string {
	if ownerType == "ORG" {
		return "ORGANIZATION"
	}
	return ownerType
}

func validAuthorityOwnerOrganization(ownerType string, owner uuid.UUID, organization *string) bool {
	if ownerType == "USER" {
		return organization == nil
	}
	if organization == nil {
		return false
	}
	id, err := uuid.Parse(*organization)
	return err == nil && id != uuid.Nil && id.String() == *organization && (ownerType == "ORG_UNIT" || id == owner)
}

func authorityOwnerDigest(ownerType string, owner uuid.UUID, organization *string, status string, version int64) string {
	if ownerType == "ORG_UNIT" {
		if organization == nil {
			return ""
		}
		id, err := uuid.Parse(*organization)
		if err != nil {
			return ""
		}
		return tokenauthority.UnitOwnerIdentityDigest(owner, id, status, version)
	}
	return tokenauthority.OwnerIdentityDigest(ownerType, owner, status, version)
}

func authorityOrganizationMatches(ownerType string, organization *uuid.UUID, observed *string) bool {
	return ownerType != "ORG_UNIT" || (organization != nil && observed != nil && organization.String() == *observed)
}

func creatorAuthorityRequest(item TokenAccountProvisioningReservation) OwnerAuthorityCheck {
	actor, owner := item.ActorUserID.String(), item.OwnerID.String()
	ownerType := authorityOwnerType(item.OwnerType)
	platformVersion, actorVersion, ownerVersion := strconv.FormatInt(item.PlatformAuthorizationVersion, 10), strconv.FormatInt(item.ActorVersion, 10), strconv.FormatInt(item.OwnerVersion, 10)
	request := OwnerAuthorityCheck{
		AuthoritySchemaVersion: tokenauthority.SchemaVersion, Mode: tokenauthority.ModeT3CreatorRecheck,
		ActorUserID: &actor, OwnerType: &ownerType, OwnerID: &owner,
		RequiredPermissions: append([]string(nil), tokenauthority.RequiredPermissions...), CommandReference: item.CommandReference.String(),
		CommandRequestDigest:                 &item.CanonicalRequestDigest,
		ExpectedPlatformAuthorizationVersion: &platformVersion, ExpectedActorVersion: &actorVersion, ExpectedOwnerVersion: &ownerVersion,
		ExpectedActorIdentityDigest: &item.ActorIdentityDigest, ExpectedOwnerIdentityDigest: &item.OwnerIdentityDigest,
	}
	request.AuthorityRequestSHA256 = request.Digest()
	return request
}

func (r *PostgresRepository) claimTokenAccountProvisioning(ctx context.Context, worker string, activation TokenCommandGovernanceActivation) (TokenAccountProvisioningReservation, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenAccountProvisioningReservation{}, err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	var v TokenAccountProvisioningReservation
	err = tx.QueryRow(ctx, `SELECT command_reference,actor_user_id,owner_type,owner_id,owner_organization_id,owner_unit_id,account_name,share_scope,default_priority,canonical_request_digest,active_platform_authorization_version,active_actor_version,active_actor_identity_digest,active_owner_version,active_owner_identity_digest,policy_version,activation_epoch,recovery_epoch,reservation_version,attempt_count,expires_at FROM token_account_provisioning_reservations WHERE status='PENDING' AND next_attempt_at<=$1 AND (lease_until IS NULL OR lease_until<$1) ORDER BY next_attempt_at,created_at FOR UPDATE SKIP LOCKED LIMIT 1`, now).Scan(&v.CommandReference, &v.ActorUserID, &v.OwnerType, &v.OwnerID, &v.OwnerOrganization, &v.OwnerUnit, &v.AccountName, &v.ShareScope, &v.DefaultPriority, &v.CanonicalRequestDigest, &v.PlatformAuthorizationVersion, &v.ActorVersion, &v.ActorIdentityDigest, &v.OwnerVersion, &v.OwnerIdentityDigest, &v.PolicyVersion, &v.ActivationEpoch, &v.RecoveryEpoch, &v.ReservationVersion, &v.AttemptCount, &v.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if v.ActivationEpoch > activation.Epoch {
		return v, ErrTokenRouteActivationChanged
	}
	selection := tokenRouteActivationSelection{TokenCommandGovernanceActivation: activation, transactional: true}
	if err = r.recheckTokenRouteActivation(context.WithValue(ctx, tokenRouteActivationContextKey{}, selection), tx); err != nil {
		return v, err
	}
	if v.AttemptCount >= 8 || !now.Before(v.ExpiresAt) {
		_, err = tx.Exec(ctx, `UPDATE token_account_provisioning_reservations SET status='MANUAL_REVIEW',lease_owner=NULL,lease_until=NULL,lease_fencing_token=NULL,stable_error_code='ACCOUNT_PROVISIONING_MANUAL_REVIEW',reservation_version=reservation_version+1,updated_at=$2 WHERE command_reference=$1`, v.CommandReference, now)
		if err == nil {
			err = recordTokenAudit(ctx, tx, v.ActorUserID, "TOKEN_ACCOUNT_PROVISIONING_MANUAL_REVIEW", "TOKEN_ACCOUNT", v.CommandReference, `jsonb_build_object('command_reference',$6,'attempt_count',$7,'recovery_epoch',$8)`, v.CommandReference, v.AttemptCount, v.RecoveryEpoch)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			return v, err
		}
		return v, ErrNotFound
	}
	v.LeaseOwner = worker
	previousActivationEpoch := v.ActivationEpoch
	v.ActivationEpoch = activation.Epoch
	v.LeaseUntil = now.Add(120 * time.Second)
	v.LeaseFencingToken = uuid.Must(uuid.NewV7())
	v.AttemptCount++
	v.ReservationVersion++
	_, err = tx.Exec(ctx, `UPDATE token_account_provisioning_reservations SET lease_owner=$2,lease_until=$3,lease_fencing_token=$4,attempt_count=$5,reservation_version=$6,updated_at=$7,activation_epoch=$8 WHERE command_reference=$1`, v.CommandReference, v.LeaseOwner, v.LeaseUntil, v.LeaseFencingToken, v.AttemptCount, v.ReservationVersion, now, v.ActivationEpoch)
	if err == nil && previousActivationEpoch != v.ActivationEpoch {
		err = recordTokenAudit(ctx, tx, v.ActorUserID, "TOKEN_ACCOUNT_PROVISIONING_EPOCH_RENEWED", "TOKEN_ACCOUNT", v.CommandReference, `jsonb_build_object('command_reference',$6,'previous_activation_epoch',$7,'activation_epoch',$8)`, v.CommandReference, previousActivationEpoch, v.ActivationEpoch)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	return v, err
}

func (r *PostgresRepository) releaseTokenAccountProvisioning(ctx context.Context, item TokenAccountProvisioningReservation, code string) error {
	delay := 5 * time.Second << min(item.AttemptCount-1, 5)
	if delay > 300*time.Second {
		delay = 300 * time.Second
	}
	result, err := r.pool.Exec(ctx, `UPDATE token_account_provisioning_reservations SET lease_owner=NULL,lease_until=NULL,lease_fencing_token=NULL,next_attempt_at=$5,stable_error_code=NULLIF($6,''),reservation_version=reservation_version+1,updated_at=$5 WHERE command_reference=$1 AND status='PENDING' AND lease_owner=$2 AND lease_fencing_token=$3 AND reservation_version=$4`, item.CommandReference, item.LeaseOwner, item.LeaseFencingToken, item.ReservationVersion, r.now().Add(delay), code)
	if err == nil && result.RowsAffected() != 1 {
		return ErrCatalogVersionConflict
	}
	return err
}

func (r *PostgresRepository) failTokenAccountProvisioning(ctx context.Context, item TokenAccountProvisioningReservation, code string) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = r.lockTokenAccountProvisioningLease(ctx, tx, item); err != nil {
		return err
	}
	if err = r.recheckTokenRouteActivation(context.WithValue(ctx, tokenRouteActivationContextKey{}, tokenRouteActivationSelection{TokenCommandGovernanceActivation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: item.ActivationEpoch}, transactional: true}), tx); err != nil {
		return err
	}
	now := r.now()
	result, err := tx.Exec(ctx, `UPDATE token_account_provisioning_reservations SET status='FAILED',stable_error_code=$5,lease_owner=NULL,lease_until=NULL,lease_fencing_token=NULL,reservation_version=reservation_version+1,updated_at=$6 WHERE command_reference=$1 AND status='PENDING' AND lease_owner=$2 AND lease_fencing_token=$3 AND reservation_version=$4`, item.CommandReference, item.LeaseOwner, item.LeaseFencingToken, item.ReservationVersion, code, now)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrCatalogVersionConflict
	}
	result, err = tx.Exec(ctx, `UPDATE token_command_results SET status='FAILED',stable_error_code=$2,completed_at=$3 WHERE command_reference=$1 AND status='PENDING'`, item.CommandReference, code, now)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrCatalogVersionConflict
	}
	if err = recordTokenAudit(ctx, tx, item.ActorUserID, "TOKEN_ACCOUNT_PROVISIONING_FAILED", "TOKEN_ACCOUNT", item.CommandReference, `jsonb_build_object('command_reference',$6,'stable_error_code',$7)`, item.CommandReference, code); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// A result from another service is only evidence until the same live claim is
// locked again. Check all frozen lease coordinates, including expiry, before
// either success or definitive failure changes reservation capacity or result.
func (r *PostgresRepository) lockTokenAccountProvisioningLease(ctx context.Context, tx pgx.Tx, item TokenAccountProvisioningReservation) error {
	var reference uuid.UUID
	err := tx.QueryRow(ctx, `SELECT command_reference FROM token_account_provisioning_reservations WHERE command_reference=$1 AND status='PENDING' AND lease_owner=$2 AND lease_fencing_token=$3 AND reservation_version=$4 AND activation_epoch=$5 AND recovery_epoch=$6 AND lease_until>$7 AND expires_at>$7 FOR UPDATE`, item.CommandReference, item.LeaseOwner, item.LeaseFencingToken, item.ReservationVersion, item.ActivationEpoch, item.RecoveryEpoch, r.now()).Scan(&reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCatalogVersionConflict
	}
	return err
}

func (r *PostgresRepository) finalizeTokenAccountProvisioning(ctx context.Context, item TokenAccountProvisioningReservation, receipt WalletProvisioningReceipt, decision OwnerAuthorityDecision) error {
	if receipt.CommandReference != item.CommandReference || receipt.OwnerID != item.OwnerID || receipt.WalletID == nil || receipt.RequestDigest != walletProvisioningDigest(receipt.OwnerType, receipt.OwnerID) {
		return r.releaseTokenAccountProvisioning(ctx, item, "")
	}
	observedPlatform, platformErr := strconv.ParseInt(decision.ObservedPlatformAuthorizationVersion, 10, 64)
	observedActor, actorErr := strconv.ParseInt(decision.ObservedActorVersion, 10, 64)
	observedOwner, ownerErr := parseAuthorityVersion(decision.ObservedOwnerVersion)
	if decision.Decision != "ALLOW" || decision.ReasonCode != nil || decision.ActorStatus != "ACTIVE" || decision.OwnerStatus == nil || *decision.OwnerStatus != "ACTIVE" || !authorityOrganizationMatches(item.OwnerType, item.OwnerOrganization, decision.OwnerOrganizationID) || platformErr != nil || actorErr != nil || ownerErr != nil || observedPlatform < item.PlatformAuthorizationVersion || observedActor < item.ActorVersion || observedOwner < item.OwnerVersion {
		return r.releaseTokenAccountProvisioning(ctx, item, "")
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = r.lockTokenAccountProvisioningLease(ctx, tx, item); err != nil {
		return err
	}
	if observedPlatform > item.PlatformAuthorizationVersion || observedActor > item.ActorVersion || observedOwner > item.OwnerVersion {
		if decision.ActorIdentityDigest != tokenauthority.ActorIdentityDigest(item.ActorUserID, "ACTIVE", observedActor) || decision.OwnerIdentityDigest == nil || *decision.OwnerIdentityDigest != authorityOwnerDigest(authorityOwnerType(item.OwnerType), item.OwnerID, decision.OwnerOrganizationID, "ACTIVE", observedOwner) {
			return ErrCatalogVersionConflict
		}
		if err = lockTokenAccountOwner(ctx, tx, item.OwnerType, item.OwnerID); err != nil {
			return err
		}
		if err = lockTokenAccountOwnerCapPolicy(ctx, tx); err != nil {
			return err
		}
		capacity, loadErr := loadTokenAccountOwnerCapacity(ctx, tx, item.OwnerType, item.OwnerID)
		if loadErr != nil {
			return loadErr
		}
		if capacity.Policy.PolicyVersion < item.PolicyVersion {
			return ErrCatalogVersionConflict
		}
		var duplicate bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM token_accounts WHERE management_mode='PLATFORM' AND owner_type=$1 AND owner_id=$2 AND lower(name)=lower($3))`, item.OwnerType, item.OwnerID, item.AccountName).Scan(&duplicate); err != nil {
			return err
		}
		if duplicate {
			return ErrCatalogVersionConflict
		}
		result, updateErr := tx.Exec(ctx, `UPDATE token_account_provisioning_reservations SET active_platform_authorization_version=$2,active_actor_version=$3,active_actor_identity_digest=$4,active_owner_version=$5,active_owner_identity_digest=$6,reservation_version=reservation_version+1,updated_at=$7 WHERE command_reference=$1 AND reservation_version=$8 AND status='PENDING'`, item.CommandReference, observedPlatform, observedActor, decision.ActorIdentityDigest, observedOwner, *decision.OwnerIdentityDigest, r.now(), item.ReservationVersion)
		if updateErr != nil {
			return updateErr
		}
		if result.RowsAffected() != 1 {
			return ErrCatalogVersionConflict
		}
		item.ReservationVersion++
		item.PlatformAuthorizationVersion = observedPlatform
		item.ActorVersion = observedActor
		item.ActorIdentityDigest = decision.ActorIdentityDigest
		item.OwnerVersion = observedOwner
		item.OwnerIdentityDigest = *decision.OwnerIdentityDigest
	} else if decision.ActorIdentityDigest != item.ActorIdentityDigest || decision.OwnerIdentityDigest == nil || *decision.OwnerIdentityDigest != item.OwnerIdentityDigest {
		return ErrCatalogVersionConflict
	}
	if err = r.recheckTokenRouteActivation(context.WithValue(ctx, tokenRouteActivationContextKey{}, tokenRouteActivationSelection{TokenCommandGovernanceActivation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: item.ActivationEpoch}, transactional: true}), tx); err != nil {
		return err
	}
	account, err := createAccountEffect(ctx, tx, CreateAccountRequest{OwnerType: item.OwnerType, OwnerID: item.OwnerID, OwnerOrganization: item.OwnerOrganization, OwnerUnit: item.OwnerUnit, WalletID: *receipt.WalletID, Name: item.AccountName, ShareScope: item.ShareScope, DefaultPriority: item.DefaultPriority, ManagementMode: "PLATFORM", CreatedBy: item.ActorUserID}, r.now())
	if err != nil {
		return err
	}
	after, _ := json.Marshal(map[string]any{"token_account_id": account.ID, "owner_type": account.OwnerType, "owner_id": account.OwnerID, "status": account.Status, "account_version": 1})
	now := r.now()
	result, err := tx.Exec(ctx, `UPDATE token_command_results SET status='SUCCEEDED',after_state=$2,redacted_result=$2,completed_at=$3 WHERE command_reference=$1 AND status='PENDING'`, item.CommandReference, after, now)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrCatalogVersionConflict
	}
	result, err = tx.Exec(ctx, `UPDATE token_account_provisioning_reservations SET status='COMPLETED',token_account_id=$2,lease_owner=NULL,lease_until=NULL,lease_fencing_token=NULL,reservation_version=reservation_version+1,updated_at=$3 WHERE command_reference=$1 AND status='PENDING' AND reservation_version=$4`, item.CommandReference, account.ID, now, item.ReservationVersion)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrCatalogVersionConflict
	}
	if err = recordTokenAudit(ctx, tx, item.ActorUserID, "TOKEN_ACCOUNT_PROVISIONING_COMPLETED", "TOKEN_ACCOUNT", account.ID, `jsonb_build_object('command_reference',$6,'token_account_id',$7,'owner_type',$8,'owner_id',$9)`, item.CommandReference, account.ID, item.OwnerType, item.OwnerID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type TokenAccountRecoveryActionResult struct {
	ActionID         uuid.UUID  `json:"action_id"`
	CommandReference uuid.UUID  `json:"command_reference"`
	CommandType      string     `json:"command_type"`
	Action           string     `json:"action"`
	Status           string     `json:"status"`
	RecoveryState    string     `json:"recovery_state"`
	RecoveryEpoch    *int64     `json:"recovery_epoch"`
	StableErrorCode  *string    `json:"stable_error_code"`
	AuditReference   uuid.UUID  `json:"audit_reference"`
	CreatedAt        time.Time  `json:"created_at"`
	CompletedAt      *time.Time `json:"completed_at"`
}

type tokenAccountManualRecoverySnapshot struct {
	CommandReference       uuid.UUID
	CreatorActorID         uuid.UUID
	OwnerType              string
	OwnerID                uuid.UUID
	OwnerOrganization      *uuid.UUID
	CanonicalRequestDigest string
	OwnerVersion           int64
	OwnerIdentityDigest    string
	ActivationEpoch        int64
	RecoveryEpoch          int64
	ReservationVersion     int64
	Status                 string
}

type tokenAccountOperatorCheckpoint struct {
	ActorID                      uuid.UUID
	PlatformAuthorizationVersion int64
	ActorVersion                 int64
	ActorIdentityDigest          string
}

func (s *Service) localRecentRecoveryPrincipal(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	principal, ok := identity.FromContext(r.Context())
	if !ok {
		api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "authentication is required", httpserver.RequestID(r.Context()))
		return identity.Principal{}, false
	}
	now := time.Now().UTC()
	if principal.AuthenticatedAt.IsZero() || principal.AuthenticatedAt.After(now.Add(30*time.Second)) || now.Sub(principal.AuthenticatedAt) > 15*time.Minute {
		api.WriteError(w, http.StatusForbidden, "RECENT_AUTH_REQUIRED", "recent authentication is required", httpserver.RequestID(r.Context()))
		return identity.Principal{}, false
	}
	return principal, true
}

func (s *Service) authorizeRecoveryOperator(ctx context.Context, principal identity.Principal, reference uuid.UUID) (tokenAccountOperatorCheckpoint, error) {
	if s.ownerAuthority == nil {
		return tokenAccountOperatorCheckpoint{}, errors.New("combined account authority unavailable")
	}
	issuer, subject := strings.TrimSpace(principal.Issuer), principal.Subject
	subjectHash := sha256.Sum256([]byte(subject))
	subjectDigest := hex.EncodeToString(subjectHash[:])
	request := OwnerAuthorityCheck{AuthoritySchemaVersion: tokenauthority.SchemaVersion, Mode: tokenauthority.ModeOperatorAuthorize, IdentityIssuer: &issuer, IdentitySubject: &subject, IdentitySubjectSHA256: &subjectDigest, RequiredPermissions: append([]string(nil), tokenauthority.RequiredPermissions...), CommandReference: reference.String()}
	request.AuthorityRequestSHA256 = request.Digest()
	decision, err := s.ownerAuthority.CheckTokenOwnerAuthority(ctx, request)
	if err != nil {
		return tokenAccountOperatorCheckpoint{}, err
	}
	if decision.Decision != "ALLOW" || decision.ReasonCode != nil || decision.ActorStatus != "ACTIVE" {
		return tokenAccountOperatorCheckpoint{}, ErrForbidden
	}
	actor, actorErr := uuid.Parse(decision.ActorUserID)
	platformVersion, platformErr := strconv.ParseInt(decision.ObservedPlatformAuthorizationVersion, 10, 64)
	actorVersion, actorVersionErr := strconv.ParseInt(decision.ObservedActorVersion, 10, 64)
	if actorErr != nil || platformErr != nil || actorVersionErr != nil {
		return tokenAccountOperatorCheckpoint{}, errors.New("operator authority evidence is invalid")
	}
	return tokenAccountOperatorCheckpoint{ActorID: actor, PlatformAuthorizationVersion: platformVersion, ActorVersion: actorVersion, ActorIdentityDigest: decision.ActorIdentityDigest}, nil
}

func (r *PostgresRepository) manualRecoverySnapshot(ctx context.Context, reference uuid.UUID) (tokenAccountManualRecoverySnapshot, error) {
	var item tokenAccountManualRecoverySnapshot
	err := r.pool.QueryRow(ctx, `SELECT command_reference,actor_user_id,owner_type,owner_id,owner_organization_id,canonical_request_digest,active_owner_version,active_owner_identity_digest,activation_epoch,recovery_epoch,reservation_version,status FROM token_account_provisioning_reservations WHERE command_reference=$1`, reference).Scan(&item.CommandReference, &item.CreatorActorID, &item.OwnerType, &item.OwnerID, &item.OwnerOrganization, &item.CanonicalRequestDigest, &item.OwnerVersion, &item.OwnerIdentityDigest, &item.ActivationEpoch, &item.RecoveryEpoch, &item.ReservationVersion, &item.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, ErrNotFound
	}
	return item, err
}

func (s *Service) recheckRecoveryOperator(ctx context.Context, operator tokenAccountOperatorCheckpoint, item tokenAccountManualRecoverySnapshot) (OwnerAuthorityDecision, error) {
	actor, owner := operator.ActorID.String(), item.OwnerID.String()
	ownerType := authorityOwnerType(item.OwnerType)
	platformVersion, actorVersion, ownerVersion := strconv.FormatInt(operator.PlatformAuthorizationVersion, 10), strconv.FormatInt(operator.ActorVersion, 10), strconv.FormatInt(item.OwnerVersion, 10)
	request := OwnerAuthorityCheck{AuthoritySchemaVersion: tokenauthority.SchemaVersion, Mode: tokenauthority.ModeT3OperatorRecheck, ActorUserID: &actor, OwnerType: &ownerType, OwnerID: &owner, RequiredPermissions: append([]string(nil), tokenauthority.RequiredPermissions...), CommandReference: item.CommandReference.String(), CommandRequestDigest: &item.CanonicalRequestDigest, ExpectedPlatformAuthorizationVersion: &platformVersion, ExpectedActorVersion: &actorVersion, ExpectedOwnerVersion: &ownerVersion, ExpectedActorIdentityDigest: &operator.ActorIdentityDigest, ExpectedOwnerIdentityDigest: &item.OwnerIdentityDigest}
	request.AuthorityRequestSHA256 = request.Digest()
	return s.ownerAuthority.CheckTokenOwnerAuthority(ctx, request)
}

func (s *Service) adminTokenCommandByIdempotencyKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	commandType := r.URL.Query().Get("command_type")
	// Only commands using the installed command transport produce these receipts.
	// Reuse its current authorization and actor/type/key-scoped read path; a
	// recovery lookup never repeats a provider call or requires a new precheck.
	if handler, installed := tokenGovernanceHandlers[commandType]; installed && handler.transport == "COMMAND" && commandType != "TOKEN_ACCOUNT_CREATE" {
		s.adminRouteCommandByIdempotencyKey(w, r)
		return
	}
	if strings.TrimSpace(r.URL.Query().Get("command_type")) != "TOKEN_ACCOUNT_CREATE" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "command type is invalid", httpserver.RequestID(r.Context()))
		return
	}
	actor, _, ok := s.requireTokenAccountCreateOperator(w, r, false)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 || len(key) > 128 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "idempotency key is invalid", httpserver.RequestID(r.Context()))
		return
	}
	d := sha256.Sum256([]byte(key))
	repository, available := s.repository.(*PostgresRepository)
	if !available || repository == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNAVAILABLE", "command result is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.tokenCommandByScopedKey(r.Context(), actor, "TOKEN_ACCOUNT_CREATE", hex.EncodeToString(d[:]))
	if errors.Is(err, ErrNotFound) {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the command result was not found", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNAVAILABLE", "command result is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, item)
}
func (r *PostgresRepository) tokenCommandByScopedKey(ctx context.Context, actor uuid.UUID, commandType, keyDigest string) (TokenCommandResult, error) {
	item, err := scanTokenCommandResult(r.pool.QueryRow(ctx, tokenCommandResultSelect+` WHERE actor_user_id=$1 AND command_type=$2 AND idempotency_key_digest=$3`, actor, commandType, keyDigest))
	if errors.Is(err, pgx.ErrNoRows) {
		return item, ErrNotFound
	}
	if err == nil {
		r.decorateTokenAccountRecovery(ctx, &item)
	}
	return item, err
}

func (r *PostgresRepository) decorateTokenAccountRecovery(ctx context.Context, item *TokenCommandResult) {
	if item == nil || item.CommandType != "TOKEN_ACCOUNT_CREATE" || item.Status != "PENDING" {
		return
	}
	var state string
	var epoch int64
	if r.pool.QueryRow(ctx, `SELECT status,recovery_epoch FROM token_account_provisioning_reservations WHERE command_reference=$1`, item.CommandReference).Scan(&state, &epoch) == nil {
		item.RecoveryState = &state
		item.RecoveryEpoch = &epoch
		if state == "MANUAL_REVIEW" {
			code := "ACCOUNT_PROVISIONING_MANUAL_REVIEW"
			item.RecoveryDiagnosticCode = &code
		}
	}
}

func (s *Service) requireTokenAccountCreateOperator(w http.ResponseWriter, r *http.Request, recent bool) (uuid.UUID, int64, bool) {
	if s.authorizer == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is unavailable", httpserver.RequestID(r.Context()))
		return uuid.Nil, 0, false
	}
	principal, ok := identity.FromContext(r.Context())
	if !ok {
		api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "authentication is required", httpserver.RequestID(r.Context()))
		return uuid.Nil, 0, false
	}
	if recent && (principal.AuthenticatedAt.IsZero() || time.Since(principal.AuthenticatedAt) > 15*time.Minute) {
		api.WriteError(w, http.StatusUnauthorized, "RECENT_AUTHENTICATION_REQUIRED", "recent authentication is required", httpserver.RequestID(r.Context()))
		return uuid.Nil, 0, false
	}
	actor, version, err := s.authorizedTokenCommandActor(r.Context(), principal, "TOKEN_ACCOUNT_CREATE")
	if errors.Is(err, authorization.ErrPermissionDenied) {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission is denied", httpserver.RequestID(r.Context()))
		return uuid.Nil, 0, false
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is unavailable", httpserver.RequestID(r.Context()))
		return uuid.Nil, 0, false
	}
	return actor, version, true
}

const recoveryActionSelect = `SELECT action_id,command_reference,'TOKEN_ACCOUNT_CREATE',action,status,CASE WHEN status='SUCCEEDED' THEN CASE action WHEN 'REQUEUE' THEN 'PENDING' ELSE 'FAILED' END ELSE 'MANUAL_REVIEW' END,result_recovery_epoch,stable_error_code,audit_reference,created_at,completed_at FROM token_account_provisioning_recovery_actions`

func scanRecoveryAction(row pgx.Row) (TokenAccountRecoveryActionResult, error) {
	var v TokenAccountRecoveryActionResult
	return v, row.Scan(&v.ActionID, &v.CommandReference, &v.CommandType, &v.Action, &v.Status, &v.RecoveryState, &v.RecoveryEpoch, &v.StableErrorCode, &v.AuditReference, &v.CreatedAt, &v.CompletedAt)
}

func (s *Service) adminTokenAccountRecoveryAction(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.localRecentRecoveryPrincipal(w, r)
	if !ok {
		return
	}
	policy, policyErr := approvalpolicy.Lookup("TOKEN_ACCOUNT_CREATE")
	if policyErr != nil || !validTokenAccountRecoveryPolicy(policy) {
		api.WriteError(w, http.StatusServiceUnavailable, "RECOVERY_POLICY_UNAVAILABLE", "recovery policy is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	reference, ok := adminUUID(w, r, "command_reference")
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 || len(key) > 128 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "idempotency key is invalid", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		Action                string `json:"action"`
		ExpectedRecoveryEpoch int64  `json:"expected_recovery_epoch"`
		Reason                string `json:"reason"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		return
	}
	body.Action = strings.ToUpper(strings.TrimSpace(body.Action))
	body.Reason = strings.TrimSpace(body.Reason)
	if (body.Action != "REQUEUE" && body.Action != "CANCEL") || body.ExpectedRecoveryEpoch < 1 || len([]rune(body.Reason)) < 10 || len([]rune(body.Reason)) > 500 {
		api.WriteError(w, http.StatusUnprocessableEntity, "INVALID_RECOVERY_ACTION", "recovery action is invalid", httpserver.RequestID(r.Context()))
		return
	}
	operator, authorityErr := s.authorizeRecoveryOperator(r.Context(), principal, reference)
	if errors.Is(authorityErr, ErrForbidden) {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission is denied", httpserver.RequestID(r.Context()))
		return
	}
	if authorityErr != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(*PostgresRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_PROVISIONING_RECOVERY_UNAVAILABLE", "recovery persistence is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	keyHash := sha256.Sum256([]byte(key))
	requestHash := sha256.Sum256([]byte(reference.String() + "\n" + body.Action + "\n" + strconv.FormatInt(body.ExpectedRecoveryEpoch, 10) + "\n" + body.Reason + "\n"))
	keyDigest, requestDigest := hex.EncodeToString(keyHash[:]), hex.EncodeToString(requestHash[:])
	reservation, reservationErr := repository.manualRecoverySnapshot(r.Context(), reference)
	if errors.Is(reservationErr, ErrNotFound) {
		writeRecoveryNotFound(w, r)
		return
	}
	if reservationErr != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_PROVISIONING_RECOVERY_UNAVAILABLE", "recovery persistence is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	decision, authorityErr := s.recheckRecoveryOperator(r.Context(), operator, reservation)
	if authorityErr != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_PROVISIONING_RECOVERY_UNAVAILABLE", "recovery authority is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if decision.Decision != "ALLOW" || decision.ReasonCode != nil || decision.ActorStatus != "ACTIVE" || decision.OwnerStatus == nil || *decision.OwnerStatus != "ACTIVE" {
		if decision.ReasonCode != nil && (*decision.ReasonCode == "OWNER_NOT_FOUND" || *decision.ReasonCode == "OWNER_INACTIVE") {
			writeRecoveryNotFound(w, r)
		} else {
			api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "permission is denied", httpserver.RequestID(r.Context()))
		}
		return
	}
	if existing, existingErr := repository.tokenAccountRecoveryActionByKey(r.Context(), reference, operator.ActorID, keyDigest, requestDigest); existingErr == nil {
		api.WriteJSON(w, http.StatusOK, existing)
		return
	} else if !errors.Is(existingErr, ErrNotFound) {
		if errors.Is(existingErr, ErrTokenCommandResultMismatch) {
			api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "the idempotency key was already used", httpserver.RequestID(r.Context()))
		} else {
			api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_PROVISIONING_RECOVERY_UNAVAILABLE", "recovery result is unavailable", httpserver.RequestID(r.Context()))
		}
		return
	}
	if body.Action == "CANCEL" {
		if s.walletProvisioning == nil {
			api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_PROVISIONING_RECOVERY_UNAVAILABLE", "wallet receipt is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		receipt, err := s.walletProvisioning.TokenAccountWalletProvisioningReceipt(r.Context(), reference)
		if err != nil && !errors.Is(err, ErrNotFound) {
			api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_PROVISIONING_RECOVERY_UNAVAILABLE", "wallet receipt is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		if err == nil && (!validWalletProvisioningReceipt(receipt) || receipt.CommandReference != reference || receipt.Status == "PENDING" || receipt.Status == "UNKNOWN") {
			api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_PROVISIONING_RECOVERY_UNAVAILABLE", "wallet receipt is not terminal", httpserver.RequestID(r.Context()))
			return
		}
	}
	item, created, err := repository.applyTokenAccountRecoveryAction(r.Context(), reservation, operator, decision, body.Action, body.ExpectedRecoveryEpoch, keyDigest, requestDigest, body.Reason)
	switch {
	case errors.Is(err, ErrNotFound):
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the command result was not found", httpserver.RequestID(r.Context()))
	case errors.Is(err, ErrTokenCommandResultMismatch):
		api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "the idempotency key was already used", httpserver.RequestID(r.Context()))
	case errors.Is(err, ErrCatalogVersionConflict):
		api.WriteError(w, http.StatusConflict, "RECOVERY_ACTION_CONFLICT", "recovery state changed", httpserver.RequestID(r.Context()))
	case err != nil && item.ActionID != uuid.Nil:
		api.WriteJSON(w, http.StatusServiceUnavailable, api.Error{Code: "COMMAND_RESULT_UNKNOWN", Message: "query the recovery action reference before retrying", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"command_reference": reference, "action_id": item.ActionID, "result_query": "/token/admin/v1/commands/" + reference.String() + "/recovery-actions/" + item.ActionID.String()}})
	case err != nil:
		api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_PROVISIONING_RECOVERY_UNAVAILABLE", "recovery result is unavailable", httpserver.RequestID(r.Context()))
	default:
		status := http.StatusOK
		if created {
			status = http.StatusAccepted
		}
		api.WriteJSON(w, status, item)
	}
}

func writeRecoveryNotFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the recovery action was not found", httpserver.RequestID(r.Context()))
}

func (r *PostgresRepository) tokenAccountRecoveryActionByKey(ctx context.Context, reference, operator uuid.UUID, keyDigest, requestDigest string) (TokenAccountRecoveryActionResult, error) {
	item, err := scanRecoveryAction(r.pool.QueryRow(ctx, recoveryActionSelect+` WHERE command_reference=$1 AND operator_actor_id=$2 AND key_digest=$3`, reference, operator, keyDigest))
	if errors.Is(err, pgx.ErrNoRows) {
		return item, ErrNotFound
	}
	if err != nil {
		return item, err
	}
	var storedDigest string
	if err = r.pool.QueryRow(ctx, `SELECT request_digest FROM token_account_provisioning_recovery_actions WHERE action_id=$1`, item.ActionID).Scan(&storedDigest); err != nil {
		return item, err
	}
	if storedDigest != requestDigest {
		return item, ErrTokenCommandResultMismatch
	}
	return item, nil
}

func (r *PostgresRepository) applyTokenAccountRecoveryAction(ctx context.Context, snapshot tokenAccountManualRecoverySnapshot, operator tokenAccountOperatorCheckpoint, decision OwnerAuthorityDecision, action string, expectedEpoch int64, keyDigest, requestDigest, reason string) (TokenAccountRecoveryActionResult, bool, error) {
	reference := snapshot.CommandReference
	observedPlatform, platformErr := strconv.ParseInt(decision.ObservedPlatformAuthorizationVersion, 10, 64)
	observedActor, actorErr := strconv.ParseInt(decision.ObservedActorVersion, 10, 64)
	observedOwner, ownerErr := parseAuthorityVersion(decision.ObservedOwnerVersion)
	if platformErr != nil || actorErr != nil || ownerErr != nil || decision.ActorUserID != operator.ActorID.String() || decision.ActorStatus != "ACTIVE" || decision.OwnerStatus == nil || *decision.OwnerStatus != "ACTIVE" || decision.OwnerIdentityDigest == nil || !authorityOrganizationMatches(snapshot.OwnerType, snapshot.OwnerOrganization, decision.OwnerOrganizationID) || observedPlatform < operator.PlatformAuthorizationVersion || observedActor < operator.ActorVersion || observedOwner < snapshot.OwnerVersion {
		return TokenAccountRecoveryActionResult{}, false, ErrCatalogVersionConflict
	}
	tx, release, err := r.beginTokenSerializedTransaction(ctx, "token-account-recovery:"+reference.String())
	if err != nil {
		return TokenAccountRecoveryActionResult{}, false, err
	}
	defer release()
	defer tx.Rollback(ctx)
	existing, err := scanRecoveryAction(tx.QueryRow(ctx, recoveryActionSelect+` WHERE operator_actor_id=$1 AND command_reference=$2 AND key_digest=$3`, operator.ActorID, reference, keyDigest))
	if err == nil {
		var existingOperator uuid.UUID
		var existingDigest string
		if e := tx.QueryRow(ctx, `SELECT operator_actor_id,request_digest FROM token_account_provisioning_recovery_actions WHERE action_id=$1`, existing.ActionID).Scan(&existingOperator, &existingDigest); e != nil {
			return TokenAccountRecoveryActionResult{}, false, e
		}
		if existingOperator != operator.ActorID || existingDigest != requestDigest {
			return TokenAccountRecoveryActionResult{}, false, ErrTokenCommandResultMismatch
		}
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return TokenAccountRecoveryActionResult{}, false, err
	}
	var creator uuid.UUID
	var state, ownerIdentityDigest string
	var epoch, reservationVersion, activationEpoch, ownerVersion int64
	err = tx.QueryRow(ctx, `SELECT actor_user_id,status,recovery_epoch,reservation_version,activation_epoch,active_owner_version,active_owner_identity_digest FROM token_account_provisioning_reservations WHERE command_reference=$1 FOR UPDATE`, reference).Scan(&creator, &state, &epoch, &reservationVersion, &activationEpoch, &ownerVersion, &ownerIdentityDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenAccountRecoveryActionResult{}, false, ErrNotFound
	}
	if err != nil {
		return TokenAccountRecoveryActionResult{}, false, err
	}
	if state != "MANUAL_REVIEW" || epoch != expectedEpoch || reservationVersion != snapshot.ReservationVersion || activationEpoch != snapshot.ActivationEpoch || ownerVersion != snapshot.OwnerVersion || ownerIdentityDigest != snapshot.OwnerIdentityDigest || observedOwner < ownerVersion || *decision.OwnerIdentityDigest != authorityOwnerDigest(authorityOwnerType(snapshot.OwnerType), snapshot.OwnerID, decision.OwnerOrganizationID, "ACTIVE", observedOwner) || decision.ActorIdentityDigest != tokenauthority.ActorIdentityDigest(operator.ActorID, "ACTIVE", observedActor) {
		return TokenAccountRecoveryActionResult{}, false, ErrCatalogVersionConflict
	}
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return TokenAccountRecoveryActionResult{}, false, err
	}
	now := r.now()
	actionID := uuid.Must(uuid.NewV7())
	auditID := uuid.Must(uuid.NewV7())
	newEpoch := epoch + 1
	_, err = tx.Exec(ctx, `INSERT INTO token_account_provisioning_recovery_actions(action_id,command_reference,creator_actor_id,operator_actor_id,operator_platform_authorization_version,operator_actor_version,operator_actor_identity_digest,operator_owner_version,operator_owner_identity_digest,action,key_digest,request_digest,expected_recovery_epoch,status,result_recovery_epoch,audit_reference,created_at,completed_at,expected_operator_platform_authorization_version,expected_operator_actor_version,expected_operator_actor_identity_digest,expected_operator_owner_version,expected_operator_owner_identity_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'SUCCEEDED',$14,$15,$16,$16,$17,$18,$19,$20,$21)`, actionID, reference, creator, operator.ActorID, observedPlatform, observedActor, decision.ActorIdentityDigest, observedOwner, *decision.OwnerIdentityDigest, action, keyDigest, requestDigest, epoch, newEpoch, auditID, now, operator.PlatformAuthorizationVersion, operator.ActorVersion, operator.ActorIdentityDigest, snapshot.OwnerVersion, snapshot.OwnerIdentityDigest)
	if err != nil {
		return TokenAccountRecoveryActionResult{}, false, err
	}
	if action == "REQUEUE" {
		_, err = tx.Exec(ctx, `UPDATE token_account_provisioning_reservations SET status='PENDING',recovery_epoch=$2,attempt_count=0,next_attempt_at=$3,expires_at=$4,stable_error_code=NULL,reservation_version=reservation_version+1,updated_at=$3 WHERE command_reference=$1 AND status='MANUAL_REVIEW' AND recovery_epoch=$5`, reference, newEpoch, now, now.Add(30*time.Minute), epoch)
	} else {
		code := "ACCOUNT_PROVISIONING_CANCELLED"
		_, err = tx.Exec(ctx, `UPDATE token_account_provisioning_reservations SET status='FAILED',recovery_epoch=$2,stable_error_code=$3,reservation_version=reservation_version+1,updated_at=$4 WHERE command_reference=$1 AND status='MANUAL_REVIEW' AND recovery_epoch=$5`, reference, newEpoch, code, now, epoch)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE token_command_results SET status='FAILED',stable_error_code=$2,completed_at=$3 WHERE command_reference=$1 AND status='PENDING'`, reference, code, now)
		}
	}
	if err != nil {
		return TokenAccountRecoveryActionResult{}, false, err
	}
	if err = recordTokenAudit(ctx, tx, operator.ActorID, "TOKEN_ACCOUNT_PROVISIONING_RECOVERY", "TOKEN_ACCOUNT", reference, `jsonb_build_object('command_reference',$6,'creator_actor_id',$7,'operator_actor_id',$8,'action',$9,'audit_reference',$10,'reason',$11)`, reference, creator, operator.ActorID, action, auditID, reason); err != nil {
		return TokenAccountRecoveryActionResult{}, false, err
	}
	item, err := scanRecoveryAction(tx.QueryRow(ctx, recoveryActionSelect+` WHERE action_id=$1`, actionID))
	if err == nil {
		err = tx.Commit(ctx)
	}
	return item, true, err
}

func (s *Service) adminTokenAccountRecoveryActionByKey(w http.ResponseWriter, r *http.Request) {
	operator, _, ok := s.requireTokenAccountCreateOperator(w, r, false)
	if !ok {
		return
	}
	reference, ok := adminUUID(w, r, "command_reference")
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 16 || len(key) > 128 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "idempotency key is invalid", httpserver.RequestID(r.Context()))
		return
	}
	hash := sha256.Sum256([]byte(key))
	repository, _ := s.repository.(*PostgresRepository)
	item, err := scanRecoveryAction(repository.pool.QueryRow(r.Context(), recoveryActionSelect+` WHERE command_reference=$1 AND operator_actor_id=$2 AND key_digest=$3`, reference, operator, hex.EncodeToString(hash[:])))
	s.writeRecoveryLookup(w, r, item, err)
}
func (s *Service) adminTokenAccountRecoveryActionResult(w http.ResponseWriter, r *http.Request) {
	operator, _, ok := s.requireTokenAccountCreateOperator(w, r, false)
	if !ok {
		return
	}
	reference, ok := adminUUID(w, r, "command_reference")
	if !ok {
		return
	}
	actionID, ok := adminUUID(w, r, "action_id")
	if !ok {
		return
	}
	repository, _ := s.repository.(*PostgresRepository)
	item, err := scanRecoveryAction(repository.pool.QueryRow(r.Context(), recoveryActionSelect+` WHERE command_reference=$1 AND operator_actor_id=$2 AND action_id=$3`, reference, operator, actionID))
	s.writeRecoveryLookup(w, r, item, err)
}
func (s *Service) writeRecoveryLookup(w http.ResponseWriter, r *http.Request, item TokenAccountRecoveryActionResult, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		api.WriteError(w, http.StatusNotFound, "NOT_FOUND", "the recovery action was not found", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ACCOUNT_PROVISIONING_RECOVERY_UNAVAILABLE", "recovery result is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}
