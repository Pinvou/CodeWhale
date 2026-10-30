package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ProductManualAdmissionDecision struct {
	ReviewerID uuid.UUID `json:"reviewer_id"`
	Decision   string    `json:"decision"`
	Reason     string    `json:"reason"`
	DecidedAt  time.Time `json:"decided_at"`
}
type ProductManualAdmission struct {
	VerificationID uuid.UUID                        `json:"verification_id"`
	CatalogItemID  uuid.UUID                        `json:"catalog_item_id"`
	MaterialID     uuid.UUID                        `json:"material_id"`
	SubmissionSHA  string                           `json:"submission_sha"`
	Snapshot       json.RawMessage                  `json:"snapshot"`
	Status         string                           `json:"status"`
	RequestedBy    uuid.UUID                        `json:"requested_by"`
	RequestedAt    time.Time                        `json:"requested_at"`
	ExpiresAt      time.Time                        `json:"expires_at"`
	Decisions      []ProductManualAdmissionDecision `json:"decisions"`
	EvidenceID     *uuid.UUID                       `json:"evidence_id,omitempty"`
}
type ProductManualAdmissionCreate struct {
	CatalogItemID, MaterialID, Actor    uuid.UUID
	ExpectedVersion                     int64
	IdempotencyKey, Reason, Environment string
}
type ProductManualAdmissionDecide struct {
	CatalogItemID, VerificationID, Actor            uuid.UUID
	ExpectedVersion                                 int64
	IdempotencyKey, Reason, SubmissionSHA, Decision string
}
type productManualAdmissionRepository interface {
	CreateManualProductAdmission(context.Context, ProductManualAdmissionCreate, func(context.Context) error) (ProductManualAdmission, error)
	DecideManualProductAdmission(context.Context, ProductManualAdmissionDecide, func(context.Context) error) (ProductManualAdmission, error)
	GetManualProductAdmission(context.Context, uuid.UUID, uuid.UUID) (ProductManualAdmission, error)
}

