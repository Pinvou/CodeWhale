package billing

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/evidenceenvelope"
	"github.com/google/uuid"
	"testing"
	"time"
)

func materialStoreInput(item RechargeProductCatalogItem, actor uuid.UUID) ProductAdmissionMaterialSave {
	id := uuid.New()
	return ProductAdmissionMaterialSave{MaterialID: id, CatalogItemID: item.CatalogItemID, Actor: actor, CatalogVersion: item.Version, IdempotencyKey: uuid.NewString(), IntendedExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond), Envelope: evidenceenvelope.Envelope{Binding: evidenceenvelope.Binding{Purpose: "product-admission", OwnerID: item.CatalogItemID, DocumentID: id, Version: 1, MediaType: "image/png"}, PlaintextSize: 1, PlaintextSHA256: sha256.Sum256([]byte("x")), KeyID: "test", WrappedDEK: []byte{1}, Nonce: make([]byte, 12), Ciphertext: make([]byte, 17)}}
}
func TestProductAdmissionMaterialStorePostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	item, _ := catalogAtomicFixture(t, r, ctx, "publish")
	actor := uuid.New()
	guard := func(context.Context) error { return nil }
	in := materialStoreInput(item, actor)
	m, err := r.SaveProductAdmissionMaterial(ctx, in, guard)
	if err != nil {
		t.Fatal(err)
	}
	if m.PurgeAfter.Year() != m.IntendedExpiresAt.Year()+3 || m.Version != 1 {
		t.Fatal("retention/version", m)
	}
	replay := in
	replay.MaterialID = uuid.New()
	replay.Envelope.Binding.DocumentID = replay.MaterialID
	replay.Envelope.Ciphertext[0] = 1
	got, err := r.SaveProductAdmissionMaterial(ctx, replay, guard)
	if err != nil || got.MaterialID != m.MaterialID {
		t.Fatal("stable replay", got, err)
	}
	replay.Envelope.PlaintextSHA256 = sha256.Sum256([]byte("y"))
	if _, err = r.SaveProductAdmissionMaterial(ctx, replay, guard); !errors.Is(err, ErrConflict) {
		t.Fatal("changed facts", err)
	}
	if _, err = r.GetProductAdmissionMaterialReceipt(ctx, item.CatalogItemID, uuid.New(), in.IdempotencyKey); !errors.Is(err, ErrNotFound) {
		t.Fatal("actor receipt", err)
	}
	if _, _, err = r.GetProductAdmissionMaterialEnvelope(ctx, uuid.New(), m.MaterialID); !errors.Is(err, ErrNotFound) {
		t.Fatal("wrong owner", err)
	}
	denied := func(context.Context) error { return ErrConflict }
	if err = r.ConfirmProductAdmissionMaterialRead(ctx, item.CatalogItemID, m.MaterialID, actor, denied); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err = r.ConfirmProductAdmissionMaterialRead(ctx, item.CatalogItemID, m.MaterialID, actor, guard); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM product_admission_material_audits WHERE action='READ' AND material_id=$1`, m.MaterialID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	review := time.Now().Add(time.Hour)
	hold := ProductAdmissionMaterialHold{CatalogItemID: item.CatalogItemID, MaterialID: m.MaterialID, Actor: actor, ExpectedVersion: 1, Hold: true, Reason: "specific legal case", ReviewAt: &review, IdempotencyKey: uuid.NewString()}
	h, err := r.SetProductAdmissionMaterialLegalHold(ctx, hold, guard)
	if err != nil || !h.LegalHold || h.Version != 2 {
		t.Fatal(h, err)
	}
	if _, err = r.SetProductAdmissionMaterialLegalHold(ctx, hold, guard); err != nil {
		t.Fatal("hold replay", err)
	}
	if n, err := r.PurgeProductAdmissionMaterials(ctx, actor, 100, guard); err != nil || n != 0 {
		t.Fatal("premature purge", n, err)
	}
	if _, err = r.pool.Exec(ctx, `DELETE FROM product_admission_material_ciphertexts WHERE material_id=$1`, m.MaterialID); err == nil {
		t.Fatal("runtime bypassed retention")
	}
	if _, err = r.pool.Exec(ctx, `UPDATE product_admission_materials SET sha256=repeat('0',64) WHERE material_id=$1`, m.MaterialID); err == nil {
		t.Fatal("mutable original facts")
	}
	if _, err = r.pool.Exec(ctx, `UPDATE product_admission_material_commands SET request_hash='changed' WHERE actor=$1`, actor); err == nil {
		t.Fatal("mutable receipt")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	verificationRecord, _, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	verification := verificationRecord.VerificationID
	if err = bindProductAdmissionMaterial(ctx, tx, item.CatalogItemID, m.MaterialID, verification, in.IntendedExpiresAt); err != nil {
		t.Fatal(err)
	}
	if err = bindProductAdmissionMaterial(ctx, tx, item.CatalogItemID, m.MaterialID, uuid.New(), in.IntendedExpiresAt); !errors.Is(err, ErrConflict) {
		t.Fatal("double bind", err)
	}
	if err = terminalProductAdmissionMaterial(ctx, tx, verification, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// A subsequent public migration run must not lend protected objects to runtime.
	var owner string
	if err = r.pool.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='product_admission_material_commands'::regclass`).Scan(&owner); err != nil || owner != "billing_schema_owner" {
		t.Fatal(owner, err)
	}
}

func TestProductAdmissionMaterialPurgePostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	item, _ := catalogAtomicFixture(t, r, ctx, "publish")
	actor := uuid.New()
	// Seed retained originals from 2019; fixture preparation does not bypass the
	// production triggers. The repository must use real database time for purge.
	seed := func() uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := r.pool.Exec(ctx, `INSERT INTO product_admission_materials(material_id,catalog_item_id,catalog_version,uploaded_by,media_type,size,sha256,intended_expires_at,created_at,purge_after) VALUES($1,$2,$3,$4,'image/png',1,repeat('a',64),'2020-01-01Z','2019-01-01Z','2099-01-01Z')`, id, item.CatalogItemID, item.Version, actor)
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.pool.Exec(ctx, `INSERT INTO product_admission_material_ciphertexts(material_id,key_id,wrapped_dek,nonce,ciphertext) VALUES($1,'old-key',$2,$3,$4)`, id, []byte{1}, make([]byte, 12), make([]byte, 17))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	purgeable, held := seed(), seed()
	// An overdue hold-review date must never release the legal hold.
	if _, err := r.pool.Exec(ctx, `UPDATE product_admission_materials SET legal_hold=true,hold_review_at='2021-01-01Z',version=version+1 WHERE material_id=$1`, held); err != nil {
		t.Fatal(err)
	}
	denied := func(context.Context) error { return ErrConflict }
	if n, err := r.PurgeProductAdmissionMaterials(ctx, actor, 100, denied); n != 0 || !errors.Is(err, ErrConflict) {
		t.Fatal(n, err)
	}
	if _, _, err := r.GetProductAdmissionMaterialEnvelope(ctx, item.CatalogItemID, purgeable); err != nil {
		t.Fatal("failed guard modified original", err)
	}
	if n, err := r.purgeDueProductAdmissionMaterials(ctx); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, _, err := r.GetProductAdmissionMaterialEnvelope(ctx, item.CatalogItemID, purgeable); !errors.Is(err, ErrNotFound) {
		t.Fatal("purged original readable", err)
	}
	if err := r.ConfirmProductAdmissionMaterialRead(ctx, item.CatalogItemID, purgeable, actor, func(context.Context) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatal("late decrypted disclosure", err)
	}
	if _, _, err := r.GetProductAdmissionMaterialEnvelope(ctx, item.CatalogItemID, held); err != nil {
		t.Fatal("expired hold-review purged original", err)
	}
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM product_admission_material_audits WHERE material_id=$1 AND action='PURGE'`, purgeable).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	m, err := r.GetProductAdmissionMaterial(ctx, item.CatalogItemID, purgeable)
	if err != nil || m.PurgedAt == nil || m.PurgeAfter.Year() != 2023 {
		t.Fatal("canonical retention and tombstone", m, err)
	}
	if n, err := r.purgeDueProductAdmissionMaterials(ctx); err != nil || n != 0 {
		t.Fatal("purge repeated", n, err)
	}
}
