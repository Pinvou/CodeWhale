package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokenauthority"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type governedModelDraftCommand struct {
	ModelID                uuid.UUID `json:"model_id"`
	ExpectedCatalogVersion int64     `json:"expected_catalog_version"`
	ExpectedDraftVersion   int64     `json:"expected_draft_version"`
	DisplayName            string    `json:"display_name"`
	Description            string    `json:"description"`
	Aliases                []string  `json:"aliases"`
	ProtocolCode           string    `json:"protocol_code"`
	ProtocolVersion        int64     `json:"protocol_version"`
	ContextLength          int64     `json:"context_length"`
	MaximumOutputTokens    int64     `json:"maximum_output_tokens"`
	SupportedModalities    []string  `json:"supported_modalities"`
	TokenizerVersion       string    `json:"tokenizer_version"`
}

type governedAccountCreateCommand struct {
	OwnerType         string     `json:"owner_type"`
	OwnerID           uuid.UUID  `json:"owner_id"`
	OwnerOrganization *uuid.UUID `json:"owner_organization_id"`
	OwnerUnit         *uuid.UUID `json:"owner_unit_id"`
	Name              string     `json:"name"`
	ShareScope        string     `json:"share_scope"`
	DefaultPriority   int        `json:"default_priority"`
}

type governedAccountStatusCommand struct {
	TargetStatus string `json:"target_status"`
}

type governedAccountCreateExpected struct {
	OwnerType                    string    `json:"owner_type"`
	OwnerID                      uuid.UUID `json:"owner_id"`
	OwnerAuthorizationVersion    int64     `json:"owner_authorization_version"`
	ActorVersion                 int64     `json:"actor_version"`
	ActorIdentityDigest          string    `json:"actor_identity_digest"`
	OwnerVersion                 int64     `json:"owner_version"`
	OwnerIdentityDigest          string    `json:"owner_identity_digest"`
	OwnerAccountCount            int64     `json:"owner_account_count"`
	OwnerAccountCap              int64     `json:"owner_account_cap"`
	OwnerAccountCapPolicyVersion int64     `json:"owner_account_cap_policy_version"`
	CreationVersion              int64     `json:"creation_version"`
	PrecheckFingerprint          string    `json:"precheck_fingerprint,omitempty"`
	RequestFingerprint           string    `json:"request_fingerprint,omitempty"`
}

func isModelAccountGovernedCommand(commandType string) bool {
	return tokenCommandFamily(commandType) == "MODEL_ACCOUNT"
}

func canonicalizeModelAccountEnvelope(envelope TokenCommandEnvelope) (TokenCommandEnvelope, error) {
	var value any
	switch envelope.CommandType {
	case "TOKEN_MODEL_DRAFT_SAVE":
		command, err := parseGovernedModelDraft(envelope.TargetResourceID, envelope.Command)
		if err != nil {
			return TokenCommandEnvelope{}, err
		}
		value = command
	case "TOKEN_MODEL_PUBLISH":
		command, err := parseModelPublishCommand(envelope.Command)
		modelID, targetErr := uuid.Parse(strings.TrimSpace(envelope.TargetResourceID))
		if err != nil || targetErr != nil || modelID == uuid.Nil || command.ModelID != modelID {
			return TokenCommandEnvelope{}, ErrTokenCommandInvalid
		}
		value = command
	case "TOKEN_ACCOUNT_CREATE":
		command, err := parseGovernedAccountCreate(envelope.TargetResourceID, envelope.Command)
		if err != nil {
			return TokenCommandEnvelope{}, err
		}
		value = command
	case "TOKEN_ACCOUNT_STATUS_CHANGE":
		command, err := parseGovernedAccountStatus(envelope.TargetResourceID, envelope.Command)
		if err != nil {
			return TokenCommandEnvelope{}, err
		}
		value = command
	default:
		return TokenCommandEnvelope{}, ErrTokenCommandInvalid
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return TokenCommandEnvelope{}, ErrTokenCommandInvalid
	}
	envelope.Command, err = canonicalJSON(raw)
	return envelope, err
}

func parseGovernedModelDraft(target string, raw json.RawMessage) (governedModelDraftCommand, error) {
	var command governedModelDraftCommand
	modelID, err := uuid.Parse(strings.TrimSpace(target))
	if err != nil || modelID == uuid.Nil || decodeCommand(raw, &command) != nil || command.ModelID != modelID {
		return command, ErrTokenCommandInvalid
	}
	input := command.catalogInput()
	if command.ExpectedCatalogVersion < 1 || command.ExpectedDraftVersion < 0 || validateCatalogDraft(&input) != nil {
		return command, ErrTokenCommandInvalid
	}
	command.DisplayName, command.Description = input.DisplayName, input.Description
	command.Aliases, command.ProtocolCode = input.Aliases, input.ProtocolCode
	command.SupportedModalities, command.TokenizerVersion = input.SupportedModalities, input.TokenizerVersion
	return command, nil
}

