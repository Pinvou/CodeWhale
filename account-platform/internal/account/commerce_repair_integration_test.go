package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/commerce"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/google/uuid"
)

type commerceRepairFixture struct {
	r                  *PostgresRepository
	j                  CommerceCustomerSyncJob
	c                  CommerceRepairCase
	proposer, reviewer identity.Principal
	ctx                context.Context
}

func newCommerceRepairFixture(t *testing.T, f *retainedWalletFenceFixture, exhaust bool) *commerceRepairFixture {
	t.Helper()
	r := f.repository(t)
	ctx := f.ctx
	p := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_COMMERCE_OPERATOR", "commerce-operator")
	v := seedEntitlementApprovalActor(t, ctx, r.pool, "PLATFORM_COMMERCE_REVIEWER", "commerce-reviewer")
	principal := identity.Principal{Issuer: "https://commerce-owned.invalid", Subject: uuid.NewString(), SessionID: uuid.NewString(), AuthenticatedAt: time.Now().UTC()}
	me, _, err := r.Bootstrap(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	wallets, err := r.LeaseWalletProvisioningJobs(ctx, 1, time.Minute)
	if err != nil || len(wallets) != 1 || wallets[0].UserID != me.UserID {
		t.Fatal("original wallet claim", err)
	}
	if err = r.CompleteWalletProvisioning(ctx, wallets[0]); err != nil {
		t.Fatal(err)
	}
	// Controlled successful SMS verification fact. The actual mapping producer,
	// claim, twelve retries, cases, permissions and completion use production code.
	if _, err = r.pool.Exec(ctx, `UPDATE users SET mobile_e164='+8613800001234',mobile_verified_at=clock_timestamp() WHERE id=$1`, me.UserID); err != nil {
		t.Fatal(err)
	}
	provider := "owned-" + uuid.NewString()
	if err = r.ConfigureCommerceProvider(provider, "v1"); err != nil {
		t.Fatal(err)
	}
	if err = r.EnsureCommerceProviderMappings(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := r.ClaimCommerceCustomerSyncJob(ctx, provider, "v1", time.Minute)
	if err != nil || j == nil || j.SubjectID != me.UserID {
		t.Fatal("original commerce claim", err)
	}
	x := &commerceRepairFixture{r: r, j: *j, proposer: entitlementApprovalPrincipal(p), reviewer: entitlementApprovalPrincipal(v), ctx: ctx}
	if exhaust {
		for attempt := 1; attempt <= commerceCustomerMaximumAttempts; attempt++ {
			if j.Attempts != attempt {
				t.Fatal("wrong original attempt", j.Attempts, attempt)
			}
			if err = r.RetryCommerceCustomerSyncJob(ctx, *j, "COMMERCE_PROVIDER_UNAVAILABLE", time.Now().UTC().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if attempt < commerceCustomerMaximumAttempts {
				j, err = r.ClaimCommerceCustomerSyncJob(ctx, provider, "v1", time.Minute)
				if err != nil || j == nil {
					t.Fatal("legitimate retry", err)
				}
			}
		}
		x.j = *j
		x.c, err = r.QuarantineCommerceRepair(ctx, x.proposer, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if x.c.Attempts != commerceCustomerMaximumAttempts || x.c.Status != "OPEN" || x.c.Version != 1 {
			t.Fatal("exhaustion case binding", x.c)
		}
	}
	return x
}

func (x *commerceRepairFixture) observe(t *testing.T, outcome string, reference ...string) (CommerceRepairObservationInput, CommerceRepairObservation) {
	t.Helper()
	in := CommerceRepairObservationInput{ExpectedVersion: x.c.Version, ObservationID: uuid.New(), Outcome: outcome, EvidenceReference: "controlled-original-record-1", EvidenceSHA256: strings.Repeat("a", 64), AllAttemptsEnded: outcome != "UNCONFIRMED"}
	if outcome == "SUCCEEDED" {
		in.ExternalReferenceRedacted = "***1234"
	}
	if len(reference) > 0 {
		in.ExternalReferenceRedacted = reference[0]
	}
	o, err := x.r.ObserveCommerceRepair(x.ctx, x.proposer, x.c.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	x.c.Version = o.CaseVersion
	return in, o
}

func (x *commerceRepairFixture) confirm(t *testing.T, o CommerceRepairObservation) CommerceRepairCase {
	t.Helper()
	c, err := x.r.ConfirmCommerceRepair(x.ctx, x.reviewer, x.c.ID, CommerceRepairConfirmation{ExpectedVersion: o.CaseVersion, ObservationID: o.ID})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func commerceRepairFacts(t *testing.T, x *commerceRepairFixture, includeCases bool) string {
	t.Helper()
	var facts string
	err := x.r.pool.QueryRow(x.ctx, `SELECT jsonb_build_object(
'job',(SELECT to_jsonb(j) FROM commerce_customer_sync_jobs j WHERE id=$1),
'mapping',(SELECT to_jsonb(m) FROM commerce_customer_mappings m WHERE id=$2),
'sync_evidence',(SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY id),'[]') FROM commerce_customer_sync_evidence e WHERE job_id=$1),
'cases',CASE WHEN $3 THEN (SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY id),'[]') FROM commerce_customer_repair_cases c WHERE job_id=$1) END,
'observations',CASE WHEN $3 THEN (SELECT COALESCE(jsonb_agg(to_jsonb(o) ORDER BY id),'[]') FROM commerce_customer_repair_observations o WHERE case_id=$4) END,
'callbacks',CASE WHEN $3 THEN (SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY observation_sequence),'[]') FROM commerce_customer_repair_callbacks c WHERE case_id=$4) END,
'manual',CASE WHEN $3 THEN (SELECT to_jsonb(c) FROM manual_review_cases c WHERE id=$4) END,
'events',CASE WHEN $3 THEN (SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY id),'[]') FROM manual_review_case_events e WHERE case_id=$4) END,
'audit',CASE WHEN $3 THEN (SELECT COALESCE(jsonb_agg(to_jsonb(a) ORDER BY id),'[]') FROM audit_logs a WHERE resource_id=$4::text) END
)::text`, x.j.ID, x.j.MappingID, includeCases, x.c.ID).Scan(&facts)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func commerceRepairHTTP(t *testing.T, handler http.Handler, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	return out
}

func waitCommerceRepairBlocker(t *testing.T, x *commerceRepairFixture, pid int32, want int, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiters int
		// A second tuple waiter can be queued behind the first waiter. Count
		// the actual transitive blocking chain, not only direct blockers.
		err := x.r.pool.QueryRow(x.ctx, `WITH RECURSIVE blocked(pid) AS (
SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND $1::integer=ANY(pg_blocking_pids(pid))
UNION SELECT a.pid FROM pg_stat_activity a JOIN blocked b ON b.pid=ANY(pg_blocking_pids(a.pid)) WHERE a.datname=current_database()
) SELECT count(*) FROM blocked`, pid).Scan(&waiters)
		if err != nil {
			t.Fatal(err)
		}
		if waiters == want {
			return
		}
		select {
		case e := <-done:
			t.Fatal("operation returned before owned PostgreSQL lock barrier", e)
		case <-x.ctx.Done():
			t.Fatal(x.ctx.Err())
		case <-time.After(2 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("required PostgreSQL wait chain not observed", want, waiters)
		}
	}
}

func TestCommerceManualCompletionPostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	t.Run("current-success-http-and-receipt-replay", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		handler := NewService(x.r, nil).Handler(func(_ context.Context, token string) (identity.Principal, error) {
			switch token {
			case "operator":
				return x.proposer, nil
			case "reviewer":
				return x.reviewer, nil
			}
			return identity.Principal{}, ErrForbidden
		})
		base := "/admin/v1/commerce-repairs/" + x.c.ID.String()
		input := CommerceRepairObservationInput{ExpectedVersion: 1, ObservationID: uuid.New(), Outcome: "SUCCEEDED", EvidenceReference: "controlled-original-record-http", EvidenceSHA256: strings.Repeat("b", 64), AllAttemptsEnded: true, ExternalReferenceRedacted: "***1234"}
		observed := commerceRepairHTTP(t, handler, "operator", http.MethodPost, base+"/observations", input)
		if observed.Code != http.StatusOK {
			t.Fatal("real observation HTTP", observed.Code, observed.Body.String())
		}
		var o CommerceRepairObservation
		if err := json.Unmarshal(observed.Body.Bytes(), &o); err != nil {
			t.Fatal(err)
		}
		body := CommerceRepairConfirmation{ExpectedVersion: o.CaseVersion, ObservationID: o.ID}
		confirmed := commerceRepairHTTP(t, handler, "reviewer", http.MethodPost, base+"/actions/confirm", body)
		if confirmed.Code != http.StatusOK || confirmed.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("real confirmation HTTP", confirmed.Code, confirmed.Body.String())
		}
		var c CommerceRepairCase
		if err := json.Unmarshal(confirmed.Body.Bytes(), &c); err != nil || c.Resolution == nil || *c.Resolution != "SUCCEEDED_CURRENT" {
			t.Fatal("wrong completion", c, err)
		}
		frozen := commerceRepairFacts(t, x, true)
		replayed := commerceRepairHTTP(t, handler, "reviewer", http.MethodPost, base+"/actions/confirm", body)
		if replayed.Code != http.StatusOK || replayed.Body.String() != confirmed.Body.String() || commerceRepairFacts(t, x, true) != frozen {
			t.Fatal("committed receipt not replayed identically")
		}
		for _, path := range []string{"/admin/v1/commerce-repairs?limit=1", base} {
			out := commerceRepairHTTP(t, handler, "operator", http.MethodGet, path, nil)
			if out.Code != http.StatusOK || strings.Contains(out.Body.String(), "+8613800001234") {
				t.Fatal("read unavailable or contact exposed", out.Code)
			}
		}
		var job, mapping string
		if err := x.r.pool.QueryRow(x.ctx, `SELECT j.status,m.status FROM commerce_customer_sync_jobs j JOIN commerce_customer_mappings m ON m.id=j.mapping_id WHERE j.id=$1`, x.j.ID).Scan(&job, &mapping); err != nil || job != "COMPLETED" || mapping != "READY" {
			t.Fatal("proven original success not applied", job, mapping, err)
		}
	})
	t.Run("no-effect-is-not-sync-success", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		_, o := x.observe(t, "ENDED_NO_EFFECT")
		c := x.confirm(t, o)
		if c.Resolution == nil || *c.Resolution != "ENDED_NO_EFFECT" {
			t.Fatal("no-effect mislabeled")
		}
		var job, mapping string
		var successes int
		if err := x.r.pool.QueryRow(x.ctx, `SELECT j.status,m.status,(SELECT count(*) FROM commerce_customer_sync_evidence WHERE job_id=j.id AND outcome='SUCCEEDED') FROM commerce_customer_sync_jobs j JOIN commerce_customer_mappings m ON m.id=j.mapping_id WHERE j.id=$1`, x.j.ID).Scan(&job, &mapping, &successes); err != nil || job != "MANUAL_NO_EFFECT" || mapping != "REPAIR_REQUIRED" || successes != 0 {
			t.Fatal("no-effect forged success", job, mapping, successes, err)
		}
		adapter := &commerceAdapterStub{}
		NewCommerceCustomerSyncWorker(x.r, adapter, x.j.ProviderID, x.j.AdapterVersion, time.Second).runOne(x.ctx)
		if adapter.request.SubjectID != "" {
			t.Fatal("no-effect automatically dispatched")
		}
		if err := x.r.ConfigureCommerceProvider(x.j.ProviderID, "v2"); err != nil {
			t.Fatal(err)
		}
		if err := x.r.EnsureCommerceProviderMappings(x.ctx); err != nil {
			t.Fatal(err)
		}
		var jobs int
		if err := x.r.pool.QueryRow(x.ctx, `SELECT count(*) FROM commerce_customer_sync_jobs WHERE mapping_id=$1`, x.j.MappingID).Scan(&jobs); err != nil || jobs != 1 {
			t.Fatal("startup configuration automatically redispatched manual generation", jobs, err)
		}
	})
	t.Run("unknown-and-superseded-observation-block", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		_, old := x.observe(t, "SUCCEEDED")
		_, latest := x.observe(t, "UNCONFIRMED")
		before := commerceRepairFacts(t, x, true)
		for _, o := range []CommerceRepairObservation{old, latest} {
			if _, err := x.r.ConfirmCommerceRepair(x.ctx, x.reviewer, x.c.ID, CommerceRepairConfirmation{ExpectedVersion: o.CaseVersion, ObservationID: o.ID}); !errors.Is(err, ErrConflict) {
				t.Fatal("unconfirmed/superseded observation closed", err)
			}
		}
		if commerceRepairFacts(t, x, true) != before {
			t.Fatal("blocked evidence mutated facts")
		}
	})
	t.Run("observation-id-replay-and-payload-conflict", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		in, o := x.observe(t, "SUCCEEDED")
		before := commerceRepairFacts(t, x, true)
		replay, err := x.r.ObserveCommerceRepair(x.ctx, x.proposer, x.c.ID, in)
		if err != nil || replay.ID != o.ID || commerceRepairFacts(t, x, true) != before {
			t.Fatal("observation receipt replay", err)
		}
		in.EvidenceSHA256 = strings.Repeat("c", 64)
		if _, err = x.r.ObserveCommerceRepair(x.ctx, x.proposer, x.c.ID, in); !errors.Is(err, ErrConflict) || commerceRepairFacts(t, x, true) != before {
			t.Fatal("same observation id accepted changed evidence", err)
		}
	})
	t.Run("self-and-unprivileged-confirmations-block", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		// Both permissions are real: this probes independence rather than
		// accidentally passing because the proposer cannot approve at all.
		both := seedEntitlementApprovalActor(t, x.ctx, x.r.pool, "PLATFORM_SUPER_ADMIN", "both-permissions")
		x.proposer = entitlementApprovalPrincipal(both)
		_, o := x.observe(t, "SUCCEEDED")
		before := commerceRepairFacts(t, x, true)
		outsider := seedEntitlementApprovalActor(t, x.ctx, x.r.pool, "PERSONAL_USER", "not-reviewer")
		for _, p := range []identity.Principal{x.proposer, entitlementApprovalPrincipal(outsider)} {
			if _, err := x.r.ConfirmCommerceRepair(x.ctx, p, x.c.ID, CommerceRepairConfirmation{ExpectedVersion: o.CaseVersion, ObservationID: o.ID}); !errors.Is(err, ErrForbidden) {
				t.Fatal("independent current authority not enforced", err)
			}
		}
		if commerceRepairFacts(t, x, true) != before {
			t.Fatal("forbidden confirmation mutated original facts")
		}
		x.confirm(t, o)
	})
	t.Run("stale-mapping-keeps-new-generation", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		_, o := x.observe(t, "SUCCEEDED")
		tx, err := x.r.pool.Begin(x.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(x.ctx)
		if err = enqueueCommerceCustomerSync(x.ctx, tx, "USER", x.j.SubjectID, "+8613800005678", x.j.ProviderID, "v2", "PROFILE_SYNC", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(x.ctx); err != nil {
			t.Fatal(err)
		}
		var before, after string
		if err = x.r.pool.QueryRow(x.ctx, `SELECT to_jsonb(m)::text FROM commerce_customer_mappings m WHERE id=$1`, x.j.MappingID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		c := x.confirm(t, o)
		if err = x.r.pool.QueryRow(x.ctx, `SELECT to_jsonb(m)::text FROM commerce_customer_mappings m WHERE id=$1`, x.j.MappingID).Scan(&after); err != nil || before != after || c.Resolution == nil || *c.Resolution != "SUCCEEDED_EVIDENCE_ONLY" {
			t.Fatal("old result modified new generation", err)
		}
	})
	t.Run("closed-identity-keeps-retired-facts", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		_, o := x.observe(t, "SUCCEEDED")
		// Controlled post-cleanup facts match the existing closure owner: no
		// claim of executing the external IdP/Billing closure saga in this test.
		if _, err := x.r.pool.Exec(x.ctx, `UPDATE users SET status='DELETED',anonymized_at=clock_timestamp(),mobile_e164=NULL,mobile_verified_at=NULL,subject_lifecycle_version=subject_lifecycle_version+1 WHERE id=$1`, x.j.SubjectID); err != nil {
			t.Fatal(err)
		}
		if _, err := x.r.pool.Exec(x.ctx, `UPDATE commerce_customer_mappings SET status='DETACHED',external_reference_redacted=NULL WHERE id=$1`, x.j.MappingID); err != nil {
			t.Fatal(err)
		}
		if _, err := x.r.pool.Exec(x.ctx, `UPDATE commerce_customer_sync_jobs SET status='CANCELLED_ACCOUNT_CLOSURE',contact_mobile_e164=NULL WHERE id=$1`, x.j.ID); err != nil {
			t.Fatal(err)
		}
		var before, after string
		query := `SELECT jsonb_build_object('user',(SELECT to_jsonb(u) FROM users u WHERE id=$1),'job',(SELECT to_jsonb(j) FROM commerce_customer_sync_jobs j WHERE id=$2),'mapping',(SELECT to_jsonb(m) FROM commerce_customer_mappings m WHERE id=$3))::text`
		if err := x.r.pool.QueryRow(x.ctx, query, x.j.SubjectID, x.j.ID, x.j.MappingID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		c := x.confirm(t, o)
		if err := x.r.pool.QueryRow(x.ctx, query, x.j.SubjectID, x.j.ID, x.j.MappingID).Scan(&after); err != nil || before != after || c.Resolution == nil || *c.Resolution != "SUCCEEDED_EVIDENCE_ONLY" {
			t.Fatal("closed identity was restored", err)
		}
	})
	t.Run("expired-lease-quarantines-without-redispatch", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, false)
		if _, err := x.r.pool.Exec(x.ctx, `UPDATE commerce_customer_sync_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, x.j.ID); err != nil {
			t.Fatal(err)
		}
		adapter := &commerceAdapterStub{}
		worker := NewCommerceCustomerSyncWorker(x.r, adapter, x.j.ProviderID, x.j.AdapterVersion, time.Second)
		worker.runOne(x.ctx)
		worker.runOne(x.ctx)
		if adapter.request.SubjectID != "" {
			t.Fatal("unknown original supplier result was automatically dispatched")
		}
		c, err := x.r.QuarantineCommerceRepair(x.ctx, x.proposer, x.j.ID)
		if err != nil || c.Status != "OPEN" || c.Attempts != 1 {
			t.Fatal("missing original unknown case", err)
		}
		again, err := x.r.QuarantineCommerceRepair(x.ctx, x.proposer, x.j.ID)
		if err != nil || again.ID != c.ID {
			t.Fatal("quarantine did not reuse case", err)
		}
		if err = x.r.CompleteCommerceCustomerSyncJob(x.ctx, x.j, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***late"}); !errors.Is(err, ErrConflict) {
			t.Fatal("retired lease accepted late result", err)
		}
		var attempts int
		if err = x.r.pool.QueryRow(x.ctx, `SELECT attempts FROM commerce_customer_sync_jobs WHERE id=$1`, x.j.ID).Scan(&attempts); err != nil || attempts != 1 {
			t.Fatal("quarantine acquired new execution authority", err)
		}
	})
	t.Run("operator-wire-boundaries-preserve-original-facts", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		handler := NewService(x.r, nil).Handler(func(context.Context, string) (identity.Principal, error) { return x.proposer, nil })
		base := "/admin/v1/commerce-repairs/" + x.c.ID.String() + "/observations"
		in := CommerceRepairObservationInput{ExpectedVersion: 1, ObservationID: uuid.New(), Outcome: "SUCCEEDED", EvidenceReference: "controlled-original-wire-1", EvidenceSHA256: strings.Repeat("a", 64), AllAttemptsEnded: true, ExternalReferenceRedacted: "***1234"}
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		valid := string(raw)
		before := commerceRepairFacts(t, x, true)
		bad := map[string]string{
			"exact-duplicate":    strings.Replace(valid, "{", `{"expected_version":1,`, 1),
			"case-folded-alias":  strings.Replace(valid, `"outcome"`, `"OUTCOME"`, 1),
			"semantic-duplicate": strings.Replace(valid, "{", `{"EXPECTED_VERSION":1,`, 1),
			"uuid-alias":         strings.Replace(valid, in.ObservationID.String(), "urn:uuid:"+in.ObservationID.String(), 1),
			"null":               "null", "array": "[]", "scalar": `"not-an-object"`,
			"unknown-member":    strings.Replace(valid, "{", `{"provider_result":"trusted",`, 1),
			"trailing-document": valid + "{}", "oversized": strings.Repeat("a", 8193),
			"not-ended":               strings.Replace(valid, `"all_attempts_ended":true`, `"all_attempts_ended":false`, 1),
			"source-url":              strings.Replace(valid, "controlled-original-wire-1", "https://records.invalid/private", 1),
			"raw-external-reference":  strings.Replace(valid, "***1234", "13800001234", 1),
			"noeffect-with-reference": strings.Replace(valid, `"SUCCEEDED"`, `"ENDED_NO_EFFECT"`, 1),
			"nullable-reference":      strings.Replace(valid, `"external_reference_redacted":"***1234"`, `"external_reference_redacted":null`, 1),
			"nullable-ended":          strings.Replace(valid, `"all_attempts_ended":true`, `"all_attempts_ended":null`, 1),
		}
		for name, body := range bad {
			req := httptest.NewRequest(http.MethodPost, base, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer controlled")
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			if out.Code != http.StatusBadRequest || commerceRepairFacts(t, x, true) != before {
				t.Fatal("invalid operator wire reached mutation", name, out.Code)
			}
		}
		for _, path := range []string{"?extra=1", "?limit=01", "?limit=1&limit=2"} {
			out := commerceRepairHTTP(t, handler, "controlled", http.MethodGet, "/admin/v1/commerce-repairs"+path, nil)
			if out.Code != http.StatusBadRequest || commerceRepairFacts(t, x, true) != before {
				t.Fatal("invalid queue query reached mutation", path, out.Code)
			}
		}
	})
	for _, who := range []string{"proposer", "reviewer"} {
		t.Run(who+"-expiry-during-mapping-wait", func(t *testing.T) {
			x := newCommerceRepairFixture(t, f, true)
			principal := x.proposer
			if who == "reviewer" {
				principal = x.reviewer
			}
			var user uuid.UUID
			if err := x.r.pool.QueryRow(x.ctx, `SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2`, principal.Issuer, principal.Subject).Scan(&user); err != nil {
				t.Fatal(err)
			}
			boundary := wallClockRoleExpiry(t, x.ctx, x.r, user)
			_, o := x.observe(t, "SUCCEEDED")
			before := commerceRepairFacts(t, x, true)
			err := wallClockAcrossLock(t, x.ctx, x.r, x.r.pool, `SELECT id FROM commerce_customer_mappings WHERE id=$1 FOR UPDATE`, x.j.MappingID, boundary, func() error {
				_, e := x.r.ConfirmCommerceRepair(x.ctx, x.reviewer, x.c.ID, CommerceRepairConfirmation{ExpectedVersion: o.CaseVersion, ObservationID: o.ID})
				return e
			})
			if !errors.Is(err, ErrForbidden) || commerceRepairFacts(t, x, true) != before {
				t.Fatal("expired waiting authority completed case", who, err)
			}
			if _, err = x.r.pool.Exec(x.ctx, `UPDATE role_assignments SET expires_at=NULL WHERE user_id=$1`, user); err != nil {
				t.Fatal(err)
			}
			_, fresh := x.observe(t, "SUCCEEDED")
			x.confirm(t, fresh)
		})
	}
	for _, who := range []string{"proposer", "reviewer"} {
		t.Run(who+"-expiry-during-sla-wait", func(t *testing.T) {
			x := newCommerceRepairFixture(t, f, true)
			principal := x.proposer
			if who == "reviewer" {
				principal = x.reviewer
			}
			var user uuid.UUID
			if err := x.r.pool.QueryRow(x.ctx, `SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2`, principal.Issuer, principal.Subject).Scan(&user); err != nil {
				t.Fatal(err)
			}
			boundary := wallClockRoleExpiry(t, x.ctx, x.r, user)
			_, o := x.observe(t, "SUCCEEDED")
			before := commerceRepairFacts(t, x, true)
			err := wallClockAcrossLock(t, x.ctx, x.r, x.r.pool, `SELECT id FROM manual_review_cases WHERE id=$1 FOR UPDATE`, x.c.ID, boundary, func() error {
				_, e := x.r.ConfirmCommerceRepair(x.ctx, x.reviewer, x.c.ID, CommerceRepairConfirmation{ExpectedVersion: o.CaseVersion, ObservationID: o.ID})
				return e
			})
			if !errors.Is(err, ErrForbidden) || commerceRepairFacts(t, x, true) != before {
				t.Fatal("expired authority after initial admission completed case", who, err)
			}
			if _, err = x.r.pool.Exec(x.ctx, `UPDATE role_assignments SET expires_at=NULL WHERE user_id=$1`, user); err != nil {
				t.Fatal(err)
			}
			_, fresh := x.observe(t, "SUCCEEDED")
			x.confirm(t, fresh)
		})
	}
	t.Run("concurrent-reviewers-write-one-receipt", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		_, o := x.observe(t, "SUCCEEDED")
		other := seedEntitlementApprovalActor(t, x.ctx, x.r.pool, "PLATFORM_COMMERCE_REVIEWER", "other-reviewer")
		blocker, err := x.r.pool.Begin(x.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(context.Background())
		if _, err = blocker.Exec(x.ctx, `SELECT id FROM commerce_customer_mappings WHERE id=$1 FOR UPDATE`, x.j.MappingID); err != nil {
			t.Fatal(err)
		}
		var pid int32
		if err = blocker.QueryRow(x.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 2)
		for _, reviewer := range []identity.Principal{x.reviewer, entitlementApprovalPrincipal(other)} {
			go func(p identity.Principal) {
				_, e := x.r.ConfirmCommerceRepair(x.ctx, p, x.c.ID, CommerceRepairConfirmation{ExpectedVersion: o.CaseVersion, ObservationID: o.ID})
				done <- e
			}(reviewer)
		}
		waitCommerceRepairBlocker(t, x, pid, 2, done)
		if err = blocker.Rollback(x.ctx); err != nil {
			t.Fatal(err)
		}
		success, conflict := 0, 0
		for range 2 {
			select {
			case e := <-done:
				if e == nil {
					success++
				} else if errors.Is(e, ErrConflict) {
					conflict++
				} else {
					t.Fatal(e)
				}
			case <-x.ctx.Done():
				t.Fatal(x.ctx.Err())
			}
		}
		if success != 1 || conflict != 1 {
			t.Fatal("competing reviewers not serialized", success, conflict)
		}
		var receipts, events, audits, successes int
		if err = x.r.pool.QueryRow(x.ctx, `SELECT
(SELECT count(*) FROM commerce_customer_repair_cases WHERE id=$1 AND status='RESOLVED'),
(SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type='COMMERCE_COMPLETION_VERIFIED'),
(SELECT count(*) FROM audit_logs WHERE resource_id=$1::text AND action='commerce.repair.confirm'),
(SELECT count(*) FROM commerce_customer_sync_evidence WHERE job_id=$2 AND outcome='SUCCEEDED')`, x.c.ID, x.j.ID).Scan(&receipts, &events, &audits, &successes); err != nil || receipts != 1 || events != 1 || audits != 1 || successes != 1 {
			t.Fatal("duplicate completion effects", receipts, events, audits, successes, err)
		}
	})
	t.Run("reviewer-revocation-during-role-wait", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		_, o := x.observe(t, "SUCCEEDED")
		before := commerceRepairFacts(t, x, true)
		gate, err := x.r.pool.Begin(x.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer gate.Rollback(context.Background())
		var binding uuid.UUID
		if err = gate.QueryRow(x.ctx, `SELECT ra.id FROM role_assignments ra JOIN identities i ON i.user_id=ra.user_id WHERE i.issuer=$1 AND i.subject=$2 AND ra.role_code='PLATFORM_COMMERCE_REVIEWER' FOR UPDATE OF ra`, x.reviewer.Issuer, x.reviewer.Subject).Scan(&binding); err != nil {
			t.Fatal(err)
		}
		var pid int32
		if err = gate.QueryRow(x.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, e := x.r.ConfirmCommerceRepair(x.ctx, x.reviewer, x.c.ID, CommerceRepairConfirmation{ExpectedVersion: o.CaseVersion, ObservationID: o.ID})
			done <- e
		}()
		waitCommerceRepairBlocker(t, x, pid, 1, done)
		if _, err = gate.Exec(x.ctx, `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE id=$1`, binding); err != nil {
			t.Fatal(err)
		}
		if err = gate.Commit(x.ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-done:
		case <-x.ctx.Done():
			t.Fatal(x.ctx.Err())
		}
		if !errors.Is(err, ErrForbidden) || commerceRepairFacts(t, x, true) != before {
			t.Fatal("revoked waiting reviewer completed case", err)
		}
		fresh := seedEntitlementApprovalActor(t, x.ctx, x.r.pool, "PLATFORM_COMMERCE_REVIEWER", "replacement-reviewer")
		if _, err = x.r.ConfirmCommerceRepair(x.ctx, entitlementApprovalPrincipal(fresh), x.c.ID, CommerceRepairConfirmation{ExpectedVersion: o.CaseVersion, ObservationID: o.ID}); err != nil {
			t.Fatal("subsequent authorized review blocked", err)
		}
	})
	t.Run("audit-cancellation-rolls-back-completion", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		_, o := x.observe(t, "SUCCEEDED")
		before := commerceRepairFacts(t, x, true)
		gate, err := x.r.pool.Begin(x.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer gate.Rollback(context.Background())
		if _, err = gate.Exec(x.ctx, `LOCK TABLE audit_logs IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		var pid int32
		if err = gate.QueryRow(x.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(x.ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, e := x.r.ConfirmCommerceRepair(ctx, x.reviewer, x.c.ID, CommerceRepairConfirmation{ExpectedVersion: o.CaseVersion, ObservationID: o.ID})
			done <- e
		}()
		waitCommerceRepairBlocker(t, x, pid, 1, done)
		cancel()
		select {
		case err = <-done:
		case <-x.ctx.Done():
			t.Fatal(x.ctx.Err())
		}
		if err == nil || errors.Is(err, ErrConflict) || errors.Is(err, ErrForbidden) {
			t.Fatal("audit infrastructure failure became completion/business denial", err)
		}
		if err = gate.Rollback(x.ctx); err != nil {
			t.Fatal(err)
		}
		if commerceRepairFacts(t, x, true) != before {
			t.Fatal("audit failure committed partial repair")
		}
		x.confirm(t, o)
	})
	t.Run("callback-evidence-invalidates-old-proposal", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		_, old := x.observe(t, "SUCCEEDED")
		if err := x.r.CompleteCommerceCustomerSyncJob(x.ctx, x.j, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***late"}); !errors.Is(err, ErrConflict) {
			t.Fatal("callback acquired completion authority", err)
		}
		detail, err := x.r.CommerceRepairDetail(x.ctx, x.proposer, x.c.ID, 0, 0)
		if err != nil || len(detail.Callbacks) != 1 || detail.Callbacks[0].Kind != "SUCCEEDED" || detail.Callbacks[0].Attempts != x.j.Attempts || detail.Callbacks[0].ExternalReferenceRedacted != "***late" || detail.Callbacks[0].AfterCompletion || detail.Case.Version != old.CaseVersion+1 {
			t.Fatal("original callback evidence not exposed", detail, err)
		}
		before := commerceRepairFacts(t, x, true)
		if err = x.r.CompleteCommerceCustomerSyncJob(x.ctx, x.j, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***late"}); !errors.Is(err, ErrConflict) || commerceRepairFacts(t, x, true) != before {
			t.Fatal("callback duplicate changed evidence/revision", err)
		}
		if _, err = x.r.ConfirmCommerceRepair(x.ctx, x.reviewer, x.c.ID, CommerceRepairConfirmation{ExpectedVersion: old.CaseVersion, ObservationID: old.ID}); !errors.Is(err, ErrConflict) {
			t.Fatal("unseen callback did not invalidate old proposal", err)
		}
		x.c.Version = detail.Case.Version
		_, fresh := x.observe(t, "SUCCEEDED")
		receipt := x.confirm(t, fresh)
		core := commerceRepairFacts(t, x, false)
		earlier := x.j
		earlier.Attempts = 1
		if err = x.r.RetryCommerceCustomerSyncJob(x.ctx, earlier, "not retained raw provider text", time.Now().Add(time.Hour)); !errors.Is(err, ErrConflict) {
			t.Fatal("old attempt gained retry authority", err)
		}
		late, err := x.r.CommerceRepairDetail(x.ctx, x.proposer, x.c.ID, 0, detail.Callbacks[0].Sequence)
		if err != nil || len(late.Callbacks) != 1 || late.Callbacks[0].Kind != "PROVIDER_ERROR" || late.Callbacks[0].Attempts != 1 || !late.Callbacks[0].AfterCompletion || late.Case.Version != receipt.Version || commerceRepairFacts(t, x, false) != core {
			t.Fatal("late callback lost evidence or rewrote completion", err)
		}
		replayed, err := x.r.ConfirmCommerceRepair(x.ctx, x.reviewer, x.c.ID, CommerceRepairConfirmation{ExpectedVersion: fresh.CaseVersion, ObservationID: fresh.ID})
		if err != nil || replayed.Version != receipt.Version || replayed.Resolution == nil || *replayed.Resolution != *receipt.Resolution {
			t.Fatal("late evidence changed immutable receipt", err)
		}
	})
	t.Run("retired-active-lifecycle-keeps-business-facts", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, true)
		_, o := x.observe(t, "SUCCEEDED")
		if _, err := x.r.pool.Exec(x.ctx, `UPDATE users SET subject_lifecycle_version=subject_lifecycle_version+1 WHERE id=$1`, x.j.SubjectID); err != nil {
			t.Fatal(err)
		}
		binding := func() string {
			var s string
			if err := x.r.pool.QueryRow(x.ctx, `SELECT jsonb_build_object('user',(SELECT to_jsonb(u) FROM users u WHERE id=$1),'job',(SELECT to_jsonb(j) FROM commerce_customer_sync_jobs j WHERE id=$2),'mapping',(SELECT to_jsonb(m) FROM commerce_customer_mappings m WHERE id=$3))::text`, x.j.SubjectID, x.j.ID, x.j.MappingID).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}
		core := binding()
		receipt := x.confirm(t, o)
		if receipt.Resolution == nil || *receipt.Resolution != "SUCCEEDED_EVIDENCE_ONLY" || binding() != core {
			t.Fatal("retired active lifecycle accepted original success")
		}
	})
	t.Run("startup-wait-rechecks-committed-quarantine", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, false)
		if e := x.r.RetryCommerceCustomerSyncJob(x.ctx, x.j, "COMMERCE_PROVIDER_UNAVAILABLE", time.Now().Add(-time.Hour)); e != nil {
			t.Fatal(e)
		}
		if e := x.r.ConfigureCommerceProvider(x.j.ProviderID, "v2"); e != nil {
			t.Fatal(e)
		}
		gate, e := x.r.pool.Begin(x.ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer gate.Rollback(x.ctx)
		var pid int32
		if e = gate.QueryRow(x.ctx, `SELECT pg_backend_pid()`).Scan(&pid); e != nil {
			t.Fatal(e)
		}
		if _, e = gate.Exec(x.ctx, `SELECT id FROM commerce_customer_mappings WHERE id=$1 FOR UPDATE`, x.j.MappingID); e != nil {
			t.Fatal(e)
		}
		manual := make(chan error, 1)
		go func() { _, e := x.r.QuarantineCommerceRepair(x.ctx, x.proposer, x.j.ID); manual <- e }()
		waitCommerceRepairBlocker(t, x, pid, 1, manual)
		startup := make(chan error, 1)
		go func() { startup <- x.r.EnsureCommerceProviderMappings(x.ctx) }()
		waitCommerceRepairBlocker(t, x, pid, 2, startup)
		if e = gate.Commit(x.ctx); e != nil {
			t.Fatal(e)
		}
		if e = <-manual; e != nil {
			t.Fatal(e)
		}
		if e = <-startup; e != nil {
			t.Fatal(e)
		}
		var jobs, cases int
		if e = x.r.pool.QueryRow(x.ctx, `SELECT (SELECT count(*) FROM commerce_customer_sync_jobs WHERE mapping_id=$1),(SELECT count(*) FROM commerce_customer_repair_cases WHERE mapping_id=$1)`, x.j.MappingID).Scan(&jobs, &cases); e != nil {
			t.Fatal(e)
		}
		next, e := x.r.ClaimCommerceCustomerSyncJob(x.ctx, x.j.ProviderID, "v2", time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		if jobs != 1 || cases != 1 || next != nil {
			t.Fatalf("startup bypassed committed quarantine jobs=%d cases=%d next=%v", jobs, cases, next)
		}
		if e = x.r.EnsureCommerceProviderMappings(x.ctx); e != nil {
			t.Fatal(e)
		}
		var protected bool
		if e = x.r.pool.QueryRow(x.ctx, `SELECT adapter_version='v1' AND status='REPAIR_REQUIRED' AND version=$2 FROM commerce_customer_mappings WHERE id=$1`, x.j.MappingID, x.j.MappingVersion).Scan(&protected); e != nil || !protected {
			t.Fatal("startup changed held mapping", e)
		}

	})

	t.Run("busy-discovery-preserves-sla-and-next-candidate", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, false)
		other := seedEntitlementApprovalActor(t, x.ctx, x.r.pool, "PERSONAL_USER", "other-commerce-subject")
		tx, err := x.r.pool.Begin(x.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(x.ctx)
		if err = enqueueCommerceCustomerSync(x.ctx, tx, "USER", other.ID, "+8613800005678", x.j.ProviderID, "v1", "PROFILE_SYNC", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(x.ctx); err != nil {
			t.Fatal(err)
		}
		second, err := x.r.ClaimCommerceCustomerSyncJob(x.ctx, x.j.ProviderID, "v1", time.Minute)
		if err != nil || second == nil || second.SubjectID != other.ID {
			t.Fatal("second original dispatch", second, err)
		}
		// Two genuinely claimed original attempts cross a controlled lease boundary.
		if _, err = x.r.pool.Exec(x.ctx, `UPDATE commerce_customer_sync_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=ANY($1::uuid[])`, []uuid.UUID{x.j.ID, second.ID}); err != nil {
			t.Fatal(err)
		}
		overdue := uuid.New()
		// Controlled pre-existing SLA record, independent of the busy sync task.
		if _, err = x.r.pool.Exec(x.ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at)
VALUES($1,'ACCOUNT','PROFILE_CHANGE_REPAIR','PROFILE_CHANGE_REQUEST',$2,'P2','OPEN',now()-interval '2 days',now()-interval '1 day',now()-interval '1 day',now()-interval '1 day',now()-interval '1 day',now())`, overdue, uuid.New()); err != nil {
			t.Fatal(err)
		}
		gate, err := x.r.pool.Begin(x.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer gate.Rollback(context.Background())
		if _, err = gate.Exec(x.ctx, `SELECT id FROM commerce_customer_mappings WHERE id=$1 FOR UPDATE`, x.j.MappingID); err != nil {
			t.Fatal(err)
		}
		before := commerceRepairFacts(t, x, false)
		schedule, err := manualreview.ParseOnCallSchedule(other.ID.String(), true)
		if err != nil {
			t.Fatal(err)
		}
		worker := &ManualReviewSLAWorker{repository: x.r, schedule: schedule, interval: time.Minute}
		// The gate remains held throughout runOne; this deadline is only a failure bound.
		ctx, cancel := context.WithTimeout(x.ctx, 10*time.Second)
		defer cancel()
		worker.runOne(ctx)
		if ctx.Err() != nil {
			t.Fatal("busy Commerce discovery blocked the shared SLA worker", ctx.Err())
		}
		var level, ready, busyCases int
		if err = x.r.pool.QueryRow(x.ctx, `SELECT (SELECT escalation_level FROM manual_review_cases WHERE id=$1),(SELECT count(*) FROM commerce_customer_repair_cases WHERE job_id=$2),(SELECT count(*) FROM commerce_customer_repair_cases WHERE job_id=$3)`, overdue, second.ID, x.j.ID).Scan(&level, &ready, &busyCases); err != nil || level != 1 || ready != 1 || busyCases != 0 || commerceRepairFacts(t, x, false) != before {
			t.Fatal("busy candidate prevented SLA/next candidate or changed original facts", level, ready, busyCases, err)
		}
		if err = gate.Commit(x.ctx); err != nil {
			t.Fatal(err)
		}
		worker.runOne(x.ctx)
		if err = x.r.pool.QueryRow(x.ctx, `SELECT count(*) FROM commerce_customer_repair_cases WHERE job_id=ANY($1::uuid[])`, []uuid.UUID{x.j.ID, second.ID}).Scan(&ready); err != nil || ready != 2 {
			t.Fatal("later tick did not recover released original task", ready, err)
		}
	})
	t.Run("closed-job-can-open-evidence-only-case", func(t *testing.T) {
		x := newCommerceRepairFixture(t, f, false)
		if _, err := x.r.pool.Exec(x.ctx, `UPDATE users SET status='DELETED',anonymized_at=clock_timestamp(),mobile_e164=NULL,mobile_verified_at=NULL,subject_lifecycle_version=subject_lifecycle_version+1 WHERE id=$1`, x.j.SubjectID); err != nil {
			t.Fatal(err)
		}
		if _, err := x.r.pool.Exec(x.ctx, `UPDATE commerce_customer_mappings SET status='DETACHED',external_reference_redacted=NULL WHERE id=$1`, x.j.MappingID); err != nil {
			t.Fatal(err)
		}
		if _, err := x.r.pool.Exec(x.ctx, `UPDATE commerce_customer_sync_jobs SET status='CANCELLED_ACCOUNT_CLOSURE',contact_mobile_e164=NULL,lease_until=NULL WHERE id=$1`, x.j.ID); err != nil {
			t.Fatal(err)
		}
		var before, after string
		q := `SELECT jsonb_build_object('job',(SELECT to_jsonb(j)-'commerce_repair_case_id' FROM commerce_customer_sync_jobs j WHERE id=$1),'mapping',(SELECT to_jsonb(m) FROM commerce_customer_mappings m WHERE id=$2))::text`
		if err := x.r.pool.QueryRow(x.ctx, q, x.j.ID, x.j.MappingID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		c, err := x.r.QuarantineCommerceRepair(x.ctx, x.proposer, x.j.ID)
		if err != nil {
			t.Fatal(err)
		}
		x.c = c
		if err = x.r.CompleteCommerceCustomerSyncJob(x.ctx, x.j, commerce.CustomerProvisionResult{ExternalReferenceRedacted: "***private"}); !errors.Is(err, ErrConflict) {
			t.Fatal("closed callback gained business authority", err)
		}
		detail, err := x.r.CommerceRepairDetail(x.ctx, x.proposer, c.ID, 0, 0)
		if err != nil || len(detail.Callbacks) != 1 || detail.Callbacks[0].ExternalReferenceRedacted != "" {
			t.Fatal("closed callback recreated a provider reference", err)
		}
		x.c.Version = detail.Case.Version
		_, o := x.observe(t, "SUCCEEDED", "")
		receipt := x.confirm(t, o)
		if err = x.r.pool.QueryRow(x.ctx, q, x.j.ID, x.j.MappingID).Scan(&after); err != nil || before != after || receipt.Resolution == nil || *receipt.Resolution != "SUCCEEDED_EVIDENCE_ONLY" {
			t.Fatal("closed case changed original job/mapping facts", err)
		}
	})
}
