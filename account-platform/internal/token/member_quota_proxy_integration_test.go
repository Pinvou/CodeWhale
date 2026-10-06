package token

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

type quotaRouteContext struct {
	value AuthorizationContext
	unit  uuid.UUID
}

func (c quotaRouteContext) Resolve(context.Context, string) (AuthorizationContext, error) {
	return c.value, nil
}
func (c quotaRouteContext) UnitActive(_ context.Context, _ string, org, unit uuid.UUID) (bool, error) {
	return c.value.OrganizationID != nil && *c.value.OrganizationID == org && unit == c.unit, nil
}

func TestPostgresMemberQuotaProxyPreservesUnitWalletAndRejectsBypass(t *testing.T) {
	for _, mode := range []string{"unit", "organization-with-grant", "organization-without-grant", "unit-wallet-mismatch", "missing-local-member-version"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"content":"owned response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer upstream.Close()
			key, err := NewLocalAESProvider("owned-quota-route", bytes.Repeat([]byte{7}, 32))
			if err != nil {
				t.Fatal(err)
			}
			tokens := &PostgresRepository{pool: testdb.Open(t, "token"), secrets: key, now: func() time.Time { return time.Now().UTC() }}
			pool := testdb.Open(t, "billing")
			billing, err := billingdomain.NewPostgresRepository(ctx, pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			defer billing.Close()
			user, org, unit := uuid.New(), uuid.New(), uuid.New()
			walletType, ownerType, ownerID, share := "ORG_UNIT", "ORG_UNIT", unit, "OWNER_UNIT_ONLY"
			var ownerUnit *uuid.UUID = &unit
			if mode == "organization-without-grant" || mode == "organization-with-grant" {
				walletType, ownerType, ownerID, share, ownerUnit = "ORGANIZATION", "ORG", org, "ORG_ALL", nil
			}
			wallet, _, err := billing.EnsureWallet(ctx, walletType, ownerID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := billing.Credit(ctx, wallet.ID, "owned-unit-credit", 1000000, false); err != nil {
				t.Fatal(err)
			}
			publishedBillingPriceFixture(t, billing, "owned-model", 1_000_000, 1_000_000, 1_000_000)
			if mode == "organization-without-grant" || mode == "organization-with-grant" {
				if _, err := billing.ApplyOrganizationMemberLifecycle(ctx, org, user, "ACTIVE", 3); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "organization-with-grant" {
				allocation := billingdomain.MemberQuotaAllocation{OrganizationID: org, MemberUserID: user, ComponentType: "BASE", AmountMicrocredits: 1000, MembershipVersion: 3, CreatedBy: user, IdempotencyKey: uuid.NewString()}
				if err := pool.QueryRow(ctx, `SELECT c.id,c.package_id FROM entitlement_components c JOIN entitlement_packages p ON p.id=c.package_id WHERE p.wallet_id=$1`, wallet.ID).Scan(&allocation.ComponentID, &allocation.PackageID); err != nil {
					t.Fatal(err)
				}
				if _, _, err := billing.AllocateMemberQuota(ctx, allocation); err != nil {
					t.Fatal(err)
				}
			}
			accountWallet := wallet.ID
			if mode == "unit-wallet-mismatch" {
				foreign, _, err := billing.EnsureWallet(ctx, "ORG_UNIT", uuid.New())
				if err != nil {
					t.Fatal(err)
				}
				accountWallet = foreign.ID
			}
			account, err := tokens.CreateAccount(ctx, CreateAccountRequest{OwnerType: ownerType, OwnerID: ownerID, OwnerOrganization: &org, OwnerUnit: ownerUnit, WalletID: accountWallet, Name: "owned-route", ShareScope: share, DefaultPriority: 100, CreatedBy: user})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tokens.RotateCredential(ctx, account.ID, user, "owned-provider", upstream.URL, []byte("owned-test-provider-key"), []string{"owned-model"}); err != nil {
				t.Fatal(err)
			}
			parsed, _ := url.Parse(upstream.URL)
			policy, err := NewProviderURLPolicy("development", []string{parsed.Hostname()})
			if err != nil {
				t.Fatal(err)
			}
			secret := strings.Repeat("q", 32)
			billingServer := httptest.NewServer(billingdomain.NewService(billing).Handler(secret))
			defer billingServer.Close()
			client, err := NewHTTPBillingClient(billingServer.URL, secret)
			if err != nil {
				t.Fatal(err)
			}
			wallets, err := NewHTTPWalletResolver(billingServer.URL, secret)
			if err != nil {
				t.Fatal(err)
			}
			resolved := AuthorizationContext{UserID: user, SessionID: "owned-session", ContextType: "ORGANIZATION", ContextVersion: 1, AuthorizationVersion: 1, OrganizationID: &org, UnitIDs: []uuid.UUID{unit}, MembershipVersion: 97, MemberVersion: 3}
			if mode == "missing-local-member-version" {
				resolved.MemberVersion = 0
			}
			service := NewService(tokens, quotaRouteContext{resolved, unit}, wallets, nil, policy, client, NewHTTPUpstreamClient(), 10)
			request := modelRequest(`{"model":"owned-model","max_tokens":1,"messages":[{"role":"user","content":"owned prompt"}]}`)
			response := httptest.NewRecorder()
			service.chatCompletions(response, request)
			var holdCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_holds`).Scan(&holdCount); err != nil {
				t.Fatal(err)
			}
			if mode == "unit" || mode == "organization-with-grant" {
				if response.Code != http.StatusOK || calls.Load() != 1 || holdCount != 1 {
					t.Fatal("existing unit wallet no longer reaches its ordinary reservation path")
				}
				var ordinary bool
				if err := pool.QueryRow(ctx, `SELECT quota_organization_id IS NULL AND quota_member_user_id IS NULL AND quota_membership_version IS NULL FROM usage_holds`).Scan(&ordinary); err != nil || ordinary != (mode == "unit") {
					t.Fatal("unit wallet was incorrectly routed through organization member grants", err)
				}
				if mode == "organization-with-grant" {
					var used, version int64
					if err := pool.QueryRow(ctx, `SELECT used_microcredits,membership_version FROM member_quota_grants WHERE organization_id=$1 AND member_user_id=$2`, org, user).Scan(&used, &version); err != nil || used != 2 || version != 3 {
						t.Fatal("quota consumption used global context membership version or wrong financial facts", err)
					}
				}
				stored, err := billing.GetWallet(ctx, wallet.ID)
				if err != nil || stored.PostedMicrocredits != 999998 || stored.ReservedMicrocredits != 0 {
					t.Fatal("ordinary unit financial settlement changed", err)
				}
			} else if response.Code < 400 || calls.Load() != 0 || holdCount != 0 {
				t.Fatal("invalid organization quota route reached a provider or created a hold")
			}
		})
	}
}
