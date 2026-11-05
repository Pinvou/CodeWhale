package billingschema

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

// The list reads real final-schema DRAFT rows and their snapshots. It does
// not manufacture an approval, decision, execution proof or broker receipt.
func TestApprovalV2ListPreservesActualDraftVersion(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit owned PostgreSQL fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	all, err := migrations.Load("../../migrations/account")
	if err != nil || accountschema.Up(ctx, c, all) != nil {
		t.Fatal("actual Account schema unavailable")
	}
	migrations.Unlock(ctx, c)
	seed := strings.Split(stageAccountSeed, "UPDATE authorization_versions")[0]
	seed = strings.ReplaceAll(seed, "PLATFORM_SUPER_ADMIN", "PLATFORM_BILLING_OPERATOR")
	if _, err = c.Exec(ctx, seed+`INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('https://owned.invalid','list-reader','11111111-1111-4111-8111-111111111111',now(),now())`); err != nil {
		t.Fatal("legitimate actor fixture rejected")
	}
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	versions := []int64{7, 12}
	for i, id := range ids {
		if _, err = c.Exec(ctx, `INSERT INTO approval_requests(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,redacted_evidence,expected_impact,status,version,expires_at,created_at,updated_at)
SELECT $1,'WALLET_ADJUSTMENT',1,ra.user_id,ra.id,1,$2,'{}',decode(repeat('00',32),'hex'),'WALLET',$3,'controlled draft list fixture','[]','{}','DRAFT',$4,now()+interval '1 hour',now(),now()+($5::integer*interval '1 second') FROM role_assignments ra WHERE ra.user_id='11111111-1111-4111-8111-111111111111' LIMIT 1`, id, "list-version-key-"+id.String(), uuid.NewString(), versions[i], i-1); err != nil {
			t.Fatal("valid DRAFT row rejected")
		}
		if _, err = c.Exec(ctx, `INSERT INTO approval_request_snapshots(approval_request_id,snapshot_schema_version,critical_resource_versions,financial_snapshot,policy_snapshot,requester_authorization_snapshot,created_at) VALUES($1,1,'{}','{}','{}','{}',now())`, id); err != nil {
			t.Fatal("valid DRAFT snapshot rejected")
		}
	}
	r, err := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal("actual repository unavailable")
	}
	t.Cleanup(r.Close)
	if err = r.ConfigureApprovalPagination(ctx, account.ApprovalPaginationConfig{Enabled: true, CurrentKeyID: "owned-list", CurrentKey: []byte(strings.Repeat("k", 32))}); err != nil || r.SetApprovalPaginationGate(ctx, true) != nil {
		t.Fatal("real pagination configuration rejected")
	}
	principal := identity.Principal{Issuer: "https://owned.invalid", Subject: "list-reader", SessionID: "owned-list-session", AuthenticatedAt: time.Now().UTC()}
	filter := account.ApprovalFilter{CommandType: "WALLET_ADJUSTMENT", Status: "DRAFT"}
	page, err := r.ListApprovalRequestsV2(ctx, principal, filter, "", 1, "127.0.0.1")
	if err != nil || len(page.Items) != 1 || page.Items[0].ApprovalRequestID != ids[1] || page.Items[0].Version != versions[1] || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("actual first page version/order/cursor lost: page=%+v err=%v", page, err)
	}
	wire, err := json.Marshal(page)
	if err != nil || !strings.Contains(string(wire), `"version":12`) {
		t.Fatal("actual summary wire version lost")
	}
	second, err := r.ListApprovalRequestsV2(ctx, principal, filter, *page.NextCursor, 1, "127.0.0.1")
	if err != nil || len(second.Items) != 1 || second.Items[0].ApprovalRequestID != ids[0] || second.Items[0].Version != versions[0] || second.HasMore {
		t.Fatalf("actual continuation version/order lost: page=%+v err=%v", second, err)
	}
	other := principal
	other.Subject = "unrecognized-reader"
	if _, err = r.ListApprovalRequestsV2(ctx, other, filter, "", 1, "127.0.0.1"); !errors.Is(err, account.ErrForbidden) {
		t.Fatal("foreign identity read accepted")
	}
}
