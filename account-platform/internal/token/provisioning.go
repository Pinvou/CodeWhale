package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrRouteProvisioningPending   = errors.New("route provisioning is pending")
	ErrRouteProvisioningFailed    = errors.New("route provisioning failed")
	ErrRouteRepairRequired        = errors.New("route provisioning requires repair")
	ErrModelPolicyDenied          = errors.New("organization model policy denied")
	ErrModelPolicyVersionConflict = errors.New("organization model policy version conflict")
)

type RouteProvisioning struct {
	ID                   uuid.UUID  `json:"provisioning_id"`
	ScopeType            string     `json:"scope_type"`
	ScopeID              uuid.UUID  `json:"scope_id"`
	ModelID              uuid.UUID  `json:"model_id"`
	ProtocolCode         string     `json:"protocol_code"`
	ProtocolVersionID    *uuid.UUID `json:"protocol_version_id,omitempty"`
	DesiredGeneration    int64      `json:"desired_generation"`
	ActualGeneration     *int64     `json:"actual_generation,omitempty"`
	Status               string     `json:"status"`
	Stage                string     `json:"stage"`
	PolicyVersion        int64      `json:"policy_version"`
	AttemptCount         int        `json:"attempt_count"`
	RepairRound          int        `json:"repair_round"`
	MaxAttempts          int        `json:"max_attempts"`
	SLADeadlineAt        time.Time  `json:"sla_deadline_at"`
	LeaseUntil           *time.Time `json:"lease_until,omitempty"`
	NextRetryAt          *time.Time `json:"next_retry_at,omitempty"`
	LastErrorCode        *string    `json:"last_error_code,omitempty"`
	LastErrorDetail      *string    `json:"last_error_detail,omitempty"`
	ActiveTokenAccountID *uuid.UUID `json:"active_token_account_id,omitempty"`
	ActiveCredentialID   *uuid.UUID `json:"active_credential_id,omitempty"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

type ProvisioningAttempt struct {
	ID            uuid.UUID  `json:"attempt_id"`
	RepairRound   int        `json:"repair_round"`
	AttemptNumber int        `json:"attempt_number"`
	WorkerID      string     `json:"worker_id"`
	Status        string     `json:"status"`
	Stage         string     `json:"stage"`
	ErrorCode     *string    `json:"error_code,omitempty"`
	ErrorDetail   *string    `json:"error_detail,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
}

type ProvisioningDetail struct {
	RouteProvisioning
	Attempts []ProvisioningAttempt `json:"attempts"`
}

const provisioningSelect = `SELECT id,scope_type,scope_id,model_id,protocol_code,protocol_version_id,desired_generation,actual_generation,status,stage,policy_version,attempt_count,repair_round,max_attempts,sla_deadline_at,lease_until,next_retry_at,last_error_code,last_error_detail,active_token_account_id,active_credential_id,updated_at FROM route_provisionings`

func scanProvisioning(row interface{ Scan(...any) error }) (RouteProvisioning, error) {
	var item RouteProvisioning
	err := row.Scan(&item.ID, &item.ScopeType, &item.ScopeID, &item.ModelID, &item.ProtocolCode, &item.ProtocolVersionID, &item.DesiredGeneration, &item.ActualGeneration, &item.Status, &item.Stage, &item.PolicyVersion, &item.AttemptCount, &item.RepairRound, &item.MaxAttempts, &item.SLADeadlineAt, &item.LeaseUntil, &item.NextRetryAt, &item.LastErrorCode, &item.LastErrorDetail, &item.ActiveTokenAccountID, &item.ActiveCredentialID, &item.UpdatedAt)
	return item, err
}

func provisioningScope(resolved AuthorizationContext) (string, uuid.UUID, error) {
	if resolved.ContextType == "PERSONAL" && resolved.UserID != uuid.Nil {
		return "PERSONAL", resolved.UserID, nil
	}
	if resolved.ContextType == "ORGANIZATION" && resolved.OrganizationID != nil {
		return "ORGANIZATION", *resolved.OrganizationID, nil
	}
	return "", uuid.Nil, ErrForbidden
}

