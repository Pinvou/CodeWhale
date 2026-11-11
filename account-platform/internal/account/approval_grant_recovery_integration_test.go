package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The fault-only handler bypasses optional response-header projection so a
// locked identity lookup is reached inside the repository operation itself.
// No authentication, authorization or recovery decision is replaced.
type grantRecoveryFaultHeaders struct{ *PostgresRepository }

func (grantRecoveryFaultHeaders) GetAuthorizationVersions(context.Context, identity.Principal) (AuthorizationVersions, error) {
	return AuthorizationVersions{}, nil
}

// The five-minute window is elapsed in real wall-clock time. No deadline,
// published schema or runtime clock is patched. Domain proof/activation and
// broker PubAck remain controlled seams; no financial domain action is called.
func TestManualGrantRecoveryWorkflowPostgres(t *testing.T) {
	raw := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if raw == "" {
		t.Skip("isolated PostgreSQL required")
	}
	t.Run("expired-grant-dual-human-recovery", func(t *testing.T) {
		// The pre-900023 package and the upgrade use unedited public SQL.
		publishedDir, pathErr := filepath.Abs("../../migrations/account")
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		inputBytes, pathErr := os.ReadFile("testdata/approval_recovery_query_command_inputs.json")
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		staged := t.TempDir()
		stagedDir := filepath.Join(staged, "migrations", "account")
		stagedWork := filepath.Join(staged, "internal", "account")
		if pathErr = os.MkdirAll(stagedDir, 0700); pathErr != nil {
			t.Fatal(pathErr)
		}
		if pathErr = os.MkdirAll(filepath.Join(stagedWork, "testdata"), 0700); pathErr != nil {
			t.Fatal(pathErr)
		}
		entries, pathErr := os.ReadDir(publishedDir)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), "900023_") {
				continue
			}
			data, e := os.ReadFile(filepath.Join(publishedDir, entry.Name()))
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(stagedDir, entry.Name()), data, 0600); e != nil {
				t.Fatal(e)
			}
		}
		if pathErr = os.WriteFile(filepath.Join(stagedWork, "testdata", "approval_recovery_query_command_inputs.json"), inputBytes, 0600); pathErr != nil {
			t.Fatal(pathErr)
		}
		t.Chdir(stagedWork)
		ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
		defer cancel()
		repo, operator, relay, _ := newEntitlementApprovalFixture(t, ctx, raw)
		tokenRecentAuthActors(t, ctx, repo)
		requester := seedEntitlementApprovalActor(t, ctx, repo.pool, "PLATFORM_SUPER_ADMIN", "prebegin-diagnostic-requester")
		approver := seedEntitlementApprovalActor(t, ctx, repo.pool, "PLATFORM_SUPER_ADMIN", "prebegin-diagnostic-approver")
		repo.WithBillingGovernanceActivation(queryLeaseBillingActivation{}).WithTokenGovernanceActivation(&tokenRecentAuthActivation{true})
		repo.WithApprovalProofSource(&tokenRecentAuthProof{})
		data, err := os.ReadFile("testdata/approval_recovery_query_command_inputs.json")
		if err != nil {
			t.Fatal(err)
		}
		var inputs map[string]approvalencoding.LegacyProofInput
		if err = json.Unmarshal(data, &inputs); err != nil {
			t.Fatal(err)
		}
		input, ok := inputs["WALLET_ADJUSTMENT"]
		if !ok {
			t.Fatal("frozen input missing")
		}
		var descriptor approvalconsumer.Descriptor
		for _, d := range approvalconsumer.BillingCommands() {
			if d.CommandType == input.CommandType {
				descriptor = d
			}
		}
		if descriptor.CommandType == "" {
			t.Fatal("registered descriptor missing")
		}
		createGrant := func() (ApprovalRequest, ApprovalAuthorizationBinding, ApprovalDeliveryState) {
			draft := ApprovalDraft{CommandType: input.CommandType, IdempotencyKey: uuid.NewString(), TargetResourceType: input.TargetResourceType, TargetResourceID: input.TargetResourceID, NormalizedCommand: input.NormalizedCommand, CriticalResourceVersions: input.CriticalResourceVersions, FinancialSnapshot: input.FinancialSnapshot, Reason: "controlled grant before begin diagnostic", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`)}
			repo.WithApprovalSnapshotSource(&tokenRecentAuthSnapshot{draft: draft})
			var created ApprovalRequest
			err = retryWhileWriterUnavailable(t, ctx, repo, func() error {
				var e error
				created, e = repo.CreateApprovalDraft(ctx, entitlementApprovalPrincipal(requester), draft)
				return e
			})
			if err != nil {
				t.Fatal("create", err)
			}
			submitted, err := repo.SubmitApprovalRequest(ctx, entitlementApprovalPrincipal(requester), created.ID, created.Version)
			if err != nil {
				t.Fatal("submit", err)
			}
			approved, err := repo.DecideApprovalRequest(ctx, entitlementApprovalPrincipal(approver), created.ID, submitted.Version, "APPROVED", "independent prebegin diagnostic", input.CriticalResourceVersions)
			if err != nil || approved.Status != "APPROVED" {
				t.Fatal("approve", err)
			}
			worker := "prebegin-diagnostic-" + uuid.NewString()
			claims, err := relay.ClaimFrozen(ctx, worker, 1, 30*time.Second)
			if err != nil || len(claims) != 1 {
				t.Fatal("claim", err)
			}
			eventID := claims[0].EventID
			disposition, err := relay.ProcessFrozen(ctx, worker, claims[0], 12, time.Now().UTC(), func(_ context.Context, message events.FrozenMessage) events.FrozenPublishResult {
				if message.EventID != eventID || len(message.Body) == 0 {
					t.Fatal("wrong frozen event")
				}
				return events.FrozenPublishResult{Disposition: events.FrozenPublished}
			})
			if err != nil || disposition != events.FrozenPublished {
				t.Fatal("publish", err)
			}
			binding, err := repo.ResolveApprovalAuthorizationBinding(ctx, eventID, descriptor.Durable, descriptor.Workload)
			if err != nil {
				t.Fatal("binding", err)
			}
			observed, err := repo.ObserveApprovalAuthorizationDelivery(ctx, ApprovalDeliveryObservation{Binding: binding, Stream: "ACCOUNT_PLATFORM", StreamSequence: 1})
			if err != nil {
				t.Fatal("observe", err)
			}
			granted, err := repo.GrantApprovalAuthorizationDomainAttempt(ctx, eventID, binding.Durable, binding.Workload, observed.Version)
			if err != nil || granted.State != "DOMAIN_INFLIGHT" || granted.DomainAttemptCount != 1 {
				t.Fatal("grant", granted, err)
			}
			return approved, binding, granted
		}

		first, firstBinding, firstGrant := createGrant()
		second, secondBinding, _ := createGrant()
		thirdReviewer := seedEntitlementApprovalActor(t, ctx, repo.pool, "PLATFORM_SUPER_ADMIN", "manual-third-reviewer")
		denied := seedEntitlementApprovalActor(t, ctx, repo.pool, "PLATFORM_SUPER_ADMIN", "manual-no-permission")
		if _, err = repo.pool.Exec(ctx, `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE user_id=$1`, denied.ID); err != nil {
			t.Fatal(err)
		}
		actors := []entitlementFixtureActor{requester, approver, thirdReviewer, denied}
		verify := func(_ context.Context, token string) (identity.Principal, error) {
			for _, a := range actors {
				if token == a.Subject {
					return entitlementApprovalPrincipal(a), nil
				}
				if token == a.Subject+"-stale" {
					p := entitlementApprovalPrincipal(a)
					p.AuthenticatedAt = time.Now().Add(-20 * time.Minute)
					return p, nil
				}
			}
			return identity.Principal{}, errors.New("controlled invalid token")
		}
		handler := NewService(repo, nil).Handler(verify)
		const base = "/admin/v1/approval-grant-recovery-cases"
		call := func(token, method, path string, body any) *httptest.ResponseRecorder {
			var raw []byte
			if body != nil {
				raw, _ = json.Marshal(body)
			}
			req := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+token)
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			return response
		}
		check := func(response *httptest.ResponseRecorder, want int) {
			t.Helper()
			if response.Code != want {
				t.Fatalf("manual HTTP status=%d want=%d body=%s", response.Code, want, response.Body.String())
			}
		}
		check(call(requester.Subject, http.MethodGet, base, nil), 503)
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "900023_") {
				continue
			}
			data, e := os.ReadFile(filepath.Join(publishedDir, entry.Name()))
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(stagedDir, entry.Name()), data, 0600); e != nil {
				t.Fatal(e)
			}
		}
		loaded, upgradeErr := migrations.Load(stagedDir)
		if upgradeErr != nil {
			t.Fatal(upgradeErr)
		}
		if upgradeErr = accountschema.Up(ctx, operator, loaded); upgradeErr != nil {
			t.Fatal("public in-place upgrade", upgradeErr)
		}
		if _, upgradeErr = accountschema.VerifyApplied(ctx, operator, loaded); upgradeErr != nil {
			t.Fatal("verify upgraded package", upgradeErr)
		}
		bindingAfter, upgradeErr := repo.ResolveApprovalAuthorizationBinding(ctx, firstBinding.EventID, firstBinding.Durable, firstBinding.Workload)
		stateAfter, stateErr := repo.ApprovalAuthorizationDeliveryState(ctx, firstBinding.EventID, firstBinding.Durable, firstBinding.Workload)
		if upgradeErr != nil || stateErr != nil || bindingAfter != firstBinding || stateAfter.State != firstGrant.State || stateAfter.Version != firstGrant.Version || stateAfter.DomainAttemptCount != firstGrant.DomainAttemptCount {
			t.Fatal("upgrade changed original authorization facts", upgradeErr, stateErr)
		}
		getCase := func(id uuid.UUID) GrantRecoveryCase {
			t.Helper()
			response := call(requester.Subject, http.MethodGet, base+"/"+id.String(), nil)
			check(response, 200)
			var detail GrantRecoveryDetail
			if json.Unmarshal(response.Body.Bytes(), &detail) != nil {
				t.Fatal("invalid case JSON")
			}
			return detail.Case
		}
		mutation := func(version int64, evidence any) GrantRecoveryMutation {
			raw, _ := json.Marshal(evidence)
			return GrantRecoveryMutation{OperationID: uuid.New(), ExpectedVersion: version, Evidence: raw}
		}
		if n, e := repo.SyncManualReviewCases(ctx, 100); e != nil || n != 0 {
			t.Fatal("live grant was prematurely admitted", n, e)
		}
		check(call(denied.Subject, http.MethodGet, base, nil), 403)
		check(call(requester.Subject, http.MethodGet, base, nil), 200)
		deadline := *second.ExecutionDeadline
		if first.ExecutionDeadline.After(deadline) {
			deadline = *first.ExecutionDeadline
		}
		wait := time.Until(deadline.Add(20 * time.Millisecond))
		t.Logf("WAIT_REAL_WINDOW: waiting %s for original five-minute deadline; no clock/deadline mutation", wait)
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				t.Fatal(ctx.Err())
			case <-timer.C:
			}
		}
		// Snapshot the request before admission, as an old binary may do.
		staleTransactions := make([]pgx.Tx, 0, 2)
		for _, level := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
			stale, beginErr := repo.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: level})
			if beginErr != nil {
				t.Fatal(beginErr)
			}
			defer stale.Rollback(ctx)
			var originalID uuid.UUID
			if beginErr = stale.QueryRow(ctx, `SELECT id FROM approval_requests WHERE id=$1`, first.ID).Scan(&originalID); beginErr != nil {
				t.Fatal(beginErr)
			}
			staleTransactions = append(staleTransactions, stale)
		}
		// A locked old candidate must not starve another eligible authorization.
		lockedID := first.ID
		if second.ID.String() < lockedID.String() {
			lockedID = second.ID
		}
		gate, e := operator.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = gate.Exec(ctx, `SELECT id FROM approval_requests WHERE id=$1 FOR UPDATE`, lockedID); e != nil {
			t.Fatal(e)
		}
		n, e := repo.syncGrantRecoveryCases(ctx, 1)
		if rollbackErr := gate.Rollback(ctx); rollbackErr != nil {
			t.Fatal(rollbackErr)
		}
		if e != nil || n != 1 {
			t.Fatal("locked candidate starved eligible work", n, e)
		}
		// A delivery-only holder must not consume this batch, including legacy
		// cases. This is an owned DB barrier, not a timing guess or fake clock.
		lockedBinding := firstBinding
		if lockedID == second.ID {
			lockedBinding = secondBinding
		}
		deliveryGate, gateErr := operator.Begin(ctx)
		if gateErr != nil {
			t.Fatal(gateErr)
		}
		defer deliveryGate.Rollback(ctx)
		if _, gateErr = deliveryGate.Exec(ctx, `SET LOCAL ROLE account_approval_authorization_owner`); gateErr != nil {
			t.Fatal(gateErr)
		}
		if _, gateErr = deliveryGate.Exec(ctx, `SELECT original_event_id FROM account_approval_authorization.approval_delivery_states WHERE original_event_id=$1 AND durable=$2 FOR UPDATE`, lockedBinding.EventID, lockedBinding.Durable); gateErr != nil {
			t.Fatal(gateErr)
		}
		// Exercise the post-discovery race too: open must NOWAIT and roll back
		// its generic case instead of treating contention as a failed grant.
		admitCtx, admitCancel := context.WithTimeout(ctx, 2*time.Second)
		admitted, admitErr := repo.admitGrantRecoveryCase(admitCtx, lockedBinding.EventID, lockedBinding.Durable, lockedBinding.ApprovalRequestID)
		admitCancel()
		if admitErr != nil || admitted {
			t.Fatal("post-discovery lock did not skip", admitted, admitErr)
		}
		var partialCases int
		if gateErr = repo.pool.QueryRow(ctx, `SELECT count(*) FROM manual_review_cases WHERE resource_type='APPROVAL_AUTHORIZATION' AND resource_id=$1`, lockedBinding.EventID).Scan(&partialCases); gateErr != nil || partialCases != 0 {
			t.Fatal("busy delivery left partial public case", partialCases, gateErr)
		}
		legacyID := uuid.New()
		if _, gateErr = repo.pool.Exec(ctx, `INSERT INTO profile_change_requests(id,user_id,identity_issuer,identity_subject,field,old_value,new_value,expected_profile_version,status,next_attempt_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,'controlled','controlled','USERNAME','old','manual-recovery-new',1,'REPAIR_REQUIRED',clock_timestamp(),clock_timestamp(),clock_timestamp(),clock_timestamp())`, legacyID, requester.ID); gateErr != nil {
			t.Fatal(gateErr)
		}
		progressCtx, progressCancel := context.WithTimeout(ctx, 2*time.Second)
		made, progressErr := repo.SyncManualReviewCases(progressCtx, 100)
		progressCancel()
		if gateErr = deliveryGate.Rollback(ctx); gateErr != nil {
			t.Fatal(gateErr)
		}
		if progressErr != nil || made != 1 {
			t.Fatal("delivery lock starved legacy case", made, progressErr)
		}
		var legacyCases int
		if gateErr = repo.pool.QueryRow(ctx, `SELECT count(*) FROM manual_review_cases WHERE case_type='PROFILE_CHANGE_REPAIR' AND resource_id=$1`, legacyID).Scan(&legacyCases); gateErr != nil || legacyCases != 1 {
			t.Fatal("legacy recovery missing", legacyCases, gateErr)
		}
		if n, e = repo.SyncManualReviewCases(ctx, 100); e != nil || n != 1 {
			t.Fatal("remaining original case missing", n, e)
		}
		if n, e = repo.SyncManualReviewCases(ctx, 100); e != nil || n != 0 {
			t.Fatal("duplicate case admission", n, e)
		}
		response := call(requester.Subject, http.MethodGet, base+"?limit=100", nil)
		check(response, 200)
		var page GrantRecoveryPage
		if json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Items) != 2 {
			t.Fatal("two original cases not visible")
		}
		byRequest := map[uuid.UUID]GrantRecoveryCase{}
		for _, c := range page.Items {
			byRequest[c.RequestID] = c
		}
		c1, c2 := byRequest[first.ID], byRequest[second.ID]
		if c1.ID == uuid.Nil || c2.ID == uuid.Nil || c1.ID == c2.ID || c1.Version != 1 || c2.Version != 1 {
			t.Fatal("cases not idempotently bound")
		}
		faultHandler := NewService(grantRecoveryFaultHeaders{repo}, nil).Handler(verify)
		identityGate, identityErr := operator.Begin(ctx)
		if identityErr != nil {
			t.Fatal(identityErr)
		}
		defer identityGate.Rollback(ctx)
		if _, identityErr = identityGate.Exec(ctx, `LOCK TABLE identities IN ACCESS EXCLUSIVE MODE`); identityErr != nil {
			t.Fatal(identityErr)
		}
		for _, probe := range []struct{ method, path, body string }{
			{http.MethodGet, base, ""},
			{http.MethodGet, base + "/" + c1.ID.String(), ""},
			{http.MethodPost, base + "/" + c1.ID.String() + "/observations", fmt.Sprintf(`{"operation_id":%q,"expected_version":%d,"evidence":{"state":"ABSENT","reference":"CONTROLLED-QUERY","sha256":%q}}`, uuid.NewString(), c1.Version, strings.Repeat("a", 64))},
		} {
			faultCtx, faultCancel := context.WithTimeout(ctx, 150*time.Millisecond)
			req := httptest.NewRequest(probe.method, probe.path, strings.NewReader(probe.body)).WithContext(faultCtx)
			req.Header.Set("Authorization", "Bearer "+requester.Subject)
			if probe.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			result := httptest.NewRecorder()
			faultHandler.ServeHTTP(result, req)
			faultCancel()
			if result.Code != 500 {
				t.Fatalf("identity dependency failure became business denial: %s status=%d body=%s", probe.path, result.Code, result.Body.String())
			}
		}
		if identityErr = identityGate.Rollback(ctx); identityErr != nil {
			t.Fatal(identityErr)
		}
		// Use the real old insert contract to reach the new DB fence. This rejected
		// statement creates no execution; its exact error rules out an older guard.
		executionSQL := `INSERT INTO approval_executions(id,approval_request_id,command_type,command_fingerprint,execution_idempotency_key,executor_identity,job_run_id,status,authorization_snapshot,critical_resource_versions,financial_snapshot,started_at,
authorization_event_id,authorization_durable,authorization_approved_request_version,authorization_event_digest,authorization_digest_kind,authorization_serialization_version,authorization_provenance,authorization_approval_epoch)
VALUES($1,$2,$3,decode($4,'hex'),$5,$6,$7,'EXECUTING','{}',$8,$9,clock_timestamp(),$10,$11,$12,$13,$14,$15,$16,$17)`
		executionArgs := []any{uuid.New(), first.ID, first.CommandType, first.CommandFingerprint, fmt.Sprintf("approval:%s:%s:%s", first.ID, first.CommandType, first.CommandFingerprint), firstBinding.Workload, firstBinding.EventID.String(), input.CriticalResourceVersions, input.FinancialSnapshot, firstBinding.EventID, firstBinding.Durable, firstBinding.ApprovedRequestVersion, firstBinding.EventDigest, firstBinding.DigestKind, firstBinding.SerializationVersion, firstBinding.Provenance, firstBinding.ApprovalEpoch}
		for _, stale := range staleTransactions {
			_, staleErr := stale.Exec(ctx, executionSQL, executionArgs...)
			var serial *pgconn.PgError
			rejected := errors.As(staleErr, &serial) && serial.Code == "40001"
			if rollbackErr := stale.Rollback(ctx); rollbackErr != nil {
				t.Fatal(rollbackErr)
			}
			if !rejected {
				t.Fatal("pre-admission snapshot missed permanent fence", staleErr)
			}
		}
		_, err = repo.pool.Exec(ctx, executionSQL, executionArgs...)

		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "55000" || pg.Message != "manual recovery owns this authorization" {
			t.Fatal("manual DB execution fence was not reached", err)
		}
		if _, e = repo.ExpireApprovalRequests(ctx, 100); e != nil {
			t.Fatal(e)
		}
		if _, e = repo.SyncManualReviewCases(ctx, 100); e != nil {
			t.Fatal(e)
		}
		if getCase(c1.ID).Status != "OPEN" || getCase(c2.ID).Status != "OPEN" {
			t.Fatal("request expiry auto-closed unknown case")
		}
		var prematurelyResolved int
		if e = repo.pool.QueryRow(ctx, `SELECT count(*) FROM manual_review_cases WHERE case_type='APPROVAL_GRANT_RECOVERY' AND status='RESOLVED'`).Scan(&prematurelyResolved); e != nil || prematurelyResolved != 0 {
			t.Fatal("generic SLA resolved unknown grant", e)
		}
		evidence := func(c GrantRecoveryCase, kind, outcome string) GrantRecoveryProposal {
			// A controlled external completion record is the manual verification
			// seam. No live Billing financial success or proof of provider truth is
			// claimed by this Account workflow test.
			record := []byte(fmt.Sprintf("BILLING original=%s request=%s fingerprint=%s epoch=%d approved=%d kind=%s outcome=%s action-ended=true fence=%s", c.OriginalEventID, c.RequestID, c.CommandFingerprint, c.ApprovalEpoch, c.ApprovedRequestVersion, kind, outcome, c.OriginalEventID))
			sum := sha256.Sum256(record)
			return GrantRecoveryProposal{Authority: "BILLING", OriginalEventID: c.OriginalEventID, RequestID: c.RequestID, CommandFingerprint: c.CommandFingerprint, ApprovedRequestVersion: c.ApprovedRequestVersion, ApprovalEpoch: c.ApprovalEpoch, Kind: kind, Outcome: outcome, Reference: "FINAL-" + c.OriginalEventID.String(), SHA256: hex.EncodeToString(sum[:]), CompletionFenceReference: "FENCE-" + c.OriginalEventID.String(), AllObservationsResolved: true}
		}
		absent := GrantRecoveryObservation{State: "ABSENT", Reference: "QUERY-1", SHA256: strings.Repeat("a", 64)}
		observation := mutation(c1.Version, absent)
		response = call(requester.Subject, http.MethodPost, base+"/"+c1.ID.String()+"/observations", observation)
		check(response, 200)
		if json.Unmarshal(response.Body.Bytes(), &c1) != nil {
			t.Fatal("bad observation response")
		}
		check(call(requester.Subject, http.MethodPost, base+"/"+c1.ID.String()+"/observations", observation), 200)
		changedObservation := observation
		changedObservation.Evidence = json.RawMessage(`{"state":"OBSERVED","reference":"QUERY-1","sha256":"` + strings.Repeat("a", 64) + `"}`)
		check(call(requester.Subject, http.MethodPost, base+"/"+c1.ID.String()+"/observations", changedObservation), 409)
		check(call(requester.Subject, http.MethodPost, base+"/"+c1.ID.String()+"/proposals", mutation(c1.Version, absent)), 400)
		proof := evidence(c1, "ACTION_ENDED_NO_EFFECT", "NO_EFFECT")
		wrongBinding := proof
		wrongBinding.OriginalEventID = c2.OriginalEventID
		check(call(requester.Subject, http.MethodPost, base+"/"+c1.ID.String()+"/proposals", mutation(c1.Version, wrongBinding)), 400)
		check(call(requester.Subject+"-stale", http.MethodPost, base+"/"+c1.ID.String()+"/proposals", mutation(c1.Version, proof)), 401)
		proposal := mutation(c1.Version, proof)
		response = call(requester.Subject, http.MethodPost, base+"/"+c1.ID.String()+"/proposals", proposal)
		check(response, 200)
		if json.Unmarshal(response.Body.Bytes(), &c1) != nil {
			t.Fatal("bad proposal response")
		}
		review := mutation(c1.Version, GrantRecoveryReview{ProposalID: proposal.OperationID, SHA256: proof.SHA256, IndependentlyVerified: true})
		check(call(requester.Subject, http.MethodPost, base+"/"+c1.ID.String()+"/reviews", review), 409)
		// Two independent eligible reviewers race; only one operation may commit.
		type result struct {
			actor    entitlementFixtureActor
			command  GrantRecoveryMutation
			response *httptest.ResponseRecorder
		}
		results := make(chan result, 2)
		for _, a := range []entitlementFixtureActor{approver, thirdReviewer} {
			a := a
			command := review
			command.OperationID = uuid.New()
			go func() {
				results <- result{a, command, call(a.Subject, http.MethodPost, base+"/"+c1.ID.String()+"/reviews", command)}
			}()
		}
		var winner result
		successes := 0
		for range 2 {
			got := <-results
			switch got.response.Code {
			case 200:
				successes++
				winner = got
			case 409:
			default:
				t.Fatalf("unexpected concurrent review response %d %s", got.response.Code, got.response.Body.String())
			}
		}
		if successes != 1 {
			t.Fatal("review had multiple/no winners", successes)
		}
		if json.Unmarshal(winner.response.Body.Bytes(), &c1) != nil || c1.Status != "COMPLETED" || c1.LastCompletionID == nil || *c1.LastCompletionID != winner.command.OperationID {
			t.Fatal("wrong immutable completion receipt")
		}
		replayed := call(winner.actor.Subject, http.MethodPost, base+"/"+c1.ID.String()+"/reviews", winner.command)
		check(replayed, 200)
		if replayed.Body.String() != winner.response.Body.String() {
			t.Fatal("completion replay changed its original response")
		}
		state, e := repo.ApprovalAuthorizationDeliveryState(ctx, firstBinding.EventID, firstBinding.Durable, firstBinding.Workload)
		if e != nil || state.State != "RESULT_CONFIRMED" || state.DomainAttemptCount != firstGrant.DomainAttemptCount || state.ExecutionStatus != nil {
			t.Fatal("manual recovery fabricated execution, attempt or ACK", state, e)
		}
		var executions, reviews int
		if e = repo.pool.QueryRow(ctx, `SELECT count(*) FROM approval_executions`).Scan(&executions); e != nil || executions != 0 {
			t.Fatal("manual recovery created execution", e)
		}
		if e = repo.pool.QueryRow(ctx, `SELECT count(*) FROM public.read_approval_grant_recovery_operations($1,0,101) op WHERE op->>'action'='REVIEW'`, c1.ID).Scan(&reviews); e != nil || reviews != 1 {
			t.Fatal("review replay duplicated receipt", reviews, e)
		}
		// New evidence reopens attention without undoing a confirmed ACK/result or
		// re-enabling the original execution. Prior receipts stay in history.
		c1 = getCase(c1.ID)
		check(call(approver.Subject, http.MethodPost, base+"/"+c1.ID.String()+"/observations", mutation(c1.Version, GrantRecoveryObservation{State: "EVIDENCE_CONFLICT", Reference: "NEW-REVIEW-1", SHA256: strings.Repeat("b", 64)})), 200)
		if getCase(c1.ID).Status != "OPEN" {
			t.Fatal("post-completion evidence was dropped")
		}
		state, e = repo.ApprovalAuthorizationDeliveryState(ctx, firstBinding.EventID, firstBinding.Durable, firstBinding.Workload)
		if e != nil || state.State != "RESULT_CONFIRMED" {
			t.Fatal("observation rewound delivery", e)
		}
		// A new observation invalidates a pending proposal. Proposer revocation and
		// a replacement grant also invalidate the old authorization checkpoint.
		proof2 := evidence(c2, "AUTHORITATIVE_FINAL_RESULT", "SUCCEEDED")
		p2 := mutation(c2.Version, proof2)
		response = call(requester.Subject, http.MethodPost, base+"/"+c2.ID.String()+"/proposals", p2)
		check(response, 200)
		if json.Unmarshal(response.Body.Bytes(), &c2) != nil {
			t.Fatal("bad proposal")
		}
		check(call(approver.Subject, http.MethodPost, base+"/"+c2.ID.String()+"/observations", mutation(c2.Version, absent)), 200)
		c2 = getCase(c2.ID)
		check(call(approver.Subject, http.MethodPost, base+"/"+c2.ID.String()+"/reviews", mutation(c2.Version, GrantRecoveryReview{ProposalID: p2.OperationID, SHA256: proof2.SHA256, IndependentlyVerified: true})), 409)
		p2 = mutation(c2.Version, proof2)
		response = call(requester.Subject, http.MethodPost, base+"/"+c2.ID.String()+"/proposals", p2)
		check(response, 200)
		if json.Unmarshal(response.Body.Bytes(), &c2) != nil {
			t.Fatal("bad replacement proposal")
		}
		if _, e = repo.pool.Exec(ctx, `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE user_id=$1 AND revoked_at IS NULL`, requester.ID); e != nil {
			t.Fatal(e)
		}
		check(call(approver.Subject, http.MethodPost, base+"/"+c2.ID.String()+"/reviews", mutation(c2.Version, GrantRecoveryReview{ProposalID: p2.OperationID, SHA256: proof2.SHA256, IndependentlyVerified: true})), 403)
		if _, e = repo.pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,starts_at,created_at) VALUES($1,$2,'PLATFORM_SUPER_ADMIN','ACCOUNT',clock_timestamp()-interval '1 second',clock_timestamp())`, uuid.New(), requester.ID); e != nil {
			t.Fatal(e)
		}
		check(call(approver.Subject, http.MethodPost, base+"/"+c2.ID.String()+"/reviews", mutation(c2.Version, GrantRecoveryReview{ProposalID: p2.OperationID, SHA256: proof2.SHA256, IndependentlyVerified: true})), 409)
		p2 = mutation(c2.Version, proof2)
		response = call(requester.Subject, http.MethodPost, base+"/"+c2.ID.String()+"/proposals", p2)
		check(response, 200)
		if json.Unmarshal(response.Body.Bytes(), &c2) != nil {
			t.Fatal("new checkpoint not stored")
		}
		response = call(approver.Subject, http.MethodPost, base+"/"+c2.ID.String()+"/reviews", mutation(c2.Version, GrantRecoveryReview{ProposalID: p2.OperationID, SHA256: proof2.SHA256, IndependentlyVerified: true}))
		check(response, 200)
		if getCase(c2.ID).Status != "COMPLETED" {
			t.Fatal("valid final result did not converge")
		}
		if _, e = repo.SyncManualReviewCases(ctx, 100); e != nil {
			t.Fatal(e)
		}
		if getCase(c1.ID).Status != "OPEN" {
			t.Fatal("generic sync erased conflict attention")
		}
		check(call(denied.Subject, http.MethodGet, base+"/"+c2.ID.String(), nil), 403)
		t.Log("MANUAL_RECOVERY: real five-minute window; 2 original cases; absence is observation only; concurrent dual review/idempotent receipt; changed evidence and revoked checkpoints block; no executions or premature ACK; delivery-only lock preserves legacy progress; stale RR/Serializable snapshots rejected; identity IO is5xx")
	})
}
