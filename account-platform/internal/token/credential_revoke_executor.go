package token

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type credentialRevokeCommand struct {
	TokenAccountID         uuid.UUID `json:"token_account_id"`
	ExpectedAccountVersion int64     `json:"expected_account_version"`
	CredentialID           uuid.UUID `json:"credential_id"`
	CredentialVersion      int       `json:"credential_version"`
}

type credentialRevokeCritical struct {
	TokenAccountID                  uuid.UUID `json:"token_account_id"`
	TokenAccountVersion             int64     `json:"token_account_version"`
	CredentialID                    uuid.UUID `json:"credential_id"`
	CredentialVersion               int       `json:"credential_version"`
	CredentialStatus                string    `json:"credential_status"`
	CredentialFingerprint           string    `json:"credential_fingerprint"`
	ActiveRouteSetFingerprint       string    `json:"active_route_set_fingerprint"`
	ProvisioningLeaseSetFingerprint string    `json:"provisioning_lease_set_fingerprint"`
	InflightRequestSetFingerprint   string    `json:"inflight_request_set_fingerprint"`
	ReplacementCredentialVersion    *int      `json:"replacement_credential_version"`
	PolicyVersion                   int64     `json:"policy_version"`
}

type PostgresApprovedCredentialExecutor struct{ repository *PostgresRepository }

func NewPostgresApprovedCredentialExecutor(repository *PostgresRepository) *PostgresApprovedCredentialExecutor {
	return &PostgresApprovedCredentialExecutor{repository: repository}
}

func parseCredentialRevokeEnvelope(envelope TokenCommandEnvelope) (credentialRevokeCommand, credentialRevokeCritical, error) {
	var command credentialRevokeCommand
	var expected credentialRevokeCritical
	if envelope.CommandType != "TOKEN_CREDENTIAL_REVOKE" || envelope.TargetResourceType != "PROVIDER_CREDENTIAL" || decodeCommand(envelope.Command, &command) != nil || decodeCommand(envelope.ExpectedVersions, &expected) != nil || command.TokenAccountID == uuid.Nil || command.CredentialID == uuid.Nil || command.CredentialID.String() != envelope.TargetResourceID || command.ExpectedAccountVersion < 1 || command.CredentialVersion < 1 || expected.TokenAccountID != command.TokenAccountID || expected.TokenAccountVersion != command.ExpectedAccountVersion || expected.CredentialID != command.CredentialID || expected.CredentialVersion != command.CredentialVersion || expected.PolicyVersion != 1 {
		return credentialRevokeCommand{}, credentialRevokeCritical{}, ErrTokenCommandInvalid
	}
	return command, expected, nil
}

func sameCredentialRevokeCritical(left, right credentialRevokeCritical) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func credentialRevokeCriticalFromEvidence(accountID, credentialID uuid.UUID, evidence ImpactLocalEvidence) (credentialRevokeCritical, error) {
	result := credentialRevokeImpactResult(accountID, credentialID, evidence)
	if len(result.Blockers) != 0 {
		return credentialRevokeCritical{}, ErrCatalogVersionConflict
	}
	raw, err := json.Marshal(result.CriticalResourceVersions)
	if err != nil {
		return credentialRevokeCritical{}, ErrCatalogVersionConflict
	}
	var critical credentialRevokeCritical
	if decodeCommand(raw, &critical) != nil {
		return credentialRevokeCritical{}, ErrCatalogVersionConflict
	}
	return critical, nil
}

func (e *PostgresApprovedCredentialExecutor) PrecheckApprovedTokenCommand(ctx context.Context, commandType string, envelope TokenCommandEnvelope) (json.RawMessage, error) {
	if e == nil || e.repository == nil {
		return nil, ErrTokenCommandInvalid
	}
	if commandType == "TOKEN_MODEL_LIFECYCLE_CHANGE" {
		return e.precheckApprovedLifecyclePlan(ctx, envelope)
	}
	if commandType == "TOKEN_MODEL_CATALOG_ROLLBACK" {
		return e.precheckApprovedCatalogRollback(ctx, envelope)
	}
	if commandType != "TOKEN_CREDENTIAL_REVOKE" {
		return nil, ErrTokenCommandInvalid
	}
	command, expected, err := parseCredentialRevokeEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	evidence, err := e.repository.TokenCredentialImpact(ctx, command.TokenAccountID, &command.CredentialID)
	if err != nil {
		return nil, err
	}
	current, err := credentialRevokeCriticalFromEvidence(command.TokenAccountID, command.CredentialID, evidence)
	if err != nil || !sameCredentialRevokeCritical(expected, current) {
		return nil, ErrCatalogVersionConflict
	}
	return json.Marshal(current)
}