func (p *PostgresDefaultRouteProvisioner) desiredSignature(scopeType string, scopeID, walletID uuid.UUID, model CatalogModel) string {
	payload, _ := json.Marshal(map[string]any{"scope_type": scopeType, "scope_id": scopeID, "wallet_id": walletID, "model_id": model.ModelID, "upstream_model_ref": model.UpstreamModelRef, "protocol_code": model.ProtocolCode, "protocol_version_id": model.ProtocolDefinitionID, "provider": p.config.Provider, "adapter_version": p.config.AdapterVersion, "base_url": p.config.BaseURL, "credential_fingerprint": Fingerprint(p.config.Secret), "priority": p.config.Priority})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func (p *PostgresDefaultRouteProvisioner) Ensure(ctx context.Context, resolved AuthorizationContext, walletID uuid.UUID, model CatalogModel) (RouteProvisioning, error) {
	scopeType, scopeID, err := provisioningScope(resolved)
	if err != nil || walletID == uuid.Nil || model.ModelID == uuid.Nil || model.ProtocolDefinitionID == uuid.Nil || model.ProtocolCode == "" || strings.TrimSpace(model.UpstreamModelRef) == "" {
		return RouteProvisioning{}, ErrNoEligibleRoute
	}
	if err := p.ValidateContextModelPolicy(ctx, resolved, model.ModelID); err != nil {
		return RouteProvisioning{}, err
	}
	signature := p.desiredSignature(scopeType, scopeID, walletID, model)
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return RouteProvisioning{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "provisioning:"+scopeType+":"+scopeID.String()+":"+model.ModelID.String()+":"+model.ProtocolCode); err != nil {
		return RouteProvisioning{}, err
	}
	item, err := scanProvisioning(tx.QueryRow(ctx, provisioningSelect+` WHERE scope_type=$1 AND scope_id=$2 AND model_id=$3 AND protocol_code=$4 AND desired_signature=$5 ORDER BY desired_generation DESC LIMIT 1`, scopeType, scopeID, model.ModelID, model.ProtocolCode, signature))
	if err == nil {
		valid := item.ProtocolVersionID != nil && *item.ProtocolVersionID == model.ProtocolDefinitionID
		if valid && item.Status == "READY" {
			valid = item.ActiveTokenAccountID != nil && item.ActiveCredentialID != nil && item.ActualGeneration != nil
			if valid {
				if checkErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM token_accounts ta JOIN provider_credentials pc ON pc.token_account_id=ta.id JOIN routing_policies rp ON rp.token_account_id=ta.id JOIN model_identities identity ON identity.id=rp.model_id WHERE ta.id=$1 AND ta.status='ACTIVE' AND ta.management_mode='PLATFORM' AND pc.id=$2 AND pc.status='ACTIVE' AND pc.validation_status='VALID' AND pc.provider=identity.provider_id AND identity.adapter_version=$6 AND identity.upstream_model_ref=ANY(pc.allowed_models) AND rp.model_id=$3 AND rp.protocol_version_id=$4 AND rp.reference_status='RESOLVED' AND rp.route_generation=$5)`, *item.ActiveTokenAccountID, *item.ActiveCredentialID, model.ModelID, model.ProtocolDefinitionID, *item.ActualGeneration, model.AdapterVersion).Scan(&valid); checkErr != nil {
					return RouteProvisioning{}, checkErr
				}
			}
		}
		if valid {
			return item, tx.Commit(ctx)
		}
		err = pgx.ErrNoRows // create a new desired generation for version drift or drifted READY resources
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RouteProvisioning{}, err
	}
	var generation int64
	ownerType := map[string]string{"PERSONAL": "USER", "ORGANIZATION": "ORG"}[scopeType]
	if err := tx.QueryRow(ctx, `SELECT GREATEST(COALESCE((SELECT max(desired_generation) FROM route_provisionings WHERE scope_type=$1 AND scope_id=$2 AND model_id=$3 AND protocol_code=$4),0),COALESCE((SELECT max(rp.route_generation) FROM token_accounts ta JOIN routing_policies rp ON rp.token_account_id=ta.id WHERE ta.owner_type=$5 AND ta.owner_id=$2 AND ta.management_mode='PLATFORM' AND rp.model_id=$3 AND rp.protocol_version_id=$6),0))+1`, scopeType, scopeID, model.ModelID, model.ProtocolCode, ownerType, model.ProtocolDefinitionID).Scan(&generation); err != nil {
		return RouteProvisioning{}, err
	}
	now := p.repository.now()
	policy := map[string]any{"version": 1, "lease_seconds": 120, "initial_backoff_seconds": 5, "multiplier": 2, "maximum_backoff_seconds": 300, "jitter_percent": 20, "max_attempts": 8, "sla_seconds": 1800}
	policyJSON, _ := json.Marshal(policy)
	id := uuid.Must(uuid.NewV7())
	_, err = tx.Exec(ctx, `INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,next_retry_at,requested_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'PENDING','QUEUED',1,$11,8,$12,$13,$14,$13,$13)`, id, scopeType, scopeID, walletID, model.ModelID, model.UpstreamModelRef, model.ProtocolCode, model.ProtocolDefinitionID, generation, signature, policyJSON, now.Add(30*time.Minute), now, resolved.UserID)
	if err != nil {
		return RouteProvisioning{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO route_provisioning_audit(id,provisioning_id,action,before_status,after_status,desired_generation,actor_id,reason,evidence,created_at) VALUES($1,$2,'DESIRED_GENERATION_CREATED',NULL,'PENDING',$3,$4,'automatic provisioning requested',$5,$6)`, uuid.Must(uuid.NewV7()), id, generation, resolved.UserID, policyJSON, now)
	if err != nil {
		return RouteProvisioning{}, err
	}
	item, err = scanProvisioning(tx.QueryRow(ctx, provisioningSelect+` WHERE id=$1`, id))
	if err != nil {
		return RouteProvisioning{}, err
	}
	return item, tx.Commit(ctx)
}

func (p *PostgresDefaultRouteProvisioner) ValidateContextModelPolicy(ctx context.Context, resolved AuthorizationContext, modelID uuid.UUID) error {
	if resolved.ContextType != "ORGANIZATION" || resolved.OrganizationID == nil {
		return nil
	}
	if !hasPermission(resolved, "model.invoke") {
		return ErrModelPolicyDenied
	}
	allowed, err := p.repository.OrganizationModelAllowed(ctx, *resolved.OrganizationID, modelID)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrModelPolicyDenied
	}
	return nil
}

