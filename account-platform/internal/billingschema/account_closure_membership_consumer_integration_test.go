package billingschema

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Real public migrations, runtime roles, Account cleanup and persisted outbox
// feed the installed Billing consumer. There is no broker/provider/worker E2E
// claim; no payload field is stripped or replaced before the actual delivery.
func TestF08AccountClosureMembershipEventConsumerPublicPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit isolated schema PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	ac := f.database(t, ctx, "account", "account_service")
	bc := f.database(t, ctx, "billing", "billing_schema_owner")
	am, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = accountschema.Up(ctx, ac, am); err != nil {
		t.Fatal(err)
	}
	if _, err = migrations.VerifyApplied(ctx, ac, am); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, ac)
	bm, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err = Up(ctx, bc, bm); err != nil {
		t.Fatal(err)
	}
	if _, err = migrations.VerifyApplied(ctx, bc, bm); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, bc)
	ar, err := account.NewPostgresRepository(ctx, f.serviceURL(t, ac, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	bp, err := pgxpool.New(ctx, f.serviceURL(t, bc, "billing_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer bp.Close()
	br := billing.NewPostgresRepositoryWithPool(bp)
	user, owner, organization, membership, ownerMembership := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	for _, id := range []uuid.UUID{user, owner} {
		if _, err = ac.Exec(ctx, `WITH capability AS (SELECT set_config('pinvou.registration_bonus_capable','1',true)) INSERT INTO users(id,status,user_code,username,created_at,updated_at) SELECT $1,'ACTIVE',generate_unique_user_code(),$2,$3,$3 FROM capability`, id, "close_"+strings.ReplaceAll(id.String(), "-", "")[:16], now); err != nil {
			t.Fatal("current legitimate user fixture", err)
		}
		if _, err = ac.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('closure-membership-contract',$1,$2,$3,$3)`, id.String(), id, now); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := ac.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,'Controlled closure membership','ACTIVE',$2,$3,$3)`, organization, owner, now); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, user uuid.UUID }{{ownerMembership, owner}, {membership, user}} {
		if _, err = tx.Exec(ctx, `INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($1,$2,$3,'ACTIVE',$4,$4)`, row.id, organization, row.user, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at) VALUES($1,$2,'OWNER',$3,$4,$4)`, uuid.New(), ownerMembership, owner, now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = br.ApplyOrganizationLifecycle(ctx, organization, "ACTIVE", 1, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err = br.EnsureWallet(ctx, "ORGANIZATION", organization); err != nil {
		t.Fatal(err)
	}
	if _, err = br.ApplyOrganizationMemberLifecycle(ctx, organization, user, "ACTIVE", 1); err != nil {
		t.Fatal(err)
	}
	beforeMoney := stageWalletFacts(t, ctx, bc)
	request, err := ar.CreateAccountClosure(ctx, identity.Principal{Issuer: "closure-membership-contract", Subject: user.String(), AuthenticatedAt: now}, uuid.NewString(), "CLOSE_MY_ACCOUNT")
	if err != nil || request.Stage != "CLOSURE_REQUESTED" {
		t.Fatalf("actual closure request: %+v %v", request, err)
	}
	claimed, err := ar.ClaimAccountClosure(ctx)
	if err != nil || claimed == nil || claimed.ID != request.ID {
		t.Fatal("actual closure claim", err)
	}
	if err = ar.EnterAccountClosureQuiescing(ctx, *claimed); err != nil {
		t.Fatal("actual quiet-period entry", err)
	}
	claimed, err = ar.ClaimAccountClosure(ctx)
	if err != nil || claimed == nil || claimed.ID != request.ID {
		t.Fatal("actual post-quiet claim", err)
	}
	if err = ar.AdvanceAccountClosureToIdentityCleanup(ctx, *claimed); err != nil {
		t.Fatal(err)
	}
	claimed, err = ar.ClaimAccountClosure(ctx)
	if err != nil || claimed == nil || claimed.ID != request.ID {
		t.Fatal("actual cleanup claim", err)
	}
	if _, err = ar.PrepareAccountClosureCleanup(ctx, claimed); err != nil {
		t.Fatal("actual Account membership cleanup", err)
	}
	var event events.Event
	if err = ac.QueryRow(ctx, `SELECT event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at FROM outbox_events WHERE event_type='account.membership.changed.v1' AND aggregate_id=$1`, membership).Scan(&event.ID, &event.Type, &event.AggregateID, &event.AggregateVersion, &event.Payload, &event.TraceID, &event.OccurredAt); err != nil {
		t.Fatal("read actual durable producer envelope", err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(event.Payload, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 5 || string(fields["reason"]) != `"ACCOUNT_CLOSURE"` || string(fields["membership_status"]) != `"REVOKED"` || event.AggregateVersion != 2 {
		t.Fatalf("unexpected producer contract: %s", event.Payload)
	}
	consumer := billing.NewMemberLifecycleConsumer(br)
	if err = consumer.Handle(ctx, event); err != nil {
		t.Fatal("actual Account outbox to Billing consumer", err)
	}
	var status string
	var version int64
	var aggregate uuid.UUID
	if err = bc.QueryRow(ctx, `SELECT status,membership_version,membership_aggregate_id FROM organization_member_lifecycle_states WHERE organization_id=$1 AND user_id=$2`, organization, user).Scan(&status, &version, &aggregate); err != nil || status != "REVOKED" || version != 2 || aggregate != membership {
		t.Fatalf("projection status=%s version=%d aggregate=%s err=%v", status, version, aggregate, err)
	}
	beforeReplay := stageValue(t, ctx, bc, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(s) ORDER BY organization_id,user_id) FROM organization_member_lifecycle_states s),(SELECT jsonb_agg(to_jsonb(i) ORDER BY event_id) FROM inbox_messages i),(SELECT jsonb_agg(to_jsonb(v) ORDER BY event_type,aggregate_id) FROM inbox_aggregate_versions v))::text`)
	if err = consumer.Handle(ctx, event); err != nil {
		t.Fatal("actual event replay", err)
	}
	if beforeMoney != stageWalletFacts(t, ctx, bc) || beforeReplay != stageValue(t, ctx, bc, `SELECT jsonb_build_array((SELECT jsonb_agg(to_jsonb(s) ORDER BY organization_id,user_id) FROM organization_member_lifecycle_states s),(SELECT jsonb_agg(to_jsonb(i) ORDER BY event_id) FROM inbox_messages i),(SELECT jsonb_agg(to_jsonb(v) ORDER BY event_type,aggregate_id) FROM inbox_aggregate_versions v))::text`) {
		t.Fatal("same producer event changed money or replay effects")
	}
	var processed int
	if err = bc.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE event_id=$1 AND processed_at IS NOT NULL`, event.ID).Scan(&processed); err != nil || processed != 1 {
		t.Fatal("processed original event exactly once", err)
	}
	var producerPayload json.RawMessage
	if err = ac.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE event_id=$1`, event.ID).Scan(&producerPayload); err != nil || string(producerPayload) != string(event.Payload) {
		t.Fatal("original producer payload changed", err)
	}
}