// ExecuteApprovedTokenCommand is deliberately unavailable: credential revoke
// must use ExecuteApprovedCredentialRevoke so the resource mutation, redacted
// audit/outbox and governed result are one Token transaction.
func (e *PostgresApprovedCredentialExecutor) ExecuteApprovedTokenCommand(context.Context, string, string, TokenCommandEnvelope) (json.RawMessage, json.RawMessage, string, error) {
	return nil, nil, "", ErrTokenCommandInvalid
}

func (e *PostgresApprovedCredentialExecutor) ExecuteApprovedCredentialRevoke(ctx context.Context, reference, actorID uuid.UUID, envelope TokenCommandEnvelope) (TokenCommandResult, error) {
	command, expected, err := parseCredentialRevokeEnvelope(envelope)
	if err != nil {
		return TokenCommandResult{}, err
	}
	tx, err := e.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenCommandResult{}, err
	}
	defer tx.Rollback(ctx)
	claimed, err := scanTokenCommandResult(tx.QueryRow(ctx, tokenCommandResultSelect+` WHERE command_reference=$1 FOR UPDATE`, reference))
	if err != nil {
		return TokenCommandResult{}, err
	}
	if claimed.ActorUserID != actorID || claimed.CommandType != "TOKEN_CREDENTIAL_REVOKE" || claimed.TargetResourceID != command.CredentialID.String() {
		return TokenCommandResult{}, ErrForbidden
	}
	if claimed.Status != "PENDING" {
		return claimed, tx.Commit(ctx)
	}
	current, blockers, err := credentialRevokeCriticalTx(ctx, tx, command.TokenAccountID, command.CredentialID)
	if err != nil {
		return TokenCommandResult{}, err
	}
	if blockers || !sameCredentialRevokeCritical(expected, current) {
		return TokenCommandResult{}, ErrCatalogVersionConflict
	}
	now := e.repository.now().UTC()
	mutation, err := tx.Exec(ctx, `UPDATE provider_credentials SET status='REVOKED',revoked_at=$3,revoked_by=$6,revocation_reason=(SELECT reason FROM token_command_results WHERE command_reference=$7) WHERE id=$1 AND token_account_id=$2 AND version=$4 AND status=$5`, command.CredentialID, command.TokenAccountID, now, command.CredentialVersion, expected.CredentialStatus, actorID, reference)
	if err != nil || mutation.RowsAffected() != 1 {
		if err != nil {
			return TokenCommandResult{}, err
		}
		return TokenCommandResult{}, ErrCatalogVersionConflict
	}
	mutation, err = tx.Exec(ctx, `UPDATE token_accounts SET management_version=management_version+1,updated_at=$2 WHERE id=$1 AND management_version=$3`, command.TokenAccountID, now, command.ExpectedAccountVersion)
	if err != nil || mutation.RowsAffected() != 1 {
		if err != nil {
			return TokenCommandResult{}, err
		}
		return TokenCommandResult{}, ErrCatalogVersionConflict
	}
	if err = recordTokenAudit(ctx, tx, actorID, "TOKEN_CREDENTIAL_REVOKED", "PROVIDER_CREDENTIAL", command.CredentialID, `jsonb_build_object('token_account_id',$6,'credential_version',$7,'status','REVOKED','account_version',$8,'approval_request_id',$9)`, command.TokenAccountID, command.CredentialVersion, command.ExpectedAccountVersion+1, claimed.ApprovalRequestID); err != nil {
		return TokenCommandResult{}, err
	}
	after, _ := json.Marshal(map[string]any{"token_account_id": command.TokenAccountID, "token_account_version": command.ExpectedAccountVersion + 1, "credential_id": command.CredentialID, "credential_version": command.CredentialVersion, "credential_status": "REVOKED"})
	result, _ := json.Marshal(map[string]any{"token_account_id": command.TokenAccountID, "token_account_version": command.ExpectedAccountVersion + 1, "credential_id": command.CredentialID, "credential_version": command.CredentialVersion, "status": "REVOKED"})
	completed, err := completeTokenCommandTx(ctx, tx, reference, "SUCCEEDED", after, result, "")
	if err != nil {
		return TokenCommandResult{}, err
	}
	return completed, tx.Commit(ctx)
}