func (p *PostgresDefaultRouteProvisioner) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if item, attemptID, ok := p.claim(ctx); ok {
			p.execute(ctx, item, attemptID)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *PostgresDefaultRouteProvisioner) claim(ctx context.Context) (RouteProvisioning, uuid.UUID, bool) {
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return RouteProvisioning{}, uuid.Nil, false
	}
	defer tx.Rollback(ctx)
	now := p.repository.now()
	_, _ = tx.Exec(ctx, `UPDATE route_provisioning_attempts a SET status='TIMED_OUT',error_code='LEASE_EXPIRED',error_detail='provisioning attempt lease expired',ended_at=$1 FROM route_provisionings p WHERE a.provisioning_id=p.id AND a.status='RUNNING' AND p.status='PENDING' AND p.lease_until<$1`, now)
	_, _ = tx.Exec(ctx, `UPDATE route_provisionings SET status='FAILED',stage='LEASE_EXPIRED',lease_owner=NULL,lease_until=NULL,next_retry_at=$1,last_error_code='LEASE_EXPIRED',last_error_detail='provisioning worker lease expired',updated_at=$1 WHERE status='PENDING' AND lease_until<$1`, now)
	item, err := scanProvisioning(tx.QueryRow(ctx, provisioningSelect+` WHERE id=(SELECT id FROM route_provisionings WHERE ((status='PENDING' AND lease_until IS NULL) OR (status='FAILED' AND next_retry_at<=$1)) ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1) FOR UPDATE`, now))
	if err != nil {
		return RouteProvisioning{}, uuid.Nil, false
	}
	if !now.Before(item.SLADeadlineAt) || item.AttemptCount >= item.MaxAttempts {
		if _, err := tx.Exec(ctx, `UPDATE route_provisionings SET status='REPAIR_REQUIRED',stage='SLA_EXCEEDED',next_retry_at=NULL,updated_at=$2 WHERE id=$1`, item.ID, now); err != nil {
			return RouteProvisioning{}, uuid.Nil, false
		}
		evidence, _ := json.Marshal(map[string]any{"attempt_count": item.AttemptCount, "max_attempts": item.MaxAttempts, "sla_deadline_at": item.SLADeadlineAt})
		if _, err := tx.Exec(ctx, `INSERT INTO route_provisioning_audit(id,provisioning_id,action,before_status,after_status,desired_generation,actor_id,reason,evidence,created_at) SELECT $1,$2,'SLA_EXCEEDED',$3,'REPAIR_REQUIRED',$4,requested_by,'provisioning retry budget exhausted',$5,$6 FROM route_provisionings WHERE id=$2`, uuid.Must(uuid.NewV7()), item.ID, item.Status, item.DesiredGeneration, evidence, now); err != nil {
			return RouteProvisioning{}, uuid.Nil, false
		}
		_ = tx.Commit(ctx)
		return RouteProvisioning{}, uuid.Nil, false
	}
	attemptID := uuid.Must(uuid.NewV7())
	item.AttemptCount++
	leaseUntil := now.Add(2 * time.Minute)
	_, err = tx.Exec(ctx, `UPDATE route_provisionings SET status='PENDING',stage='PROVISIONING',attempt_count=$2,lease_owner=$3,lease_until=$4,next_retry_at=NULL,updated_at=$5 WHERE id=$1`, item.ID, item.AttemptCount, p.workerID, leaseUntil, now)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO route_provisioning_attempts(id,provisioning_id,repair_round,attempt_number,worker_id,status,stage,started_at) VALUES($1,$2,$3,$4,$5,'RUNNING','PROVISIONING',$6)`, attemptID, item.ID, item.RepairRound, item.AttemptCount, p.workerID, now)
	}
	if err != nil || tx.Commit(ctx) != nil {
		return RouteProvisioning{}, uuid.Nil, false
	}
	return item, attemptID, true
}

func (p *PostgresDefaultRouteProvisioner) execute(ctx context.Context, item RouteProvisioning, attemptID uuid.UUID) {
	resolved := AuthorizationContext{UserID: item.ScopeID, ContextType: "PERSONAL"}
	if item.ScopeType == "ORGANIZATION" {
		resolved = AuthorizationContext{UserID: p.requestedBy(ctx, item.ID), ContextType: "ORGANIZATION", OrganizationID: &item.ScopeID}
	}
	model, modelErr := p.provisioningModel(ctx, item.ID)
	if modelErr != nil {
		p.failAttempt(ctx, item, attemptID, modelErr, p.repository.now())
		return
	}
	err := p.provisionResources(ctx, resolved, p.walletID(ctx, item.ID), model)
	now := p.repository.now()
	if err != nil {
		p.failAttempt(ctx, item, attemptID, err, now)
		return
	}
	var accountID, credentialID uuid.UUID
	var generation int64
	err = p.repository.pool.QueryRow(ctx, `SELECT ta.id,pc.id,rp.route_generation FROM route_provisionings p JOIN token_accounts ta ON ta.owner_type=CASE p.scope_type WHEN 'PERSONAL' THEN 'USER' ELSE 'ORG' END AND ta.owner_id=p.scope_id AND ta.management_mode='PLATFORM' JOIN routing_policies rp ON rp.token_account_id=ta.id AND rp.model_id=p.model_id AND rp.protocol_version_id=$2 AND rp.reference_status='RESOLVED' JOIN model_identities identity ON identity.id=rp.model_id AND identity.adapter_version=$3 JOIN provider_credentials pc ON pc.token_account_id=ta.id AND pc.provider=identity.provider_id AND identity.upstream_model_ref=ANY(pc.allowed_models) AND pc.status='ACTIVE' AND pc.validation_status='VALID' WHERE p.id=$1`, item.ID, model.ProtocolDefinitionID, model.AdapterVersion).Scan(&accountID, &credentialID, &generation)
	if err != nil || generation != item.DesiredGeneration {
		p.failAttempt(ctx, item, attemptID, errors.New("resource verification failed"), now)
		return
	}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE route_provisioning_attempts SET status='SUCCEEDED',stage='VERIFIED',ended_at=$2 WHERE id=$1 AND status='RUNNING'`, attemptID, now); err != nil {
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE route_provisionings SET status='READY',stage='VERIFIED',protocol_version_id=$2,actual_generation=$3,active_token_account_id=$4,active_credential_id=$5,lease_owner=NULL,lease_until=NULL,next_retry_at=NULL,last_error_code=NULL,last_error_detail=NULL,updated_at=$6 WHERE id=$1 AND lease_owner=$7`, item.ID, model.ProtocolDefinitionID, generation, accountID, credentialID, now, p.workerID); err != nil {
		return
	}
	evidence, _ := json.Marshal(map[string]any{"attempt_id": attemptID, "actual_generation": generation, "token_account_id": accountID, "credential_id": credentialID})
	if _, err = tx.Exec(ctx, `INSERT INTO route_provisioning_audit(id,provisioning_id,action,before_status,after_status,desired_generation,actor_id,reason,evidence,created_at) SELECT $1,$2,'PROVISIONING_READY','PENDING','READY',$3,requested_by,'managed resources verified',$4,$5 FROM route_provisionings WHERE id=$2`, uuid.Must(uuid.NewV7()), item.ID, item.DesiredGeneration, evidence, now); err != nil {
		return
	}
	_ = tx.Commit(ctx)
}

func (p *PostgresDefaultRouteProvisioner) failAttempt(ctx context.Context, item RouteProvisioning, attemptID uuid.UUID, cause error, now time.Time) {
	code, detail, repair := "PROVISIONING_TEMPORARY", "managed resources could not be provisioned", false
	if errors.Is(cause, ErrInvalidSecret) || errors.Is(cause, ErrForbidden) || errors.Is(cause, ErrNoEligibleRoute) {
		code, detail, repair = "CONFIGURATION_INVALID", "managed route configuration requires operator repair", true
	}
	next := now.Add(provisioningBackoff(item.ID, item.AttemptCount))
	status := "FAILED"
	if repair || item.AttemptCount >= item.MaxAttempts || !next.Before(item.SLADeadlineAt) {
		status, next = "REPAIR_REQUIRED", time.Time{}
	}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE route_provisioning_attempts SET status='FAILED',stage='PROVISIONING',error_code=$2,error_detail=$3,ended_at=$4 WHERE id=$1 AND status='RUNNING'`, attemptID, code, detail, now); err != nil {
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE route_provisionings SET status=$2,stage='PROVISIONING',lease_owner=NULL,lease_until=NULL,next_retry_at=$3,last_error_code=$4,last_error_detail=$5,updated_at=$6 WHERE id=$1 AND lease_owner=$7`, item.ID, status, nullableTime(next), code, detail, now, p.workerID); err != nil {
		return
	}
	evidence, _ := json.Marshal(map[string]any{"attempt_id": attemptID, "error_code": code, "repair_required": status == "REPAIR_REQUIRED"})
	if _, err = tx.Exec(ctx, `INSERT INTO route_provisioning_audit(id,provisioning_id,action,before_status,after_status,desired_generation,actor_id,reason,evidence,created_at) SELECT $1,$2,'PROVISIONING_FAILED','PENDING',$3,$4,requested_by,$5,$6,$7 FROM route_provisionings WHERE id=$2`, uuid.Must(uuid.NewV7()), item.ID, status, item.DesiredGeneration, detail, evidence, now); err != nil {
		return
	}
	_ = tx.Commit(ctx)
}

func provisioningBackoff(id uuid.UUID, attempt int) time.Duration {
	seconds := 5
	for index := 1; index < attempt && seconds < 300; index++ {
		seconds *= 2
	}
	if seconds > 300 {
		seconds = 300
	}
	jitter := int(id[15])%41 - 20
	return time.Duration(seconds*(100+jitter)/100) * time.Second
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func (p *PostgresDefaultRouteProvisioner) requestedBy(ctx context.Context, id uuid.UUID) uuid.UUID {
	var value uuid.UUID
	_ = p.repository.pool.QueryRow(ctx, `SELECT requested_by FROM route_provisionings WHERE id=$1`, id).Scan(&value)
	return value
}
func (p *PostgresDefaultRouteProvisioner) walletID(ctx context.Context, id uuid.UUID) uuid.UUID {
	var value uuid.UUID
	_ = p.repository.pool.QueryRow(ctx, `SELECT wallet_id FROM route_provisionings WHERE id=$1`, id).Scan(&value)
	return value
}
func (p *PostgresDefaultRouteProvisioner) provisioningModel(ctx context.Context, id uuid.UUID) (CatalogModel, error) {
	return scanCatalogModel(p.repository.pool.QueryRow(ctx, catalogSelect+` JOIN route_provisionings provisioning ON provisioning.model_id=e.model_id AND provisioning.protocol_version_id=e.protocol_definition_id WHERE provisioning.id=$1 AND provisioning.protocol_version_id IS NOT NULL AND e.catalog_version=(SELECT max(exact.catalog_version) FROM model_catalog_entries exact WHERE exact.model_id=provisioning.model_id AND exact.protocol_definition_id=provisioning.protocol_version_id) AND e.lifecycle_status='PUBLISHED' AND e.source_available=true AND p.id=provisioning.protocol_version_id`, id))
}

func provisioningError(item RouteProvisioning) error {
	switch item.Status {
	case "READY":
		return nil
	case "FAILED":
		return ErrRouteProvisioningFailed
	case "REPAIR_REQUIRED":
		return ErrRouteRepairRequired
	default:
		return ErrRouteProvisioningPending
	}
}

func sanitizeProvisioningReason(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		value = value[:500]
	}
	return value
}

func provisioningDigest(value any) string {
	payload, _ := json.Marshal(value)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func (p *PostgresDefaultRouteProvisioner) List(ctx context.Context, status string, limit int) ([]RouteProvisioning, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	status = strings.ToUpper(strings.TrimSpace(status))
	rows, err := p.repository.pool.Query(ctx, provisioningSelect+` WHERE ($1='' OR status=$1) ORDER BY updated_at DESC,id DESC LIMIT $2`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []RouteProvisioning{}
	for rows.Next() {
		item, scanErr := scanProvisioning(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (p *PostgresDefaultRouteProvisioner) Detail(ctx context.Context, id uuid.UUID) (ProvisioningDetail, error) {
	item, err := scanProvisioning(p.repository.pool.QueryRow(ctx, provisioningSelect+` WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProvisioningDetail{}, ErrNotFound
	}
	if err != nil {
		return ProvisioningDetail{}, err
	}
	rows, err := p.repository.pool.Query(ctx, `SELECT id,repair_round,attempt_number,worker_id,status,stage,error_code,error_detail,started_at,ended_at FROM route_provisioning_attempts WHERE provisioning_id=$1 ORDER BY repair_round,attempt_number`, id)
	if err != nil {
		return ProvisioningDetail{}, err
	}
	defer rows.Close()
	detail := ProvisioningDetail{RouteProvisioning: item, Attempts: []ProvisioningAttempt{}}
	for rows.Next() {
		var attempt ProvisioningAttempt
		if err := rows.Scan(&attempt.ID, &attempt.RepairRound, &attempt.AttemptNumber, &attempt.WorkerID, &attempt.Status, &attempt.Stage, &attempt.ErrorCode, &attempt.ErrorDetail, &attempt.StartedAt, &attempt.EndedAt); err != nil {
			return ProvisioningDetail{}, err
		}
		detail.Attempts = append(detail.Attempts, attempt)
	}
	return detail, rows.Err()
}