func (c governedModelDraftCommand) catalogInput() CatalogDraftInput {
	return CatalogDraftInput{ExpectedCatalogVersion: c.ExpectedCatalogVersion, ExpectedDraftVersion: c.ExpectedDraftVersion, DisplayName: c.DisplayName, Description: c.Description, Aliases: c.Aliases, ProtocolCode: c.ProtocolCode, ProtocolVersion: c.ProtocolVersion, ContextLength: c.ContextLength, MaximumOutputTokens: c.MaximumOutputTokens, SupportedModalities: c.SupportedModalities, TokenizerVersion: c.TokenizerVersion}
}

func parseGovernedAccountCreate(target string, raw json.RawMessage) (governedAccountCreateCommand, error) {
	var command governedAccountCreateCommand
	ownerID, err := uuid.Parse(strings.TrimSpace(target))
	if err != nil || ownerID == uuid.Nil || decodeCommand(raw, &command) != nil {
		return command, ErrTokenCommandInvalid
	}
	command.OwnerType = strings.ToUpper(strings.TrimSpace(command.OwnerType))
	command.ShareScope = strings.ToUpper(strings.TrimSpace(command.ShareScope))
	command.Name = strings.TrimSpace(command.Name)
	request := command.createRequest(uuid.Nil, uuid.New())
	if command.OwnerType != "USER" || command.ShareScope != "OWNER_UNIT_ONLY" || command.OwnerID != ownerID || !validOwner(request) || !validShareScope(command.ShareScope) || command.Name == "" || len(command.Name) > 120 || command.DefaultPriority < 0 || command.DefaultPriority > 10000 {
		return command, ErrTokenCommandInvalid
	}
	return command, nil
}

func (c governedAccountCreateCommand) createRequest(walletID, actor uuid.UUID) CreateAccountRequest {
	return CreateAccountRequest{OwnerType: c.OwnerType, OwnerID: c.OwnerID, OwnerOrganization: c.OwnerOrganization, OwnerUnit: c.OwnerUnit, WalletID: walletID, Name: c.Name, ShareScope: c.ShareScope, DefaultPriority: c.DefaultPriority, ManagementMode: "PLATFORM", CreatedBy: actor}
}

func parseGovernedAccountStatus(target string, raw json.RawMessage) (governedAccountStatusCommand, error) {
	var command governedAccountStatusCommand
	accountID, err := uuid.Parse(strings.TrimSpace(target))
	if err != nil || accountID == uuid.Nil || decodeCommand(raw, &command) != nil {
		return command, ErrTokenCommandInvalid
	}
	command.TargetStatus = strings.ToUpper(strings.TrimSpace(command.TargetStatus))
	if !slices.Contains([]string{"ACTIVE", "DISABLED"}, command.TargetStatus) {
		return command, ErrTokenCommandInvalid
	}
	return command, nil
}

type accountCreateAuthorityEvidence struct {
	ActorID                      uuid.UUID
	PlatformAuthorizationVersion int64
	ActorVersion                 int64
	ActorIdentityDigest          string
	OwnerVersion                 int64
	OwnerIdentityDigest          string
}

func (s *Service) accountCreateAuthoritySnapshot(ctx context.Context, command governedAccountCreateCommand, reference uuid.UUID, commandDigest string) (accountCreateAuthorityEvidence, error) {
	if s.ownerAuthority == nil {
		return accountCreateAuthorityEvidence{}, errors.New("combined account authority unavailable")
	}
	principal, ok := identityPrincipal(ctx)
	if !ok {
		return accountCreateAuthorityEvidence{}, ErrForbidden
	}
	issuer, subject := strings.TrimSpace(principal.Issuer), principal.Subject
	subjectHash := sha256.Sum256([]byte(subject))
	subjectDigest := hex.EncodeToString(subjectHash[:])
	ownerType, ownerID := authorityOwnerType(command.OwnerType), command.OwnerID.String()
	request := OwnerAuthorityCheck{AuthoritySchemaVersion: tokenauthority.SchemaVersion, Mode: tokenauthority.ModeT1Snapshot, IdentityIssuer: &issuer, IdentitySubject: &subject, IdentitySubjectSHA256: &subjectDigest, OwnerType: &ownerType, OwnerID: &ownerID, RequiredPermissions: append([]string(nil), tokenauthority.RequiredPermissions...), CommandReference: reference.String(), CommandRequestDigest: &commandDigest}
	request.AuthorityRequestSHA256 = request.Digest()
	decision, err := s.ownerAuthority.CheckTokenOwnerAuthority(ctx, request)
	if err != nil {
		return accountCreateAuthorityEvidence{}, err
	}
	if command.OwnerType == "ORG_UNIT" && (command.OwnerOrganization == nil || decision.OwnerOrganizationID == nil || command.OwnerOrganization.String() != *decision.OwnerOrganizationID) {
		return accountCreateAuthorityEvidence{}, ErrNotFound
	}
	if decision.Decision != "ALLOW" || decision.ReasonCode != nil || decision.ActorStatus != "ACTIVE" || decision.OwnerStatus == nil || *decision.OwnerStatus != "ACTIVE" {
		if decision.ReasonCode != nil && (*decision.ReasonCode == "OWNER_NOT_FOUND" || *decision.ReasonCode == "OWNER_INACTIVE") {
			return accountCreateAuthorityEvidence{}, ErrNotFound
		}
		return accountCreateAuthorityEvidence{}, ErrForbidden
	}
	actor, actorErr := uuid.Parse(decision.ActorUserID)
	platformVersion, platformErr := strconv.ParseInt(decision.ObservedPlatformAuthorizationVersion, 10, 64)
	actorVersion, actorVersionErr := strconv.ParseInt(decision.ObservedActorVersion, 10, 64)
	ownerVersion, ownerVersionErr := parseAuthorityVersion(decision.ObservedOwnerVersion)
	if actorErr != nil || platformErr != nil || actorVersionErr != nil || ownerVersionErr != nil || decision.OwnerIdentityDigest == nil {
		return accountCreateAuthorityEvidence{}, ErrCatalogVersionConflict
	}
	return accountCreateAuthorityEvidence{ActorID: actor, PlatformAuthorizationVersion: platformVersion, ActorVersion: actorVersion, ActorIdentityDigest: decision.ActorIdentityDigest, OwnerVersion: ownerVersion, OwnerIdentityDigest: *decision.OwnerIdentityDigest}, nil
}

