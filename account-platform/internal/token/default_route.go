package token

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const platformDefaultAccountName = "Pinvou 平台默认模型"

type DefaultRouteConfig struct {
	Provider string
	BaseURL  string
	Secret   []byte
	Models   []string
	Priority int
}

type DefaultRouteProvisioner interface {
	Ensure(context.Context, AuthorizationContext, uuid.UUID, string) error
}

type PostgresDefaultRouteProvisioner struct {
	repository *PostgresRepository
	config     DefaultRouteConfig
}

func NewDefaultRouteProvisioner(repository *PostgresRepository, policy ProviderURLPolicy, config DefaultRouteConfig) (*PostgresDefaultRouteProvisioner, error) {
	config.Provider = strings.TrimSpace(config.Provider)
	config.BaseURL = strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	config.Models = normalizeModels(config.Models)
	if repository == nil || config.Provider == "" || config.BaseURL == "" || len(config.Secret) < 8 || len(config.Models) == 0 || config.Priority < 0 || config.Priority > 10000 {
		return nil, ErrInvalidSecret
	}
	if err := policy.Validate(config.BaseURL); err != nil {
		return nil, err
	}
	return &PostgresDefaultRouteProvisioner{repository: repository, config: config}, nil
}

func (p *PostgresDefaultRouteProvisioner) Ensure(ctx context.Context, resolved AuthorizationContext, walletID uuid.UUID, model string) error {
	model = strings.TrimSpace(model)
	if walletID == uuid.Nil || resolved.UserID == uuid.Nil || !slices.Contains(p.config.Models, model) {
		return ErrNoEligibleRoute
	}
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
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ownerType+":"+ownerID.String()); err != nil {
		return err
	}
	now := p.repository.now()
	accountID := uuid.Must(uuid.NewV7())
	_, err = tx.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,owner_organization_id,owner_unit_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode)
VALUES($1,$2,$3,$4,NULL,$5,$6,'ACTIVE',$7,$8,$9,$9,'PLATFORM')
ON CONFLICT(owner_type,owner_id,name) DO NOTHING`, accountID, ownerType, ownerID, ownerOrganization, walletID, platformDefaultAccountName, p.config.Priority, resolved.UserID, now)
	if err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM token_accounts WHERE owner_type=$1 AND owner_id=$2 AND name=$3 AND management_mode='PLATFORM' FOR UPDATE`, ownerType, ownerID, platformDefaultAccountName).Scan(&accountID); errors.Is(err, pgx.ErrNoRows) {
		return ErrForbidden
	} else if err != nil {
		return err
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
		_, err = tx.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,activated_at,created_by,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'ACTIVE',$9,$10,$10,$11,$10)`, credentialID, accountID, p.config.Provider, p.config.BaseURL, ciphertext, p.repository.secrets.KeyID(), fingerprint, version, p.config.Models, now, resolved.UserID)
	} else if err != nil {
		return err
	} else {
		_, err = tx.Exec(ctx, `UPDATE provider_credentials SET provider=$2,base_url=$3,ciphertext=$4,key_id=$5,status='ACTIVE',allowed_models=$6,validated_at=$7,activated_at=$7,retired_at=NULL WHERE id=$1 AND status<>'REVOKED'`, credentialID, p.config.Provider, p.config.BaseURL, ciphertext, p.repository.secrets.KeyID(), p.config.Models, now)
	}
	if err != nil {
		return err
	}
	for _, allowedModel := range p.config.Models {
		if _, err := tx.Exec(ctx, `INSERT INTO routing_policies(id,token_account_id,model,priority,default_scope,health_status,updated_at,updated_by)
VALUES($1,$2,$3,$4,$5,'HEALTHY',$6,$7)
ON CONFLICT(token_account_id,model) DO UPDATE SET priority=EXCLUDED.priority,default_scope=EXCLUDED.default_scope,health_status='HEALTHY',updated_at=EXCLUDED.updated_at,updated_by=EXCLUDED.updated_by`, uuid.Must(uuid.NewV7()), accountID, allowedModel, p.config.Priority, defaultScope, now, resolved.UserID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