// ExpirePendingManualProductAdmissions closes an unsubmitted or partially
// reviewed FORMAL request at its operator-supplied expiry. It preserves the
// review record and original terminal anchor, while releasing the unique
// pending slot for a new material/request.
func (r *PostgresRepository) ExpirePendingManualProductAdmissions(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT s.verification_id,s.catalog_item_id FROM product_manual_admission_submissions s JOIN product_admission_verifications v USING(verification_id) WHERE v.status='PENDING' AND s.expires_at<=clock_timestamp() ORDER BY s.expires_at,s.verification_id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type expiredManual struct{ verificationID, catalogID uuid.UUID }
	items := make([]expiredManual, 0, limit)
	for rows.Next() {
		var item expiredManual
		if err = rows.Scan(&item.verificationID, &item.catalogID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, item := range items {
		if err = lockTransactionKey(ctx, tx, "catalog.admission", item.catalogID.String()); err != nil {
			return 0, err
		}
		var locked uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT s.verification_id FROM product_manual_admission_submissions s JOIN product_admission_verifications v USING(verification_id) WHERE s.verification_id=$1 AND v.status='PENDING' AND s.expires_at<=clock_timestamp() FOR UPDATE OF s,v SKIP LOCKED`, item.verificationID).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			return 0, err
		}
		var now time.Time
		if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `UPDATE product_admission_verifications SET status='FAILED',failure_code='MANUAL_ADMISSION_EXPIRED',completed_at=$2 WHERE verification_id=$1 AND status='PENDING'`, item.verificationID, now); err != nil {
			return 0, err
		}
		if err = terminalProductAdmissionMaterial(ctx, tx, item.verificationID, now); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(items), nil
}

func (r *PostgresRepository) GetManualProductAdmission(ctx context.Context, catalogID, verificationID uuid.UUID) (ProductManualAdmission, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ProductManualAdmission{}, err
	}
	defer tx.Rollback(ctx)
	value, err := manualProductAdmission(ctx, tx, catalogID, verificationID)
	if err != nil {
		return value, err
	}
	return value, tx.Commit(ctx)
}
func manualProductAdmission(ctx context.Context, tx pgx.Tx, catalogID, verificationID uuid.UUID) (ProductManualAdmission, error) {
	var value ProductManualAdmission
	err := tx.QueryRow(ctx, `SELECT s.verification_id,s.catalog_item_id,s.material_id,s.submission_sha,s.snapshot,v.status,s.requested_by,s.requested_at,s.expires_at,e.evidence_id FROM product_manual_admission_submissions s JOIN product_admission_verifications v USING(verification_id) LEFT JOIN product_admission_evidence e USING(verification_id) WHERE s.catalog_item_id=$1 AND s.verification_id=$2`, catalogID, verificationID).Scan(&value.VerificationID, &value.CatalogItemID, &value.MaterialID, &value.SubmissionSHA, &value.Snapshot, &value.Status, &value.RequestedBy, &value.RequestedAt, &value.ExpiresAt, &value.EvidenceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, err
	}
	value.Decisions = make([]ProductManualAdmissionDecision, 0)
	rows, err := tx.Query(ctx, `SELECT reviewer_id,decision,reason,decided_at FROM product_manual_admission_decisions WHERE verification_id=$1 ORDER BY decided_at,reviewer_id`, verificationID)
	if err != nil {
		return value, err
	}
	defer rows.Close()
	for rows.Next() {
		var d ProductManualAdmissionDecision
		if err = rows.Scan(&d.ReviewerID, &d.Decision, &d.Reason, &d.DecidedAt); err != nil {
			return value, err
		}
		value.Decisions = append(value.Decisions, d)
	}
	return value, rows.Err()
}
func manualCommandActorValid(actor uuid.UUID, reason string, guard func(context.Context) error) bool {
	return actor != uuid.Nil && len(strings.TrimSpace(reason)) > 0 && len(reason) <= 2000 && guard != nil
}
func (r *PostgresRepository) CreateManualProductAdmission(ctx context.Context, in ProductManualAdmissionCreate, guard func(context.Context) error) (ProductManualAdmission, error) {
	if !manualCommandActorValid(in.Actor, in.Reason, guard) || in.MaterialID == uuid.Nil {
		return ProductManualAdmission{}, ErrConflict
	}
	if err := guard(ctx); err != nil {
		return ProductManualAdmission{}, err
	}
	hash := catalogResourceRequestHash("BILLING_PRODUCT_MANUAL_SUBMIT", in.CatalogItemID, in)
	meta := rechargeProductAtomicCommand{CatalogItemID: in.CatalogItemID, ExpectedVersion: in.ExpectedVersion, Kind: "BILLING_PRODUCT_MANUAL_SUBMIT", IdempotencyKey: in.IdempotencyKey, RequestHash: hash, Actor: in.Actor.String(), Reason: in.Reason, Status: 201}
	result, commandErr := executeRechargeProductAtomicCommand(ctx, r, meta, func(tx pgx.Tx) (ProductManualAdmission, int64, error) {
		if err := lockProductAdmissionCommand(ctx, tx, in.CatalogItemID, in.ExpectedVersion); err != nil {
			return ProductManualAdmission{}, 0, err
		}
		item, err := rechargeProductCatalogItem(ctx, tx, in.CatalogItemID)
		if err != nil {
			return ProductManualAdmission{}, 0, err
		}
		if item.Mapping.AdmissionClass != "FORMAL" || item.LifecycleStatus == "DELETED" {
			return ProductManualAdmission{}, 0, ErrConflict
		}
		// A pre-manual FORMAL request never had original evidence. Retain its fact,
		// close it explicitly, and avoid reusing its unique pending slot.
		_, err = tx.Exec(ctx, `UPDATE product_admission_verifications v SET status='FAILED',failure_code='MANUAL_EVIDENCE_REQUIRED',completed_at=clock_timestamp() WHERE catalog_item_id=$1 AND status='PENDING' AND NOT EXISTS(SELECT 1 FROM product_manual_admission_submissions s WHERE s.verification_id=v.verification_id)`, in.CatalogItemID)
		if err != nil {
			return ProductManualAdmission{}, 0, err
		}
		verification, created, err := r.startProductAdmissionVerification(ctx, tx, in.CatalogItemID, in.Actor, nil)
		if err != nil {
			return ProductManualAdmission{}, 0, err
		}
		if !created {
			return ProductManualAdmission{}, 0, ErrConflict
		}
		material, err := productAdmissionMaterial(ctx, tx, in.CatalogItemID, in.MaterialID, true)
		if err != nil {
			return ProductManualAdmission{}, 0, err
		}
		if material.CatalogVersion != in.ExpectedVersion {
			return ProductManualAdmission{}, 0, ErrConflict
		}
		var now time.Time
		if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return ProductManualAdmission{}, 0, err
		}
		product := Product{ProductID: item.ProductID, BasePoints: item.BasePoints, BonusPoints: item.BonusPoints, ValidityDays: item.ValidityDays, InvoiceProductName: item.InvoiceProductName}
		original := ProductAdmissionOriginal{DocumentID: material.MaterialID, Version: 1, MediaType: material.MediaType, Size: material.Size, SHA256: material.SHA256}
		snapshot, sha, err := freezeProductAdmissionSubmission(in.Environment, item, product, original, material.IntendedExpiresAt, now)
		if err != nil {
			return ProductManualAdmission{}, 0, err
		}
		if err = bindProductAdmissionMaterial(ctx, tx, in.CatalogItemID, in.MaterialID, verification.VerificationID, material.IntendedExpiresAt); err != nil {
			return ProductManualAdmission{}, 0, err
		}
		if err = guard(ctx); err != nil {
			return ProductManualAdmission{}, 0, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO product_manual_admission_submissions(verification_id,catalog_item_id,material_id,submission_sha,snapshot,requested_by,requested_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, verification.VerificationID, in.CatalogItemID, in.MaterialID, sha, snapshot, in.Actor, now, material.IntendedExpiresAt)
		if err != nil {
			return ProductManualAdmission{}, 0, err
		}
		result, err := manualProductAdmission(ctx, tx, in.CatalogItemID, verification.VerificationID)
		return result, in.ExpectedVersion, err
	})
	if commandErr == nil {
		commandErr = guard(ctx)
	}
	return result, commandErr
}
func (r *PostgresRepository) DecideManualProductAdmission(ctx context.Context, in ProductManualAdmissionDecide, guard func(context.Context) error) (ProductManualAdmission, error) {
	if !manualCommandActorValid(in.Actor, in.Reason, guard) || (in.Decision != "APPROVE" && in.Decision != "REJECT") {
		return ProductManualAdmission{}, ErrConflict
	}
	if err := guard(ctx); err != nil {
		return ProductManualAdmission{}, err
	}
	hash := catalogResourceRequestHash("BILLING_PRODUCT_MANUAL_DECIDE", in.CatalogItemID, in)
	meta := rechargeProductAtomicCommand{CatalogItemID: in.CatalogItemID, ExpectedVersion: in.ExpectedVersion, Kind: "BILLING_PRODUCT_MANUAL_DECIDE", IdempotencyKey: in.IdempotencyKey, RequestHash: hash, Actor: in.Actor.String(), Reason: in.Reason, Status: 200}
	result, commandErr := executeRechargeProductAtomicCommand(ctx, r, meta, func(tx pgx.Tx) (ProductManualAdmission, int64, error) {
		if err := lockProductAdmissionCommand(ctx, tx, in.CatalogItemID, in.ExpectedVersion); err != nil {
			return ProductManualAdmission{}, 0, err
		}
		var ignored uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT verification_id FROM product_admission_verifications WHERE verification_id=$1 AND catalog_item_id=$2 FOR UPDATE`, in.VerificationID, in.CatalogItemID).Scan(&ignored); err != nil {
			return ProductManualAdmission{}, 0, err
		}
		record, err := manualProductAdmission(ctx, tx, in.CatalogItemID, in.VerificationID)
		if err != nil {
			return record, 0, err
		}
		if record.Status != "PENDING" || record.SubmissionSHA != in.SubmissionSHA {
			return record, 0, ErrConflict
		}
		for _, d := range record.Decisions {
			if d.ReviewerID == in.Actor {
				return record, 0, ErrConflict
			}
		}
		material, err := productAdmissionMaterial(ctx, tx, in.CatalogItemID, record.MaterialID, true)
		if err != nil {
			return record, 0, err
		}
		var now time.Time
		if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return record, 0, err
		}
		if material.PurgedAt != nil || !record.ExpiresAt.After(now) {
			return record, 0, ErrConflict
		}
		item, err := rechargeProductCatalogItem(ctx, tx, in.CatalogItemID)
		if err != nil {
			return record, 0, err
		}
		var frozen productAdmissionSubmission
		if json.Unmarshal(record.Snapshot, &frozen) != nil {
			return record, 0, ErrConflict
		}
		product := Product{ProductID: item.ProductID, BasePoints: item.BasePoints, BonusPoints: item.BonusPoints, ValidityDays: item.ValidityDays, InvoiceProductName: item.InvoiceProductName}
		if item.LifecycleStatus == "DELETED" || frozen.Publication.CatalogVersion != item.Version || frozen.PublishFingerprint != catalogFingerprint(item, product) {
			return record, 0, ErrConflict
		}
		if err = guard(ctx); err != nil {
			return record, 0, err
		}
		var stillUnexpired bool
		if err = tx.QueryRow(ctx, `SELECT $1::timestamptz>clock_timestamp()`, record.ExpiresAt).Scan(&stillUnexpired); err != nil {
			return record, 0, err
		}
		if !stillUnexpired {
			return record, 0, ErrConflict
		}
		_, err = tx.Exec(ctx, `INSERT INTO product_manual_admission_decisions(verification_id,reviewer_id,decision,reason,decided_at) VALUES($1,$2,$3,$4,$5)`, in.VerificationID, in.Actor, in.Decision, in.Reason, now)
		if err != nil {
			return record, 0, err
		}
		if in.Decision == "REJECT" {
			_, err = tx.Exec(ctx, `UPDATE product_admission_verifications SET status='FAILED',failure_code='MANUAL_REJECTED',completed_at=$2 WHERE verification_id=$1`, in.VerificationID, now)
			if err == nil {
				err = terminalProductAdmissionMaterial(ctx, tx, in.VerificationID, now)
			}
		} else if len(record.Decisions) == 1 {
			_, err = r.completeProductAdmissionVerificationTx(ctx, tx, in.VerificationID, "manual:two-human", "MANUAL_TWO_PERSON", record.SubmissionSHA, record.ExpiresAt, true)
		}
		if err != nil {
			return record, 0, err
		}
		result, err := manualProductAdmission(ctx, tx, in.CatalogItemID, in.VerificationID)
		return result, in.ExpectedVersion, err
	})
	if commandErr == nil {
		commandErr = guard(ctx)
	}
	return result, commandErr
}
