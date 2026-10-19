package token

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const platformDefaultAccountName = "Pinvou 平台默认模型"

type DefaultRouteConfig struct {
	Provider       string
	AdapterVersion string
	BaseURL        string
	Secret         []byte
	Models         []string
	Priority       int
}

type DefaultRouteProvisioner interface {
	Ensure(context.Context, AuthorizationContext, uuid.UUID, CatalogModel) (RouteProvisioning, error)
}

type PostgresDefaultRouteProvisioner struct {
	repository         *PostgresRepository
	config             DefaultRouteConfig
	workerID           string
	accountCreateReady atomic.Bool
}

func (p *PostgresDefaultRouteProvisioner) WithTokenAccountCreateCapability(ready bool) *PostgresDefaultRouteProvisioner {
	p.accountCreateReady.Store(ready)
	return p
}

func NewDefaultRouteProvisioner(repository *PostgresRepository, policy ProviderURLPolicy, config DefaultRouteConfig) (*PostgresDefaultRouteProvisioner, error) {
	config.Provider = strings.TrimSpace(config.Provider)
	config.AdapterVersion = strings.TrimSpace(config.AdapterVersion)
	config.BaseURL = strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	config.Models = normalizeModels(config.Models)
	if repository == nil || config.Provider == "" || config.AdapterVersion == "" || config.BaseURL == "" || len(config.Secret) < 8 || config.Priority < 0 || config.Priority > 10000 {
		return nil, ErrInvalidSecret
	}
	if err := policy.Validate(config.BaseURL); err != nil {
		return nil, err
	}
	return &PostgresDefaultRouteProvisioner{repository: repository, config: config, workerID: uuid.Must(uuid.NewV7()).String()}, nil
}

func (p *PostgresDefaultRouteProvisioner) provisionResources(ctx context.Context, resolved AuthorizationContext, walletID uuid.UUID, model CatalogModel, desiredGeneration int64) error {
	return p.provisionResourcesForAttempt(ctx, resolved, walletID, model, desiredGeneration, nil, uuid.Nil)
}

