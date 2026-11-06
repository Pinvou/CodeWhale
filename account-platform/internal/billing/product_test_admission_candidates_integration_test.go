package billing

import (
	"context"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
)

func testActiveProviderAdmissionCandidates(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	actor := uuid.New()
	capabilities := payment.Capabilities{ProviderID: "provider", Version: "v2", Channels: []string{" alipay "}}
	requestedAt := time.Now().UTC().Add(-time.Minute)
	r.now = func() time.Time { return requestedAt }
	start := func(provider, version, channel string) uuid.UUID {
		t.Helper()
		requestedAt = requestedAt.Add(time.Second)
		input := validRechargeProductForEditTests()
		input.ProductID = "TEST_ADMISSION_" + uuid.NewString()
		input.Mapping.ExternalSKUBOM = input.ProductID
		input.Mapping.ProviderID, input.Mapping.ProviderVersion = provider, version
		input.Mapping.Channels = []string{channel}
		item, err := r.CreateRechargeProduct(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		verification, created, err := r.StartProductAdmissionVerification(ctx, item.CatalogItemID, actor, nil)
		if err != nil || !created {
			t.Fatalf("start verification: created=%v err=%v", created, err)
		}
		return verification.VerificationID
	}
	var old []uuid.UUID
	for range 10 {
		old = append(old, start("previous-provider", capabilities.Version, "ALIPAY"), start(capabilities.ProviderID, "v1", "ALIPAY"))
	}
	first := start(capabilities.ProviderID, capabilities.Version, "ALIPAY")
	second := start(capabilities.ProviderID, capabilities.Version, "ALIPAY")
	unsupported := start(capabilities.ProviderID, capabilities.Version, "WECHAT")
	state := func(id uuid.UUID) (string, string) {
		t.Helper()
		var status, source string
		err := r.pool.QueryRow(ctx, `SELECT verification.status,COALESCE(evidence.source_reference,'')
 FROM product_admission_verifications verification LEFT JOIN product_admission_evidence evidence USING(verification_id)
 WHERE verification.verification_id=$1`, id).Scan(&status, &source)
		if err != nil {
			t.Fatal(err)
		}
		return status, source
	}
	if n, err := r.VerifyPendingTestProductAdmissions(ctx, payment.Capabilities{}, time.Hour, 20); err != nil || n != 0 {
		t.Fatalf("empty candidate set: completed=%d err=%v", n, err)
	}
	if n, err := r.VerifyPendingTestProductAdmissions(ctx, capabilities, time.Hour, 1); err != nil || n != 1 {
		t.Fatalf("old provider/version rows starved current candidates: completed=%d err=%v", n, err)
	}
	if status, source := state(first); status != "VERIFIED" || source != "provider/v2" {
		t.Fatalf("first matching row: status=%q source=%q", status, source)
	}
	if status, source := state(second); status != "PENDING" || source != "" {
		t.Fatalf("limit did not preserve FIFO: status=%q source=%q", status, source)
	}

	if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE SEQUENCE fixture_admission_evidence_attempts;
 GRANT USAGE,SELECT ON SEQUENCE fixture_admission_evidence_attempts TO billing_service;
 CREATE FUNCTION fixture_fail_admission_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
   PERFORM nextval('fixture_admission_evidence_attempts');
   RAISE EXCEPTION 'controlled evidence write failure';
 END $$;
 CREATE TRIGGER fixture_fail_admission_evidence BEFORE INSERT ON product_admission_evidence
 FOR EACH ROW EXECUTE FUNCTION fixture_fail_admission_evidence()`); err != nil {
		t.Fatal(err)
	}
	if n, err := r.VerifyPendingTestProductAdmissions(ctx, capabilities, time.Hour, 20); err != nil || n != 0 {
		t.Fatalf("failed completion reported success: completed=%d err=%v", n, err)
	}
	if status, source := state(second); status != "PENDING" || source != "" {
		t.Fatalf("failed completion changed facts: status=%q source=%q", status, source)
	}
	workerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewProductAdmissionVerificationWorker(r, capabilities, 20*time.Millisecond, time.Hour).Run(workerCtx)
	}()
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	// The fixture sequence survives rollback. Observe a failed attempt by the
	// running worker before removing the fault and waiting for its next tick.
	for {
		var attempts int64
		if err := r.pool.QueryRow(workerCtx, `SELECT last_value FROM fixture_admission_evidence_attempts`).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts >= 2 {
			break
		}
		select {
		case <-ticker.C:
		case <-workerCtx.Done():
			t.Fatal("worker did not reach the controlled completion failure")
		}
	}
	if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER fixture_fail_admission_evidence ON product_admission_evidence`); err != nil {
		t.Fatal(err)
	}
	for {
		if status, source := state(second); status == "VERIFIED" && source == "provider/v2" {
			break
		}
		select {
		case <-ticker.C:
		case <-workerCtx.Done():
			t.Fatal("worker did not complete the current candidate on its next pass")
		}
	}
	for _, id := range append(old, unsupported) {
		if status, source := state(id); status != "PENDING" || source != "" {
			t.Fatalf("unrelated row changed: status=%q source=%q", status, source)
		}
	}
}
