package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type RechargeProductCatalogItem struct {
	CatalogItemID      uuid.UUID                   `json:"catalog_item_id"`
	ProductID          string                      `json:"product_id"`
	DisplayNameZhCN    string                      `json:"display_name_zh_cn"`
	DisplayNameEN      string                      `json:"display_name_en"`
	DisplayNameJA      string                      `json:"display_name_ja"`
	DisplayOrder       int                         `json:"display_order"`
	ReplacementProduct *string                     `json:"replacement_product_id,omitempty"`
	LifecycleStatus    string                      `json:"lifecycle_status"`
	Version            int64                       `json:"version"`
	AdmissionVersion   int64                       `json:"admission_version"`
	EverPublished      bool                        `json:"ever_published"`
	DeletedAt          *time.Time                  `json:"deleted_at,omitempty"`
	UpdatedAt          time.Time                   `json:"updated_at"`
	Mapping            RechargeProductMappingDraft `json:"mapping"`
}

const defaultTestAdmissionEvidenceTTL = 30 * 24 * time.Hour

func (r *PostgresRepository) VerifyPendingTestProductAdmissions(ctx context.Context, capabilities payment.Capabilities, evidenceTTL time.Duration, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if evidenceTTL <= 0 {
		evidenceTTL = defaultTestAdmissionEvidenceTTL
	}
	rows, err := r.pool.Query(ctx, `SELECT verification.verification_id,draft.provider_id,draft.provider_version,draft.channels
FROM product_admission_verifications verification
JOIN recharge_product_mapping_drafts draft USING(catalog_item_id)
WHERE verification.status='PENDING' AND draft.admission_class='TEST_ONLY'
ORDER BY verification.requested_at,verification.verification_id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type candidate struct {
		id                          uuid.UUID
		providerID, providerVersion string
		channels                    []string
	}
	items := make([]candidate, 0)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.providerID, &item.providerVersion, &item.channels); err != nil {
			return 0, err
		}
		items = append(items, item)
	}
	completed := 0
	for _, item := range items {
		if item.providerID != capabilities.ProviderID || item.providerVersion != capabilities.Version {
			continue
		}
		valid := true
		for _, channel := range item.channels {
			valid = valid && capabilities.SupportsChannel(channel)
		}
		if !valid {
			continue
		}
		if _, err := r.CompleteProductAdmissionVerification(ctx, item.id, "billing-admission-verifier", "TEST_ADAPTER_CAPABILITY_VALIDATION", capabilities.ProviderID+"/"+capabilities.Version, r.now().Add(evidenceTTL)); err == nil {
			completed++
		}
	}
	return completed, nil
}

type ProductAdmissionVerificationWorker struct {
	repository   *PostgresRepository
	capabilities payment.Capabilities
	interval     time.Duration
	evidenceTTL  time.Duration
}

func NewProductAdmissionVerificationWorker(repository *PostgresRepository, capabilities payment.Capabilities, interval, evidenceTTL time.Duration) *ProductAdmissionVerificationWorker {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if evidenceTTL <= 0 {
		evidenceTTL = defaultTestAdmissionEvidenceTTL
	}
	return &ProductAdmissionVerificationWorker{repository: repository, capabilities: capabilities, interval: interval, evidenceTTL: evidenceTTL}
}
func (w *ProductAdmissionVerificationWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		_, _ = w.repository.VerifyPendingTestProductAdmissions(ctx, w.capabilities, w.evidenceTTL, 20)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type RechargeProductMappingDraft struct {
	DraftVersion    int64    `json:"draft_version"`
	ProviderID      string   `json:"provider_id"`
	ProviderVersion string   `json:"provider_version"`
	ExternalSKUBOM  string   `json:"sku_bom"`
	AmountFen       int64    `json:"amount_fen"`
	Currency        string   `json:"currency"`
	Channels        []string `json:"channels"`
	AdmissionClass  string   `json:"admission_class"`
}

type CreateRechargeProduct struct {
	ProductID          string                      `json:"product_id"`
	DisplayNameZhCN    string                      `json:"display_name_zh_cn"`
	DisplayNameEN      string                      `json:"display_name_en"`
	DisplayNameJA      string                      `json:"display_name_ja"`
	DisplayOrder       int                         `json:"display_order"`
	ReplacementProduct *string                     `json:"replacement_product_id,omitempty"`
	BasePoints         int64                       `json:"base_points"`
	BonusPoints        int64                       `json:"bonus_points"`
	CreditMicrocredits int64                       `json:"credit_microcredits"`
	ValidityDays       int                         `json:"validity_days"`
	InvoiceProductName string                      `json:"invoice_product_name"`
	Mapping            RechargeProductMappingDraft `json:"mapping"`
}

type ProductAdmissionVerification struct {
	VerificationID     uuid.UUID  `json:"verification_id"`
	CatalogItemID      uuid.UUID  `json:"catalog_item_id"`
	RetryOf            *uuid.UUID `json:"retry_of,omitempty"`
	PublishFingerprint string     `json:"publish_fingerprint"`
	Status             string     `json:"status"`
	FailureCode        *string    `json:"failure_code,omitempty"`
	RequestedAt        time.Time  `json:"requested_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
	EvidenceID         *uuid.UUID `json:"evidence_id,omitempty"`
}

type RechargeProductCatalogRepository interface {
	CreateRechargeProduct(context.Context, CreateRechargeProduct) (RechargeProductCatalogItem, error)
	RechargeProductCatalogItem(context.Context, uuid.UUID) (RechargeProductCatalogItem, error)
	ListRechargeProducts(context.Context, string, bool, int, *time.Time, *uuid.UUID) ([]RechargeProductCatalogItem, error)
	StartProductAdmissionVerification(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (ProductAdmissionVerification, bool, error)
	ListProductAdmissionVerifications(context.Context, uuid.UUID) ([]ProductAdmissionVerification, error)
	EvidenceIDForVerification(context.Context, uuid.UUID, uuid.UUID) (uuid.UUID, error)
	CompleteProductAdmissionVerification(context.Context, uuid.UUID, string, string, string, time.Time) (uuid.UUID, error)
	TransitionRechargeProduct(context.Context, uuid.UUID, int64, string, *uuid.UUID) (RechargeProductCatalogItem, error)
	RevokeProductAdmission(context.Context, uuid.UUID, string, string, string) error
	RechargeProductCommand(context.Context, string, string) (int, json.RawMessage, bool, error)
	RecordRechargeProductCommand(context.Context, string, string, string, *uuid.UUID, string, string, int64, *int64, *int64, int, json.RawMessage) error
}

func (r *PostgresRepository) EvidenceIDForVerification(ctx context.Context, catalogItemID, verificationID uuid.UUID) (uuid.UUID, error) {
	var evidenceID uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT evidence_id FROM product_admission_evidence WHERE catalog_item_id=$1 AND verification_id=$2`, catalogItemID, verificationID).Scan(&evidenceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	return evidenceID, err
}

func (r *PostgresRepository) ListRechargeProducts(ctx context.Context, lifecycle string, includeDeleted bool, limit int, cursorTime *time.Time, cursorID *uuid.UUID) ([]RechargeProductCatalogItem, error) {
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	lifecycle = strings.ToUpper(strings.TrimSpace(lifecycle))
	rows, err := r.pool.Query(ctx, `SELECT item.catalog_item_id,item.product_id,item.display_name_zh_cn,item.display_name_en,item.display_name_ja,item.display_order,item.replacement_product_id,item.lifecycle_status,item.version,item.admission_version,item.ever_published,item.deleted_at,item.updated_at,
draft.draft_version,draft.provider_id,draft.provider_version,draft.external_sku_bom,draft.amount_fen,draft.currency,draft.channels,draft.admission_class
FROM recharge_product_catalog_items item JOIN recharge_product_mapping_drafts draft USING(catalog_item_id)
WHERE ($1='' OR item.lifecycle_status=$1) AND ($2 OR item.lifecycle_status<>'DELETED')
  AND ($3::timestamptz IS NULL OR (item.updated_at,item.catalog_item_id)<($3,$4))
ORDER BY item.updated_at DESC,item.catalog_item_id DESC LIMIT $5`, lifecycle, includeDeleted, cursorTime, cursorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]RechargeProductCatalogItem, 0)
	for rows.Next() {
		var item RechargeProductCatalogItem
		if err := rows.Scan(&item.CatalogItemID, &item.ProductID, &item.DisplayNameZhCN, &item.DisplayNameEN, &item.DisplayNameJA, &item.DisplayOrder, &item.ReplacementProduct, &item.LifecycleStatus, &item.Version, &item.AdmissionVersion, &item.EverPublished, &item.DeletedAt, &item.UpdatedAt, &item.Mapping.DraftVersion, &item.Mapping.ProviderID, &item.Mapping.ProviderVersion, &item.Mapping.ExternalSKUBOM, &item.Mapping.AmountFen, &item.Mapping.Currency, &item.Mapping.Channels, &item.Mapping.AdmissionClass); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) ListProductAdmissionVerifications(ctx context.Context, catalogItemID uuid.UUID) ([]ProductAdmissionVerification, error) {
	rows, err := r.pool.Query(ctx, `SELECT verification.verification_id,verification.catalog_item_id,verification.retry_of,verification.publish_fingerprint,verification.status,verification.failure_code,verification.requested_at,verification.completed_at,evidence.evidence_id FROM product_admission_verifications verification LEFT JOIN product_admission_evidence evidence USING(verification_id) WHERE verification.catalog_item_id=$1 ORDER BY verification.requested_at DESC,verification.verification_id DESC`, catalogItemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ProductAdmissionVerification, 0)
	for rows.Next() {
		var item ProductAdmissionVerification
		if err := rows.Scan(&item.VerificationID, &item.CatalogItemID, &item.RetryOf, &item.PublishFingerprint, &item.Status, &item.FailureCode, &item.RequestedAt, &item.CompletedAt, &item.EvidenceID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) RechargeProductCommand(ctx context.Context, idempotencyKey, requestHash string) (int, json.RawMessage, bool, error) {
	var hash string
	var status int
	var body []byte
	err := r.pool.QueryRow(ctx, `SELECT request_hash,response_status,response_body FROM recharge_product_command_records WHERE idempotency_key=$1 AND expires_at>now()`, idempotencyKey).Scan(&hash, &status, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, err
	}
	if hash != requestHash {
		return 0, nil, false, ErrConflict
	}
	return status, json.RawMessage(body), true, nil
}

func (r *PostgresRepository) RecordRechargeProductCommand(ctx context.Context, idempotencyKey, commandType, requestHash string, catalogItemID *uuid.UUID, actor, reason string, expectedVersion int64, beforeVersion, afterVersion *int64, status int, body json.RawMessage) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	command, err := tx.Exec(ctx, `INSERT INTO recharge_product_command_records(idempotency_key,command_type,request_hash,catalog_item_id,actor_subject,reason,expected_version,response_status,response_body,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`, idempotencyKey, commandType, requestHash, catalogItemID, actor, reason, expectedVersion, status, body, now, now.Add(24*time.Hour))
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		existingStatus, existingBody, found, loadErr := r.RechargeProductCommand(ctx, idempotencyKey, requestHash)
		_ = existingStatus
		_ = existingBody
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, `INSERT INTO recharge_product_audits(audit_id,command_type,catalog_item_id,actor_subject,reason,idempotency_key,request_hash,expected_version,before_version,after_version,result,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'SUCCEEDED',$11)`, uuid.Must(uuid.NewV7()), commandType, catalogItemID, actor, reason, idempotencyKey, requestHash, expectedVersion, beforeVersion, afterVersion, now)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) CreateRechargeProduct(ctx context.Context, input CreateRechargeProduct) (RechargeProductCatalogItem, error) {
	input.ProductID = strings.TrimSpace(input.ProductID)
	input.DisplayNameZhCN = strings.TrimSpace(input.DisplayNameZhCN)
	input.DisplayNameEN = strings.TrimSpace(input.DisplayNameEN)
	input.DisplayNameJA = strings.TrimSpace(input.DisplayNameJA)
	input.InvoiceProductName = strings.TrimSpace(input.InvoiceProductName)
	input.Mapping.ProviderID = strings.TrimSpace(input.Mapping.ProviderID)
	input.Mapping.ProviderVersion = strings.TrimSpace(input.Mapping.ProviderVersion)
	input.Mapping.ExternalSKUBOM = strings.TrimSpace(input.Mapping.ExternalSKUBOM)
	input.Mapping.Currency = strings.ToUpper(strings.TrimSpace(input.Mapping.Currency))
	input.Mapping.AdmissionClass = strings.ToUpper(strings.TrimSpace(input.Mapping.AdmissionClass))
	if input.ProductID == "" || input.DisplayNameZhCN == "" || input.DisplayNameEN == "" || input.DisplayNameJA == "" ||
		input.InvoiceProductName == "" || input.BasePoints < 0 || input.BonusPoints < 0 || input.BasePoints+input.BonusPoints <= 0 ||
		input.CreditMicrocredits <= 0 || input.ValidityDays <= 0 || input.Mapping.ProviderID == "" || input.Mapping.ProviderVersion == "" ||
		input.Mapping.ExternalSKUBOM == "" || input.Mapping.AmountFen <= 0 || input.Mapping.Currency != "CNY" || len(input.Mapping.Channels) == 0 ||
		(input.Mapping.AdmissionClass != "FORMAL" && input.Mapping.AdmissionClass != "TEST_ONLY") {
		return RechargeProductCatalogItem{}, ErrConflict
	}
	for index := range input.Mapping.Channels {
		input.Mapping.Channels[index] = strings.ToUpper(strings.TrimSpace(input.Mapping.Channels[index]))
		if !validPaymentChannel(input.Mapping.Channels[index]) {
			return RechargeProductCatalogItem{}, ErrConflict
		}
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RechargeProductCatalogItem{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "catalog.product", input.ProductID); err != nil {
		return RechargeProductCatalogItem{}, err
	}
	if err := lockTransactionKey(ctx, tx, "catalog.sku", input.Mapping.ProviderID+"|"+input.Mapping.ProviderVersion+"|"+input.Mapping.ExternalSKUBOM); err != nil {
		return RechargeProductCatalogItem{}, err
	}
	now := r.now()
	itemID := uuid.Must(uuid.NewV7())
	displayPoints := input.BasePoints + input.BonusPoints
	_, err = tx.Exec(ctx, `INSERT INTO recharge_products(product_id,amount_fen,credit_microcredits,display_points,currency,channels,catalog_version,enabled,created_at,plan_name,sku_code,base_points,bonus_points,validity_days,price_status,invoice_product_name)
VALUES($1,$2,$3,$4,$5,$6,1,FALSE,$7,$8,'',$9,$10,$11,$12,$13)`, input.ProductID, input.Mapping.AmountFen, input.CreditMicrocredits, displayPoints, input.Mapping.Currency, input.Mapping.Channels, now, input.DisplayNameZhCN, input.BasePoints, input.BonusPoints, input.ValidityDays, map[bool]string{true: "FROZEN", false: "UAT_TENTATIVE"}[input.Mapping.AdmissionClass == "FORMAL"], input.InvoiceProductName)
	if err != nil {
		return RechargeProductCatalogItem{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO recharge_product_catalog_items(catalog_item_id,product_id,display_name_zh_cn,display_name_en,display_name_ja,display_order,replacement_product_id,lifecycle_status,version,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,'DRAFT',1,$8,$8)`, itemID, input.ProductID, input.DisplayNameZhCN, input.DisplayNameEN, input.DisplayNameJA, input.DisplayOrder, input.ReplacementProduct, now)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO recharge_product_mapping_drafts(catalog_item_id,draft_version,provider_id,provider_version,external_sku_bom,amount_fen,currency,channels,admission_class,created_at,updated_at)
VALUES($1,1,$2,$3,$4,$5,$6,$7,$8,$9,$9)`, itemID, input.Mapping.ProviderID, input.Mapping.ProviderVersion, input.Mapping.ExternalSKUBOM, input.Mapping.AmountFen, input.Mapping.Currency, input.Mapping.Channels, input.Mapping.AdmissionClass, now)
	}
	if err != nil {
		return RechargeProductCatalogItem{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return RechargeProductCatalogItem{}, err
	}
	return r.RechargeProductCatalogItem(ctx, itemID)
}

func (r *PostgresRepository) RechargeProductCatalogItem(ctx context.Context, catalogItemID uuid.UUID) (RechargeProductCatalogItem, error) {
	var item RechargeProductCatalogItem
	err := r.pool.QueryRow(ctx, `SELECT item.catalog_item_id,item.product_id,item.display_name_zh_cn,item.display_name_en,item.display_name_ja,item.display_order,item.replacement_product_id,item.lifecycle_status,item.version,item.admission_version,item.ever_published,item.deleted_at,item.updated_at,
draft.draft_version,draft.provider_id,draft.provider_version,draft.external_sku_bom,draft.amount_fen,draft.currency,draft.channels,draft.admission_class
FROM recharge_product_catalog_items item JOIN recharge_product_mapping_drafts draft USING(catalog_item_id)
WHERE item.catalog_item_id=$1`, catalogItemID).Scan(&item.CatalogItemID, &item.ProductID, &item.DisplayNameZhCN, &item.DisplayNameEN, &item.DisplayNameJA, &item.DisplayOrder, &item.ReplacementProduct, &item.LifecycleStatus, &item.Version, &item.AdmissionVersion, &item.EverPublished, &item.DeletedAt, &item.UpdatedAt, &item.Mapping.DraftVersion, &item.Mapping.ProviderID, &item.Mapping.ProviderVersion, &item.Mapping.ExternalSKUBOM, &item.Mapping.AmountFen, &item.Mapping.Currency, &item.Mapping.Channels, &item.Mapping.AdmissionClass)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, ErrNotFound
	}
	return item, err
}

func catalogFingerprint(item RechargeProductCatalogItem, product Product) string {
	payload, _ := json.Marshal(struct {
		CatalogID uuid.UUID                   `json:"catalog_item_id"`
		ProductID string                      `json:"product_id"`
		Zh        string                      `json:"zh"`
		En        string                      `json:"en"`
		Ja        string                      `json:"ja"`
		Base      int64                       `json:"base"`
		Bonus     int64                       `json:"bonus"`
		Validity  int                         `json:"validity"`
		Invoice   string                      `json:"invoice"`
		Mapping   RechargeProductMappingDraft `json:"mapping"`
	}{item.CatalogItemID, item.ProductID, item.DisplayNameZhCN, item.DisplayNameEN, item.DisplayNameJA, product.BasePoints, product.BonusPoints, product.ValidityDays, product.InvoiceProductName, item.Mapping})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func (r *PostgresRepository) StartProductAdmissionVerification(ctx context.Context, catalogItemID, requestedBy uuid.UUID, retryOf *uuid.UUID) (ProductAdmissionVerification, bool, error) {
	item, err := r.RechargeProductCatalogItem(ctx, catalogItemID)
	if err != nil {
		return ProductAdmissionVerification{}, false, err
	}
	var product Product
	err = r.pool.QueryRow(ctx, `SELECT product_id,base_points,bonus_points,validity_days,invoice_product_name FROM recharge_products WHERE product_id=$1`, item.ProductID).Scan(&product.ProductID, &product.BasePoints, &product.BonusPoints, &product.ValidityDays, &product.InvoiceProductName)
	if err != nil {
		return ProductAdmissionVerification{}, false, err
	}
	fingerprint := catalogFingerprint(item, product)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ProductAdmissionVerification{}, false, err
	}
	defer tx.Rollback(ctx)
	if err = lockTransactionKey(ctx, tx, "catalog.admission", catalogItemID.String()+"|"+fingerprint); err != nil {
		return ProductAdmissionVerification{}, false, err
	}
	var existing ProductAdmissionVerification
	err = tx.QueryRow(ctx, `SELECT verification_id,catalog_item_id,retry_of,publish_fingerprint,status,failure_code,requested_at,completed_at FROM product_admission_verifications WHERE catalog_item_id=$1 AND publish_fingerprint=$2 AND status='PENDING'`, catalogItemID, fingerprint).Scan(&existing.VerificationID, &existing.CatalogItemID, &existing.RetryOf, &existing.PublishFingerprint, &existing.Status, &existing.FailureCode, &existing.RequestedAt, &existing.CompletedAt)
	if err == nil {
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return existing, false, err
	}
	verification := ProductAdmissionVerification{VerificationID: uuid.Must(uuid.NewV7()), CatalogItemID: catalogItemID, RetryOf: retryOf, PublishFingerprint: fingerprint, Status: "PENDING", RequestedAt: r.now()}
	var targetMappingID *uuid.UUID
	var targetDraftVersion *int64
	if item.LifecycleStatus == "DRAFT" {
		value := item.Mapping.DraftVersion
		targetDraftVersion = &value
	} else {
		var value uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM payment_product_mappings WHERE product_id=$1 ORDER BY mapping_version DESC LIMIT 1`, item.ProductID).Scan(&value)
		if err != nil {
			return ProductAdmissionVerification{}, false, err
		}
		targetMappingID = &value
	}
	_, err = tx.Exec(ctx, `INSERT INTO product_admission_verifications(verification_id,catalog_item_id,retry_of,target_mapping_id,target_draft_version,publish_fingerprint,status,requested_by,requested_at) VALUES($1,$2,$3,$4,$5,$6,'PENDING',$7,$8)`, verification.VerificationID, catalogItemID, retryOf, targetMappingID, targetDraftVersion, fingerprint, requestedBy, verification.RequestedAt)
	if err != nil {
		return ProductAdmissionVerification{}, false, err
	}
	return verification, true, tx.Commit(ctx)
}

