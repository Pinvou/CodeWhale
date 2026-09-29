package billing

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRegistrationBonusLifetimeEligibilityPostgresIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("the real PostgreSQL registration bonus test runs in its dedicated CI step")
	}
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)

	fingerprint := func(seed byte) string {
		return strings.Repeat(string([]byte{"0123456789abcdef"[seed%16]}), 64)
	}
	provision := func(userID, operationID uuid.UUID, value string) (RegistrationBonusReceipt, bool, error) {
		return repository.ProvisionRegistrationBonus(ctx, RegistrationBonusProvisionRequest{
			UserID: userID, RegistrationOperationID: operationID,
			FingerprintKeyID: "registration-bonus-v1", MobileFingerprint: value,
		})
	}

	disabledUser, disabledOperation := uuid.New(), uuid.New()
	disabled, created, err := provision(disabledUser, disabledOperation, fingerprint(1))
	if err != nil || !created || disabled.Outcome != "NOT_GRANTED_DISABLED" || disabled.GrantedMicrocredits != 0 || disabled.PolicyRevision != 1 {
		t.Fatalf("disabled receipt=%+v created=%v err=%v", disabled, created, err)
	}

	actor := uuid.New()
	var policy RegistrationBonusPolicy
	policyResults := make(chan RegistrationBonusPolicy, 2)
	policyReplays := make(chan bool, 2)
	policyErrors := make(chan error, 2)
	policyStart := make(chan struct{})
	var policyWait sync.WaitGroup
	for range 2 {
		policyWait.Add(1)
		go func() {
			defer policyWait.Done()
			<-policyStart
			result, replayed, callErr := repository.UpdateRegistrationBonusPolicy(ctx, actor, true, 250000, 90, 1, "Enable registration bonus integration policy", "registration-bonus-policy-0001")
			if callErr != nil {
				policyErrors <- callErr
				return
			}
			policyResults <- result
			policyReplays <- replayed
		}()
	}
	close(policyStart)
	policyWait.Wait()
	close(policyResults)
	close(policyReplays)
	close(policyErrors)
	for callErr := range policyErrors {
		t.Fatalf("concurrent policy update: %v", callErr)
	}
	policyReplayCounts := map[bool]int{}
	for result := range policyResults {
		policy = result
		if policy.Revision != 2 || !policy.Enabled || policy.AmountMicrocredits != 250000 {
			t.Fatalf("concurrent enabled policy=%+v", policy)
		}
	}
	for replayed := range policyReplays {
		policyReplayCounts[replayed]++
	}
	if policyReplayCounts[false] != 1 || policyReplayCounts[true] != 1 {
		t.Fatalf("concurrent policy replay results=%v", policyReplayCounts)
	}
	disabledReplay, _, err := provision(uuid.New(), uuid.New(), fingerprint(1))
	if err != nil || disabledReplay.Outcome != "ALREADY_CLAIMED" || disabledReplay.GrantedMicrocredits != 0 || disabledReplay.ClaimID != disabled.ClaimID || disabledReplay.PolicyRevision != 1 {
		t.Fatalf("disabled eligibility was not permanently consumed: %+v err=%v", disabledReplay, err)
	}

	grantedUser, grantedOperation := uuid.New(), uuid.New()
	granted, created, err := provision(grantedUser, grantedOperation, fingerprint(2))
	if err != nil || !created || granted.Outcome != "GRANTED" || granted.GrantedMicrocredits != 250000 || granted.PolicyRevision != 2 {
		t.Fatalf("granted receipt=%+v created=%v err=%v", granted, created, err)
	}
	replayedReceipt, created, err := provision(grantedUser, grantedOperation, fingerprint(2))
	if err != nil || created || replayedReceipt != granted {
		t.Fatalf("idempotent replay=%+v created=%v err=%v", replayedReceipt, created, err)
	}
	_, _, err = provision(uuid.New(), grantedOperation, fingerprint(2))
	if !errors.Is(err, ErrRegistrationBonusReplayChange) {
		t.Fatalf("changed replay error=%v", err)
	}

	var posted, journalBalance, packages, receipts int64
	err = repository.pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, granted.WalletID).Scan(&posted)
	if err != nil || posted != 250000 {
		t.Fatalf("wallet posted=%d err=%v", posted, err)
	}
	err = repository.pool.QueryRow(ctx, `SELECT COALESCE(sum(posting.amount_microcredits),0)
FROM ledger_postings posting JOIN journal_entries entry ON entry.id=posting.entry_id WHERE entry.reference=$1`, "registration-bonus:"+granted.ClaimID.String()).Scan(&journalBalance)
	if err != nil || journalBalance != 0 {
		t.Fatalf("journal balance=%d err=%v", journalBalance, err)
	}
	err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM entitlement_packages package JOIN entitlement_components component ON component.package_id=package.id