// Production workers hold their exact claimed lease in the resource transaction.
// Losing that lease cannot leave a new account, credential or route behind.
func (p *PostgresDefaultRouteProvisioner) provisionResourcesForAttempt(ctx context.Context, resolved AuthorizationContext, walletID uuid.UUID, model CatalogModel, desiredGeneration int64, claimed *RouteProvisioning, attemptID uuid.UUID) error {
	var selectionErr error
	ctx, selectionErr = p.repository.selectConfigurationWorkloadActivation(ctx)
	if selectionErr != nil {
		return selectionErr
	}
	model.UpstreamModelRef = strings.TrimSpace(model.UpstreamModelRef)
	if walletID == uuid.Nil || resolved.UserID == uuid.Nil || model.ModelID == uuid.Nil || model.ProtocolDefinitionID == uuid.Nil || model.UpstreamModelRef == "" || model.ProtocolCode == "" || model.LifecycleStatus != "PUBLISHED" || !model.SourceAvailable || model.ProviderID != p.config.Provider || model.AdapterVersion != p.config.AdapterVersion || desiredGeneration < 1 {
		return ErrNoEligibleRoute
	}
	allowedModels := normalizeModels(append(append([]string{}, p.config.Models...), model.UpstreamModelRef))
	ownerType, ownerID, shareScope, defaultScope := "USER", resolved.UserID, "OWNER_UNIT_ONLY", "NONE"
	var ownerOrganization *uuid.UUID
	if resolved.ContextType == "ORGANIZATION" && resolved.OrganizationID != nil {
		ownerType, ownerID, shareScope, defaultScope = "ORG", *resolved.OrganizationID, "ORG_ALL", "ORG"
		organizationID := *resolved.OrganizationID
		ownerOrganization = &organizationID
	} else if resolved.ContextType != "PERSONAL" {
		return ErrNoEligibleRoute
	}

	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if claimed != nil {
		if claimed.DesiredGeneration != desiredGeneration {
			return ErrRouteProvisioningPending
		}
		if _, live, err := p.lockCompletionLease(ctx, tx, *claimed, attemptID); err != nil {
			return err
		} else if !live {
			return ErrRouteProvisioningPending
		}
	}
	if err := p.repository.recheckTokenRouteActivation(ctx, tx); err != nil {
		return err
	}
	if err := lockTokenAccountOwner(ctx, tx, ownerType, ownerID); err != nil {
		return err
	}
	// Provisioning can also create an organization account, credential or route.
	// Keep the owner-before-lifecycle advisory ordering of governed creation.
	if ownerOrganization != nil {
		if err := lockActiveOrganization(ctx, tx, *ownerOrganization); err != nil {
			return err
		}
	}
	now := p.repository.now()
	var accountID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM token_accounts WHERE owner_type=$1 AND owner_id=$2 AND management_mode='PLATFORM' ORDER BY created_at,id LIMIT 1 FOR UPDATE`, ownerType, ownerID).Scan(&accountID); errors.Is(err, pgx.ErrNoRows) {
		if !p.accountCreateReady.Load() {
			return ErrTokenAccountCreateNotActive
		}
		if err := lockTokenAccountOwnerCapPolicy(ctx, tx); err != nil {
			return err
		}
		capacity, capacityErr := loadTokenAccountOwnerCapacity(ctx, tx, ownerType, ownerID)
		if capacityErr != nil {
			return capacityErr
		}
		if capacity.Count >= capacity.Policy.MaxPerOwner {
			return ErrTokenAccountCapReached
		}
		accountID = uuid.Must(uuid.NewV7())
		name := platformDefaultAccountName
		for {
			command, insertErr := tx.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,owner_organization_id,owner_unit_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode)
VALUES($1,$2,$3,$4,NULL,$5,$6,'ACTIVE',$7,$8,$9,$9,'PLATFORM') ON CONFLICT(owner_type,owner_id,name) DO NOTHING`, accountID, ownerType, ownerID, ownerOrganization, walletID, name, p.config.Priority, resolved.UserID, now)
			if insertErr != nil {
				return insertErr
			}
			if command.RowsAffected() == 1 {
				break
			}
			// Names are user-owned presentation, never platform identity.
			name = platformDefaultAccountName + " " + uuid.NewString()
		}
	} else if err != nil {
		return err
	} else {
		if err := p.provisionFirstModelRoute(ctx, tx, resolved, walletID, accountID, model, desiredGeneration, defaultScope); err != nil {
			return err
		}
		return p.commitProvisioningResources(ctx, tx, claimed, attemptID)
	}
	if _, err := tx.Exec(ctx, `UPDATE token_accounts SET wallet_id=$2,status='ACTIVE',default_priority=$3,updated_at=$4 WHERE id=$1`, accountID, walletID, p.config.Priority, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO token_share_policies(token_account_id,scope,owner_organization_id,owner_unit_id,updated_at,updated_by)
VALUES($1,$2,$3,NULL,$4,$5)
ON CONFLICT(token_account_id) DO UPDATE SET scope=EXCLUDED.scope,owner_organization_id=EXCLUDED.owner_organization_id,owner_unit_id=NULL,updated_at=EXCLUDED.updated_at,updated_by=EXCLUDED.updated_by`, accountID, shareScope, ownerOrganization, now, resolved.UserID); err != nil {
		return err
	}

	fingerprint := Fingerprint(p.config.Secret)
	credentialID := uuid.Nil
	if err := tx.QueryRow(ctx, `SELECT id FROM provider_credentials WHERE token_account_id=$1 AND fingerprint=$2 AND status<>'REVOKED'`, accountID, fingerprint).Scan(&credentialID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if credentialID == uuid.Nil {
		credentialID = uuid.Must(uuid.NewV7())
	}
	ciphertext, err := p.repository.secrets.Encrypt(p.config.Secret, []byte(accountID.String()+":"+credentialID.String()))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE provider_credentials SET status='RETIRED',retired_at=$2 WHERE token_account_id=$1 AND status='ACTIVE' AND id<>$3`, accountID, now, credentialID); err != nil {
		return err
	}
	var version int
	if err := tx.QueryRow(ctx, `SELECT version FROM provider_credentials WHERE id=$1`, credentialID).Scan(&version); errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM provider_credentials WHERE token_account_id=$1`, accountID).Scan(&version); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,activated_at,created_by,created_at,validation_status,validation_checked_by)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'ACTIVE',$9,$10,$10,$11,$10,'VALID',$11)`, credentialID, accountID, p.config.Provider, p.config.BaseURL, ciphertext, p.repository.secrets.KeyID(), fingerprint, version, allowedModels, now, resolved.UserID)
	} else if err != nil {
		return err
	} else {
		_, err = tx.Exec(ctx, `UPDATE provider_credentials SET provider=$2,base_url=$3,ciphertext=$4,key_id=$5,status='ACTIVE',allowed_models=$6,validated_at=$7,activated_at=$7,retired_at=NULL,validation_status='VALID',validation_checked_by=$8 WHERE id=$1 AND status<>'REVOKED'`, credentialID, p.config.Provider, p.config.BaseURL, ciphertext, p.repository.secrets.KeyID(), allowedModels, now, resolved.UserID)
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,updated_by,route_generation,model_id,protocol_version_id,reference_status,route_version,health_version,health_diagnostic_code,next_probe_at)
VALUES($1,$2,$3,$4,$5,$6,'UNKNOWN',$7,$8,$11,$9,$10,'RESOLVED',1,1,'AWAITING_INITIAL_PROBE',$7)
ON CONFLICT(token_account_id,model_id,protocol_version_id) WHERE reference_status='RESOLVED' DO NOTHING`, uuid.Must(uuid.NewV7()), accountID, model.UpstreamModelRef, model.ProtocolCode, p.config.Priority, defaultScope, now, resolved.UserID, model.ModelID, model.ProtocolDefinitionID, desiredGeneration); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE token_accounts SET management_version=management_version+1,updated_at=$2 WHERE id=$1`, accountID, now); err != nil {
		return err
	}
	return p.commitProvisioningResources(ctx, tx, claimed, attemptID)
}

