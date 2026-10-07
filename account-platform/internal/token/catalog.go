package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrCatalogVersionConflict = errors.New("model catalog version conflict")
	ErrModelUnavailable       = errors.New("model is unavailable")
	ErrModelDeprecated        = errors.New("model deprecation cutoff reached")
	ErrCatalogAliasConflict   = errors.New("model catalog alias conflict")
	ErrIdempotencyConflict    = errors.New("idempotency key payload conflict")
)

type DiscoveredModel struct {
	UpstreamModelRef string
	DisplayName      string
}

type CatalogModel struct {
	ModelID                    uuid.UUID  `json:"model_id"`
	CatalogVersion             int64      `json:"catalog_version"`
	DisplayName                string     `json:"display_name"`
	Description                string     `json:"description"`
	Aliases                    []string   `json:"aliases"`
	LifecycleStatus            string     `json:"lifecycle_status"`
	ProtocolCode               string     `json:"protocol_code"`
	ProtocolVersion            int64      `json:"protocol_version"`
	ProtocolDefinitionID       uuid.UUID  `json:"protocol_definition_id"`
	ModelCapabilityVersionID   uuid.UUID  `json:"model_capability_version_id"`
	CapabilityVersion          int64      `json:"capability_version"`
	ContextLength              int64      `json:"context_length"`
	MaximumOutputTokens        int64      `json:"maximum_output_tokens"`
	SupportedModalities        []string   `json:"supported_modalities"`
	TokenizerVersion           string     `json:"tokenizer_version"`
	SourceAvailable            bool       `json:"source_available"`
	SourceMissingSince         *time.Time `json:"source_missing_since,omitempty"`
	SourceMissingConfirmations int        `json:"source_missing_confirmations"`
	DeprecationEffectiveAt     *time.Time `json:"deprecation_effective_at,omitempty"`
	ReplacementModelID         *uuid.UUID `json:"replacement_model_id,omitempty"`
	NewSessionCutoffAt         *time.Time `json:"new_session_cutoff_at,omitempty"`
	ContinuationCutoffAt       *time.Time `json:"continuation_cutoff_at,omitempty"`
	ForceCancelInflight        bool       `json:"force_cancel_inflight"`
	ProviderID                 string     `json:"-"`
	AdapterVersion             string     `json:"-"`
	UpstreamModelRef           string     `json:"-"`
}

type CatalogDraft struct {
	ModelID             uuid.UUID `json:"model_id"`
	BaseCatalogVersion  int64     `json:"base_catalog_version"`
	DraftVersion        int64     `json:"draft_version"`
	DisplayName         string    `json:"display_name"`
	Description         string    `json:"description"`
	Aliases             []string  `json:"aliases"`
	ProtocolCode        string    `json:"protocol_code"`
	ProtocolVersion     int64     `json:"protocol_version"`
	ContextLength       int64     `json:"context_length"`
	MaximumOutputTokens int64     `json:"maximum_output_tokens"`
	SupportedModalities []string  `json:"supported_modalities"`
	TokenizerVersion    string    `json:"tokenizer_version"`
	UpdatedBy           uuid.UUID `json:"updated_by"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type AdminCatalogModel struct {
	CatalogModel
	Draft *CatalogDraft `json:"draft,omitempty"`
}

type CatalogDraftInput struct {
	ExpectedCatalogVersion int64
	ExpectedDraftVersion   int64
	DisplayName            string
	Description            string
	Aliases                []string
	ProtocolCode           string
	ProtocolVersion        int64
	ContextLength          int64
	MaximumOutputTokens    int64
	SupportedModalities    []string
	TokenizerVersion       string
}

type CatalogLifecycleInput struct {
	ExpectedCatalogVersion int64
	TargetStatus           string
	EffectiveAt            time.Time
	ReplacementModelID     *uuid.UUID
	NewSessionCutoffAt     *time.Time
	ContinuationCutoffAt   *time.Time
	ForceCancelInflight    bool
	IdempotencyKey         string
	Reason                 string
}

type CatalogRepository interface {
	ListCatalog(context.Context) (int64, []CatalogModel, error)
	ResolveCatalogModel(context.Context, string, string, time.Time) (CatalogModel, error)
	AdminCatalogModels(context.Context) (int64, []AdminCatalogModel, error)
	CatalogModelsAtVersion(context.Context, int64) ([]CatalogModel, error)
	SaveCatalogDraft(context.Context, uuid.UUID, CatalogDraftInput, uuid.UUID) (CatalogDraft, error)
	PublishCatalogDraft(context.Context, uuid.UUID, int64, int64, string, string, uuid.UUID, []GatewayProtocolCapability) (CatalogModel, error)
	TransitionCatalogModel(context.Context, uuid.UUID, CatalogLifecycleInput, uuid.UUID) (CatalogModel, error)
	RollbackCatalog(context.Context, int64, int64, string, string, uuid.UUID, []GatewayProtocolCapability) (int64, error)
	SyncCatalog(context.Context, string, string, []DiscoveredModel, int64, int64, uuid.UUID) (int64, bool, error)
}

func (r *PostgresRepository) CatalogModelsAtVersion(ctx context.Context, version int64) ([]CatalogModel, error) {
	rows, err := r.pool.Query(ctx, catalogSelect+` WHERE e.catalog_version=$1 AND e.lifecycle_status<>'REMOVED' ORDER BY e.model_id`, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []CatalogModel{}
	for rows.Next() {
		item, scanErr := scanCatalogModel(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

const catalogSelect = `SELECT e.model_id,e.catalog_version,e.display_name,e.description,e.aliases,e.lifecycle_status,
p.protocol_code,p.version,p.id,e.model_capability_version_id,c.version,c.context_length,c.maximum_output_tokens,
c.supported_modalities,c.tokenizer_version,e.source_available,e.source_missing_since,e.source_missing_confirmations,
e.deprecation_effective_at,e.replacement_model_id,e.new_session_cutoff_at,e.continuation_cutoff_at,e.force_cancel_inflight,
i.provider_id,i.adapter_version,i.upstream_model_ref
FROM model_catalog_entries e
JOIN model_identities i ON i.id=e.model_id
JOIN model_capability_versions c ON c.id=e.model_capability_version_id
JOIN protocol_definitions p ON p.id=e.protocol_definition_id`

func scanCatalogModel(row interface{ Scan(...any) error }) (CatalogModel, error) {
	var item CatalogModel
	var modalities []byte
	err := row.Scan(&item.ModelID, &item.CatalogVersion, &item.DisplayName, &item.Description, &item.Aliases, &item.LifecycleStatus,
		&item.ProtocolCode, &item.ProtocolVersion, &item.ProtocolDefinitionID, &item.ModelCapabilityVersionID, &item.CapabilityVersion,
		&item.ContextLength, &item.MaximumOutputTokens, &modalities, &item.TokenizerVersion, &item.SourceAvailable,
		&item.SourceMissingSince, &item.SourceMissingConfirmations, &item.DeprecationEffectiveAt, &item.ReplacementModelID,
		&item.NewSessionCutoffAt, &item.ContinuationCutoffAt, &item.ForceCancelInflight, &item.ProviderID, &item.AdapterVersion, &item.UpstreamModelRef)
	if err == nil {
		_ = json.Unmarshal(modalities, &item.SupportedModalities)
	}
	return item, err
}

func currentCatalogVersion(ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (int64, error) {
	var version int64
	err := database.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM model_catalog_versions`).Scan(&version)
	return version, err
}

type catalogQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func listCatalog(ctx context.Context, database catalogQueryer) (int64, []CatalogModel, error) {
	version, err := currentCatalogVersion(ctx, database)
	if err != nil || version == 0 {
		return version, []CatalogModel{}, err
	}
	rows, err := database.Query(ctx, catalogSelect+` WHERE e.catalog_version=$1 AND e.lifecycle_status<>'REMOVED' ORDER BY e.display_name,e.model_id`, version)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	items := make([]CatalogModel, 0)
	for rows.Next() {
		item, scanErr := scanCatalogModel(rows)
		if scanErr != nil {
			return 0, nil, scanErr
		}
		items = append(items, item)
	}
	return version, items, rows.Err()
}

func (r *PostgresRepository) ListCatalog(ctx context.Context) (int64, []CatalogModel, error) {
	return listCatalog(ctx, r.pool)
}

func (r *PostgresRepository) ResolveCatalogModel(ctx context.Context, reference, conversationMode string, now time.Time) (CatalogModel, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return CatalogModel{}, ErrNotFound
	}
	version, err := currentCatalogVersion(ctx, r.pool)
	if err != nil || version == 0 {
		return CatalogModel{}, ErrNotFound
	}
	modelID, parseErr := uuid.Parse(reference)
	normalized := normalizeCatalogKey(reference)
	query := catalogSelect + ` WHERE e.catalog_version=$1 AND `
	args := []any{version}
	if parseErr == nil {
		query += `e.model_id=$2`
		args = append(args, modelID)
	} else {
		query += `(e.normalized_name=$2 OR $2=ANY(e.aliases))`
		args = append(args, normalized)
	}
	item, err := scanCatalogModel(r.pool.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return CatalogModel{}, ErrNotFound
	}
	if err != nil {
		return CatalogModel{}, err
	}
	if !item.SourceAvailable || item.LifecycleStatus == "REMOVED" {
		return CatalogModel{}, ErrModelUnavailable
	}
	if item.LifecycleStatus == "DEPRECATED" {
		cutoff := item.NewSessionCutoffAt
		if strings.EqualFold(strings.TrimSpace(conversationMode), "CONTINUE") {
			cutoff = item.ContinuationCutoffAt
		}
		if cutoff == nil || !now.Before(*cutoff) {
			return CatalogModel{}, ErrModelDeprecated
		}
	}
	return item, nil
}

func (r *PostgresRepository) AdminCatalogModels(ctx context.Context) (int64, []AdminCatalogModel, error) {
	version, err := currentCatalogVersion(ctx, r.pool)
	if err != nil {
		return 0, nil, err
	}
	models := make([]CatalogModel, 0)
	if version > 0 {
		rows, queryErr := r.pool.Query(ctx, catalogSelect+` WHERE e.catalog_version=$1 ORDER BY e.display_name,e.model_id`, version)
		if queryErr != nil {
			return 0, nil, queryErr
		}
		for rows.Next() {
			item, scanErr := scanCatalogModel(rows)
			if scanErr != nil {
				rows.Close()
				return 0, nil, scanErr
			}
			models = append(models, item)
		}
		if rows.Err() != nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return 0, nil, err
		}
	}
	byID := make(map[uuid.UUID]*AdminCatalogModel, len(models))
	items := make([]AdminCatalogModel, len(models))
	for index := range models {
		items[index].CatalogModel = models[index]
		byID[models[index].ModelID] = &items[index]
	}
	rows, err := r.pool.Query(ctx, `SELECT model_id,base_catalog_version,draft_version,display_name,description,aliases,protocol_code,protocol_version,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,updated_by,updated_at FROM model_catalog_drafts ORDER BY updated_at DESC`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var draft CatalogDraft
		var modalities []byte
		if err := rows.Scan(&draft.ModelID, &draft.BaseCatalogVersion, &draft.DraftVersion, &draft.DisplayName, &draft.Description, &draft.Aliases, &draft.ProtocolCode, &draft.ProtocolVersion, &draft.ContextLength, &draft.MaximumOutputTokens, &modalities, &draft.TokenizerVersion, &draft.UpdatedBy, &draft.UpdatedAt); err != nil {
			return 0, nil, err
		}
		_ = json.Unmarshal(modalities, &draft.SupportedModalities)
		if existing := byID[draft.ModelID]; existing != nil {
			existing.Draft = &draft
		}
	}
	return version, items, rows.Err()
}

