package account

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Original self-owned Account actors and all original migrations, without a
// service/database fallback. The public Account phase must select this root.
func TestF06OrganizationReviewGovernancePublicPostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := context.Background()
	newUser := func(name string) (identity.Principal, uuid.UUID) {
		p := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "g" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
		me, _, err := r.Bootstrap(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE users SET status='ACTIVE',display_name=$2 WHERE id=$1`, me.UserID, name); err != nil {
			t.Fatal(err)
		}
		return p, me.UserID
	}
	owner, ownerID := newUser("Same name")
	outsider, _ := newUser("Outside")
	targetPrincipal, targetID := newUser("Same name")
	org, err := r.AdminCreateOrganization(ctx, owner, ownerID, "Review governance", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AdminAddMember(ctx, owner, org.ID, targetID, []string{"MEMBER"}, nil, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var value string
		err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'users',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM users x),
'members',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM memberships x),
'bindings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_bindings x),
'assignments',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_assignments x),
'commands',(SELECT jsonb_agg(to_jsonb(x) ORDER BY idempotency_key) FROM membership_commands x),
'audit',(SELECT count(*) FROM audit_logs),'outbox',(SELECT count(*) FROM outbox_events))::text`).Scan(&value)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	t.Run("registered-model-admin-and-safe-down", func(t *testing.T) {
		request := RoleBindingGrant{UserID: targetID, RoleCode: "ORG_MODEL_ADMIN", OrganizationID: &org.ID, IdempotencyKey: uuid.NewString()}
		binding, err := r.GrantOrganizationRoleBinding(ctx, owner, request)
		if err != nil || binding.RoleCode != "ORG_MODEL_ADMIN" || binding.UserID != targetID {
			t.Fatal("registered role cannot be assigned", binding, err)
		}
		before := snapshot()
		replay, err := r.GrantOrganizationRoleBinding(ctx, owner, request)
		if err != nil || replay.ID != binding.ID || snapshot() != before {
			t.Fatal("grant replay changed effects", err)
		}
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := requireOrganizationPermission(ctx, tx, targetID, org.ID, nil, "org.models.manage"); err != nil {
			t.Fatal("assigned role did not project its organization authority", err)
		}
		if err := requireOrganizationPermission(ctx, tx, targetID, uuid.New(), nil, "org.models.manage"); !errors.Is(err, ErrForbidden) {
			t.Fatal("role crossed organization boundary", err)
		}
		if err := requireOrganizationPermission(ctx, tx, targetID, org.ID, nil, "platform.token.credentials.read"); !errors.Is(err, ErrForbidden) {
			t.Fatal("model role acquired credential authority", err)
		}
		tx.Rollback(ctx)
		// Execute the actual down SQL inside an explicit rollback boundary.
		// Existing historical bindings must prevent shrinking the role CHECK.
		down, err := os.ReadFile("../../migrations/account/900006_organization_model_admin_binding.down.sql")
		if err != nil {
			t.Fatal(err)
		}
		tx, err = r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		_, err = tx.Exec(ctx, string(down))
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23514" {
			t.Fatal("unsafe down did not reject a real existing binding", err)
		}
		tx.Rollback(ctx)
		if snapshot() != before {
			t.Fatal("rejected down changed binding or authority facts")
		}
		if _, err := r.RevokeOrganizationRoleBinding(ctx, owner, org.ID, binding.ID); err != nil {
			t.Fatal(err)
		}
		tx, err = r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := requireOrganizationPermission(ctx, tx, targetID, org.ID, nil, "org.models.manage"); !errors.Is(err, ErrForbidden) {
			t.Fatal("revoked model role remained effective", err)
		}
	})
	t.Run("removal-checks-authority-before-target", func(t *testing.T) {
		for _, userID := range []uuid.UUID{targetID, uuid.New()} {
			before := snapshot()
			_, err := r.RevokeMembership(ctx, outsider, org.ID, userID, uuid.NewString())
			if !errors.Is(err, ErrForbidden) || snapshot() != before {
				t.Fatal("target existence escaped authority or denial mutated facts", err)
			}
		}
		before := snapshot()
		_, err := r.RevokeMembership(ctx, owner, org.ID, uuid.New(), uuid.NewString())
		if !errors.Is(err, ErrNotFound) || snapshot() != before {
			t.Fatal("authorized missing target contract changed", err)
		}
	})
	t.Run("missing-admin-targets-are-not-found", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			call func() error
		}{
			{"add-org", func() error {
				_, e := r.AdminAddMember(ctx, owner, uuid.New(), targetID, []string{"MEMBER"}, nil, uuid.NewString())
				return e
			}},
			{"add-user", func() error {
				_, e := r.AdminAddMember(ctx, owner, org.ID, uuid.New(), []string{"MEMBER"}, nil, uuid.NewString())
				return e
			}},
			{"transfer-org", func() error {
				_, e := r.AdminTransferOwnership(ctx, owner, uuid.New(), targetID, uuid.NewString())
				return e
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				before := snapshot()
				if err := tc.call(); !errors.Is(err, ErrNotFound) || snapshot() != before {
					t.Fatal("missing target mapping or zero-effects violated", err)
				}
			})
		}
	})
	t.Run("disabled-target-role-replacement-is-zero-effect", func(t *testing.T) {
		t.Cleanup(func() {
			if _, err := r.pool.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, targetID); err != nil {
				t.Error(err)
			}
		})
		if _, err := r.pool.Exec(ctx, `UPDATE users SET status='DISABLED' WHERE id=$1`, targetID); err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		_, err := r.ReplaceMemberRoles(ctx, owner, org.ID, targetID, []string{"AUDITOR"})
		if !errors.Is(err, ErrNotFound) || snapshot() != before {
			t.Fatal("disabled target received replacement grants", err)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, targetID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ReplaceMemberRoles(ctx, owner, org.ID, targetID, []string{"AUDITOR"}); err != nil {
			t.Fatal("active target no longer supports original role replacement", err)
		}
	})
	t.Run("old-removal-replay-does-not-remove-readded-member", func(t *testing.T) {
		key := uuid.NewString()
		if _, err := r.RevokeMembership(ctx, owner, org.ID, targetID, key); err != nil {
			t.Fatal(err)
		}
		if _, err := r.AdminAddMember(ctx, owner, org.ID, targetID, []string{"MEMBER"}, nil, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		replay, err := r.RevokeMembership(ctx, owner, org.ID, targetID, key)
		if err != nil || !replay.Replayed || replay.Member.Status != "ACTIVE" || snapshot() != before {
			t.Fatal("old intent was reinterpreted as a new removal", replay, err)
		}
		removed, err := r.RevokeMembership(ctx, owner, org.ID, targetID, uuid.NewString())
		if err != nil || removed.Replayed || removed.Member.Status != "REVOKED" {
			t.Fatal("new intent did not perform the second removal", removed, err)
		}
		if _, err := r.AdminAddMember(ctx, owner, org.ID, targetID, []string{"MEMBER"}, nil, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("complete-bounded-pages-with-name-ties", func(t *testing.T) {
		expected := []uuid.UUID{ownerID, targetID}
		for i := 0; i < 3; i++ {
			_, id := newUser("Same name")
			expected = append(expected, id)
			if _, err := r.AdminAddMember(ctx, owner, org.ID, id, []string{"MEMBER"}, nil, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
		}
		slices.SortFunc(expected, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		var memberIDs, quotaIDs []uuid.UUID
		cursor := ""
		for pageNumber := 0; ; pageNumber++ {
			if pageNumber > len(expected) {
				t.Fatal("member pagination did not terminate")
			}
			page, err := r.ListMembers(ctx, owner, org.ID, nil, MemberListQuery{Limit: 2, Cursor: cursor})
			if err != nil || len(page.Items) > 2 {
				t.Fatal("unbounded or failed member page", err)
			}
			for _, item := range page.Items {
				memberIDs = append(memberIDs, item.UserID)
			}
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
		cursor = ""
		for pageNumber := 0; ; pageNumber++ {
			if pageNumber > len(expected) {
				t.Fatal("quota pagination did not terminate")
			}
			page, err := r.ListQuotaMembers(ctx, owner, org.ID, MemberListQuery{Limit: 2, Cursor: cursor})
			if err != nil || len(page.Items) > 2 {
				t.Fatal("unbounded or failed quota page", err)
			}
			for _, item := range page.Items {
				quotaIDs = append(quotaIDs, item.UserID)
			}
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
		if !slices.Equal(memberIDs, expected) || !slices.Equal(quotaIDs, expected) {
			t.Fatalf("page lost, duplicated or reordered members: %v %v expected %v", memberIDs, quotaIDs, expected)
		}
		if _, err := r.ListMembers(ctx, outsider, org.ID, nil, MemberListQuery{Limit: 2}); !errors.Is(err, ErrForbidden) {
			t.Fatal("pagination weakened member authority", err)
		}
		if _, err := r.ListQuotaMembers(ctx, targetPrincipal, org.ID, MemberListQuery{Limit: 2}); !errors.Is(err, ErrForbidden) {
			t.Fatal("pagination weakened quota allocation authority", err)
		}
	})
}
