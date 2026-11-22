package billing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/evidenceenvelope"
	"github.com/google/uuid"
)

func manualAdmissionFixture(t *testing.T, r *PostgresRepository, ctx context.Context) (RechargeProductCatalogItem, ProductAdmissionMaterial, uuid.UUID) {
	t.Helper()
	input := validRechargeProductForEditTests()
	input.ProductID = "MANUAL_" + uuid.NewString()
	input.Mapping.ExternalSKUBOM = input.ProductID
	input.Mapping.AdmissionClass = "FORMAL"
	item, err := r.CreateRechargeProduct(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if item.Mapping.AdmissionClass != "FORMAL" {
		t.Fatal("fixture must test FORMAL, not simulator-only admission")
	}
	actor := uuid.New()
	return item, saveManualAdmissionFixtureMaterial(t, r, ctx, item, actor, time.Now().UTC().Add(time.Hour)), actor
}
func saveManualAdmissionFixtureMaterial(t *testing.T, r *PostgresRepository, ctx context.Context, item RechargeProductCatalogItem, actor uuid.UUID, expiresAt time.Time) ProductAdmissionMaterial {
	t.Helper()

	id := uuid.New()
	plain := []byte("controlled validated document substitute")
	// Persistence/transaction tests deliberately substitute validated encryption.
	// File parser and real KMS are exercised separately; this is not a real file.
	env := evidenceenvelope.Envelope{Binding: evidenceenvelope.Binding{Purpose: "product-admission", OwnerID: item.CatalogItemID, DocumentID: id, Version: 1, MediaType: "image/png"}, PlaintextSize: int64(len(plain)), PlaintextSHA256: sha256.Sum256(plain), KeyID: "controlled", WrappedDEK: []byte("controlled-wrapped-key"), Nonce: make([]byte, 12), Ciphertext: make([]byte, len(plain)+16)}
	material, err := r.SaveProductAdmissionMaterial(ctx, ProductAdmissionMaterialSave{MaterialID: id, CatalogItemID: item.CatalogItemID, CatalogVersion: item.Version, Actor: actor, IdempotencyKey: "manual-material-" + uuid.NewString(), IntendedExpiresAt: expiresAt, Envelope: env}, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return material
}
func TestProductManualAdmissionPostgres(t *testing.T) {
	allow := func(context.Context) error { return nil }
	t.Run("two-explicit-humans-first-publish-and-immutable-replay", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, material, submitter := manualAdmissionFixture(t, r, ctx)
		create := ProductManualAdmissionCreate{CatalogItemID: item.CatalogItemID, MaterialID: material.MaterialID, Actor: submitter, ExpectedVersion: item.Version, Environment: "controlled", IdempotencyKey: "manual-create-" + uuid.NewString(), Reason: "verify controlled merchant original"}
		record, err := r.CreateManualProductAdmission(ctx, create, allow)
		if err != nil {
			t.Fatal(err)
		}
		if record.Status != "PENDING" || len(record.Decisions) != 0 || record.EvidenceID != nil {
			t.Fatal("upload or submission counted as vote", record)
		}
		if _, err = r.CompleteProductAdmissionVerification(ctx, record.VerificationID, "workload", "asserted", "untrusted", time.Now().Add(time.Hour)); !errors.Is(err, ErrConflict) {
			t.Fatal("workload minted FORMAL evidence", err)
		}
		vote := ProductManualAdmissionDecide{CatalogItemID: item.CatalogItemID, VerificationID: record.VerificationID, Actor: submitter, ExpectedVersion: item.Version, SubmissionSHA: record.SubmissionSHA, Decision: "APPROVE", Reason: "first explicit review", IdempotencyKey: "manual-vote-one-" + uuid.NewString()}
		first, err := r.DecideManualProductAdmission(ctx, vote, allow)
		if err != nil {
			t.Fatal(err)
		}
		if first.Status != "PENDING" || len(first.Decisions) != 1 || first.EvidenceID != nil {
			t.Fatal(first)
		}
		duplicate := vote
		duplicate.IdempotencyKey = "manual-double-vote-" + uuid.NewString()
		if _, err = r.DecideManualProductAdmission(ctx, duplicate, allow); !errors.Is(err, ErrConflict) {
			t.Fatal("same human counted twice", err)
		}
		second := vote
		second.Actor = uuid.New()
		second.Reason = "independent second review"
		second.IdempotencyKey = "manual-vote-two-" + uuid.NewString()
		complete, err := r.DecideManualProductAdmission(ctx, second, allow)
		if err != nil {
			t.Fatal(err)
		}
		if complete.Status != "VERIFIED" || len(complete.Decisions) != 2 || complete.EvidenceID == nil {
			t.Fatal(complete)
		}
		list, err := r.ListProductAdmissionVerifications(ctx, item.CatalogItemID)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || !list[0].UsableForPublish || list[0].Active || !list[0].ManualSubmission {
			t.Fatal("fresh draft evidence incorrectly needs prior binding", list)
		}
		published, err := r.TransitionRechargeProduct(ctx, item.CatalogItemID, item.Version, "ON_SALE", complete.EvidenceID)
		if err != nil || published.LifecycleStatus != "ON_SALE" {
			t.Fatal(published, err)
		}
		replay, err := r.DecideManualProductAdmission(ctx, second, allow)
		if err != nil || replay.EvidenceID == nil || *replay.EvidenceID != *complete.EvidenceID {
			t.Fatal("original receipt failed after catalogue advanced", replay, err)
		}
		changed := second
		changed.Decision = "REJECT"
		if _, err = r.DecideManualProductAdmission(ctx, changed, allow); !errors.Is(err, ErrConflict) {
			t.Fatal("same key changed vote", err)
		}
		denied := errors.New("current authority withdrawn")
		if _, err = r.DecideManualProductAdmission(ctx, second, func(context.Context) error { return denied }); !errors.Is(err, denied) {
			t.Fatal("replay bypassed current authority", err)
		}
	})
	t.Run("runtime-cannot-rewrite-frozen-manual-facts", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, material, actor := manualAdmissionFixture(t, r, ctx)
		record, err := r.CreateManualProductAdmission(ctx, ProductManualAdmissionCreate{CatalogItemID: item.CatalogItemID, MaterialID: material.MaterialID, Actor: actor, ExpectedVersion: item.Version, Environment: "controlled", IdempotencyKey: "manual-permission-" + uuid.NewString(), Reason: "permission guard"}, allow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `UPDATE product_manual_admission_submissions SET snapshot='{}'::jsonb WHERE verification_id=$1`, record.VerificationID); err == nil {
			t.Fatal("billing_service rewrote the frozen manual submission snapshot")
		}
		if _, err = r.pool.Exec(ctx, `UPDATE product_manual_admission_decisions SET reason='rewritten' WHERE verification_id=$1`, record.VerificationID); err == nil {
			t.Fatal("billing_service rewrote the frozen manual decision reason")
		}
	})
	t.Run("concurrent-second-votes-issue-once-and-audit-failure-rolls-back", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, material, actor := manualAdmissionFixture(t, r, ctx)
		rec, err := r.CreateManualProductAdmission(ctx, ProductManualAdmissionCreate{CatalogItemID: item.CatalogItemID, MaterialID: material.MaterialID, Actor: actor, ExpectedVersion: item.Version, Environment: "controlled", IdempotencyKey: "manual-create-" + uuid.NewString(), Reason: "submission"}, allow)
		if err != nil {
			t.Fatal(err)
		}
		vote := ProductManualAdmissionDecide{CatalogItemID: item.CatalogItemID, VerificationID: rec.VerificationID, Actor: actor, ExpectedVersion: item.Version, SubmissionSHA: rec.SubmissionSHA, Decision: "APPROVE", Reason: "controlled first vote", IdempotencyKey: "manual-first-" + uuid.NewString()}
		if _, err = r.DecideManualProductAdmission(ctx, vote, allow); err != nil {
			t.Fatal(err)
		}
		if _, err = retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION manual_reject_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled audit failure'; END $$; CREATE TRIGGER manual_reject_audit BEFORE INSERT ON recharge_product_audits FOR EACH ROW EXECUTE FUNCTION manual_reject_audit()`); err != nil {
			t.Fatal(err)
		}
		second := vote
		second.Actor = uuid.New()
		second.IdempotencyKey = "manual-second-" + uuid.NewString()
		if _, err = r.DecideManualProductAdmission(ctx, second, allow); err == nil {
			t.Fatal("audit failure accepted")
		}
		pending, err := r.GetManualProductAdmission(ctx, item.CatalogItemID, rec.VerificationID)
		if err != nil || len(pending.Decisions) != 1 || pending.EvidenceID != nil {
			t.Fatal("partial financial admission committed", pending, err)
		}
		if _, err = retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER manual_reject_audit ON recharge_product_audits; DROP FUNCTION manual_reject_audit()`); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 2)
		start := make(chan struct{})
		for range 2 {
			go func() { <-start; _, err := r.DecideManualProductAdmission(ctx, second, allow); done <- err }()
		}
		close(start)
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		var proofs, votes, receipts int
		if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM product_admission_evidence WHERE verification_id=$1),(SELECT count(*) FROM product_manual_admission_decisions WHERE verification_id=$1),(SELECT count(*) FROM recharge_product_command_records WHERE idempotency_key=$2)`, rec.VerificationID, second.IdempotencyKey).Scan(&proofs, &votes, &receipts); err != nil || proofs != 1 || votes != 2 || receipts != 1 {
			t.Fatal(proofs, votes, receipts, err)
		}
	})
	t.Run("reject-retains-original-and-no-evidence", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, material, actor := manualAdmissionFixture(t, r, ctx)
		rec, err := r.CreateManualProductAdmission(ctx, ProductManualAdmissionCreate{CatalogItemID: item.CatalogItemID, MaterialID: material.MaterialID, Actor: actor, ExpectedVersion: item.Version, Environment: "controlled", IdempotencyKey: "manual-create-" + uuid.NewString(), Reason: "submission"}, allow)
		if err != nil {
			t.Fatal(err)
		}
		result, err := r.DecideManualProductAdmission(ctx, ProductManualAdmissionDecide{CatalogItemID: item.CatalogItemID, VerificationID: rec.VerificationID, Actor: actor, ExpectedVersion: item.Version, SubmissionSHA: rec.SubmissionSHA, Decision: "REJECT", Reason: "original does not match", IdempotencyKey: "manual-reject-" + uuid.NewString()}, allow)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "FAILED" || result.EvidenceID != nil {
			t.Fatal(result)
		}
		original, err := r.GetProductAdmissionMaterial(ctx, item.CatalogItemID, material.MaterialID)
		if err != nil || original.TerminalAt == nil || original.PurgedAt != nil {
			t.Fatal("rejection did not retain original with terminal anchor", original, err)
		}
	})
	t.Run("stale-product-and-foreign-original-rejected", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, material, actor := manualAdmissionFixture(t, r, ctx)
		other, _ := catalogAtomicFixture(t, r, ctx, "delete-draft")
		in := ProductManualAdmissionCreate{CatalogItemID: other.CatalogItemID, MaterialID: material.MaterialID, Actor: actor, ExpectedVersion: other.Version, Environment: "controlled", IdempotencyKey: "manual-other-" + uuid.NewString(), Reason: "submission"}
		if _, err := r.CreateManualProductAdmission(ctx, in, allow); err == nil {
			t.Fatal("foreign material accepted")
		}
		in.CatalogItemID = item.CatalogItemID
		in.ExpectedVersion = item.Version
		in.IdempotencyKey = "manual-own-" + uuid.NewString()
		rec, err := r.CreateManualProductAdmission(ctx, in, allow)
		if err != nil {
			t.Fatal(err)
		}
		for _, missing := range []struct {
			catalogID, verificationID uuid.UUID
			version                   int64
		}{
			{item.CatalogItemID, uuid.New(), item.Version},
			{other.CatalogItemID, rec.VerificationID, other.Version},
		} {
			key := "manual-missing-" + uuid.NewString()
			_, err = r.DecideManualProductAdmission(ctx, ProductManualAdmissionDecide{CatalogItemID: missing.catalogID, VerificationID: missing.verificationID, Actor: actor, ExpectedVersion: missing.version, SubmissionSHA: rec.SubmissionSHA, Decision: "APPROVE", Reason: "missing or foreign submission", IdempotencyKey: key}, allow)
			if !errors.Is(err, ErrNotFound) {
				t.Fatal("missing or foreign verification lost not-found semantics", missing, err)
			}
			var receipts, audits int
			if err = r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM recharge_product_command_records WHERE idempotency_key=$1),(SELECT count(*) FROM recharge_product_audits WHERE idempotency_key=$1)`, key).Scan(&receipts, &audits); err != nil || receipts != 0 || audits != 0 {
				t.Fatal("failed lookup committed a command receipt or audit", receipts, audits, err)
			}
		}
		unchanged, err := r.GetManualProductAdmission(ctx, item.CatalogItemID, rec.VerificationID)
		if err != nil || unchanged.Status != "PENDING" || len(unchanged.Decisions) != 0 || unchanged.EvidenceID != nil || unchanged.SubmissionSHA != rec.SubmissionSHA || string(unchanged.Snapshot) != string(rec.Snapshot) {
			t.Fatal("foreign lookup changed the original submission", unchanged, err)
		}
		_ = catalogAtomicEditDraft(t, r, ctx, item)
		_, err = r.DecideManualProductAdmission(ctx, ProductManualAdmissionDecide{CatalogItemID: item.CatalogItemID, VerificationID: rec.VerificationID, Actor: actor, ExpectedVersion: item.Version, SubmissionSHA: rec.SubmissionSHA, Decision: "APPROVE", Reason: "stale", IdempotencyKey: "manual-stale-" + uuid.NewString()}, allow)
		if !errors.Is(err, ErrConflict) {
			t.Fatal("stale snapshot accepted", err)
		}
	})
	t.Run("retention-purges-submission-snapshot-and-review-reason", func(t *testing.T) {
		r, ctx := retainedFinanceRepository(t)
		item, _, actor := manualAdmissionFixture(t, r, ctx)
		materialID := uuid.New()
		created := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
		expiry := created.AddDate(1, 0, 0)
		// Retained history uses the existing purge fixture's old timestamps.
		// No trigger is disabled. Canonical snapshot bytes come from the actual
		// producer under that historical clock; this is not new-submit acceptance.
		if _, err := r.pool.Exec(ctx, `INSERT INTO product_admission_materials(material_id,catalog_item_id,catalog_version,uploaded_by,media_type,size,sha256,intended_expires_at,created_at,purge_after) VALUES($1,$2,$3,$4,'image/png',1,repeat('a',64),$5,$6,'2099-01-01Z')`, materialID, item.CatalogItemID, item.Version, actor, expiry, created); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `INSERT INTO product_admission_material_ciphertexts(material_id,key_id,wrapped_dek,nonce,ciphertext) VALUES($1,'controlled-old-key',$2,$3,$4)`, materialID, []byte{1}, make([]byte, 12), make([]byte, 17)); err != nil {
			t.Fatal(err)
		}
		verification, _, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, actor, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `UPDATE product_admission_materials SET verification_id=$2,version=version+1 WHERE material_id=$1`, materialID, verification.VerificationID); err != nil {
			t.Fatal(err)
		}
		product := Product{ProductID: item.ProductID, BasePoints: item.BasePoints, BonusPoints: item.BonusPoints, ValidityDays: item.ValidityDays, InvoiceProductName: item.InvoiceProductName}
		original := ProductAdmissionOriginal{DocumentID: materialID, Version: 1, MediaType: "image/png", Size: 1, SHA256: strings.Repeat("a", 64)}
		snapshot, digest, err := freezeProductAdmissionSubmission("controlled-retention", item, product, original, expiry, created)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `INSERT INTO product_manual_admission_submissions(verification_id,catalog_item_id,material_id,submission_sha,snapshot,requested_by,requested_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, verification.VerificationID, item.CatalogItemID, materialID, digest, snapshot, actor, created, expiry); err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `INSERT INTO product_manual_admission_decisions(verification_id,reviewer_id,decision,reason,decided_at) VALUES($1,$2,'APPROVE','controlled retained review',$3)`, verification.VerificationID, actor, created.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if n, err := r.ExpirePendingManualProductAdmissions(ctx, 100); err != nil || n != 1 {
			t.Fatal(n, err)
		}
		before, err := r.GetManualProductAdmission(ctx, item.CatalogItemID, verification.VerificationID)
		if err != nil || before.Status != "FAILED" || len(before.Decisions) != 1 {
			t.Fatal(before, err)
		}
		material, err := r.GetProductAdmissionMaterial(ctx, item.CatalogItemID, materialID)
		if err != nil {
			t.Fatal(err)
		}
		reviewAt := time.Now().Add(time.Hour)
		allow := func(context.Context) error { return nil }
		held, err := r.SetProductAdmissionMaterialLegalHold(ctx, ProductAdmissionMaterialHold{CatalogItemID: item.CatalogItemID, MaterialID: materialID, Actor: actor, ExpectedVersion: material.Version, Hold: true, Reason: "controlled retention case", ReviewAt: &reviewAt, IdempotencyKey: uuid.NewString()}, allow)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := r.purgeDueProductAdmissionMaterials(ctx); err != nil || n != 0 {
			t.Fatal("legal hold did not protect retained facts", n, err)
		}
		protected, err := r.GetManualProductAdmission(ctx, item.CatalogItemID, verification.VerificationID)
		if err != nil || string(protected.Snapshot) != string(before.Snapshot) || protected.Decisions[0].Reason != before.Decisions[0].Reason {
			t.Fatal("hold changed retained review", protected, err)
		}
		if _, err = r.SetProductAdmissionMaterialLegalHold(ctx, ProductAdmissionMaterialHold{CatalogItemID: item.CatalogItemID, MaterialID: materialID, Actor: actor, ExpectedVersion: held.Version, Hold: false, Reason: "controlled case released", IdempotencyKey: uuid.NewString()}, allow); err != nil {
			t.Fatal(err)
		}
		if n, err := r.purgeDueProductAdmissionMaterials(ctx); err != nil || n != 1 {
			t.Fatal("manual facts blocked eligible original purge", n, err)
		}
		after, err := r.GetManualProductAdmission(ctx, item.CatalogItemID, verification.VerificationID)
		if err != nil {
			t.Fatal(err)
		}
		var remaining map[string]any
		if err = json.Unmarshal(after.Snapshot, &remaining); err != nil || len(remaining) != 0 || len(after.Decisions) != 1 || after.Decisions[0].Reason != "REMOVED_AFTER_RETENTION" {
			t.Fatal("retained private review content was not removed", after, err)
		}
		if after.VerificationID != before.VerificationID || after.MaterialID != before.MaterialID || after.SubmissionSHA != before.SubmissionSHA || after.RequestedBy != before.RequestedBy || !after.RequestedAt.Equal(before.RequestedAt) || !after.ExpiresAt.Equal(before.ExpiresAt) || after.Status != before.Status || after.Decisions[0].ReviewerID != before.Decisions[0].ReviewerID || after.Decisions[0].Decision != before.Decisions[0].Decision || !after.Decisions[0].DecidedAt.Equal(before.Decisions[0].DecidedAt) {
			t.Fatal("purge changed retained identity or decision facts", before, after)
		}
		if _, _, err = r.GetProductAdmissionMaterialEnvelope(ctx, item.CatalogItemID, materialID); !errors.Is(err, ErrNotFound) {
			t.Fatal("purged original readable", err)
		}
		var updates bool
		if err = r.pool.QueryRow(ctx, `SELECT has_table_privilege(current_user,'public.product_manual_admission_submissions','UPDATE') OR has_table_privilege(current_user,'public.product_manual_admission_decisions','UPDATE')`).Scan(&updates); err != nil || updates {
			t.Fatal("runtime immutable authority widened", updates, err)
		}
		if n, err := r.purgeDueProductAdmissionMaterials(ctx); err != nil || n != 0 {
			t.Fatal("retention replay repeated effects", n, err)
		}
	})
}