func normalizeCatalogKey(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func normalizeCatalogList(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = normalizeCatalogKey(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func validateCatalogDraft(input *CatalogDraftInput) error {
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.Description = strings.TrimSpace(input.Description)
	input.ProtocolCode = strings.ToUpper(strings.TrimSpace(input.ProtocolCode))
	input.TokenizerVersion = strings.TrimSpace(input.TokenizerVersion)
	input.Aliases = normalizeCatalogList(input.Aliases)
	input.SupportedModalities = normalizeCatalogList(input.SupportedModalities)
	if input.DisplayName == "" || len(input.DisplayName) > 160 || input.ProtocolCode == "" || input.ProtocolVersion <= 0 || input.ContextLength <= 0 || input.MaximumOutputTokens < 0 || input.MaximumOutputTokens > input.ContextLength || input.TokenizerVersion == "" || len(input.SupportedModalities) == 0 {
		return ErrForbidden
	}
	name := normalizeCatalogKey(input.DisplayName)
	for _, alias := range input.Aliases {
		if alias == name {
			return ErrCatalogAliasConflict
		}
	}
	return nil
}

func (r *PostgresRepository) SaveCatalogDraft(ctx context.Context, modelID uuid.UUID, input CatalogDraftInput, actor uuid.UUID) (CatalogDraft, error) {
	if modelID == uuid.Nil || actor == uuid.Nil {
		return CatalogDraft{}, ErrForbidden
	}
	if err := validateCatalogDraft(&input); err != nil {
		return CatalogDraft{}, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return CatalogDraft{}, err
	}
	defer tx.Rollback(ctx)
	if err := r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return CatalogDraft{}, err
	}
	draft, err := saveCatalogDraftEffect(ctx, tx, modelID, input, actor, r.now())
	if err != nil {
		return CatalogDraft{}, err
	}
	return draft, r.commitGovernedMutation(ctx, tx)
}

func strPtr(value string) *string { return &value }

func appendCatalogAudit(ctx context.Context, tx pgx.Tx, actor uuid.UUID, action string, modelID *uuid.UUID, before, after *string, version int64, reason string, evidence any) error {
	payload, _ := json.Marshal(evidence)
	_, err := tx.Exec(ctx, `INSERT INTO model_catalog_audit(id,action,model_id,before_status,after_status,catalog_version,actor_id,reason,evidence,created_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,0),$7,$8,$9,$10)`, uuid.Must(uuid.NewV7()), action, modelID, before, after, version, actor, strings.TrimSpace(reason), payload, time.Now().UTC())
	return err
}

func catalogCommandDigest(value any) string {
	payload, _ := json.Marshal(value)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func claimCatalogCommand(ctx context.Context, tx pgx.Tx, key, commandType, digest string, actor uuid.UUID, now time.Time) (int64, *uuid.UUID, bool, error) {
	key = strings.TrimSpace(key)
	if len(key) < 16 || len(key) > 128 {
		return 0, nil, false, ErrForbidden
	}
	command, err := tx.Exec(ctx, `INSERT INTO model_catalog_commands(idempotency_key,command_type,request_digest,created_by,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(idempotency_key) DO NOTHING`, key, commandType, digest, actor, now)
	if err != nil {
		return 0, nil, false, err
	}
	if command.RowsAffected() == 1 {
		return 0, nil, true, nil
	}
	var storedType, storedDigest string
	var version *int64
	var modelID *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT command_type,request_digest,result_catalog_version,result_model_id FROM model_catalog_commands WHERE idempotency_key=$1`, key).Scan(&storedType, &storedDigest, &version, &modelID); err != nil {
		return 0, nil, false, err
	}
	if storedType != commandType || storedDigest != digest {
		return 0, nil, false, ErrIdempotencyConflict
	}
	if version == nil {
		return 0, nil, false, ErrCatalogVersionConflict
	}
	return *version, modelID, false, nil
}

func createCatalogVersion(ctx context.Context, tx pgx.Tx, source *int64, reason string, actor uuid.UUID, now time.Time) (int64, error) {
	var version int64
	err := tx.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,rollback_source_version,reason,created_by,created_at,published_at) VALUES($1,$2,$3,$4,$5,$5) RETURNING version`, uuid.Must(uuid.NewV7()), source, strings.TrimSpace(reason), actor, now).Scan(&version)
	return version, err
}

func lockCatalogPublication(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('model-catalog-publication',0))`)
	return err
}

func copyCatalogExcept(ctx context.Context, tx pgx.Tx, from, to int64, excluded []uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,source_missing_since,source_missing_confirmations,deprecation_effective_at,replacement_model_id,new_session_cutoff_at,continuation_cutoff_at,force_cancel_inflight)
SELECT $2,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,source_missing_since,source_missing_confirmations,deprecation_effective_at,replacement_model_id,new_session_cutoff_at,continuation_cutoff_at,force_cancel_inflight
FROM model_catalog_entries WHERE catalog_version=$1 AND NOT(model_id=ANY($3::uuid[]))`, from, to, excluded)
	return err
}

func protocolSupported(capabilities []GatewayProtocolCapability, code string, version int64) bool {
	for _, capability := range capabilities {
		if capability.ProtocolCode == code && capability.ProtocolVersion == version {
			return true
		}
	}
	return false
}