func (p *PostgresDefaultRouteProvisioner) commitProvisioningResources(ctx context.Context, tx pgx.Tx, claimed *RouteProvisioning, attemptID uuid.UUID) error {
	if claimed != nil {
		if _, live, err := p.lockCompletionLease(ctx, tx, *claimed, attemptID); err != nil {
			return err
		} else if !live {
			return ErrRouteProvisioningPending
		}
	}
	return p.repository.commitGovernedMutation(ctx, tx)
}

// Only a first route for an untouched automatic credential can be added. An
// existing route, administrator removal or previously completed provisioning
// remains authoritative even when configuration is reloaded.
func (p *PostgresDefaultRouteProvisioner) provisionFirstModelRoute(ctx context.Context, tx pgx.Tx, resolved AuthorizationContext, walletID, accountID uuid.UUID, model CatalogModel, generation int64, defaultScope string) error {
	account, err := getTokenAccount(ctx, tx, accountID, true)
	if err != nil {
		return err
	}
	if account.Status != "ACTIVE" || account.WalletID != walletID {
		return ErrNoEligibleRoute
	}
	scopeType, scopeID, err := provisioningScope(resolved)
	if err != nil {
		return err
	}
	var known bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM routing_policies WHERE token_account_id=$1 AND model_id=$2)
OR EXISTS(SELECT 1 FROM routing_policy_tombstones WHERE token_account_id=$1 AND model_id=$2)
OR EXISTS(SELECT 1 FROM token_audit_logs WHERE resource_type='TOKEN_ACCOUNT' AND resource_id=$1 AND action='TOKEN_ACCOUNT_CREATED')
OR EXISTS(SELECT 1 FROM route_provisionings WHERE scope_type=$3 AND scope_id=$4 AND model_id=$2 AND actual_generation IS NOT NULL)`, accountID, model.ModelID, scopeType, scopeID).Scan(&known)
	if err != nil || known {
		return err
	}
	var credentialID uuid.UUID
	var version int
	var allowed []string
	err = tx.QueryRow(ctx, `SELECT id,version,allowed_models FROM provider_credentials WHERE token_account_id=$1 AND status='ACTIVE' AND validation_status='VALID' AND provider=$2 AND base_url=$3 AND fingerprint=$4 FOR UPDATE`, accountID, p.config.Provider, p.config.BaseURL, Fingerprint(p.config.Secret)).Scan(&credentialID, &version, &allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !slices.Contains(allowed, model.UpstreamModelRef) {
		var untouched bool
		err = tx.QueryRow(ctx, `SELECT $2::integer=1 AND (SELECT count(*) FROM provider_credentials WHERE token_account_id=$1)=1
AND NOT EXISTS(SELECT 1 FROM token_audit_logs WHERE resource_type='PROVIDER_CREDENTIAL' AND resource_id=$3)`, accountID, version, credentialID).Scan(&untouched)
		if err != nil || !untouched {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT p.model_id,p.upstream_model_ref,p.protocol_code,p.protocol_version_id,i.provider_id,i.adapter_version,p.desired_signature,p.wallet_id
FROM route_provisionings p JOIN model_identities i ON i.id=p.model_id WHERE p.scope_type=$1 AND p.scope_id=$2 AND p.status='READY' AND p.active_token_account_id=$3 AND p.active_credential_id=$4 AND p.actual_generation=p.desired_generation`, scopeType, scopeID, accountID, credentialID)
		if err != nil {
			return err
		}
		expected := append([]string{}, p.config.Models...)
		count := 0
		for rows.Next() {
			var prior CatalogModel
			var signature string
			var priorWallet uuid.UUID
			if err := rows.Scan(&prior.ModelID, &prior.UpstreamModelRef, &prior.ProtocolCode, &prior.ProtocolDefinitionID, &prior.ProviderID, &prior.AdapterVersion, &signature, &priorWallet); err != nil {
				rows.Close()
				return err
			}
			if priorWallet != walletID || signature != p.desiredSignature(scopeType, scopeID, priorWallet, prior) {
				rows.Close()
				return nil
			}
			expected = append(expected, prior.UpstreamModelRef)
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if count == 0 || !slices.Equal(normalizeModels(allowed), normalizeModels(expected)) {
			return nil
		}
		allowed = normalizeModels(append(allowed, model.UpstreamModelRef))
		if _, err := tx.Exec(ctx, `UPDATE provider_credentials SET allowed_models=$2 WHERE id=$1`, credentialID, allowed); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,updated_by,route_generation,model_id,protocol_version_id,reference_status,route_version,health_version,health_diagnostic_code,next_probe_at)
VALUES($1,$2,$3,$4,$5,$6,'UNKNOWN',$7,$8,$9,$10,$11,'RESOLVED',1,1,'AWAITING_INITIAL_PROBE',$7) ON CONFLICT(token_account_id,model_id,protocol_version_id) WHERE reference_status='RESOLVED' DO NOTHING`, uuid.Must(uuid.NewV7()), accountID, model.UpstreamModelRef, model.ProtocolCode, p.config.Priority, defaultScope, p.repository.now(), resolved.UserID, generation, model.ModelID, model.ProtocolDefinitionID)
	return err
}
