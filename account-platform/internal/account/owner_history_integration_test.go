package account

import (
	"context"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestPostgresHistoricalOwnerDoesNotBlockAdminMemberOrUserRemoval(t *testing.T) {
	for _, action := range []string{"remove-member", "delete-user"} {
		t.Run(action, func(t *testing.T) {
			repo := accountDatabase(t)
			ctx := context.Background()
			owner, former := accountUser(t, repo)
			member, target := accountUser(t, repo)
			admin := fixturePlatformAdmin(t, repo)
			org := accountOrganization(t, repo, owner)
			accountInvitation(t, repo, owner, member, org.ID, []string{"MEMBER"})
			if _, err := repo.TransferOwnership(ctx, owner, org.ID, target.UserID, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			if action == "remove-member" {
				if _, err := repo.AdminRemoveMember(ctx, admin, org.ID, former.UserID, uuid.NewString()); err != nil {
					t.Fatal("revoked historical owner blocked ordinary member removal")
				}
				accountInvitation(t, repo, member, owner, org.ID, []string{"MEMBER"})
				var historical, liveOwner int
				if err := repo.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE rb.revoked_at IS NOT NULL),count(*) FILTER(WHERE rb.revoked_at IS NULL) FROM role_bindings rb JOIN memberships m ON m.id=rb.membership_id WHERE m.organization_id=$1 AND m.user_id=$2 AND rb.role='OWNER'`, org.ID, former.UserID).Scan(&historical, &liveOwner); err != nil || historical != 1 || liveOwner != 0 {
					t.Fatal("member removal or invitation rejoin erased or reactivated historical owner authority")
				}
			} else if err := repo.AdminDeleteUser(ctx, admin, former.UserID); err != nil {
				t.Fatal("revoked historical owner blocked user deletion")
			}
			if _, err := repo.AdminRemoveMember(ctx, admin, org.ID, target.UserID, uuid.NewString()); err == nil {
				t.Fatal("current owner protection was lost")
			}
		})
	}
}

func TestPostgresAdminOwnershipTransferPreservesRevokedBinding(t *testing.T) {
	repo := accountDatabase(t)
	ctx := context.Background()
	owner, former := accountUser(t, repo)
	member, target := accountUser(t, repo)
	admin := fixturePlatformAdmin(t, repo)
	org := accountOrganization(t, repo, owner)
	accountInvitation(t, repo, owner, member, org.ID, []string{"MEMBER"})
	if _, err := repo.AdminTransferOwnership(ctx, admin, org.ID, target.UserID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	var revoked *time.Time
	if err := repo.pool.QueryRow(ctx, `SELECT rb.revoked_at FROM role_bindings rb JOIN memberships m ON m.id=rb.membership_id WHERE m.organization_id=$1 AND m.user_id=$2 AND rb.role='OWNER'`, org.ID, former.UserID).Scan(&revoked); err != nil || revoked == nil {
		t.Fatal("admin ownership transfer erased the historical binding")
	}
	var effective, members int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM role_bindings rb JOIN memberships m ON m.id=rb.membership_id WHERE m.organization_id=$1 AND rb.role='OWNER' AND rb.revoked_at IS NULL AND rb.starts_at<=statement_timestamp() AND (rb.expires_at IS NULL OR rb.expires_at>statement_timestamp())`, org.ID).Scan(&effective); err != nil || effective != 1 {
		t.Fatal("transfer did not retain one current owner")
	}
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM role_bindings rb JOIN memberships m ON m.id=rb.membership_id WHERE m.organization_id=$1 AND m.user_id=$2 AND rb.role='MEMBER' AND rb.revoked_at IS NULL`, org.ID, former.UserID).Scan(&members); err != nil || members != 1 {
		t.Fatal("former owner lost ordinary membership when only historical roles remain")
	}
}