func (r *PostgresRepository) PublishCatalogDraft(ctx context.Context, modelID uuid.UUID, expectedCatalog, expectedDraft int64, key, reason string, actor uuid.UUID, capabilities []GatewayProtocolCapability) (CatalogModel, error) {
	if modelID == uuid.Nil || actor == uuid.Nil || strings.TrimSpace(reason) == "" {
		return CatalogModel{}, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return CatalogModel{}, err
	}
	defer tx.Rollback(ctx)
	if err := r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return CatalogModel{}, err
	}
	if err := lockCatalogPublication(ctx, tx); err != nil {
		return CatalogModel{}, err
	}
	now := r.now()
	digest := catalogCommandDigest(map[string]any{"model_id": modelID, "expected_catalog_version": expectedCatalog, "expected_draft_version": expectedDraft, "reason": reason})
	replayedVersion, replayedModel, claimed, err := claimCatalogCommand(ctx, tx, key, "MODEL_DRAFT_PUBLISH", digest, actor, now)
	if err != nil {
		return CatalogModel{}, err
	}
	if !claimed {
		if replayedModel == nil || *replayedModel != modelID {
			return CatalogModel{}, ErrIdempotencyConflict
		}
		item, scanErr := scanCatalogModel(tx.QueryRow(ctx, catalogSelect+` WHERE e.catalog_version=$1 AND e.model_id=$2`, replayedVersion, modelID))
		if scanErr != nil {
			return CatalogModel{}, scanErr
		}
		return item, r.commitGovernedMutation(ctx, tx)
	}
	item, err := publishCatalogDraftEffect(ctx, tx, modelID, expectedCatalog, expectedDraft, reason, actor, capabilities, now)
	if err != nil {
		return CatalogModel{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE model_catalog_commands SET result_catalog_version=$2,result_model_id=$3 WHERE idempotency_key=$1`, key, item.CatalogVersion, modelID); err != nil {
		return CatalogModel{}, err
	}
	return item, r.commitGovernedMutation(ctx, tx)
}

// publishCatalogDraftEffect is the single transactional domain effect shared
// by immediate and scheduled publication. The caller owns the transaction and
// must hold the catalog publication lock. It deliberately does not claim an
// immediate-publication idempotency row: scheduled publication is governed by
// token_command_results and the immutable lifecycle plan instead.
func publishCatalogDraftEffect(ctx context.Context, tx pgx.Tx, modelID uuid.UUID, expectedCatalog, expectedDraft int64, reason string, actor uuid.UUID, capabilities []GatewayProtocolCapability, now time.Time) (CatalogModel, error) {
	current, err := currentCatalogVersion(ctx, tx)
	if err != nil || current != expectedCatalog {
		return CatalogModel{}, ErrCatalogVersionConflict
	}
	var draft CatalogDraft
	var modalities []byte
	if err := tx.QueryRow(ctx, `SELECT model_id,base_catalog_version,draft_version,display_name,description,aliases,protocol_code,protocol_version,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,updated_by,updated_at FROM model_catalog_drafts WHERE model_id=$1 FOR UPDATE`, modelID).Scan(&draft.ModelID, &draft.BaseCatalogVersion, &draft.DraftVersion, &draft.DisplayName, &draft.Description, &draft.Aliases, &draft.ProtocolCode, &draft.ProtocolVersion, &draft.ContextLength, &draft.MaximumOutputTokens, &modalities, &draft.TokenizerVersion, &draft.UpdatedBy, &draft.UpdatedAt); errors.Is(err, pgx.ErrNoRows) {
		return CatalogModel{}, ErrNotFound
	} else if err != nil {
		return CatalogModel{}, err
	}
	if draft.DraftVersion != expectedDraft || draft.BaseCatalogVersion != current || !protocolSupported(capabilities, draft.ProtocolCode, draft.ProtocolVersion) {
		return CatalogModel{}, ErrCatalogVersionConflict
	}
	keys := append([]string{normalizeCatalogKey(draft.DisplayName)}, draft.Aliases...)
	var conflict bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_catalog_entries WHERE catalog_version=$1 AND model_id<>$2 AND (normalized_name=ANY($3) OR aliases && $3::text[]))`, current, modelID, keys).Scan(&conflict); err != nil {
		return CatalogModel{}, err
	}
	if conflict {
		return CatalogModel{}, ErrCatalogAliasConflict
	}
	var protocolID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE protocol_code=$1 AND version=$2 AND status='PUBLISHED'`, draft.ProtocolCode, draft.ProtocolVersion).Scan(&protocolID); errors.Is(err, pgx.ErrNoRows) {
		return CatalogModel{}, ErrModelUnavailable
	} else if err != nil {
		return CatalogModel{}, err
	}
	capabilityID, err := ensureCapabilityVersion(ctx, tx, modelID, protocolID, draft.ContextLength, draft.MaximumOutputTokens, modalities, draft.TokenizerVersion, actor, now)
	if err != nil {
		return CatalogModel{}, err
	}
	version, err := createCatalogVersion(ctx, tx, nil, reason, actor, now)
	if err != nil {
		return CatalogModel{}, err
	}
	if err := copyCatalogExcept(ctx, tx, current, version, []uuid.UUID{modelID}); err != nil {
		return CatalogModel{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,source_missing_since,source_missing_confirmations,force_cancel_inflight)
SELECT $1,e.model_id,$2,$3,$4,$5,'PUBLISHED',$6,$7,e.source_available,e.source_missing_since,e.source_missing_confirmations,false FROM model_catalog_entries e WHERE e.catalog_version=$8 AND e.model_id=$9`, version, draft.DisplayName, normalizeCatalogKey(draft.DisplayName), draft.Description, draft.Aliases, capabilityID, protocolID, current, modelID)
	if err != nil {
		return CatalogModel{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM model_catalog_drafts WHERE model_id=$1`, modelID); err != nil {
		return CatalogModel{}, err
	}
	if err := appendCatalogAudit(ctx, tx, actor, "MODEL_PUBLISHED", &modelID, strPtr("DRAFT"), strPtr("PUBLISHED"), version, reason, map[string]any{"draft_version": expectedDraft}); err != nil {
		return CatalogModel{}, err
	}
	item, err := scanCatalogModel(tx.QueryRow(ctx, catalogSelect+` WHERE e.catalog_version=$1 AND e.model_id=$2`, version, modelID))
	if err != nil {
		return CatalogModel{}, err
	}
	return item, nil
}

func ensureCapabilityVersion(ctx context.Context, tx pgx.Tx, modelID, protocolID uuid.UUID, contextLength, maximumOutput int64, modalities []byte, tokenizer string, actor uuid.UUID, now time.Time) (uuid.UUID, error) {
	var existing uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM model_capability_versions WHERE model_id=$1 AND protocol_definition_id=$2 AND context_length=$3 AND maximum_output_tokens=$4 AND supported_modalities=$5 AND tokenizer_version=$6 ORDER BY version DESC LIMIT 1`, modelID, protocolID, contextLength, maximumOutput, modalities, tokenizer).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	var next int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM model_capability_versions WHERE model_id=$1`, modelID).Scan(&next); err != nil {
		return uuid.Nil, err
	}
	id := uuid.Must(uuid.NewV7())
	_, err = tx.Exec(ctx, `INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, modelID, next, protocolID, contextLength, maximumOutput, modalities, tokenizer, actor, now)
	return id, err
}