func (p *PostgresDefaultRouteProvisioner) Repair(ctx context.Context, id, actor uuid.UUID, key, reason string) (RouteProvisioning, error) {
	reason = sanitizeProvisioningReason(reason)
	if id == uuid.Nil || actor == uuid.Nil || reason == "" || len(strings.TrimSpace(key)) < 16 {
		return RouteProvisioning{}, ErrForbidden
	}
	tx, err := p.repository.pool.Begin(ctx)
	if err != nil {
		return RouteProvisioning{}, err
	}
	defer tx.Rollback(ctx)
	item, err := scanProvisioning(tx.QueryRow(ctx, provisioningSelect+` WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return RouteProvisioning{}, ErrNotFound
	}
	digest := provisioningDigest(map[string]any{"provisioning_id": id, "reason": reason})
	var storedDigest string
	var resultSnapshot []byte
	var resultRound int
	err = tx.QueryRow(ctx, `SELECT request_digest,repair_round,result_snapshot FROM route_provisioning_commands WHERE idempotency_key=$1`, key).Scan(&storedDigest, &resultRound, &resultSnapshot)
	if err == nil {
		if storedDigest != digest {
			return RouteProvisioning{}, ErrIdempotencyConflict
		}
		var replay RouteProvisioning
		if jsonErr := json.Unmarshal(resultSnapshot, &replay); jsonErr != nil {
			return RouteProvisioning{}, jsonErr
		}
		return replay, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) || (item.Status != "REPAIR_REQUIRED" && item.Status != "FAILED") {
		return RouteProvisioning{}, ErrForbidden
	}
	now := p.repository.now()
	item.RepairRound++
	_, err = tx.Exec(ctx, `UPDATE route_provisionings SET status='PENDING',stage='MANUAL_REPAIR',repair_round=$2,attempt_count=0,sla_deadline_at=$3,next_retry_at=$4,lease_owner=NULL,lease_until=NULL,last_error_code=NULL,last_error_detail=NULL,updated_at=$4 WHERE id=$1`, id, item.RepairRound, now.Add(30*time.Minute), now)
	if err != nil {
		return RouteProvisioning{}, err
	}
	result, err := scanProvisioning(tx.QueryRow(ctx, provisioningSelect+` WHERE id=$1`, id))
	if err != nil {
		return RouteProvisioning{}, err
	}
	resultSnapshot, _ = json.Marshal(result)
	_, err = tx.Exec(ctx, `INSERT INTO route_provisioning_commands(idempotency_key,command_type,request_digest,provisioning_id,repair_round,actor_id,reason,result_snapshot,created_at) VALUES($1,'PROVISIONING_REPAIR',$2,$3,$4,$5,$6,$7,$8)`, key, digest, id, item.RepairRound, actor, reason, resultSnapshot, now)
	if err == nil {
		evidence, _ := json.Marshal(map[string]any{"repair_round": item.RepairRound})
		_, err = tx.Exec(ctx, `INSERT INTO route_provisioning_audit(id,provisioning_id,action,before_status,after_status,desired_generation,actor_id,reason,evidence,created_at) VALUES($1,$2,'REPAIR_REQUESTED',$3,'PENDING',$4,$5,$6,$7,$8)`, uuid.Must(uuid.NewV7()), id, item.Status, item.DesiredGeneration, actor, reason, evidence, now)
	}
	if err != nil {
		return RouteProvisioning{}, err
	}
	return result, tx.Commit(ctx)
}

func (p *PostgresDefaultRouteProvisioner) ValidateModelReady(ctx context.Context, modelID uuid.UUID, protocol string) error {
	var ready bool
	err := p.repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM route_provisionings p JOIN token_accounts ta ON ta.id=p.active_token_account_id AND ta.status='ACTIVE' JOIN routing_policies rp ON rp.token_account_id=ta.id AND rp.model_id=p.model_id AND rp.protocol_version_id=p.protocol_version_id AND rp.reference_status='RESOLVED' AND rp.route_generation=p.actual_generation JOIN model_identities identity ON identity.id=rp.model_id AND identity.provider_id=$3 AND identity.adapter_version=$4 JOIN provider_credentials pc ON pc.id=p.active_credential_id AND pc.token_account_id=ta.id AND pc.provider=identity.provider_id AND identity.upstream_model_ref=ANY(pc.allowed_models) AND pc.status='ACTIVE' AND pc.validation_status='VALID' WHERE p.model_id=$1 AND p.protocol_code=$2 AND p.protocol_version_id IS NOT NULL AND p.status='READY')`, modelID, protocol, p.config.Provider, p.config.AdapterVersion).Scan(&ready)
	if err != nil {
		return err
	}
	if !ready {
		return ErrModelUnavailable
	}
	return nil
}

func (p *PostgresDefaultRouteProvisioner) ContextEligible(ctx context.Context, resolved AuthorizationContext, modelID uuid.UUID, protocol string) (bool, string, error) {
	scopeType, scopeID, err := provisioningScope(resolved)
	if err != nil {
		return false, "CONTEXT_INVALID", nil
	}
	if scopeType == "ORGANIZATION" {
		if policyErr := p.ValidateContextModelPolicy(ctx, resolved, modelID); policyErr != nil {
			if errors.Is(policyErr, ErrModelPolicyDenied) {
				return false, "MODEL_POLICY_DENIED", nil
			}
			return false, "POLICY_UNAVAILABLE", policyErr
		}
	}
	var status string
	err = p.repository.pool.QueryRow(ctx, `SELECT status FROM route_provisionings WHERE scope_type=$1 AND scope_id=$2 AND model_id=$3 AND protocol_code=$4 ORDER BY desired_generation DESC LIMIT 1`, scopeType, scopeID, modelID, protocol).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "ROUTE_PROVISIONING_REQUIRED", nil
	}
	if err != nil {
		return false, "ROUTE_PROVISIONING_UNAVAILABLE", err
	}
	if status != "READY" {
		return false, "ROUTE_PROVISIONING_" + status, nil
	}
	var ready bool
	err = p.repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM route_provisionings p JOIN token_accounts ta ON ta.id=p.active_token_account_id AND ta.status='ACTIVE' JOIN routing_policies rp ON rp.token_account_id=ta.id AND rp.model_id=p.model_id AND rp.protocol_version_id=p.protocol_version_id AND rp.reference_status='RESOLVED' AND rp.route_generation=p.actual_generation JOIN model_identities identity ON identity.id=rp.model_id AND identity.provider_id=$5 AND identity.adapter_version=$6 JOIN provider_credentials pc ON pc.id=p.active_credential_id AND pc.token_account_id=ta.id AND pc.provider=identity.provider_id AND identity.upstream_model_ref=ANY(pc.allowed_models) AND pc.status='ACTIVE' AND pc.validation_status='VALID' WHERE p.scope_type=$1 AND p.scope_id=$2 AND p.model_id=$3 AND p.protocol_code=$4 AND p.protocol_version_id IS NOT NULL AND p.status='READY')`, scopeType, scopeID, modelID, protocol, p.config.Provider, p.config.AdapterVersion).Scan(&ready)
	if err != nil {
		return false, "ROUTE_PROVISIONING_UNAVAILABLE", err
	}
	if !ready {
		return false, "ROUTE_PROVISIONING_DRIFTED", nil
	}
	return true, "READY", nil
}