func (s *Service) buildAccountCreatePrecheck(ctx context.Context, input TokenCommandPrecheckRequest) (TokenCommandPrecheckResult, error) {
	command, err := parseGovernedAccountCreate(input.TargetResourceID, input.Command)
	if err != nil {
		return TokenCommandPrecheckResult{}, err
	}
	commandHash := sha256.Sum256(input.Command)
	authority, err := s.accountCreateAuthoritySnapshot(ctx, command, uuid.Must(uuid.NewV7()), hex.EncodeToString(commandHash[:]))
	if err != nil {
		return TokenCommandPrecheckResult{}, err
	}
	repository, ok := s.repository.(*PostgresRepository)
	if !ok {
		return TokenCommandPrecheckResult{}, errors.New("command precheck repository unavailable")
	}
	tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return TokenCommandPrecheckResult{}, err
	}
	defer tx.Rollback(ctx)
	var observedAt time.Time
	var existing int64
	var capacity tokenAccountOwnerCapacity
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&observedAt); err == nil {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM token_accounts WHERE management_mode='PLATFORM' AND owner_type=$1 AND owner_id=$2 AND lower(name)=lower($3)`, command.OwnerType, command.OwnerID, command.Name).Scan(&existing)
	}
	if err == nil {
		capacity, err = loadTokenAccountOwnerCapacity(ctx, tx, command.OwnerType, command.OwnerID)
	}
	if err != nil {
		return TokenCommandPrecheckResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return TokenCommandPrecheckResult{}, err
	}
	snapshot := commandPrecheckSnapshot{Status: "CREATABLE", ExpectedVersions: map[string]any{
		"owner_type": command.OwnerType, "owner_id": command.OwnerID, "owner_authorization_version": authority.PlatformAuthorizationVersion,
		"actor_version": authority.ActorVersion, "actor_identity_digest": authority.ActorIdentityDigest,
		"owner_version": authority.OwnerVersion, "owner_identity_digest": authority.OwnerIdentityDigest,
		"owner_account_count": capacity.Count, "owner_account_cap": capacity.Policy.MaxPerOwner,
		"owner_account_cap_policy_version": capacity.Policy.PolicyVersion, "creation_version": int64(0),
	}, EvidenceValues: []string{command.OwnerType, command.OwnerID.String(), command.Name, strconv.FormatInt(authority.PlatformAuthorizationVersion, 10), strconv.FormatInt(authority.ActorVersion, 10), authority.ActorIdentityDigest, strconv.FormatInt(authority.OwnerVersion, 10), authority.OwnerIdentityDigest, strconv.FormatInt(existing, 10), strconv.FormatInt(capacity.Count, 10), strconv.FormatInt(capacity.Policy.MaxPerOwner, 10), strconv.FormatInt(capacity.Policy.PolicyVersion, 10), capacity.Policy.ConfigDigest}}
	if existing != 0 {
		snapshot.Blockers = append(snapshot.Blockers, "TOKEN_ACCOUNT_ALREADY_EXISTS")
	}
	if capacity.Count >= capacity.Policy.MaxPerOwner {
		snapshot.Blockers = append(snapshot.Blockers, "TOKEN_ACCOUNT_CAP_REACHED")
	}
	expected, fingerprint, err := commandPrecheckFingerprint("TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT", input.TargetResourceID, input.Command, snapshot)
	if err != nil {
		return TokenCommandPrecheckResult{}, err
	}
	evidenceDigest := sha256.Sum256([]byte(strings.Join(snapshot.EvidenceValues, "\x00")))
	evidenceHash := hex.EncodeToString(evidenceDigest[:])
	return TokenCommandPrecheckResult{ImpactPrecheckResult: ImpactPrecheckResult{
		Action: "TOKEN_ACCOUNT_CREATE", Evidence: ImpactLocalEvidence{ResourceType: "TOKEN_ACCOUNT", ResourceID: input.TargetResourceID, CurrentStatus: snapshot.Status, CurrentVersion: ImpactResourceVersion{EvidenceETag: `"impact-` + evidenceHash + `"`, EvidenceSetSHA256: evidenceHash}, Dependencies: ImpactDependencySummary{}, ObservedAt: observedAt},
		Blockers: snapshot.Blockers, TokenObservedAt: observedAt.UTC(), SnapshotSemantics: "INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS", PrecheckFingerprint: fingerprint, RequiresCASRevalidation: true,
	}, ExpectedVersions: expected}, nil
}

// identityPrincipal is kept as a narrow seam so account creation authority is
// read from the same authenticated request context as the generic command.
func identityPrincipal(ctx context.Context) (identity.Principal, bool) {
	return identity.FromContext(ctx)
}

func (s *Service) executeGovernedModelAccountCommand(ctx context.Context, repository *PostgresRepository, input runtimeTokenCommandBegin, authoritativeExpected json.RawMessage) (TokenCommandResult, bool, error) {
	if repository == nil || !isModelAccountGovernedCommand(input.Envelope.CommandType) || input.ActorUserID == uuid.Nil || input.AuthorizationVersion < 1 || len(input.RequestDigest) != 64 || len(input.KeyDigest) != 64 || len(input.IdempotencyFingerprint) != 64 {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	// Existing-first avoids repeating owner/wallet authority calls on an exact
	// retry, including a response-loss replay.
	if existing, err := repository.TokenCommandByActorKey(ctx, input.ActorUserID, input.Envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return TokenCommandResult{}, false, err
	}
	if input.Envelope.CommandType == "TOKEN_MODEL_PUBLISH" && s.upstream == nil {
		return TokenCommandResult{}, false, errors.New("gateway protocol capabilities unavailable")
	}
	reference := uuid.Must(uuid.NewV7())
	var tx pgx.Tx
	var err error
	if input.Envelope.CommandType == "TOKEN_ACCOUNT_CREATE" {
		command, parseErr := parseGovernedAccountCreate(input.Envelope.TargetResourceID, input.Envelope.Command)
		if parseErr != nil {
			return TokenCommandResult{}, false, parseErr
		}
		var release func()
		tx, release, err = repository.beginOwnerLockedSerializableTransaction(ctx, command.OwnerType, command.OwnerID)
		if err == nil {
			defer release()
		}
	} else {
		tx, err = repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	}
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	defer tx.Rollback(ctx)
	if existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, input.Envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint); existingErr == nil {
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	if err = lockModelAccountCommandTarget(ctx, tx, input.Envelope); err != nil {
		return TokenCommandResult{}, false, err
	}
	if existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, input.Envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint); existingErr == nil {
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	if input.Envelope.CommandType == "TOKEN_ACCOUNT_CREATE" {
		command, parseErr := parseGovernedAccountCreate(input.Envelope.TargetResourceID, input.Envelope.Command)
		var expected governedAccountCreateExpected
		if parseErr != nil || decodeCommand(input.Envelope.ExpectedVersions, &expected) != nil || expected.OwnerType != command.OwnerType || expected.OwnerID != command.OwnerID || expected.CreationVersion != 0 {
			return TokenCommandResult{}, false, ErrCatalogVersionConflict
		}
		authority, authorityErr := s.accountCreateAuthoritySnapshot(ctx, command, reference, input.RequestDigest)
		if authorityErr != nil || authority.ActorID != input.ActorUserID || authority.PlatformAuthorizationVersion != input.AuthorizationVersion || authority.PlatformAuthorizationVersion != expected.OwnerAuthorizationVersion || authority.ActorVersion != expected.ActorVersion || authority.ActorIdentityDigest != expected.ActorIdentityDigest || authority.OwnerVersion != expected.OwnerVersion || authority.OwnerIdentityDigest != expected.OwnerIdentityDigest {
			if authorityErr != nil && !errors.Is(authorityErr, ErrCatalogVersionConflict) {
				return TokenCommandResult{}, false, authorityErr
			}
			return TokenCommandResult{}, false, ErrCatalogVersionConflict
		}
		if capacityErr := lockTokenAccountOwnerCapPolicy(ctx, tx); capacityErr != nil {
			return TokenCommandResult{}, false, capacityErr
		}
		capacity, capacityErr := loadTokenAccountOwnerCapacity(ctx, tx, command.OwnerType, command.OwnerID)
		if capacityErr != nil {
			return TokenCommandResult{}, false, capacityErr
		}
		if capacityErr = capacity.validateExpected(expected); capacityErr != nil {
			return TokenCommandResult{}, false, capacityErr
		}
		var duplicateCount int64
		if capacityErr = tx.QueryRow(ctx, `SELECT count(*) FROM token_accounts WHERE management_mode='PLATFORM' AND owner_type=$1 AND owner_id=$2 AND lower(name)=lower($3)`, command.OwnerType, command.OwnerID, command.Name).Scan(&duplicateCount); capacityErr != nil {
			return TokenCommandResult{}, false, capacityErr
		}
		if duplicateCount != 0 {
			return TokenCommandResult{}, false, ErrCatalogVersionConflict
		}
		current := commandPrecheckSnapshot{Status: "CREATABLE", ExpectedVersions: map[string]any{
			"owner_type": command.OwnerType, "owner_id": command.OwnerID, "owner_authorization_version": authority.PlatformAuthorizationVersion,
			"actor_version": authority.ActorVersion, "actor_identity_digest": authority.ActorIdentityDigest,
			"owner_version": authority.OwnerVersion, "owner_identity_digest": authority.OwnerIdentityDigest,
			"owner_account_count": capacity.Count, "owner_account_cap": capacity.Policy.MaxPerOwner,
			"owner_account_cap_policy_version": capacity.Policy.PolicyVersion, "creation_version": int64(0),
		}, EvidenceValues: []string{command.OwnerType, command.OwnerID.String(), command.Name, strconv.FormatInt(authority.PlatformAuthorizationVersion, 10), strconv.FormatInt(authority.ActorVersion, 10), authority.ActorIdentityDigest, strconv.FormatInt(authority.OwnerVersion, 10), authority.OwnerIdentityDigest, strconv.FormatInt(duplicateCount, 10), strconv.FormatInt(capacity.Count, 10), strconv.FormatInt(capacity.Policy.MaxPerOwner, 10), strconv.FormatInt(capacity.Policy.PolicyVersion, 10), capacity.Policy.ConfigDigest}}
		currentExpected, currentFingerprint, fingerprintErr := commandPrecheckFingerprint("TOKEN_ACCOUNT_CREATE", "TOKEN_ACCOUNT", input.Envelope.TargetResourceID, input.Envelope.Command, current)
		if fingerprintErr != nil || currentFingerprint != input.Envelope.PrecheckFingerprint || !tokenCommandExpectedVersionsMatch(input.Envelope.ExpectedVersions, currentExpected) {
			return TokenCommandResult{}, false, ErrCatalogVersionConflict
		}
		authoritativeExpected = currentExpected
	}
	before, err := modelAccountPrecheckInTx(ctx, tx, input.Envelope, authoritativeExpected)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if err = repository.recheckTokenRouteActivation(ctx, tx); err != nil {
		return TokenCommandResult{}, false, err
	}
	policy, _ := approvalpolicy.Lookup(input.Envelope.CommandType)
	insert, err := tx.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,status,before_state,audit_request_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'PENDING',$15,$16) ON CONFLICT(actor_user_id,command_type,idempotency_key_digest) DO NOTHING`, reference, input.Envelope.CommandType, policy.PolicyVersion, input.ActorUserID, input.ActorPermission, input.AuthorizationVersion, input.KeyDigest, input.IdempotencyFingerprint, input.RequestDigest, input.Envelope.TargetResourceType, input.Envelope.TargetResourceID, input.Envelope.Reason, input.Envelope.ExpectedVersions, input.Envelope.PrecheckFingerprint, before, input.AuditRequestID)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if insert.RowsAffected() == 0 {
		existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, input.Envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint)
		return existing, false, existingErr
	}
	if input.Envelope.CommandType == "TOKEN_ACCOUNT_CREATE" {
		command, _ := parseGovernedAccountCreate(input.Envelope.TargetResourceID, input.Envelope.Command)
		var expected governedAccountCreateExpected
		_ = json.Unmarshal(input.Envelope.ExpectedVersions, &expected)
		selection, ok := ctx.Value(tokenRouteActivationContextKey{}).(tokenRouteActivationSelection)
		if !ok || selection.State != "ACTIVE" || selection.Epoch < 1 || !selection.transactional {
			return TokenCommandResult{}, false, ErrTokenRouteActivationChanged
		}
		now := repository.now()
		_, err = tx.Exec(ctx, `INSERT INTO token_account_provisioning_reservations(command_reference,actor_user_id,owner_type,owner_id,owner_organization_id,owner_unit_id,account_name,share_scope,default_priority,canonical_request_digest,initial_platform_authorization_version,initial_actor_version,initial_actor_identity_digest,initial_owner_version,initial_owner_identity_digest,active_platform_authorization_version,active_actor_version,active_actor_identity_digest,active_owner_version,active_owner_identity_digest,policy_version,activation_epoch,status,next_attempt_at,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$11,$12,$13,$14,$15,$16,$17,'PENDING',$18,$19,$18,$18)`, reference, input.ActorUserID, command.OwnerType, command.OwnerID, command.OwnerOrganization, command.OwnerUnit, command.Name, command.ShareScope, command.DefaultPriority, input.RequestDigest, expected.OwnerAuthorizationVersion, expected.ActorVersion, expected.ActorIdentityDigest, expected.OwnerVersion, expected.OwnerIdentityDigest, expected.OwnerAccountCapPolicyVersion, selection.Epoch, now, now.Add(30*time.Minute))
		if err != nil {
			return TokenCommandResult{}, false, err
		}
		if err = recordTokenAudit(ctx, tx, input.ActorUserID, "TOKEN_ACCOUNT_PROVISIONING_REQUESTED", "TOKEN_ACCOUNT", command.OwnerID, `jsonb_build_object('command_reference',$6,'owner_type',$7,'owner_id',$8,'status','PENDING')`, reference, command.OwnerType, command.OwnerID); err != nil {
			return TokenCommandResult{}, false, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at) VALUES($1,'token.account-provisioning.requested.v1',$2,1,jsonb_build_object('command_reference',$2::uuid,'owner_type',$3::text,'owner_id',$4::uuid),$5,$6)`, uuid.Must(uuid.NewV7()), reference, command.OwnerType, command.OwnerID, input.AuditRequestID, now)
		if err != nil {
			return TokenCommandResult{}, false, err
		}
		pending, scanErr := scanTokenCommandResult(tx.QueryRow(ctx, tokenCommandResultSelect+` WHERE command_reference=$1`, reference))
		if scanErr != nil {
			return TokenCommandResult{}, false, scanErr
		}
		pendingState, pendingEpoch := "PENDING", int64(1)
		pending.RecoveryState, pending.RecoveryEpoch = &pendingState, &pendingEpoch
		if err = tx.Commit(ctx); err != nil {
			return pending, true, err
		}
		return pending, true, nil
	}
	var after, redacted json.RawMessage
	switch input.Envelope.CommandType {
	case "TOKEN_MODEL_DRAFT_SAVE":
		command, _ := parseGovernedModelDraft(input.Envelope.TargetResourceID, input.Envelope.Command)
		draft, applyErr := saveCatalogDraftEffect(ctx, tx, command.ModelID, command.catalogInput(), input.ActorUserID, repository.now())
		err = applyErr
		after, _ = json.Marshal(map[string]any{"model_id": draft.ModelID, "base_catalog_version": draft.BaseCatalogVersion, "draft_version": draft.DraftVersion})
		redacted = after
	case "TOKEN_MODEL_PUBLISH":
		command, _ := parseModelPublishCommand(input.Envelope.Command)
		var expected struct {
			ExpectedCatalogVersion int64 `json:"expected_catalog_version"`
			ExpectedDraftVersion   int64 `json:"expected_draft_version"`
		}
		_ = json.Unmarshal(input.Envelope.ExpectedVersions, &expected)
		var model CatalogModel
		model, err = publishCatalogDraftEffect(ctx, tx, command.ModelID, expected.ExpectedCatalogVersion, expected.ExpectedDraftVersion, input.Envelope.Reason, input.ActorUserID, s.upstream.Protocols(), repository.now())
		after, _ = json.Marshal(map[string]any{"model_id": model.ModelID, "catalog_version": model.CatalogVersion, "lifecycle_status": model.LifecycleStatus})
		redacted = after
	case "TOKEN_ACCOUNT_CREATE":
		command, _ := parseGovernedAccountCreate(input.Envelope.TargetResourceID, input.Envelope.Command)
		var account TokenAccount
		account, err = createAccountEffect(ctx, tx, command.createRequest(uuid.Nil, input.ActorUserID), repository.now())
		after, _ = json.Marshal(map[string]any{"token_account_id": account.ID, "owner_type": account.OwnerType, "owner_id": account.OwnerID, "status": account.Status, "account_version": 1})
		redacted = after
	case "TOKEN_ACCOUNT_STATUS_CHANGE":
		command, _ := parseGovernedAccountStatus(input.Envelope.TargetResourceID, input.Envelope.Command)
		accountID, _ := uuid.Parse(input.Envelope.TargetResourceID)
		var account TokenAccount
		var version int64
		account, version, err = setAccountStatusEffect(ctx, tx, accountID, command.TargetStatus, input.ActorUserID, input.Envelope.Reason, repository.now())
		after, _ = json.Marshal(map[string]any{"token_account_id": account.ID, "status": account.Status, "account_version": version})
		redacted = after
	}
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	item, err := completeRuntimeTokenCommandTx(ctx, tx, input.tokenCommandBegin, input.Envelope, reference, input.IdempotencyFingerprint, before, after, redacted)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		if errors.Is(err, pgx.ErrTxCommitRollback) {
			return TokenCommandResult{}, false, err
		}
		return TokenCommandResult{CommandReference: reference, Status: "PENDING"}, true, err
	}
	return item, true, nil
}

func (r *PostgresRepository) TokenCommandByActorKey(ctx context.Context, actor uuid.UUID, commandType, keyDigest, fingerprint string) (TokenCommandResult, error) {
	item, err := scanTokenCommandResult(r.pool.QueryRow(ctx, tokenCommandResultSelect+` WHERE actor_user_id=$1 AND command_type=$2 AND idempotency_key_digest=$3`, actor, commandType, keyDigest))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenCommandResult{}, ErrNotFound
	}
	if err == nil && item.IdempotencyFingerprint != fingerprint {
		return TokenCommandResult{}, ErrTokenCommandResultMismatch
	}
	return item, err
}

func lockModelAccountCommandTarget(ctx context.Context, tx pgx.Tx, envelope TokenCommandEnvelope) error {
	switch envelope.CommandType {
	case "TOKEN_MODEL_DRAFT_SAVE", "TOKEN_MODEL_PUBLISH":
		return lockCatalogPublication(ctx, tx)
	case "TOKEN_ACCOUNT_CREATE":
		command, err := parseGovernedAccountCreate(envelope.TargetResourceID, envelope.Command)
		if err != nil {
			return err
		}
		return lockTokenAccountOwner(ctx, tx, command.OwnerType, command.OwnerID)
	case "TOKEN_ACCOUNT_STATUS_CHANGE":
		accountID, err := uuid.Parse(envelope.TargetResourceID)
		if err != nil {
			return ErrTokenCommandInvalid
		}
		var locked uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM token_accounts WHERE id=$1 AND management_mode='PLATFORM' FOR UPDATE`, accountID).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	default:
		return ErrTokenCommandInvalid
	}
}