func (r *PostgresRepository) TransitionCatalogModel(ctx context.Context, modelID uuid.UUID, input CatalogLifecycleInput, actor uuid.UUID) (CatalogModel, error) {
	input.TargetStatus = strings.ToUpper(strings.TrimSpace(input.TargetStatus))
	input.Reason = strings.TrimSpace(input.Reason)
	if modelID == uuid.Nil || actor == uuid.Nil || input.Reason == "" || (input.TargetStatus != "DEPRECATED" && input.TargetStatus != "REMOVED") {
		return CatalogModel{}, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return CatalogModel{}, err
	}
	defer tx.Rollback(ctx)
	if err := r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return CatalogModel{}, err
	}
	if err := lockCatalogPublication(ctx, tx); err != nil {
		return CatalogModel{}, err
	}
	now := r.now()
	digest := catalogCommandDigest(map[string]any{"model_id": modelID, "input": input})
	replayedVersion, replayedModel, claimed, err := claimCatalogCommand(ctx, tx, input.IdempotencyKey, "MODEL_LIFECYCLE_TRANSITION", digest, actor, now)
	if err != nil {
		return CatalogModel{}, err
	}
	if !claimed {
		if replayedModel == nil || *replayedModel != modelID {
			return CatalogModel{}, ErrIdempotencyConflict
		}
		item, scanErr := scanCatalogModel(tx.QueryRow(ctx, catalogSelect+` WHERE e.catalog_version=$1 AND e.model_id=$2`, replayedVersion, modelID))
		if scanErr != nil {
			return CatalogModel{}, scanErr
		}
		return item, r.commitGovernedMutation(ctx, tx)
	}
	current, err := currentCatalogVersion(ctx, tx)
	if err != nil || current != input.ExpectedCatalogVersion {
		return CatalogModel{}, ErrCatalogVersionConflict
	}
	item, err := scanCatalogModel(tx.QueryRow(ctx, catalogSelect+` WHERE e.catalog_version=$1 AND e.model_id=$2`, current, modelID))
	if errors.Is(err, pgx.ErrNoRows) {
		return CatalogModel{}, ErrNotFound
	}
	if err != nil {
		return CatalogModel{}, err
	}
	valid := item.LifecycleStatus == "PUBLISHED" && (input.TargetStatus == "DEPRECATED" || input.TargetStatus == "REMOVED") || item.LifecycleStatus == "DEPRECATED" && input.TargetStatus == "REMOVED"
	if !valid {
		return CatalogModel{}, ErrForbidden
	}
	effective := input.EffectiveAt.UTC()
	if effective.IsZero() {
		effective = now
	}
	if input.ReplacementModelID != nil {
		var replacementOK bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_catalog_entries WHERE catalog_version=$1 AND model_id=$2 AND lifecycle_status='PUBLISHED')`, current, *input.ReplacementModelID).Scan(&replacementOK); err != nil || !replacementOK {
			return CatalogModel{}, ErrModelUnavailable
		}
	}
	version, err := createCatalogVersion(ctx, tx, nil, input.Reason, actor, now)
	if err != nil {
		return CatalogModel{}, err
	}
	if err := copyCatalogExcept(ctx, tx, current, version, []uuid.UUID{modelID}); err != nil {
		return CatalogModel{}, err
	}
	newCutoff, continuationCutoff := input.NewSessionCutoffAt, input.ContinuationCutoffAt
	if input.TargetStatus == "REMOVED" {
		newCutoff, continuationCutoff = &effective, &effective
	}
	_, err = tx.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,source_missing_since,source_missing_confirmations,deprecation_effective_at,replacement_model_id,new_session_cutoff_at,continuation_cutoff_at,force_cancel_inflight)
SELECT $1,model_id,display_name,normalized_name,description,aliases,$2,model_capability_version_id,protocol_definition_id,source_available,source_missing_since,source_missing_confirmations,$3,$4,$5,$6,$7 FROM model_catalog_entries WHERE catalog_version=$8 AND model_id=$9`, version, input.TargetStatus, effective, input.ReplacementModelID, newCutoff, continuationCutoff, input.ForceCancelInflight, current, modelID)
	if err != nil {
		return CatalogModel{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE model_catalog_commands SET result_catalog_version=$2,result_model_id=$3 WHERE idempotency_key=$1`, input.IdempotencyKey, version, modelID); err != nil {
		return CatalogModel{}, err
	}
	if err := appendCatalogAudit(ctx, tx, actor, "MODEL_LIFECYCLE_CHANGED", &modelID, &item.LifecycleStatus, &input.TargetStatus, version, input.Reason, input); err != nil {
		return CatalogModel{}, err
	}
	if input.TargetStatus == "REMOVED" && input.ForceCancelInflight {
		if _, err := tx.Exec(ctx, `INSERT INTO model_emergency_cancellation_policies(id,catalog_version,model_id,effective_at,requested_by,reason,created_at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(catalog_version,model_id) DO NOTHING`, uuid.Must(uuid.NewV7()), version, modelID, effective, actor, input.Reason, now); err != nil {
			return CatalogModel{}, err
		}
	}
	result, err := scanCatalogModel(tx.QueryRow(ctx, catalogSelect+` WHERE e.catalog_version=$1 AND e.model_id=$2`, version, modelID))
	if err != nil {
		return CatalogModel{}, err
	}
	return result, r.commitGovernedMutation(ctx, tx)
}

func (r *PostgresRepository) RollbackCatalog(ctx context.Context, sourceVersion, expectedVersion int64, key, reason string, actor uuid.UUID, capabilities []GatewayProtocolCapability) (int64, error) {
	if sourceVersion <= 0 || expectedVersion <= 0 || actor == uuid.Nil || strings.TrimSpace(reason) == "" {
		return 0, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err := r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return 0, err
	}
	if err := lockCatalogPublication(ctx, tx); err != nil {
		return 0, err
	}
	now := r.now()
	digest := catalogCommandDigest(map[string]any{"source_version": sourceVersion, "expected_version": expectedVersion, "reason": reason})
	replayedVersion, _, claimed, err := claimCatalogCommand(ctx, tx, key, "MODEL_CATALOG_ROLLBACK", digest, actor, now)
	if err != nil {
		return 0, err
	}
	if !claimed {
		return replayedVersion, r.commitGovernedMutation(ctx, tx)
	}
	version, err := rollbackCatalogEffect(ctx, tx, sourceVersion, expectedVersion, reason, actor, capabilities, now)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE model_catalog_commands SET result_catalog_version=$2 WHERE idempotency_key=$1`, key, version); err != nil {
		return 0, err
	}
	return version, r.commitGovernedMutation(ctx, tx)
}

// rollbackCatalogEffect applies a forward-only catalog rollback inside the
// caller's transaction. Governance adapters use it so the new catalog
// version, immutable domain audit and generic command result/outbox commit as
// one Token transaction.
func rollbackCatalogEffect(ctx context.Context, tx pgx.Tx, sourceVersion, expectedVersion int64, reason string, actor uuid.UUID, capabilities []GatewayProtocolCapability, now time.Time) (int64, error) {
	current, err := currentCatalogVersion(ctx, tx)
	if err != nil || current != expectedVersion {
		return 0, ErrCatalogVersionConflict
	}
	rows, err := tx.Query(ctx, `SELECT p.protocol_code,p.version,e.source_available,EXISTS(SELECT 1 FROM route_provisionings provision JOIN token_accounts ta ON ta.id=provision.active_token_account_id AND ta.status='ACTIVE' JOIN provider_credentials credential ON credential.id=provision.active_credential_id AND credential.status='ACTIVE' AND credential.validation_status='VALID' JOIN routing_policies route ON route.token_account_id=ta.id AND route.model_id=provision.model_id AND route.protocol_version_id=provision.protocol_version_id AND route.route_generation=provision.actual_generation WHERE provision.model_id=e.model_id AND provision.protocol_version_id=p.id AND provision.status='READY') FROM model_catalog_entries e JOIN protocol_definitions p ON p.id=e.protocol_definition_id WHERE e.catalog_version=$1 AND e.lifecycle_status<>'REMOVED'`, sourceVersion)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var code string
		var version int64
		var available bool
		var ready bool
		if err := rows.Scan(&code, &version, &available, &ready); err != nil {
			rows.Close()
			return 0, err
		}
		if !available || !ready || !protocolSupported(capabilities, code, version) {
			rows.Close()
			return 0, ErrModelUnavailable
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	version, err := createCatalogVersion(ctx, tx, &sourceVersion, reason, actor, now)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,source_missing_since,source_missing_confirmations,deprecation_effective_at,replacement_model_id,new_session_cutoff_at,continuation_cutoff_at,force_cancel_inflight)
SELECT $2,s.model_id,s.display_name,s.normalized_name,s.description,s.aliases,s.lifecycle_status,s.model_capability_version_id,s.protocol_definition_id,s.source_available,s.source_missing_since,s.source_missing_confirmations,s.deprecation_effective_at,s.replacement_model_id,s.new_session_cutoff_at,s.continuation_cutoff_at,s.force_cancel_inflight
FROM model_catalog_entries s WHERE s.catalog_version=$1 AND NOT EXISTS(SELECT 1 FROM model_catalog_entries c WHERE c.catalog_version=$3 AND c.model_id=s.model_id AND c.lifecycle_status='REMOVED')`, sourceVersion, version, current)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,source_missing_since,source_missing_confirmations,deprecation_effective_at,replacement_model_id,new_session_cutoff_at,continuation_cutoff_at,force_cancel_inflight)
SELECT $2,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,source_missing_since,source_missing_confirmations,deprecation_effective_at,replacement_model_id,new_session_cutoff_at,continuation_cutoff_at,force_cancel_inflight FROM model_catalog_entries WHERE catalog_version=$1 AND lifecycle_status='REMOVED'`, current, version)
	if err != nil {
		return 0, err
	}
	if err := appendCatalogAudit(ctx, tx, actor, "MODEL_CATALOG_ROLLED_BACK", nil, nil, nil, version, reason, map[string]any{"rollback_source_version": sourceVersion, "previous_version": current}); err != nil {
		return 0, err
	}
	return version, nil
}

