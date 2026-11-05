package token

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Exercise the original BeginUsage -> admitUsage production path with the
// public migrations and restricted runtime. This stops before dispatch/holds;
// it is not an end-to-end organization model invocation or policy change.
func TestF06OrganizationAdmissionBoundariesPublicPostgres(t *testing.T) {
	for _, boundary := range []string{"organization-minute-600", "organization-inflight-100", "combined-user-member-inflight-5"} {
		t.Run(boundary, func(t *testing.T) {
			r, ctx := retainedManagementTokenRepository(t)
			funds, _, moneyPool := retainedManagementBillingRepository(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			r.now = func() time.Time { return now }
			org, otherOrg := uuid.New(), uuid.New()
			for _, organization := range []uuid.UUID{org, otherOrg} {
				if assessment, err := funds.ApplyOrganizationLifecycle(ctx, organization, "ACTIVE", 1, false); err != nil || !assessment.Ready {
					t.Fatal("original organization wallet lifecycle evidence missing", err)
				}
			}
			wallet, _, err := funds.EnsureWallet(ctx, "ORGANIZATION", org)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = funds.Credit(ctx, wallet.ID, "admission-credit-"+org.String(), 1000, false); err != nil {
				t.Fatal(err)
			}
			otherWallet, _, err := funds.EnsureWallet(ctx, "ORGANIZATION", otherOrg)
			if err != nil {
				t.Fatal(err)
			}
			type route struct{ account, credential, wallet uuid.UUID }
			allowedModels := []string{"controlled-candidate-" + org.String(), "controlled-foreign-" + otherOrg.String()}
			for i := 0; i < 6; i++ {
				allowedModels = append(allowedModels, fmt.Sprintf("controlled-seed-%d", i))
			}
			makeRoute := func(owner, walletID uuid.UUID) route {
				t.Helper()
				item := route{uuid.New(), uuid.New(), walletID}
				if _, err := r.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,owner_organization_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'ORG',$2,$2,$3,$4,'ACTIVE',100,$2,$5,$5,'PLATFORM')`, item.account, owner, walletID, item.account.String(), now); err != nil {
					t.Fatal(err)
				}
				cipher, err := r.secrets.Encrypt([]byte("controlled-admission-secret"), []byte(item.account.String()+":"+item.credential.String()))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = r.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at) VALUES($1,$2,'controlled','https://provider.invalid',$3,$4,$5,1,'ACTIVE',$8,$6,$7)`, item.credential, item.account, cipher, r.secrets.KeyID(), strings.Repeat("a", 64), owner, now, allowedModels); err != nil {
					t.Fatal(err)
				}
				return item
			}
			routes := []route{makeRoute(org, wallet.ID), makeRoute(org, wallet.ID), makeRoute(org, wallet.ID)}
			candidateRoute, foreignRoute := makeRoute(org, wallet.ID), makeRoute(otherOrg, otherWallet.ID)
			makeRequest := func(user, organization uuid.UUID, source route, model string) UsageRequest {
				id := uuid.New()
				return UsageRequest{ID: id, UserID: user, SessionID: "controlled-admission", OrganizationID: &organization, TokenAccountID: source.account, CredentialID: source.credential, WalletID: source.wallet, Model: model, MaximumMicrocredits: 0, IdempotencyNamespace: "model-inference", CanonicalVersion: 1, IdempotencyKeyDigest: digestString([]byte(id.String())), RequestFingerprint: strings.Repeat("b", 64)}
			}
			accept := func(request UsageRequest) UsageRequest {
				t.Helper()
				got, created, err := r.BeginUsage(ctx, request, map[string]any{"context_type": "ORGANIZATION", "organization_id": request.OrganizationID})
				if err != nil || !created || got.ID != request.ID {
					t.Fatalf("real admission failed: created=%t error=%v", created, err)
				}
				return got
			}
			seedCount := 4
			if boundary == "organization-minute-600" {
				seedCount = 599
			} else if boundary == "organization-inflight-100" {
				seedCount = 99
			}
			candidateUser := uuid.New()
			for i := 0; i < seedCount; i++ {
				user := uuid.New()
				if boundary == "combined-user-member-inflight-5" {
					user = candidateUser
				}
				request := accept(makeRequest(user, org, routes[i%len(routes)], fmt.Sprintf("controlled-seed-%d", i%6)))
				if boundary == "organization-minute-600" {
					// Real completion, not invented counter rows: no hold was created.
					if err = r.MarkReleased(ctx, request.ID, "controlled pre-dispatch admission history"); err != nil {
						t.Fatal(err)
					}
				}
			}
			candidate := makeRequest(candidateUser, org, candidateRoute, "controlled-candidate-"+org.String())
			assertCounts := func(wantOrgMinute, wantOrgInflight, wantUserInflight int) {
				t.Helper()
				var orgMinute, orgInflight, userInflight, modelMinute, modelInflight, routeMinute, routeInflight, globalMinute, globalInflight int
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE organization_id=$1 AND started_at>$5::timestamptz-interval '1 minute'),count(*) FILTER(WHERE organization_id=$1 AND execution_status=ANY($6)),count(*) FILTER(WHERE user_id=$2 AND execution_status=ANY($6)),count(*) FILTER(WHERE model=$3 AND started_at>$5::timestamptz-interval '1 minute'),count(*) FILTER(WHERE model=$3 AND execution_status=ANY($6)),count(*) FILTER(WHERE token_account_id=$4 AND started_at>$5::timestamptz-interval '1 minute'),count(*) FILTER(WHERE token_account_id=$4 AND execution_status=ANY($6)),count(*) FILTER(WHERE started_at>$5::timestamptz-interval '1 minute'),count(*) FILTER(WHERE execution_status=ANY($6)) FROM usage_requests`, org, candidateUser, candidate.Model, candidateRoute.account, now, usageInflightStates).Scan(&orgMinute, &orgInflight, &userInflight, &modelMinute, &modelInflight, &routeMinute, &routeInflight, &globalMinute, &globalInflight); err != nil {
					t.Fatal(err)
				}
				if orgMinute != wantOrgMinute || orgInflight != wantOrgInflight || userInflight != wantUserInflight || modelMinute >= 300 || modelInflight >= 20 || routeMinute >= 300 || routeInflight >= 50 || globalMinute >= 1000 || globalInflight >= 200 {
					t.Fatalf("boundary obscured by another quota: org=%d/%d user=%d model=%d/%d route=%d/%d global=%d/%d", orgMinute, orgInflight, userInflight, modelMinute, modelInflight, routeMinute, routeInflight, globalMinute, globalInflight)
				}
			}
			seedInflight := seedCount
			if boundary == "organization-minute-600" {
				seedInflight = 0
			}
			seedUser := 0
			if boundary == "combined-user-member-inflight-5" {
				seedUser = 4
			}
			assertCounts(seedCount, seedInflight, seedUser)
			accepted := accept(candidate)
			assertCounts(seedCount+1, seedInflight+1, seedUser+1)
			snapshot := func() string {
				t.Helper()
				var tokenFacts, moneyFacts string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('requests',(SELECT count(*) FROM usage_requests),'transitions',(SELECT count(*) FROM usage_execution_transitions),'attempts',(SELECT count(*) FROM usage_attempts),'dispatch',(SELECT count(*) FROM usage_dispatch_intents),'evidence',(SELECT count(*) FROM usage_convergence_evidence))::text`).Scan(&tokenFacts); err != nil {
					t.Fatal(err)
				}
				if err := moneyPool.QueryRow(ctx, `SELECT jsonb_build_object('wallets',(SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),'accounts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM ledger_accounts a),'journals',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j),'postings',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM ledger_postings p),'holds',(SELECT jsonb_agg(to_jsonb(h) ORDER BY id) FROM usage_holds h))::text`).Scan(&moneyFacts); err != nil {
					t.Fatal(err)
				}
				return tokenFacts + moneyFacts
			}
			before := snapshot()
			rejected := makeRequest(candidateUser, org, candidateRoute, candidate.Model)
			var limited *RateLimitError
			if _, created, err := r.BeginUsage(ctx, rejected, map[string]any{"context_type": "ORGANIZATION"}); !errors.As(err, &limited) || created || limited.RetryAfter < time.Second {
				t.Fatalf("next invocation crossed %s: created=%t err=%v", boundary, created, err)
			}
			if snapshot() != before {
				t.Fatal("rejected admission wrote usage, dispatch, evidence or financial facts")
			}
			// At the same threshold, the original logical invocation remains replayable.
			replayed, created, err := r.BeginUsage(ctx, candidate, map[string]any{"context_type": "ORGANIZATION", "organization_id": candidate.OrganizationID})
			if err != nil || created || replayed.ID != accepted.ID || snapshot() != before {
				t.Fatal("same-key receipt was counted as a new admission or changed facts", err)
			}
			changed := candidate
			changed.RequestFingerprint = strings.Repeat("c", 64)
			if _, _, err = r.BeginUsage(ctx, changed, nil); !errors.Is(err, ErrIdempotencyConflict) || snapshot() != before {
				t.Fatal("changed facts reused an admitted key", err)
			}
			// Fresh user/model/route keeps every other limit below its boundary.
			accept(makeRequest(uuid.New(), otherOrg, foreignRoute, "controlled-foreign-"+otherOrg.String()))
			if boundary == "combined-user-member-inflight-5" {
				// Member(org,user) is a subset of global user requests; both limits
				// are five. This verifies their existing combined boundary and does
				// not claim to isolate the member predicate from the user predicate.
				var count int
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_requests WHERE organization_id=$1 AND user_id=$2 AND execution_status=ANY($3)`, org, candidateUser, usageInflightStates).Scan(&count); err != nil || count != 5 {
					t.Fatal("combined user/member five boundary not exercised", err)
				}
			}
		})
	}
}