func (r *PostgresRepository) CompleteProductAdmissionVerification(ctx context.Context, verificationID uuid.UUID, issuer, method, source string, expiresAt time.Time) (uuid.UUID, error) {
	issuer, method, source = strings.TrimSpace(issuer), strings.TrimSpace(method), strings.TrimSpace(source)
	if issuer == "" || method == "" || source == "" || !expiresAt.After(r.now()) {
		return uuid.Nil, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	var catalogID uuid.UUID
	var targetMappingID *uuid.UUID
	var fingerprint, status string
	err = tx.QueryRow(ctx, `SELECT catalog_item_id,target_mapping_id,publish_fingerprint,status FROM product_admission_verifications WHERE verification_id=$1 FOR UPDATE`, verificationID).Scan(&catalogID, &targetMappingID, &fingerprint, &status)
	if err != nil {
		return uuid.Nil, err
	}
	if status == "VERIFIED" {
		var id uuid.UUID
		err = tx.QueryRow(ctx, `SELECT evidence_id FROM product_admission_evidence WHERE verification_id=$1`, verificationID).Scan(&id)
		return id, err
	}
	if status != "PENDING" {
		return uuid.Nil, ErrConflict
	}
	evidenceID := uuid.Must(uuid.NewV7())
	now := r.now()
	_, err = tx.Exec(ctx, `INSERT INTO product_admission_evidence(evidence_id,verification_id,catalog_item_id,publish_fingerprint,issuer_workload,verification_method,source_reference,issued_at,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$8)`, evidenceID, verificationID, catalogID, fingerprint, issuer, method, source, now, expiresAt)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE product_admission_verifications SET status='VERIFIED',completed_at=$2 WHERE verification_id=$1`, verificationID, now)
	}
	if err == nil && targetMappingID != nil {
		if _, err = tx.Exec(ctx, `SELECT catalog_item_id FROM recharge_product_catalog_items WHERE catalog_item_id=$1 FOR UPDATE`, catalogID); err == nil {
			_, err = tx.Exec(ctx, `UPDATE product_admission_bindings SET effective_to=$2 WHERE catalog_item_id=$1 AND effective_to IS NULL`, catalogID, now)
		}
		var admissionVersion int64
		if err == nil {
			err = tx.QueryRow(ctx, `UPDATE recharge_product_catalog_items SET admission_version=admission_version+1,updated_at=$2 WHERE catalog_item_id=$1 RETURNING admission_version`, catalogID, now).Scan(&admissionVersion)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO product_admission_bindings(catalog_item_id,admission_version,evidence_id,mapping_id,effective_from,created_at) VALUES($1,$2,$3,$4,$5,$5)`, catalogID, admissionVersion, evidenceID, *targetMappingID, now)
		}
	}
	if err != nil {
		return uuid.Nil, err
	}
	return evidenceID, tx.Commit(ctx)
}