func credentialRevokeCriticalTx(ctx context.Context, tx pgx.Tx, accountID, credentialID uuid.UUID) (credentialRevokeCritical, bool, error) {
	var value credentialRevokeCritical
	value.TokenAccountID, value.CredentialID, value.PolicyVersion = accountID, credentialID, 1
	if err := tx.QueryRow(ctx, `SELECT account.management_version,credential.version,credential.status,credential.fingerprint FROM token_accounts account JOIN provider_credentials credential ON credential.token_account_id=account.id WHERE account.id=$1 AND account.management_mode='PLATFORM' AND credential.id=$2 FOR UPDATE OF account,credential`, accountID, credentialID).Scan(&value.TokenAccountVersion, &value.CredentialVersion, &value.CredentialStatus, &value.CredentialFingerprint); err != nil {
		return credentialRevokeCritical{}, false, err
	}
	var err error
	value.ActiveRouteSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,model_id,protocol_version_id,route_version,route_generation,reference_status,health_status)::text FROM routing_policies WHERE token_account_id=$1 ORDER BY id`, accountID)
	if err != nil {
		return credentialRevokeCritical{}, false, err
	}
	value.ProvisioningLeaseSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,status,desired_generation,actual_generation,lease_until,active_token_account_id,active_credential_id,protocol_version_id)::text FROM route_provisionings WHERE active_token_account_id=$1 AND active_credential_id=$2 AND lease_until>transaction_timestamp() ORDER BY id`, accountID, credentialID)
	if err != nil {
		return credentialRevokeCritical{}, false, err
	}
	value.InflightRequestSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,session_id,status,execution_status,execution_version)::text FROM usage_requests WHERE token_account_id=$1 AND credential_id=$2 AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING') ORDER BY id`, accountID, credentialID)
	if err != nil {
		return credentialRevokeCritical{}, false, err
	}
	if err = tx.QueryRow(ctx, `WITH serving AS `+canServeCoverageRelation+`,`+impactAffectedCoverageCTE+`,`+alternativeCredentialCandidatesCTE+` SELECT max(credential_version) FROM alternative_credentials`, accountID, "", credentialID).Scan(&value.ReplacementCredentialVersion); err != nil {
		return credentialRevokeCritical{}, false, err
	}
	var leases int64
	if err = tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM route_provisionings WHERE active_token_account_id=$1 AND active_credential_id=$2 AND lease_until>transaction_timestamp())`, accountID, credentialID).Scan(&leases); err != nil {
		return credentialRevokeCritical{}, false, err
	}
	for _, fingerprint := range []string{value.CredentialFingerprint, value.ActiveRouteSetFingerprint, value.ProvisioningLeaseSetFingerprint, value.InflightRequestSetFingerprint} {
		if !validImpactFingerprint(fingerprint) {
			return credentialRevokeCritical{}, false, errors.New("invalid credential revoke snapshot")
		}
	}
	ready, err := canServeCoverageCount(ctx, tx, nil, &accountID, "", &credentialID, nil)
	if err != nil {
		return credentialRevokeCritical{}, false, err
	}
	alternatives, _, err := alternativeCoverageCounts(ctx, tx, accountID, "", &credentialID)
	if err != nil {
		return credentialRevokeCritical{}, false, err
	}
	return value, leases > 0 || ready > 0 && alternatives == 0, nil
}

var _ ApprovedTokenCommandExecutor = (*PostgresApprovedCredentialExecutor)(nil)