func TestExpirePendingManualProductAdmissionsReleasesFingerprintSlot(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	item, material, actor := manualAdmissionFixture(t, r, ctx)
	record, err := r.CreateManualProductAdmission(ctx, ProductManualAdmissionCreate{CatalogItemID: item.CatalogItemID, MaterialID: material.MaterialID, Actor: actor, ExpectedVersion: item.Version, Environment: "controlled", IdempotencyKey: "manual-expiry-create-" + uuid.NewString(), Reason: "expiry"}, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	allow := func(context.Context) error { return nil }
	voted, err := r.DecideManualProductAdmission(ctx, ProductManualAdmissionDecide{CatalogItemID: item.CatalogItemID, VerificationID: record.VerificationID, Actor: actor, ExpectedVersion: item.Version, SubmissionSHA: record.SubmissionSHA, Decision: "APPROVE", Reason: "one review before expiry", IdempotencyKey: uuid.NewString()}, allow)
	if err != nil || len(voted.Decisions) != 1 || voted.Status != "PENDING" {
		t.Fatal("partial review fixture", voted, err)
	}
	var service, updates bool
	if err = r.pool.QueryRow(ctx, `SELECT current_user='billing_service',has_table_privilege(current_user,'public.product_manual_admission_submissions','UPDATE')`).Scan(&service, &updates); err != nil || !service || updates {
		t.Fatal("expiry must use the actual immutable runtime role", service, updates, err)
	}
	if _, err = retainedFinanceOwnerExec(t, r, ctx, `UPDATE product_manual_admission_submissions SET requested_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE verification_id='`+record.VerificationID.String()+`'`); err != nil {
		t.Fatal(err)
	}
	if n, err := r.ExpirePendingManualProductAdmissions(ctx, 100); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	closed, err := r.GetManualProductAdmission(ctx, item.CatalogItemID, record.VerificationID)
	if err != nil || closed.Status != "FAILED" || len(closed.Decisions) != 1 || closed.Decisions[0].ReviewerID != actor || closed.Decisions[0].Reason != voted.Decisions[0].Reason || closed.SubmissionSHA != record.SubmissionSHA || string(closed.Snapshot) != string(record.Snapshot) {
		t.Fatal(closed, err)
	}
	var code string
	if err = r.pool.QueryRow(ctx, `SELECT failure_code FROM product_admission_verifications WHERE verification_id=$1`, record.VerificationID).Scan(&code); err != nil || code != "MANUAL_ADMISSION_EXPIRED" {
		t.Fatal(code, err)
	}
	terminal, err := r.GetProductAdmissionMaterial(ctx, item.CatalogItemID, material.MaterialID)
	if err != nil || terminal.TerminalAt == nil {
		t.Fatal("expired material remained unanchored", terminal, err)
	}
	retry := ProductManualAdmissionCreate{CatalogItemID: item.CatalogItemID, MaterialID: material.MaterialID, Actor: actor, ExpectedVersion: item.Version, Environment: "controlled", IdempotencyKey: uuid.NewString(), Reason: "new review after expiry"}
	if _, err = r.CreateManualProductAdmission(ctx, retry, allow); !errors.Is(err, ErrConflict) {
		t.Fatal("expired original reused for a fresh review", err)
	}
	freshMaterial := saveManualAdmissionFixtureMaterial(t, r, ctx, item, actor, time.Now().UTC().Add(time.Hour))
	retry.MaterialID, retry.IdempotencyKey = freshMaterial.MaterialID, uuid.NewString()
	next, err := r.CreateManualProductAdmission(ctx, retry, allow)
	if err != nil || next.VerificationID == record.VerificationID || next.Status != "PENDING" {
		t.Fatal("expired fingerprint slot not released", next, err)
	}
	if n, err := r.ExpirePendingManualProductAdmissions(ctx, 100); err != nil || n != 0 {
		t.Fatal("next expiry tick changed a fresh request", n, err)
	}
}

// Existing FORMAL finance fixtures now provision real two-person admission.
// Only parsing/encryption/Account identity are controlled substitutes; all
// ownership, decisions, receipts, audits, evidence and binding SQL is real.
func completeManualAdmissionFixture(t *testing.T, r *PostgresRepository, ctx context.Context, item RechargeProductCatalogItem, expiresAt time.Time) uuid.UUID {
	t.Helper()
	actor := uuid.New()
	allow := func(context.Context) error { return nil }
	material := saveManualAdmissionFixtureMaterial(t, r, ctx, item, actor, expiresAt)
	record, err := r.CreateManualProductAdmission(ctx, ProductManualAdmissionCreate{CatalogItemID: item.CatalogItemID, MaterialID: material.MaterialID, Actor: actor, ExpectedVersion: item.Version, Environment: "controlled", IdempotencyKey: "manual-fixture-create-" + uuid.NewString(), Reason: "controlled merchant original"}, allow)
	if err != nil {
		t.Fatal(err)
	}
	var result ProductManualAdmission
	for range 2 {
		result, err = r.DecideManualProductAdmission(ctx, ProductManualAdmissionDecide{CatalogItemID: item.CatalogItemID, VerificationID: record.VerificationID, Actor: uuid.New(), ExpectedVersion: item.Version, SubmissionSHA: record.SubmissionSHA, Decision: "APPROVE", Reason: "controlled independent review", IdempotencyKey: "manual-fixture-vote-" + uuid.NewString()}, allow)
		if err != nil {
			t.Fatal(err)
		}
	}
	if result.EvidenceID == nil {
		t.Fatal("two votes produced no evidence")
	}
	return *result.EvidenceID
}