func (r *PostgresRepository) TransitionRechargeProduct(ctx context.Context, catalogItemID uuid.UUID, expectedVersion int64, target string, evidenceID *uuid.UUID) (RechargeProductCatalogItem, error) {
	target = strings.ToUpper(strings.TrimSpace(target))
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RechargeProductCatalogItem{}, err
	}
	defer tx.Rollback(ctx)
	var productID, current string
	var version int64
	var everPublished bool
	err = tx.QueryRow(ctx, `SELECT product_id,lifecycle_status,version,ever_published FROM recharge_product_catalog_items WHERE catalog_item_id=$1 FOR UPDATE`, catalogItemID).Scan(&productID, &current, &version, &everPublished)
	if err != nil {
		return RechargeProductCatalogItem{}, err
	}
	if version != expectedVersion {
		return RechargeProductCatalogItem{}, ErrConflict
	}
	now := r.now()
	if target == "ON_SALE" {
		if evidenceID == nil {
			return RechargeProductCatalogItem{}, ErrConflict
		}
		var fingerprint string
		var expires time.Time
		err = tx.QueryRow(ctx, `SELECT publish_fingerprint,expires_at FROM product_admission_evidence evidence WHERE evidence_id=$1 AND catalog_item_id=$2 AND NOT EXISTS(SELECT 1 FROM product_admission_revocations revocation WHERE revocation.evidence_id=evidence.evidence_id)`, *evidenceID, catalogItemID).Scan(&fingerprint, &expires)
		if err != nil || !expires.After(now) {
			return RechargeProductCatalogItem{}, ErrConflict
		}
		item, loadErr := r.RechargeProductCatalogItem(ctx, catalogItemID)
		if loadErr != nil {
			return RechargeProductCatalogItem{}, loadErr
		}
		var product Product
		loadErr = r.pool.QueryRow(ctx, `SELECT product_id,base_points,bonus_points,validity_days,invoice_product_name FROM recharge_products WHERE product_id=$1`, productID).Scan(&product.ProductID, &product.BasePoints, &product.BonusPoints, &product.ValidityDays, &product.InvoiceProductName)
		if loadErr != nil || catalogFingerprint(item, product) != fingerprint {
			return RechargeProductCatalogItem{}, ErrConflict
		}
		var mappingVersion int64
		err = tx.QueryRow(ctx, `SELECT COALESCE(max(mapping_version),0)+1 FROM payment_product_mappings WHERE provider_id=$1 AND provider_version=$2 AND product_id=$3`, item.Mapping.ProviderID, item.Mapping.ProviderVersion, productID).Scan(&mappingVersion)
		if err != nil {
			return RechargeProductCatalogItem{}, err
		}
		mappingID := uuid.Must(uuid.NewV7())
		status := map[bool]string{true: "FROZEN", false: "TEST_ONLY"}[item.Mapping.AdmissionClass == "FORMAL"]
		_, err = tx.Exec(ctx, `INSERT INTO payment_product_mappings(id,provider_id,provider_version,product_id,mapping_version,external_sku,amount_fen,currency,channels,status,effective_from,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)`, mappingID, item.Mapping.ProviderID, item.Mapping.ProviderVersion, productID, mappingVersion, item.Mapping.ExternalSKUBOM, item.Mapping.AmountFen, item.Mapping.Currency, item.Mapping.Channels, status, now)
		if err != nil {
			return RechargeProductCatalogItem{}, err
		}
		_, _ = tx.Exec(ctx, `UPDATE product_admission_bindings SET effective_to=$2 WHERE catalog_item_id=$1 AND effective_to IS NULL`, catalogItemID, now)
		var admissionVersion int64
		err = tx.QueryRow(ctx, `UPDATE recharge_product_catalog_items SET admission_version=admission_version+1 WHERE catalog_item_id=$1 RETURNING admission_version`, catalogItemID).Scan(&admissionVersion)
		if err != nil {
			return RechargeProductCatalogItem{}, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO product_admission_bindings(catalog_item_id,admission_version,evidence_id,mapping_id,effective_from,created_at) VALUES($1,$2,$3,$4,$5,$5)`, catalogItemID, admissionVersion, *evidenceID, mappingID, now)
		if err != nil {
			return RechargeProductCatalogItem{}, err
		}
	} else if target == "OFF_SALE" {
		_, err = tx.Exec(ctx, `UPDATE payment_product_mappings SET status='RETIRED',effective_to=$2 WHERE product_id=$1 AND status IN ('FROZEN','TEST_ONLY') AND effective_to IS NULL`, productID, now)
		if err != nil {
			return RechargeProductCatalogItem{}, err
		}
		_, err = tx.Exec(ctx, `UPDATE product_admission_bindings SET effective_to=$2 WHERE catalog_item_id=$1 AND effective_to IS NULL`, catalogItemID, now)
		if err != nil {
			return RechargeProductCatalogItem{}, err
		}
	} else if target != "DELETED" {
		return RechargeProductCatalogItem{}, ErrConflict
	}
	if target == "DELETED" && current == "DRAFT" && !everPublished {
		if _, err = tx.Exec(ctx, `UPDATE recharge_product_mapping_drafts SET released_at=$2,updated_at=$2 WHERE catalog_item_id=$1 AND released_at IS NULL`, catalogItemID, now); err != nil {
			return RechargeProductCatalogItem{}, err
		}
	}
	var ignoredID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT catalog_item_id FROM set_recharge_product_lifecycle($1,$2,$3,$4)`, catalogItemID, expectedVersion, target, now).Scan(&ignoredID)
	if err != nil {
		return RechargeProductCatalogItem{}, fmt.Errorf("transition recharge product: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return RechargeProductCatalogItem{}, err
	}
	return r.RechargeProductCatalogItem(ctx, catalogItemID)
}

func (r *PostgresRepository) RevokeProductAdmission(ctx context.Context, evidenceID uuid.UUID, sourceType, actor, reason string) error {
	sourceType = strings.ToUpper(strings.TrimSpace(sourceType))
	actor = strings.TrimSpace(actor)
	reason = strings.TrimSpace(reason)
	if (sourceType != "HUMAN" && sourceType != "PROVIDER") || actor == "" || reason == "" {
		return ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var catalogID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT catalog_item_id FROM product_admission_evidence WHERE evidence_id=$1`, evidenceID).Scan(&catalogID)
	if err != nil {
		return err
	}
	if err = lockTransactionKey(ctx, tx, "catalog.admission", catalogID.String()); err != nil {
		return err
	}
	var sequence int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(max(revocation_sequence),0)+1 FROM product_admission_revocations WHERE evidence_id=$1`, evidenceID).Scan(&sequence)
	if err != nil {
		return err
	}
	now := r.now()
	_, err = tx.Exec(ctx, `INSERT INTO product_admission_revocations(revocation_id,evidence_id,revocation_sequence,source_type,actor_subject,reason,observed_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$7)`, uuid.Must(uuid.NewV7()), evidenceID, sequence, sourceType, actor, reason, now)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE product_admission_bindings SET effective_to=$2 WHERE catalog_item_id=$1 AND evidence_id=$3 AND effective_to IS NULL`, catalogID, now, evidenceID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_product_controls(provider_id,provider_version,product_id,new_orders_enabled,reason_code,reason,version,updated_at)
SELECT draft.provider_id,draft.provider_version,item.product_id,FALSE,'MANUAL_SAFETY_PAUSE',$2,1,$3
FROM recharge_product_catalog_items item JOIN recharge_product_mapping_drafts draft USING(catalog_item_id)
WHERE item.catalog_item_id=$1
ON CONFLICT(provider_id,provider_version,product_id) DO UPDATE SET new_orders_enabled=FALSE,reason_code='MANUAL_SAFETY_PAUSE',reason=EXCLUDED.reason,version=payment_product_controls.version+1,updated_at=EXCLUDED.updated_at`, catalogID, reason, now)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE recharge_product_catalog_items SET admission_version=admission_version+1,updated_at=$2 WHERE catalog_item_id=$1`, catalogID, now)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) ExpireProductAdmissions(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT binding.catalog_item_id,evidence.evidence_id
FROM product_admission_bindings binding
JOIN product_admission_evidence evidence USING(evidence_id)
WHERE binding.effective_to IS NULL AND evidence.expires_at<=now()
ORDER BY evidence.expires_at,binding.catalog_item_id FOR UPDATE OF binding SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type expired struct{ catalogID, evidenceID uuid.UUID }
	items := make([]expired, 0)
	for rows.Next() {
		var item expired
		if err := rows.Scan(&item.catalogID, &item.evidenceID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	rows.Close()
	now := r.now()
	for _, item := range items {
		_, err = tx.Exec(ctx, `UPDATE product_admission_bindings SET effective_to=$2 WHERE catalog_item_id=$1 AND evidence_id=$3 AND effective_to IS NULL`, item.catalogID, now, item.evidenceID)
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO payment_product_controls(provider_id,provider_version,product_id,new_orders_enabled,reason_code,reason,version,updated_at)
SELECT draft.provider_id,draft.provider_version,catalog.product_id,FALSE,'AUTOMATIC_STOP_LOSS','PRODUCT_ADMISSION_EXPIRED',1,$2
FROM recharge_product_catalog_items catalog JOIN recharge_product_mapping_drafts draft USING(catalog_item_id)
WHERE catalog.catalog_item_id=$1
ON CONFLICT(provider_id,provider_version,product_id) DO UPDATE SET new_orders_enabled=FALSE,reason_code='AUTOMATIC_STOP_LOSS',reason='PRODUCT_ADMISSION_EXPIRED',version=payment_product_controls.version+1,updated_at=EXCLUDED.updated_at`, item.catalogID, now)
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(ctx, `UPDATE recharge_product_catalog_items SET admission_version=admission_version+1,updated_at=$2 WHERE catalog_item_id=$1`, item.catalogID, now)
		if err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(items), nil
}

type ProductAdmissionExpiryWorker struct {
	repository *PostgresRepository
	interval   time.Duration
}

func NewProductAdmissionExpiryWorker(repository *PostgresRepository, interval time.Duration) *ProductAdmissionExpiryWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	return &ProductAdmissionExpiryWorker{repository: repository, interval: interval}
}

func (w *ProductAdmissionExpiryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		_, _ = w.repository.ExpireProductAdmissions(ctx, 100)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
