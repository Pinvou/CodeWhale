package billingschema

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"os"
	"testing"
	"time"
)

func TestAdminAddReplayReadsCurrentMembership(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit isolated PostgreSQL configuration is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	m, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = migrations.Up(ctx, c, selectVersions(m, []int64{1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 16, 17})); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Exec(ctx, stageAccountSeed); err != nil {
		t.Fatal(err)
	}
	if err = accountschema.Up(ctx, c, m); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Exec(ctx, "SELECT pg_advisory_unlock_all()"); err != nil {
		t.Fatal(err)
	}
	r, err := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	owner := identity.Principal{Issuer: "https://owned.invalid", Subject: "owned-subject", SessionID: "owned-session"}
	org, err := r.AdminCreateOrganization(ctx, owner, uuid.MustParse("11111111-1111-4111-8111-111111111111"), "expiry owned org", "role-expiry-organization")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := func(label string) (identity.Principal, uuid.UUID) {
		t.Helper()
		p := identity.Principal{Issuer: "https://owned.invalid", Subject: "expiry-" + label, SessionID: "expiry-session-" + label, Username: "expiry_" + label}
		me, _, e := r.Bootstrap(ctx, p)
		if e != nil {
			t.Fatal(e)
		}
		jobs, e := r.LeaseWalletProvisioningJobs(ctx, 100, time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, j := range jobs {
			if j.UserID == me.UserID {
				if e = r.CompleteWalletProvisioning(ctx, j); e != nil {
					t.Fatal(e)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("actual wallet job absent")
		}
		return p, me.UserID
	}
	_, targetID := bootstrap("target")

	unit, err := r.AdminCreateUnit(ctx, owner, org.ID, nil, "original ordinary unit", "original-add-unit-key")
	if err != nil {
		t.Fatal(err)
	}
	originalKey := "original-add-member-key"
	units := []uuid.UUID{unit.Unit.ID}
	first, err := r.AdminAddMemberWithUnits(ctx, owner, org.ID, targetID, []string{"MEMBER"}, nil, units, originalKey)
	if err != nil || first.Member.Status != "ACTIVE" {
		t.Fatal("legitimate original ADD failed", first, err)
	}
	receipt := func() string {
		return stageValue(t, ctx, c, `SELECT row_to_json(x)::text FROM membership_commands x WHERE organization_id='`+org.ID.String()+`' AND command_type='ADD' AND idempotency_key='`+originalKey+`'`)
	}
	originalReceipt := receipt()
	removed, err := r.AdminRemoveMember(ctx, owner, org.ID, targetID, "original-remove-member-key")
	if err != nil || removed.Member.Status != "REVOKED" {
		t.Fatal("legitimate remove failed", removed, err)
	}
	snapshot := func() string {
		return stageValue(t, ctx, c, `SELECT jsonb_build_object('memberships',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM memberships x),'users',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM users x),'units',(SELECT jsonb_agg(to_jsonb(x) ORDER BY membership_id,unit_id) FROM membership_units x),'roles',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_bindings x),'assignments',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_assignments x),'commands',(SELECT jsonb_agg(to_jsonb(x) ORDER BY actor_user_id,organization_id,command_type,idempotency_key) FROM membership_commands x),'audit',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM audit_logs x),'outbox',(SELECT jsonb_agg(to_jsonb(x) ORDER BY event_id) FROM outbox_events x))::text`)
	}
	checkReplay := func(t *testing.T, want account.MembershipMutationResult) {
		t.Helper()
		old := snapshot()
		got, e := r.AdminAddMemberWithUnits(ctx, owner, org.ID, targetID, []string{"MEMBER"}, nil, units, originalKey)
		if e != nil || !got.Replayed || got.Member.Status != want.Member.Status || got.Member.Version != want.Member.Version || got.Member.MembershipID != want.Member.MembershipID || got.Member.UserID != targetID || got.OrganizationID != org.ID {
			t.Errorf("original ADD replay returned historical status/version: got=%+v want=%+v err=%v", got, want, e)
		}
		if snapshot() != old || receipt() != originalReceipt {
			t.Fatal("ADD replay mutated receipt or persistent facts")
		}
		if _, e = r.AdminAddMemberWithUnits(ctx, owner, org.ID, targetID, []string{"AUDITOR"}, nil, units, originalKey); !errors.Is(e, account.ErrConflict) || snapshot() != old {
			t.Fatal("original key different facts accepted", e)
		}
	}
	t.Run("add-remove-old-add-replay-current-revoked", func(t *testing.T) { checkReplay(t, removed) })
	readded, err := r.AdminAddMemberWithUnits(ctx, owner, org.ID, targetID, []string{"MEMBER"}, nil, units, "second-add-operation-key")
	if err != nil || readded.Member.Status != "ACTIVE" || readded.Member.Version <= removed.Member.Version {
		t.Fatal("legitimate readd failed", readded, err)
	}
	t.Run("readd-old-add-replay-current-higher-version", func(t *testing.T) { checkReplay(t, readded) })
}