func modelAccountPrecheckInTx(ctx context.Context, tx pgx.Tx, envelope TokenCommandEnvelope, authoritativeExpected json.RawMessage) (json.RawMessage, error) {
	if envelope.CommandType == "TOKEN_ACCOUNT_CREATE" {
		command, err := parseGovernedAccountCreate(envelope.TargetResourceID, envelope.Command)
		if err != nil {
			return nil, err
		}
		var expected governedAccountCreateExpected
		if decodeCommand(envelope.ExpectedVersions, &expected) != nil {
			return nil, ErrTokenCommandInvalid
		}
		var duplicateCount int64
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM token_accounts WHERE management_mode='PLATFORM' AND owner_type=$1 AND owner_id=$2 AND lower(name)=lower($3)`, command.OwnerType, command.OwnerID, command.Name).Scan(&duplicateCount); err != nil {
			return nil, err
		}
		if duplicateCount != 0 {
			return nil, ErrCatalogVersionConflict
		}
		capacity, err := loadTokenAccountOwnerCapacity(ctx, tx, command.OwnerType, command.OwnerID)
		if err != nil {
			return nil, err
		}
		if err = capacity.validateExpected(expected); err != nil {
			return nil, err
		}
		return authoritativeExpected, nil
	}
	snapshot, err := loadCommandPrecheckSnapshot(ctx, tx, envelope.CommandType, envelope.TargetResourceID, envelope.Command)
	if err != nil || len(snapshot.Blockers) > 0 {
		if err == nil {
			err = ErrCatalogVersionConflict
		}
		return nil, err
	}
	expected, fingerprint, err := commandPrecheckFingerprint(envelope.CommandType, envelope.TargetResourceType, envelope.TargetResourceID, envelope.Command, snapshot)
	if err != nil {
		return nil, err
	}
	if envelope.CommandType == "TOKEN_MODEL_PUBLISH" {
		if !jsonObjectSubset(expected, authoritativeExpected) {
			return nil, ErrCatalogVersionConflict
		}
		return authoritativeExpected, nil
	}
	if fingerprint != envelope.PrecheckFingerprint || !tokenCommandExpectedVersionsMatch(envelope.ExpectedVersions, expected) {
		return nil, ErrCatalogVersionConflict
	}
	return expected, nil
}

func jsonObjectSubset(subsetRaw, completeRaw json.RawMessage) bool {
	var subset, complete map[string]any
	if decodeExactCommandJSON(subsetRaw, &subset) != nil || decodeExactCommandJSON(completeRaw, &complete) != nil || subset == nil || complete == nil {
		return false
	}
	for key, value := range subset {
		right, ok := complete[key]
		if !ok || !commandJSONValuesEqual(value, right) {
			return false
		}
	}
	return true
}

func saveCatalogDraftEffect(ctx context.Context, tx pgx.Tx, modelID uuid.UUID, input CatalogDraftInput, actor uuid.UUID, now time.Time) (CatalogDraft, error) {
	if err := validateCatalogDraft(&input); err != nil {
		return CatalogDraft{}, err
	}
	current, err := currentCatalogVersion(ctx, tx)
	if err != nil || current == 0 {
		return CatalogDraft{}, ErrNotFound
	}
	if input.ExpectedCatalogVersion != current {
		return CatalogDraft{}, ErrCatalogVersionConflict
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_catalog_entries WHERE catalog_version=$1 AND model_id=$2 AND lifecycle_status<>'REMOVED')`, current, modelID).Scan(&exists); err != nil || !exists {
		if err == nil {
			err = ErrModelUnavailable
		}
		return CatalogDraft{}, err
	}
	modalities, _ := json.Marshal(input.SupportedModalities)
	var draft CatalogDraft
	var storedModalities []byte
	err = tx.QueryRow(ctx, `INSERT INTO model_catalog_drafts(model_id,base_catalog_version,draft_version,display_name,description,aliases,protocol_code,protocol_version,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,updated_by,updated_at)
VALUES($1,$2,1,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT(model_id) DO UPDATE SET base_catalog_version=EXCLUDED.base_catalog_version,draft_version=model_catalog_drafts.draft_version+1,display_name=EXCLUDED.display_name,description=EXCLUDED.description,aliases=EXCLUDED.aliases,protocol_code=EXCLUDED.protocol_code,protocol_version=EXCLUDED.protocol_version,context_length=EXCLUDED.context_length,maximum_output_tokens=EXCLUDED.maximum_output_tokens,supported_modalities=EXCLUDED.supported_modalities,tokenizer_version=EXCLUDED.tokenizer_version,updated_by=EXCLUDED.updated_by,updated_at=EXCLUDED.updated_at
WHERE model_catalog_drafts.draft_version=$14
RETURNING model_id,base_catalog_version,draft_version,display_name,description,aliases,protocol_code,protocol_version,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,updated_by,updated_at`, modelID, current, input.DisplayName, input.Description, input.Aliases, input.ProtocolCode, input.ProtocolVersion, input.ContextLength, input.MaximumOutputTokens, modalities, input.TokenizerVersion, actor, now, input.ExpectedDraftVersion).Scan(&draft.ModelID, &draft.BaseCatalogVersion, &draft.DraftVersion, &draft.DisplayName, &draft.Description, &draft.Aliases, &draft.ProtocolCode, &draft.ProtocolVersion, &draft.ContextLength, &draft.MaximumOutputTokens, &storedModalities, &draft.TokenizerVersion, &draft.UpdatedBy, &draft.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CatalogDraft{}, ErrCatalogVersionConflict
	}
	if err != nil {
		return CatalogDraft{}, err
	}
	_ = json.Unmarshal(storedModalities, &draft.SupportedModalities)
	if err = appendCatalogAudit(ctx, tx, actor, "MODEL_DRAFT_SAVED", &modelID, nil, strPtr("DRAFT"), current, "draft edited", map[string]any{"draft_version": draft.DraftVersion}); err != nil {
		return CatalogDraft{}, err
	}
	return draft, nil
}

