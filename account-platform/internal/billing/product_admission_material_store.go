package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/evidenceenvelope"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"
)

type ProductAdmissionMaterialGuard func(context.Context) error

type ProductAdmissionMaterial struct {
	MaterialID        uuid.UUID  `json:"material_id"`
	CatalogItemID     uuid.UUID  `json:"catalog_item_id"`
	CatalogVersion    int64      `json:"catalog_version"`
	UploadedBy        uuid.UUID  `json:"uploaded_by"`
	MediaType         string     `json:"media_type"`
	Size              int64      `json:"size"`
	SHA256            string     `json:"sha256"`
	IntendedExpiresAt time.Time  `json:"intended_expires_at"`
	CreatedAt         time.Time  `json:"created_at"`
	TerminalAt        *time.Time `json:"terminal_at,omitempty"`
	PurgeAfter        time.Time  `json:"purge_after"`
	PurgedAt          *time.Time `json:"purged_at,omitempty"`
	Version           int64      `json:"version"`
	LegalHold         bool       `json:"legal_hold"`
	HoldReviewAt      *time.Time `json:"hold_review_at,omitempty"`
	VerificationID    *uuid.UUID `json:"verification_id,omitempty"`
}
type ProductAdmissionMaterialSave struct {
	MaterialID, CatalogItemID, Actor uuid.UUID
	CatalogVersion                   int64
	IdempotencyKey                   string
	IntendedExpiresAt                time.Time
	Envelope                         evidenceenvelope.Envelope
}
type ProductAdmissionMaterialHold struct {
	CatalogItemID, MaterialID, Actor uuid.UUID
	ExpectedVersion                  int64
	Hold                             bool
	Reason                           string
	ReviewAt                         *time.Time
	IdempotencyKey                   string
}

const materialColumns = `material_id,catalog_item_id,catalog_version,uploaded_by,media_type,size,sha256,intended_expires_at,created_at,terminal_at,purge_after,purged_at,version,legal_hold,hold_review_at,verification_id`