WHERE package.wallet_id=$1 AND package.product_id='REGISTRATION_BONUS' AND package.source_type='SYSTEM' AND component.component_type='BONUS'`, granted.WalletID).Scan(&packages)
	if err != nil || packages != 1 {
		t.Fatalf("registration packages=%d err=%v", packages, err)
	}
	listedPackages, _, err := repository.ListPackages(ctx, grantedUser, granted.WalletID, 10)
	if err != nil || len(listedPackages) != 1 {
		t.Fatalf("list registration packages=%d err=%v", len(listedPackages), err)
	}
	listed := listedPackages[0]
	if listed.ProductID != registrationBonusProductID || listed.PlanName != registrationBonusProductID || listed.CurrentPlanNameZhCN != registrationBonusDisplayNameZhCN || listed.CurrentPlanNameEN != registrationBonusDisplayNameEN || listed.CurrentPlanNameJA != registrationBonusDisplayNameJA {
		t.Fatalf("registration package localized projection=%+v", listed)
	}
	err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM registration_bonus_receipts WHERE registration_operation_id=$1`, grantedOperation).Scan(&receipts)
	if err != nil || receipts != 1 {
		t.Fatalf("registration receipts=%d err=%v", receipts, err)
	}
	usageID := uuid.New()
	hold, err := repository.Reserve(ctx, usageID, granted.WalletID, 100000, nil, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("reserve registration bonus: %v", err)
	}
	if _, err = repository.Commit(ctx, usageID, hold.ID, 50000, map[string]any{"input_tokens": 1, "output_tokens": 1}); err != nil {
		t.Fatalf("commit registration bonus: %v", err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, granted.WalletID).Scan(&posted); err != nil || posted != 200000 {
		t.Fatalf("wallet after committed usage=%d err=%v", posted, err)
	}

	policy, _, err = repository.UpdateRegistrationBonusPolicy(ctx, actor, true, 0, 90, 2, "Exercise zero registration bonus policy", "registration-bonus-policy-0002")
	if err != nil || policy.Revision != 3 || policy.AmountMicrocredits != 0 {
		t.Fatalf("zero policy=%+v err=%v", policy, err)
	}
	zero, _, err := provision(uuid.New(), uuid.New(), fingerprint(3))
	if err != nil || zero.Outcome != "NOT_GRANTED_ZERO" || zero.PolicyRevision != 3 {
		t.Fatalf("zero receipt=%+v err=%v", zero, err)
	}
	policy, _, err = repository.UpdateRegistrationBonusPolicy(ctx, actor, true, 250000, 90, 3, "Restore registration bonus integration policy", "registration-bonus-policy-0003")
	if err != nil || policy.Revision != 4 {
		t.Fatalf("restored policy=%+v err=%v", policy, err)
	}
	zeroReplay, _, err := provision(uuid.New(), uuid.New(), fingerprint(3))
	if err != nil || zeroReplay.Outcome != "ALREADY_CLAIMED" || zeroReplay.ClaimID != zero.ClaimID || zeroReplay.PolicyRevision != 3 {
		t.Fatalf("zero eligibility was not permanently consumed: %+v err=%v", zeroReplay, err)
	}

	concurrentFingerprint := fingerprint(4)
	var wait sync.WaitGroup
	results := make(chan RegistrationBonusReceipt, 2)
	errorsChannel := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			receipt, _, callErr := provision(uuid.New(), uuid.New(), concurrentFingerprint)
			if callErr != nil {
				errorsChannel <- callErr
				return
			}
			results <- receipt
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsChannel)
	for callErr := range errorsChannel {
		t.Fatalf("concurrent provisioning: %v", callErr)
	}
	outcomes := map[string]int{}
	var claimIDs [][]byte
	for receipt := range results {
		outcomes[receipt.Outcome]++
		claimIDs = append(claimIDs, receipt.ClaimID[:])
	}
	if outcomes["GRANTED"] != 1 || outcomes["ALREADY_CLAIMED"] != 1 || len(claimIDs) != 2 || !bytes.Equal(claimIDs[0], claimIDs[1]) {
		t.Fatalf("concurrent outcomes=%v claimIDs=%x", outcomes, claimIDs)
	}
	repository.now = func() time.Time { return granted.CreatedAt.AddDate(0, 0, 91) }
	if expired, expireErr := repository.ExpirePackages(ctx, 100); expireErr != nil || expired < 1 {
		t.Fatalf("expire registration bonus packages=%d err=%v", expired, expireErr)
	}
	var packageStatus string
	if err = repository.pool.QueryRow(ctx, `SELECT status FROM entitlement_packages WHERE id=(SELECT entitlement_package_id FROM registration_bonus_claims WHERE id=$1)`, granted.ClaimID).Scan(&packageStatus); err != nil || packageStatus != "EXPIRED" {
		t.Fatalf("registration bonus package status=%s err=%v", packageStatus, err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, granted.WalletID).Scan(&posted); err != nil || posted != 0 {
		t.Fatalf("wallet after registration bonus expiry=%d err=%v", posted, err)
	}
	for _, table := range []string{"registration_bonus_policy_versions", "registration_bonus_policy_commands", "registration_bonus_policy_audit", "registration_bonus_claims", "registration_bonus_receipts"} {
		if _, truncateErr := repository.pool.Exec(ctx, `TRUNCATE `+table); truncateErr == nil {
			t.Fatalf("immutable registration bonus table %s allowed TRUNCATE", table)
		}
	}
}
