package billing

import (
	"context"
	"crypto/sha256"
	"errors"
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
		_ = catalogAtomicEditDraft(t, r, ctx, item)
		_, err = r.DecideManualProductAdmission(ctx, ProductManualAdmissionDecide{CatalogItemID: item.CatalogItemID, VerificationID: rec.VerificationID, Actor: actor, ExpectedVersion: item.Version, SubmissionSHA: rec.SubmissionSHA, Decision: "APPROVE", Reason: "stale", IdempotencyKey: "manual-stale-" + uuid.NewString()}, allow)
		if !errors.Is(err, ErrConflict) {
			t.Fatal("stale snapshot accepted", err)
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
	if _, err = retainedFinanceOwnerExec(t, r, ctx, `UPDATE product_manual_admission_submissions SET requested_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE verification_id='`+record.VerificationID.String()+`'`); err != nil {
		t.Fatal(err)
	}
	if n, err := r.ExpirePendingManualProductAdmissions(ctx, 100); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	closed, err := r.GetManualProductAdmission(ctx, item.CatalogItemID, record.VerificationID)
	if err != nil || closed.Status != "FAILED" || len(closed.Decisions) != 0 {
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
