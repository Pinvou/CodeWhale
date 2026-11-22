package account

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestRestoredAdminReadsWithPublicMigrationsAndRuntimeActor(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := context.Background()
	before, err := r.AdminOverview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	users := make([]uuid.UUID, 3)
	principals := make([]identity.Principal, 3)
	for index, status := range []string{"ACTIVE", "DISABLED", "DELETED"} {
		p := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "u" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
		me, _, err := r.Bootstrap(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		users[index] = me.UserID
		principals[index] = p
		if _, err := r.pool.Exec(ctx, `UPDATE users SET status=$2 WHERE id=$1`, me.UserID, status); err != nil {
			t.Fatal(err)
		}
	}
	organizations := make([]uuid.UUID, 3)
	for index, status := range []string{"ACTIVE", "DISABLED", "DELETED"} {
		created, err := r.AdminCreateOrganization(ctx, principals[0], users[0], "Controlled "+uuid.NewString(), uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		id := created.ID
		organizations[index] = id
		if status != "ACTIVE" {
			if err := r.AdminSetOrganizationStatus(ctx, principals[0], id, 1, "DISABLED"); err != nil {
				t.Fatal(err)
			}
		}
		if status == "DELETED" {
			if err := r.AdminDeleteOrganization(ctx, principals[0], id, 2, "Controlled read fixture"); err != nil {
				t.Fatal(err)
			}
		}
		item, err := r.AdminOrganizationDetail(ctx, id)
		if err != nil || item.ID != id || item.Status != status || item.Version != int64(index+1) {
			t.Fatal("organization detail projection mismatch")
		}
	}
	for index, status := range []string{"SUSPENDED"} {
		if _, err := r.pool.Exec(ctx, `INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($1,$2,$3,$4,now(),now())`, uuid.New(), organizations[0], users[index+1], status); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("public_writer_readonly_projections", func(t *testing.T) {
		after, err := r.AdminOverview(ctx)
		if err != nil || after.Users != before.Users+2 || after.ActiveUsers != before.ActiveUsers+1 || after.DisabledUsers != before.DisabledUsers+1 || after.Organizations != before.Organizations+2 || after.ActiveMemberships != before.ActiveMemberships+1 {
			t.Fatalf("overview projection mismatch: %v", err)
		}
		item, err := r.AdminUserDetail(ctx, users[0])
		if err != nil || item.ID != users[0] || item.Status != "ACTIVE" || item.UserCode == "" || item.Email != nil {
			t.Fatal("user detail projection mismatch")
		}
		for _, id := range []uuid.UUID{users[2], uuid.New()} {
			if _, err := r.AdminUserDetail(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Fatal("deleted or missing user was exposed")
			}
		}
		if _, err := r.AdminOrganizationDetail(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatal("missing organization not distinguished")
		}
		var audits, events int64
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_logs),(SELECT count(*) FROM outbox_events)`).Scan(&audits, &events); err != nil {
			t.Fatal(err)
		}
		if _, err := r.AdminOverview(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := r.AdminUserDetail(ctx, users[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := r.AdminOrganizationDetail(ctx, organizations[0]); err != nil {
			t.Fatal(err)
		}
		var auditsAfter, eventsAfter int64
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_logs),(SELECT count(*) FROM outbox_events)`).Scan(&auditsAfter, &eventsAfter); err != nil || auditsAfter != audits || eventsAfter != events {
			t.Fatal("read introduced a write")
		}
	})
}