func createAccountEffect(ctx context.Context, tx pgx.Tx, request CreateAccountRequest, now time.Time) (TokenAccount, error) {
	// The effect is shared by direct provisioning and governed creation. Serialize
	// new organization accounts with disable/delete before writing any resource.
	if request.OwnerOrganization != nil {
		if err := lockActiveOrganization(ctx, tx, *request.OwnerOrganization); err != nil {
			return TokenAccount{}, err
		}
	}
	account := TokenAccount{ID: uuid.Must(uuid.NewV7()), OwnerType: request.OwnerType, OwnerID: request.OwnerID, OwnerOrganization: request.OwnerOrganization, OwnerUnit: request.OwnerUnit, WalletID: request.WalletID, Name: request.Name, Status: "ACTIVE", ShareScope: request.ShareScope, DefaultPriority: request.DefaultPriority, ManagementMode: "PLATFORM"}
	if request.OwnerUnit != nil && request.OwnerOrganization != nil {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "organization-unit:"+request.OwnerOrganization.String()+":"+request.OwnerUnit.String()); err != nil {
			return TokenAccount{}, err
		}
		var lifecycleStatus string
		err := tx.QueryRow(ctx, `SELECT status FROM organization_unit_lifecycle_states WHERE organization_id=$1 AND unit_id=$2`, *request.OwnerOrganization, *request.OwnerUnit).Scan(&lifecycleStatus)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) || err == nil && lifecycleStatus != "ACTIVE" {
			return TokenAccount{}, ErrForbidden
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,owner_organization_id,owner_unit_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,$2,$3,$4,$5,$6,$7,'ACTIVE',$8,$9,$10,$10,'PLATFORM')`, account.ID, account.OwnerType, account.OwnerID, account.OwnerOrganization, account.OwnerUnit, account.WalletID, account.Name, account.DefaultPriority, request.CreatedBy, now)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO token_share_policies(token_account_id,scope,owner_organization_id,owner_unit_id,updated_at) VALUES($1,$2,$3,$4,$5)`, account.ID, account.ShareScope, account.OwnerOrganization, account.OwnerUnit, now)
	}
	if err != nil {
		return TokenAccount{}, err
	}
	if err = recordTokenAudit(ctx, tx, request.CreatedBy, "TOKEN_ACCOUNT_CREATED", "TOKEN_ACCOUNT", account.ID, `jsonb_build_object('owner_type',$6,'owner_id',$7,'management_mode',$8)`, account.OwnerType, account.OwnerID, account.ManagementMode); err != nil {
		return TokenAccount{}, err
	}
	return account, nil
}