type catalogEntryMutation struct {
	modelID                uuid.UUID
	displayName            string
	description            string
	aliases                []string
	lifecycle              string
	capabilityID           uuid.UUID
	protocolID             uuid.UUID
	sourceAvailable        bool
	missingSince           *time.Time
	missingConfirmations   int
	deprecationEffectiveAt *time.Time
	newSessionCutoffAt     *time.Time
	continuationCutoffAt   *time.Time
	replacementModelID     *uuid.UUID
	forceCancelInflight    bool
}

func (r *PostgresRepository) SyncCatalog(ctx context.Context, providerID, adapterVersion string, discovered []DiscoveredModel, contextLength, maximumOutput int64, actor uuid.UUID) (int64, bool, error) {
	providerID = strings.ToUpper(strings.TrimSpace(providerID))
	adapterVersion = strings.TrimSpace(adapterVersion)
	if providerID == "" || adapterVersion == "" || contextLength <= 0 || maximumOutput < 0 || maximumOutput > contextLength || actor == uuid.Nil {
		return 0, false, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	if err := lockCatalogPublication(ctx, tx); err != nil {
		return 0, false, err
	}
	version, changed, err := r.syncCatalogTx(ctx, tx, providerID, adapterVersion, discovered, contextLength, maximumOutput, actor)
	if err != nil {
		return 0, false, err
	}
	return version, changed, tx.Commit(ctx)
}

// syncCatalogTx applies only the exact normalized provider observation passed
// by its caller. Callers that govern a human command must acquire the catalog
// publication lock and validate the frozen observation before entering here.
func (r *PostgresRepository) syncCatalogTx(ctx context.Context, tx pgx.Tx, providerID, adapterVersion string, discovered []DiscoveredModel, contextLength, maximumOutput int64, actor uuid.UUID) (int64, bool, error) {
	now := r.now()
	current, err := currentCatalogVersion(ctx, tx)
	if err != nil {
		return 0, false, err
	}
	var protocolID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE protocol_code='CHAT' AND version=1 AND status='PUBLISHED'`).Scan(&protocolID); err != nil {
		return 0, false, err
	}
	modalities, _ := json.Marshal([]string{"text"})
	seen := map[uuid.UUID]bool{}
	mutations := make([]catalogEntryMutation, 0)
	for _, source := range discovered {
		ref := strings.TrimSpace(source.UpstreamModelRef)
		if ref == "" {
			continue
		}
		digest := sha256.Sum256([]byte(ref))
		var modelID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM model_identities WHERE provider_id=$1 AND upstream_model_ref_digest=$2`, providerID, hex.EncodeToString(digest[:])).Scan(&modelID)
		isNew := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !isNew {
			return 0, false, err
		}
		if isNew {
			modelID = uuid.Must(uuid.NewV7())
			if _, err := tx.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, modelID, providerID, adapterVersion, ref, hex.EncodeToString(digest[:]), actor, now); err != nil {
				return 0, false, err
			}
		}
		if seen[modelID] {
			continue
		}
		seen[modelID] = true
		var currentItem CatalogModel
		if current > 0 {
			currentItem, err = scanCatalogModel(tx.QueryRow(ctx, catalogSelect+` WHERE e.catalog_version=$1 AND e.model_id=$2`, current, modelID))
		}
		if current == 0 || errors.Is(err, pgx.ErrNoRows) {
			capabilityID, capErr := ensureCapabilityVersion(ctx, tx, modelID, protocolID, contextLength, maximumOutput, modalities, "ADAPTER_DECLARED_V1", actor, now)
			if capErr != nil {
				return 0, false, capErr
			}
			display := strings.TrimSpace(source.DisplayName)
			if display == "" {
				display = ref
			}
			aliases := []string{}
			if normalizeCatalogKey(display) != normalizeCatalogKey(ref) {
				aliases = []string{normalizeCatalogKey(ref)}
			}
			mutations = append(mutations, catalogEntryMutation{modelID: modelID, displayName: display, aliases: aliases, lifecycle: "PUBLISHED", capabilityID: capabilityID, protocolID: protocolID, sourceAvailable: true})
			continue
		}
		if err != nil {
			return 0, false, err
		}
		if !currentItem.SourceAvailable || currentItem.SourceMissingConfirmations != 0 {
			mutations = append(mutations, mutationFromCatalog(currentItem, true, nil, 0))
		}
	}
	if current > 0 {
		rows, err := tx.Query(ctx, catalogSelect+` WHERE e.catalog_version=$1 AND i.provider_id=$2`, current, providerID)
		if err != nil {
			return 0, false, err
		}
		for rows.Next() {
			item, scanErr := scanCatalogModel(rows)
			if scanErr != nil {
				rows.Close()
				return 0, false, scanErr
			}
			if seen[item.ModelID] || item.LifecycleStatus == "REMOVED" {
				continue
			}
			confirmations := item.SourceMissingConfirmations + 1
			missingSince := item.SourceMissingSince
			if missingSince == nil {
				value := now
				missingSince = &value
			}
			mutation := mutationFromCatalog(item, false, missingSince, confirmations)
			if confirmations >= 2 && item.LifecycleStatus == "PUBLISHED" {
				mutation.lifecycle = "DEPRECATED"
				mutation.deprecationEffectiveAt = &now
				mutation.newSessionCutoffAt = &now
				mutation.continuationCutoffAt = &now
			}
			mutations = append(mutations, mutation)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, false, err
		}
		rows.Close()
	}
	if len(mutations) == 0 {
		return current, false, nil
	}
	version, err := createCatalogVersion(ctx, tx, nil, "adapter catalog synchronization", actor, now)
	if err != nil {
		return 0, false, err
	}
	excluded := make([]uuid.UUID, 0, len(mutations))
	for _, mutation := range mutations {
		excluded = append(excluded, mutation.modelID)
	}
	if current > 0 {
		if err := copyCatalogExcept(ctx, tx, current, version, excluded); err != nil {
			return 0, false, err
		}
	}
	for _, mutation := range mutations {
		_, err := tx.Exec(ctx, `INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,source_missing_since,source_missing_confirmations,deprecation_effective_at,replacement_model_id,new_session_cutoff_at,continuation_cutoff_at,force_cancel_inflight) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, version, mutation.modelID, mutation.displayName, normalizeCatalogKey(mutation.displayName), mutation.description, mutation.aliases, mutation.lifecycle, mutation.capabilityID, mutation.protocolID, mutation.sourceAvailable, mutation.missingSince, mutation.missingConfirmations, mutation.deprecationEffectiveAt, mutation.replacementModelID, mutation.newSessionCutoffAt, mutation.continuationCutoffAt, mutation.forceCancelInflight)
		if err != nil {
			return 0, false, err
		}
		action := "MODEL_SOURCE_SYNCHRONIZED"
		if mutation.lifecycle == "DEPRECATED" {
			action = "MODEL_SOURCE_MISSING_DEPRECATED"
		}
		if err := appendCatalogAudit(ctx, tx, actor, action, &mutation.modelID, nil, &mutation.lifecycle, version, "adapter catalog synchronization", map[string]any{"source_available": mutation.sourceAvailable, "missing_confirmations": mutation.missingConfirmations}); err != nil {
			return 0, false, err
		}
	}
	return version, true, nil
}

func mutationFromCatalog(item CatalogModel, available bool, missingSince *time.Time, confirmations int) catalogEntryMutation {
	return catalogEntryMutation{modelID: item.ModelID, displayName: item.DisplayName, description: item.Description, aliases: item.Aliases, lifecycle: item.LifecycleStatus, capabilityID: item.ModelCapabilityVersionID, protocolID: item.ProtocolDefinitionID, sourceAvailable: available, missingSince: missingSince, missingConfirmations: confirmations, deprecationEffectiveAt: item.DeprecationEffectiveAt, replacementModelID: item.ReplacementModelID, newSessionCutoffAt: item.NewSessionCutoffAt, continuationCutoffAt: item.ContinuationCutoffAt, forceCancelInflight: item.ForceCancelInflight}
}

func (item CatalogModel) Snapshot() map[string]any {
	return map[string]any{"model_id": item.ModelID, "catalog_version": item.CatalogVersion, "display_name": item.DisplayName, "lifecycle_status": item.LifecycleStatus, "deprecation_effective_at": item.DeprecationEffectiveAt, "protocol_definition_id": item.ProtocolDefinitionID, "protocol_code": item.ProtocolCode, "protocol_version": item.ProtocolVersion, "model_capability_version_id": item.ModelCapabilityVersionID, "capability_version": item.CapabilityVersion, "context_length": item.ContextLength, "maximum_output_tokens": item.MaximumOutputTokens, "supported_modalities": item.SupportedModalities, "tokenizer_version": item.TokenizerVersion}
}

func (item CatalogModel) ValidateRequestLimits(input, output int64) error {
	if input < 0 || output < 0 || output > item.MaximumOutputTokens || input+output > item.ContextLength {
		return fmt.Errorf("%w: request exceeds frozen model capability", ErrModelUnavailable)
	}
	return nil
}

type CatalogSynchronizer struct {
	repository     CatalogRepository
	adapter        ModelCatalogAdapter
	providerID     string
	adapterVersion string
	baseURL        string
	secret         []byte
	contextLength  int64
	maximumOutput  int64
	interval       time.Duration
	actor          uuid.UUID
}

type CatalogSyncPreview struct {
	CatalogVersion         int64
	AdapterObservedVersion string
	DiffFingerprint        string
	Added                  int
	Missing                int
	ObservedAt             time.Time
	Difference             *CatalogDifference
}

type catalogSyncObservation struct {
	ProviderID     string                        `json:"provider_id"`
	AdapterVersion string                        `json:"adapter_version"`
	Models         []catalogSyncObservationModel `json:"models"`
}

type catalogSyncObservationModel struct {
	UpstreamModelRef string `json:"upstream_model_ref"`
	DisplayName      string `json:"display_name"`
}

// CatalogSourceAdapter needs discovery and immutable adapter identity, without
// exposing an invocation contract from the later personal execution feature.
type CatalogSourceAdapter interface {
	ModelCatalogAdapter
	AdapterVersion() string
}

func NewCatalogSynchronizer(repository CatalogRepository, adapter CatalogSourceAdapter, providerID, baseURL string, secret []byte, contextLength, maximumOutput int64, interval time.Duration) (*CatalogSynchronizer, error) {
	catalogAdapter, ok := adapter.(ModelCatalogAdapter)
	providerID = strings.ToUpper(strings.TrimSpace(providerID))
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !ok || repository == nil || providerID == "" || baseURL == "" || len(secret) < 8 || contextLength <= 0 || maximumOutput < 0 || maximumOutput > contextLength {
		return nil, ErrForbidden
	}
	if interval < time.Minute {
		interval = 15 * time.Minute
	}
	return &CatalogSynchronizer{repository: repository, adapter: catalogAdapter, providerID: providerID, adapterVersion: adapter.AdapterVersion(), baseURL: baseURL, secret: append([]byte(nil), secret...), contextLength: contextLength, maximumOutput: maximumOutput, interval: interval, actor: uuid.NewSHA1(uuid.NameSpaceURL, []byte("pinvou:model-catalog-sync"))}, nil
}

func (s *CatalogSynchronizer) Sync(ctx context.Context) (int64, bool, error) {
	preview, err := s.Preview(ctx)
	if err != nil {
		return 0, false, err
	}
	return s.SyncObserved(ctx, preview.AdapterObservedVersion)
}

// SyncObserved performs the sole write-before re-observation allowed by the
// catalog synchronization contract. The exact normalized observation whose
// digest is compared here is passed to the repository; it is never discovered
// a third time between approval and persistence.
func (s *CatalogSynchronizer) SyncObserved(ctx context.Context, expectedObservedVersion string) (int64, bool, error) {
	models, observedVersion, err := s.discoverObservation(ctx)
	if err != nil {
		return 0, false, err
	}
	if len(expectedObservedVersion) != 64 || strings.ToLower(expectedObservedVersion) != expectedObservedVersion || observedVersion != expectedObservedVersion {
		return 0, false, ErrCatalogVersionConflict
	}
	return s.repository.SyncCatalog(ctx, s.providerID, s.adapterVersion, models, s.contextLength, s.maximumOutput, s.actor)
}

func (s *CatalogSynchronizer) discoverObservation(ctx context.Context) ([]DiscoveredModel, string, error) {
	discovered, err := s.adapter.DiscoverModels(ctx, s.baseURL, s.secret)
	if err != nil {
		return nil, "", err
	}
	return normalizeCatalogObservation(s.providerID, s.adapterVersion, discovered)
}

func normalizeCatalogObservation(providerID, adapterVersion string, discovered []DiscoveredModel) ([]DiscoveredModel, string, error) {
	byReference := make(map[string]string, len(discovered))
	for _, item := range discovered {
		reference := strings.TrimSpace(item.UpstreamModelRef)
		if reference == "" {
			continue
		}
		displayName := strings.TrimSpace(item.DisplayName)
		if previous, exists := byReference[reference]; exists && previous != displayName {
			return nil, "", ErrCatalogVersionConflict
		}
		byReference[reference] = displayName
	}
	normalized := make([]DiscoveredModel, 0, len(byReference))
	for reference, displayName := range byReference {
		normalized = append(normalized, DiscoveredModel{UpstreamModelRef: reference, DisplayName: displayName})
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].UpstreamModelRef != normalized[j].UpstreamModelRef {
			return normalized[i].UpstreamModelRef < normalized[j].UpstreamModelRef
		}
		return normalized[i].DisplayName < normalized[j].DisplayName
	})
	observedModels := make([]catalogSyncObservationModel, 0, len(normalized))
	for _, item := range normalized {
		observedModels = append(observedModels, catalogSyncObservationModel{
			UpstreamModelRef: item.UpstreamModelRef,
			DisplayName:      item.DisplayName,
		})
	}
	payload, err := json.Marshal(catalogSyncObservation{ProviderID: providerID, AdapterVersion: adapterVersion, Models: observedModels})
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(payload)
	return normalized, hex.EncodeToString(digest[:]), nil
}

func (s *CatalogSynchronizer) Preview(ctx context.Context) (CatalogSyncPreview, error) {
	discovered, observedVersion, err := s.discoverObservation(ctx)
	if err != nil {
		return CatalogSyncPreview{}, err
	}
	version, current, err := s.repository.ListCatalog(ctx)
	if err != nil {
		return CatalogSyncPreview{}, err
	}
	return catalogPreviewFromObservation(version, current, discovered, observedVersion, s.providerID, time.Now().UTC()), nil
}

func catalogPreviewFromObservation(version int64, current []CatalogModel, discovered []DiscoveredModel, observedVersion, providerID string, observedAt time.Time) CatalogSyncPreview {
	currentRefs := make([]string, 0, len(current))
	currentSet := make(map[string]CatalogModel, len(current))
	for _, item := range current {
		if strings.ToUpper(strings.TrimSpace(item.ProviderID)) != providerID || item.LifecycleStatus == "REMOVED" {
			continue
		}
		key := strings.TrimSpace(item.UpstreamModelRef)
		currentRefs = append(currentRefs, key)
		currentSet[key] = item
	}
	discoveredRefs := make([]string, 0, len(discovered))
	discoveredSet := make(map[string]struct{}, len(discovered))
	added, modified := 0, 0
	differenceItems := make([]CatalogDifferenceItem, 0)
	for _, item := range discovered {
		key := strings.TrimSpace(item.UpstreamModelRef)
		if key == "" {
			continue
		}
		if _, duplicate := discoveredSet[key]; duplicate {
			continue
		}
		discoveredRefs = append(discoveredRefs, key)
		discoveredSet[key] = struct{}{}
		currentItem, exists := currentSet[key]
		if !exists {
			added++
			differenceItems = append(differenceItems, CatalogDifferenceItem{ChangeType: "ADDED", DisplayName: strings.TrimSpace(item.DisplayName)})
		} else if !currentItem.SourceAvailable {
			modified++
			id := currentItem.ModelID
			differenceItems = append(differenceItems, CatalogDifferenceItem{ChangeType: "MODIFIED", ModelID: &id, DisplayName: currentItem.DisplayName})
		}
	}
	missing := 0
	for key, item := range currentSet {
		if _, exists := discoveredSet[key]; !exists {
			missing++
			id := item.ModelID
			differenceItems = append(differenceItems, CatalogDifferenceItem{ChangeType: "REMOVED", ModelID: &id, DisplayName: item.DisplayName})
		}
	}
	sort.Slice(differenceItems, func(i, j int) bool {
		if differenceItems[i].ChangeType != differenceItems[j].ChangeType {
			return differenceItems[i].ChangeType < differenceItems[j].ChangeType
		}
		if differenceItems[i].DisplayName != differenceItems[j].DisplayName {
			return differenceItems[i].DisplayName < differenceItems[j].DisplayName
		}
		return valueUUID(differenceItems[i].ModelID) < valueUUID(differenceItems[j].ModelID)
	})
	sort.Strings(currentRefs)
	sort.Strings(discoveredRefs)
	diffDigest := sha256.Sum256([]byte(strings.Join(append(append([]string{strconv.FormatInt(version, 10), observedVersion}, currentRefs...), discoveredRefs...), "\x00")))
	observed := observedVersion
	visible := differenceItems
	if len(visible) > catalogDifferencePreviewLimit {
		visible = visible[:catalogDifferencePreviewLimit]
	}
	difference := &CatalogDifference{BaselineCatalogVersion: version, TargetObservedVersion: &observed, AddedCount: int64(added), ModifiedCount: int64(modified), RemovedCount: int64(missing), TotalCount: int64(added + modified + missing), Truncated: len(differenceItems) > len(visible), Items: append([]CatalogDifferenceItem(nil), visible...)}
	return CatalogSyncPreview{CatalogVersion: version, AdapterObservedVersion: observedVersion, DiffFingerprint: hex.EncodeToString(diffDigest[:]), Added: added, Missing: missing, ObservedAt: observedAt.UTC(), Difference: difference}
}

func (s *CatalogSynchronizer) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			timeout, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, _, _ = s.Sync(timeout)
			cancel()
		}
	}
}