func scanProductAdmissionMaterial(row pgx.Row) (m ProductAdmissionMaterial, err error) {
	err = row.Scan(&m.MaterialID, &m.CatalogItemID, &m.CatalogVersion, &m.UploadedBy, &m.MediaType, &m.Size, &m.SHA256, &m.IntendedExpiresAt, &m.CreatedAt, &m.TerminalAt, &m.PurgeAfter, &m.PurgedAt, &m.Version, &m.LegalHold, &m.HoldReviewAt, &m.VerificationID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}
func productAdmissionMaterial(ctx context.Context, tx pgx.Tx, catalogID, materialID uuid.UUID, lock bool) (ProductAdmissionMaterial, error) {
	q := `SELECT ` + materialColumns + ` FROM product_admission_materials WHERE catalog_item_id=$1 AND material_id=$2`
	if lock {
		q += ` FOR UPDATE`
	}
	return scanProductAdmissionMaterial(tx.QueryRow(ctx, q, catalogID, materialID))
}
func (r *PostgresRepository) GetProductAdmissionMaterial(ctx context.Context, catalogID, materialID uuid.UUID) (ProductAdmissionMaterial, error) {
	return scanProductAdmissionMaterial(r.pool.QueryRow(ctx, `SELECT `+materialColumns+` FROM product_admission_materials WHERE catalog_item_id=$1 AND material_id=$2`, catalogID, materialID))
}
func materialGuard(ctx context.Context, guard ProductAdmissionMaterialGuard) error {
	if guard == nil {
		return ErrConflict
	}
	return guard(ctx)
}
func materialHash(value any) string {
	raw, _ := json.Marshal(value)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func materialReceipt(ctx context.Context, tx pgx.Tx, catalogID, actor uuid.UUID, key, hash string) (ProductAdmissionMaterial, bool, error) {
	var body []byte
	var stored string
	var owner uuid.UUID
	err := tx.QueryRow(ctx, `SELECT request_hash,catalog_item_id,response FROM product_admission_material_commands WHERE actor=$1 AND operation_key=$2`, actor, key).Scan(&stored, &owner, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProductAdmissionMaterial{}, false, nil
	}
	if err != nil {
		return ProductAdmissionMaterial{}, false, err
	}
	if owner != catalogID || (hash != "" && hash != stored) {
		return ProductAdmissionMaterial{}, false, ErrConflict
	}
	var m ProductAdmissionMaterial
	err = json.Unmarshal(body, &m)
	return m, true, err
}
func (r *PostgresRepository) GetProductAdmissionMaterialReceipt(ctx context.Context, catalogID, actor uuid.UUID, key string) (ProductAdmissionMaterial, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ProductAdmissionMaterial{}, err
	}
	defer tx.Rollback(ctx)
	m, found, err := materialReceipt(ctx, tx, catalogID, actor, key, "")
	if err != nil {
		return m, err
	}
	if !found {
		return m, ErrNotFound
	}
	return m, nil
}
func materialAudit(ctx context.Context, tx pgx.Tx, m, actor uuid.UUID, action, reason string, review *time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO product_admission_material_audits(audit_id,material_id,actor,action,reason,review_at) VALUES($1,$2,$3,$4,$5,$6)`, uuid.Must(uuid.NewV7()), m, actor, action, reason, review)
	return err
}
func materialCommand(ctx context.Context, tx pgx.Tx, actor uuid.UUID, key, hash string, m ProductAdmissionMaterial) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO product_admission_material_commands(actor,operation_key,request_hash,catalog_item_id,response) VALUES($1,$2,$3,$4,$5)`, actor, key, hash, m.CatalogItemID, body)
	return err
}
func validMaterialKey(key string) bool {
	if len(key) < 16 || len(key) > 128 || key != strings.TrimSpace(key) {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func (r *PostgresRepository) SaveProductAdmissionMaterial(ctx context.Context, in ProductAdmissionMaterialSave, guard ProductAdmissionMaterialGuard) (ProductAdmissionMaterial, error) {
	var empty ProductAdmissionMaterial
	in.IntendedExpiresAt = in.IntendedExpiresAt.UTC().Truncate(time.Microsecond)
	e := in.Envelope
	if in.Actor == uuid.Nil || in.MaterialID == uuid.Nil || in.CatalogItemID == uuid.Nil || in.CatalogVersion < 1 || !validMaterialKey(in.IdempotencyKey) || e.Binding != (evidenceenvelope.Binding{Purpose: "product-admission", OwnerID: in.CatalogItemID, DocumentID: in.MaterialID, Version: 1, MediaType: e.Binding.MediaType}) || e.PlaintextSize < 1 || e.PlaintextSize > evidenceenvelope.MaxPlaintextBytes || int64(len(e.Ciphertext)) != e.PlaintextSize+16 || len(e.Nonce) != 12 || len(e.WrappedDEK) < 1 || len(e.WrappedDEK) > evidenceenvelope.MaxWrappedKeyBytes || len(e.KeyID) < 1 || len(e.KeyID) > 128 {
		return empty, ErrConflict
	}
	hash := materialHash(struct {
		Kind             string
		CatalogID, Actor uuid.UUID
		Version          int64
		Expiry           string
		Media            string
		Size             int64
		SHA              string
	}{"UPLOAD", in.CatalogItemID, in.Actor, in.CatalogVersion, in.IntendedExpiresAt.UTC().Format(time.RFC3339Nano), e.Binding.MediaType, e.PlaintextSize, hex.EncodeToString(e.PlaintextSHA256[:])})
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err = lockTransactionKey(ctx, tx, "product.material.command", in.Actor.String()+":"+in.IdempotencyKey); err != nil {
		return empty, err
	}
	if err = materialGuard(ctx, guard); err != nil {
		return empty, err
	}
	if m, found, err := materialReceipt(ctx, tx, in.CatalogItemID, in.Actor, in.IdempotencyKey, hash); err != nil || found {
		return m, err
	}
	if err = lockProductAdmissionCommand(ctx, tx, in.CatalogItemID, in.CatalogVersion); err != nil {
		return empty, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return empty, err
	}
	if !in.IntendedExpiresAt.After(now) {
		return empty, ErrConflict
	}
	if err = materialGuard(ctx, guard); err != nil {
		return empty, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO product_admission_materials(material_id,catalog_item_id,catalog_version,uploaded_by,media_type,size,sha256,intended_expires_at,created_at,purge_after) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,($8::timestamptz AT TIME ZONE 'UTC'+interval '3 years') AT TIME ZONE 'UTC')`, in.MaterialID, in.CatalogItemID, in.CatalogVersion, in.Actor, e.Binding.MediaType, e.PlaintextSize, hex.EncodeToString(e.PlaintextSHA256[:]), in.IntendedExpiresAt, now)
	if err != nil {
		return empty, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO product_admission_material_ciphertexts(material_id,key_id,wrapped_dek,nonce,ciphertext) VALUES($1,$2,$3,$4,$5)`, in.MaterialID, e.KeyID, e.WrappedDEK, e.Nonce, e.Ciphertext)
	if err != nil {
		return empty, err
	}
	m, err := productAdmissionMaterial(ctx, tx, in.CatalogItemID, in.MaterialID, false)
	if err != nil {
		return empty, err
	}
	if err = materialAudit(ctx, tx, in.MaterialID, in.Actor, "UPLOAD", "product admission original", nil); err != nil {
		return empty, err
	}
	if err = materialCommand(ctx, tx, in.Actor, in.IdempotencyKey, hash, m); err != nil {
		return empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	return m, nil
}
func (r *PostgresRepository) GetProductAdmissionMaterialEnvelope(ctx context.Context, catalogID, materialID uuid.UUID) (ProductAdmissionMaterial, evidenceenvelope.Envelope, error) {
	var empty evidenceenvelope.Envelope
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ProductAdmissionMaterial{}, empty, err
	}
	defer tx.Rollback(ctx)
	m, err := productAdmissionMaterial(ctx, tx, catalogID, materialID, false)
	if err != nil {
		return m, empty, err
	}
	if m.PurgedAt != nil {
		return m, empty, ErrNotFound
	}
	e := evidenceenvelope.Envelope{Binding: evidenceenvelope.Binding{Purpose: "product-admission", OwnerID: catalogID, DocumentID: materialID, Version: 1, MediaType: m.MediaType}, PlaintextSize: m.Size}
	digest, err := hex.DecodeString(m.SHA256)
	if err != nil {
		return m, empty, ErrConflict
	}
	copy(e.PlaintextSHA256[:], digest)
	err = tx.QueryRow(ctx, `SELECT key_id,wrapped_dek,nonce,ciphertext FROM product_admission_material_ciphertexts WHERE material_id=$1`, materialID).Scan(&e.KeyID, &e.WrappedDEK, &e.Nonce, &e.Ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err != nil {
		return m, empty, err
	}
	return m, e, tx.Commit(ctx)
}
func (r *PostgresRepository) ConfirmProductAdmissionMaterialRead(ctx context.Context, catalogID, materialID, actor uuid.UUID, guard ProductAdmissionMaterialGuard) error {
	if actor == uuid.Nil {
		return ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	m, err := productAdmissionMaterial(ctx, tx, catalogID, materialID, true)
	if err != nil {
		return err
	}
	if m.PurgedAt != nil {
		return ErrNotFound
	}
	if err = materialGuard(ctx, guard); err != nil {
		return err
	}
	if err = materialAudit(ctx, tx, materialID, actor, "READ", "authorized original read", nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (r *PostgresRepository) SetProductAdmissionMaterialLegalHold(ctx context.Context, in ProductAdmissionMaterialHold, guard ProductAdmissionMaterialGuard) (ProductAdmissionMaterial, error) {
	var empty ProductAdmissionMaterial
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Actor == uuid.Nil || !validMaterialKey(in.IdempotencyKey) || in.ExpectedVersion < 1 || len(in.Reason) < 1 || len(in.Reason) > 1000 || (in.Hold && (in.ReviewAt == nil)) || (!in.Hold && in.ReviewAt != nil) {
		return empty, ErrConflict
	}
	hash := materialHash(struct {
		Kind  string
		Input ProductAdmissionMaterialHold
	}{"HOLD", in})
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err = lockTransactionKey(ctx, tx, "product.material.command", in.Actor.String()+":"+in.IdempotencyKey); err != nil {
		return empty, err
	}
	if err = materialGuard(ctx, guard); err != nil {
		return empty, err
	}
	if m, found, err := materialReceipt(ctx, tx, in.CatalogItemID, in.Actor, in.IdempotencyKey, hash); err != nil || found {
		return m, err
	}
	if in.Hold && !in.ReviewAt.After(r.now()) {
		return empty, ErrConflict
	}
	m, err := productAdmissionMaterial(ctx, tx, in.CatalogItemID, in.MaterialID, true)
	if err != nil {
		return empty, err
	}
	if m.Version != in.ExpectedVersion || m.PurgedAt != nil {
		return empty, ErrConflict
	}
	if err = materialGuard(ctx, guard); err != nil {
		return empty, err
	}
	_, err = tx.Exec(ctx, `UPDATE product_admission_materials SET legal_hold=$2,hold_review_at=$3,version=version+1 WHERE material_id=$1`, in.MaterialID, in.Hold, in.ReviewAt)
	if err != nil {
		return empty, err
	}
	action := "RELEASE_HOLD"
	if in.Hold {
		action = "HOLD"
	}
	if err = materialAudit(ctx, tx, in.MaterialID, in.Actor, action, in.Reason, in.ReviewAt); err != nil {
		return empty, err
	}
	m, err = productAdmissionMaterial(ctx, tx, in.CatalogItemID, in.MaterialID, false)
	if err != nil {
		return empty, err
	}
	if err = materialCommand(ctx, tx, in.Actor, in.IdempotencyKey, hash, m); err != nil {
		return empty, err
	}
	return m, tx.Commit(ctx)
}

// Purge is finite and serializes against reads, legal holds and lifecycle hooks.
// The database trigger independently rejects deletion of an ineligible original.
func (r *PostgresRepository) PurgeProductAdmissionMaterials(ctx context.Context, actor uuid.UUID, limit int, guard ProductAdmissionMaterialGuard) (int, error) {
	if actor == uuid.Nil || limit < 1 || limit > 100 {
		return 0, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT material_id FROM product_admission_materials WHERE purged_at IS NULL AND NOT legal_hold AND purge_after<=clock_timestamp() ORDER BY purge_after,material_id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err = materialGuard(ctx, guard); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM product_admission_material_ciphertexts WHERE material_id=$1`, id); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `UPDATE product_admission_materials SET purged_at=clock_timestamp(),version=version+1 WHERE material_id=$1`, id); err != nil {
			return 0, err
		}
		if err = materialAudit(ctx, tx, id, actor, "PURGE", "lifecycle retention elapsed", nil); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit(ctx)
}

// Caller owns catalog/admission and verification locks before taking material lock.
func bindProductAdmissionMaterial(ctx context.Context, tx pgx.Tx, catalogID, materialID, verificationID uuid.UUID, expiry time.Time) error {
	var verificationCatalog uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT catalog_item_id FROM product_admission_verifications WHERE verification_id=$1`, verificationID).Scan(&verificationCatalog); err != nil || verificationCatalog != catalogID {
		return ErrConflict
	}
	m, err := productAdmissionMaterial(ctx, tx, catalogID, materialID, true)
	if err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	var catalogVersion int64
	if err = tx.QueryRow(ctx, `SELECT version FROM recharge_product_catalog_items WHERE catalog_item_id=$1`, catalogID).Scan(&catalogVersion); err != nil {
		return err
	}
	if m.CatalogVersion != catalogVersion {
		return ErrConflict
	}
	if verificationID == uuid.Nil || m.VerificationID != nil || m.PurgedAt != nil || m.TerminalAt != nil || !m.IntendedExpiresAt.Equal(expiry.UTC().Truncate(time.Microsecond)) || !expiry.After(now) {
		return ErrConflict
	}
	_, err = tx.Exec(ctx, `UPDATE product_admission_materials SET verification_id=$2,version=version+1 WHERE material_id=$1`, materialID, verificationID)
	return err
}
func terminalProductAdmissionMaterial(ctx context.Context, tx pgx.Tx, verificationID uuid.UUID, terminalAt time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE product_admission_materials SET terminal_at=$2,version=version+1 WHERE verification_id=$1 AND terminal_at IS NULL`, verificationID, terminalAt)
	return err
}

// Runs under the existing Billing-owned expiry task, not a human permission.
// No bypass of retention or legal holds is granted to this workload identity.
func (r *PostgresRepository) purgeDueProductAdmissionMaterials(ctx context.Context) (int, error) {
	actor := uuid.NewSHA1(uuid.NameSpaceOID, []byte("billing-product-admission-material-retention"))
	return r.PurgeProductAdmissionMaterials(ctx, actor, 100, func(ctx context.Context) error { return ctx.Err() })
}