func setAccountStatusEffect(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, status string, actor uuid.UUID, reason string, now time.Time) (TokenAccount, int64, error) {
	account, err := getTokenAccount(ctx, tx, accountID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenAccount{}, 0, ErrNotFound
	}
	if err != nil || account.ManagementMode != "PLATFORM" || account.Status == "FROZEN" {
		return TokenAccount{}, 0, ErrForbidden
	}
	if account.Status != status {
		result, updateErr := tx.Exec(ctx, `UPDATE token_accounts SET status=$2,status_changed_by=$3,status_change_reason=$4,management_version=management_version+1,updated_at=$5 WHERE id=$1 AND management_mode='PLATFORM' AND status<>'FROZEN'`, accountID, status, actor, reason, now)
		if updateErr != nil || result.RowsAffected() != 1 {
			return TokenAccount{}, 0, ErrCatalogVersionConflict
		}
		account.Status = status
		if err = recordTokenAudit(ctx, tx, actor, "TOKEN_ACCOUNT_STATUS_CHANGED", "TOKEN_ACCOUNT", accountID, `jsonb_build_object('status',$6,'reason',$7)`, status, reason); err != nil {
			return TokenAccount{}, 0, err
		}
	}
	var version int64
	if err = tx.QueryRow(ctx, `SELECT management_version FROM token_accounts WHERE id=$1`, accountID).Scan(&version); err != nil {
		return TokenAccount{}, 0, err
	}
	return account, version, nil
}

func modelAccountEvidenceFingerprint(values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(digest[:])
}

func tokenOwnerIdentityDigest(ownerType string, ownerID uuid.UUID, status string, version int64) string {
	digest := sha256.Sum256([]byte(ownerType + "\n" + ownerID.String() + "\n" + status + "\n" + strconv.FormatInt(version, 10) + "\n"))
	return hex.EncodeToString(digest[:])
}
