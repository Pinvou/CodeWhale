package account

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The public Account migrations and original non-superuser role fixture are
// real. Historical rows and three external cleanup acknowledgements below are
// controlled inputs; this does not claim provider deletion or a three-domain
// worker/E2E run.
func TestAccountClosureAnonymizesOwnedHistoricalContacts(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := f.ctx
	now := time.Now().UTC()
	user, other, org := uuid.New(), uuid.New(), uuid.New()
	username := "Privacy_" + strings.ReplaceAll(user.String(), "-", "")[:12]
	for index, id := range []uuid.UUID{user, other} {
		name := username
		if id != user {
			name = "other_" + strings.ReplaceAll(id.String(), "-", "")[:12]
		}
		_, err := r.pool.Exec(ctx, `WITH capability AS (SELECT set_config('pinvou.registration_bonus_capable','1',true)) INSERT INTO users(id,status,user_code,username,mobile_e164,created_at,updated_at) SELECT $1,'ACTIVE',generate_unique_user_code(),$2,$3,$4,$4 FROM capability`, id, name, fmt.Sprintf("+861380000%04d", index), now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('closure-owned-history',$1,$2,$3,$3)`, id.String(), id, now); err != nil {
			t.Fatal(err)
		}
	}
	// A valid organization owned by the other user does not block this subject's
	// closure. Its retained invitations may still identify the closing recipient.
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ownerMembership := uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,'Controlled privacy history','ACTIVE',$2,$3,$3)`, []any{org, other, now}},
		{`INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($1,$2,$3,'ACTIVE',$4,$4)`, []any{ownerMembership, org, other, now}},
		{`INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at) VALUES($1,$2,'OWNER',$3,$4,$4)`, []any{uuid.New(), ownerMembership, other, now}},
	} {
		if _, err = tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// The actual invitation creator lowercases this target, while an existing
	// OIDC username can legitimately keep its original case.
	pendingInvitation, err := r.CreateInvitation(ctx, identity.Principal{Issuer: "closure-owned-history", Subject: other.String()}, org, InvitationCreateRequest{TargetType: "USERNAME", Target: username, Roles: []string{"MEMBER"}, ExpiresInHours: 1, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	ownedInvitations := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	foreignInvitation := uuid.New()
	for index, id := range append(append([]uuid.UUID{}, ownedInvitations...), foreignInvitation) {
		inviter, recipient := other, user
		if id == foreignInvitation {
			inviter, recipient = user, other
		}
		target := []string{"EMAIL", "PHONE", "USERNAME", "EMAIL"}[index]
		if _, err = r.pool.Exec(ctx, `INSERT INTO invitations(id,organization_id,email_normalized,token_hash,status,roles,invited_by,accepted_by,expires_at,created_at,updated_at,target_type,target_digest,target_hint,idempotency_key,request_hash,accepted_at) VALUES($1,$2,$3,gen_random_bytes(32),'ACCEPTED','["MEMBER"]'::jsonb,$4,$5,$6,$7,$7,$8,gen_random_bytes(32),$9,$10,repeat('a',64),$7)`, id, org, fmt.Sprintf("owned-history-%d@example.invalid", index), inviter, recipient, now.Add(time.Hour), now, target, "controlled recipient hint", "privacy-invite-"+id.String()); err != nil {
			t.Fatal(err)
		}
	}
	ownedCredentials := []uuid.UUID{}
	foreignCredential := uuid.New()
	for index, status := range []string{"CONSUMED", "EXPIRED", "LOCKED", "SUPERSEDED", "CONSUMED"} {
		id, owner := uuid.New(), user
		if index == 4 {
			id, owner = foreignCredential, other
		} else {
			ownedCredentials = append(ownedCredentials, id)
		}
		var consumed any
		if status == "CONSUMED" {
			consumed = now
		}
		if _, err = r.pool.Exec(ctx, `INSERT INTO super_admin_bootstrap_credentials(id,deployment_identity,deployment_generation,user_id,identity_subject,mode,status,expires_at,consumed_at,created_at,updated_at) VALUES($1,'controlled-privacy-history',$2,$3,$4,'ONE_TIME_BOOTSTRAP',$5,$6,$7,$8,$8)`, id, index+1, owner, owner.String(), status, now.Add(time.Hour), consumed, now); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func() string {
		t.Helper()
		facts := []string{}
		for _, table := range []string{"invitations", "super_admin_bootstrap_credentials", "users", "identities", "memberships", "account_closure_requests", "account_closure_commands", "account_closure_audit", "account_subject_retention_records", "audit_logs", "outbox_events", "registration_operations", "role_bindings", "role_assignments"} {
			var rows string
			if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM `+pgx.Identifier{table}.Sanitize()+` t`).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			facts = append(facts, table+"="+rows)
		}
		return strings.Join(facts, "\n")
	}
	row := func(table string, id uuid.UUID, omitted []string) string {
		t.Helper()
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT (to_jsonb(t)-$2::text[])::text FROM `+pgx.Identifier{table}.Sanitize()+` t WHERE id=$1`, id, omitted).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	retained := map[uuid.UUID]string{}
	for _, id := range ownedInvitations {
		retained[id] = row("invitations", id, []string{"email_normalized", "target_hint", "version", "updated_at"})
	}
	for _, id := range ownedCredentials {
		retained[id] = row("super_admin_bootstrap_credentials", id, []string{"identity_subject", "updated_at"})
	}
	foreignInvitationBefore := row("invitations", foreignInvitation, []string{})
	foreignCredentialBefore := row("super_admin_bootstrap_credentials", foreignCredential, []string{})
	principal := identity.Principal{Issuer: "closure-owned-history", Subject: user.String(), AuthenticatedAt: now}
	for name, authenticatedAt := range map[string]time.Time{"missing-recent-auth": {}, "expired-recent-auth": now.Add(-accountClosureRecentAuth - time.Minute), "future-recent-auth": now.Add(3 * time.Minute)} {
		t.Run(name, func(t *testing.T) {
			before := snapshot()
			candidate := principal
			candidate.AuthenticatedAt = authenticatedAt
			if _, err := r.CreateAccountClosure(ctx, candidate, uuid.NewString(), accountClosureConfirmation); !errors.Is(err, ErrRecentAuthenticationRequired) || snapshot() != before {
				t.Fatal("public repository recent-auth gate changed facts", err)
			}
		})
	}
	created, err := r.CreateAccountClosure(ctx, principal, uuid.NewString(), accountClosureConfirmation)
	if err != nil {
		t.Fatal(err)
	}
	// Use an ACTIVE, genuinely authorized operations actor and an existing
	// eligible request: removing the Retry precondition would otherwise write.
	command, err := r.pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,created_by,created_at,starts_at) SELECT $1,$2,ro.code,$2,$3,$3 FROM roles ro WHERE ro.scope='PLATFORM' AND EXISTS(SELECT 1 FROM role_permissions rp WHERE rp.role_code=ro.code AND rp.permission_code=$4) ORDER BY ro.code LIMIT 1`, uuid.New(), other, now, PermissionPlatformAccountClosuresRepair)
	if err != nil || command.RowsAffected() != 1 {
		t.Fatal("missing original platform repair role", err)
	}
	for name, authenticatedAt := range map[string]time.Time{"missing-recent-auth": {}, "expired-recent-auth": now.Add(-accountClosureRecentAuth - time.Minute), "future-recent-auth": now.Add(3 * time.Minute)} {
		t.Run("retry/"+name, func(t *testing.T) {
			before := snapshot()
			actor := identity.Principal{Issuer: "closure-owned-history", Subject: other.String(), AuthenticatedAt: authenticatedAt}
			if _, err := r.RetryAccountClosure(ctx, actor, created.ID, uuid.NewString(), "controlled original-stage retry"); !errors.Is(err, ErrRecentAuthenticationRequired) || snapshot() != before {
				t.Fatal("public repair recent-auth gate changed facts", err)
			}
		})
	}
	claim := func() *AccountClosureRequest {
		t.Helper()
		item, err := r.ClaimAccountClosure(ctx)
		if err != nil || item == nil || item.UserID != user {
			t.Fatalf("owned claim: %v", err)
		}
		return item
	}
	item := claim()
	if err = r.AdvanceAccountClosureToIdentityCleanup(ctx, *item); err != nil {
		t.Fatal(err)
	}
	item = claim()
	preparation, err := r.PrepareAccountClosureCleanup(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := preparation.SubjectLifecycleVersion
	t.Run("normalized-pending-username-cancelled", func(t *testing.T) {
		var cancelled bool
		if err := r.pool.QueryRow(ctx, `SELECT status='CANCELLED_ACCOUNT_CLOSURE' AND email_normalized IS NULL AND target_hint IS NULL FROM invitations WHERE id=$1`, pendingInvitation.ID).Scan(&cancelled); err != nil || !cancelled {
			t.Fatal("mixed-case username invitation survived preparation", err)
		}
	})
	reject := func(t *testing.T, candidate AccountClosureRequest, version int64) {
		t.Helper()
		before := snapshot()
		if err := r.AnonymizeAccountClosure(ctx, &candidate, version); !errors.Is(err, ErrConflict) {
			t.Fatalf("expected original closure fence: %v", err)
		}
		if snapshot() != before {
			t.Fatal("rejected anonymization changed retained facts")
		}
	}
	t.Run("missing-identity-ack", func(t *testing.T) { reject(t, *item, lifecycle) })
	if err = r.MarkAccountClosureIdentityDeleted(ctx, item, lifecycle); err != nil {
		t.Fatal(err)
	}
	t.Run("missing-grants-ack", func(t *testing.T) { reject(t, *item, lifecycle) })
	if err = r.MarkAccountClosureGrantsIsolated(ctx, item, lifecycle); err != nil {
		t.Fatal(err)
	}
	t.Run("missing-replay-ack", func(t *testing.T) { reject(t, *item, lifecycle) })
	if err = r.MarkAccountClosureReplayCleared(ctx, item, lifecycle); err != nil {
		t.Fatal(err)
	}
	t.Run("wrong-lifecycle", func(t *testing.T) { reject(t, *item, lifecycle+1) })
	t.Run("stale-claim-version", func(t *testing.T) {
		stale := *item
		stale.Version--
		reject(t, stale, lifecycle)
	})
	t.Run("expired-claim", func(t *testing.T) {
		if _, err := r.pool.Exec(ctx, `UPDATE account_closure_requests SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, item.ID); err != nil {
			t.Fatal(err)
		}
		reject(t, *item, lifecycle)
		if _, err := r.pool.Exec(ctx, `UPDATE account_closure_requests SET lease_until=clock_timestamp()+interval '30 seconds' WHERE id=$1`, item.ID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("owned-contacts-only", func(t *testing.T) {
		if err := r.AnonymizeAccountClosure(ctx, item, lifecycle); err != nil {
			t.Fatal(err)
		}
		for _, id := range ownedInvitations {
			var clear bool
			if err := r.pool.QueryRow(ctx, `SELECT email_normalized IS NULL AND target_hint IS NULL FROM invitations WHERE id=$1`, id).Scan(&clear); err != nil || !clear {
				t.Fatal("accepted recipient contacts retained", err)
			}
			if row("invitations", id, []string{"email_normalized", "target_hint", "version", "updated_at"}) != retained[id] {
				t.Fatal("accepted invitation frozen evidence changed")
			}
		}
		for _, id := range ownedCredentials {
			var clear bool
			if err := r.pool.QueryRow(ctx, `SELECT identity_subject LIKE 'closed:%' AND identity_subject<>$2 AND verifier IS NULL FROM super_admin_bootstrap_credentials WHERE id=$1`, id, user.String()).Scan(&clear); err != nil || !clear {
				t.Fatal("terminal bootstrap subject retained", err)
			}
			if row("super_admin_bootstrap_credentials", id, []string{"identity_subject", "updated_at"}) != retained[id] {
				t.Fatal("terminal credential frozen evidence changed")
			}
		}
		if row("invitations", foreignInvitation, []string{}) != foreignInvitationBefore || row("super_admin_bootstrap_credentials", foreignCredential, []string{}) != foreignCredentialBefore {
			t.Fatal("another recipient or bootstrap owner's data changed")
		}
		var complete bool
		if err := r.pool.QueryRow(ctx, `SELECT u.status='DELETED' AND q.stage='ANONYMIZED' AND q.identity_deleted_at IS NOT NULL AND q.grants_isolated_at IS NOT NULL AND q.token_replays_cleared_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM identities WHERE user_id=u.id) AND EXISTS(SELECT 1 FROM account_subject_retention_records WHERE user_id=u.id) AND EXISTS(SELECT 1 FROM outbox_events WHERE aggregate_id=q.id AND event_type='account.user.anonymized.v1') FROM users u JOIN account_closure_requests q ON q.user_id=u.id WHERE u.id=$1`, user).Scan(&complete); err != nil || !complete {
			t.Fatal("original anonymization closure facts missing", err)
		}
	})
	t.Run("same-stage-replay-preserves-all-facts", func(t *testing.T) {
		item = claim()
		before := snapshot()
		if err := r.AnonymizeAccountClosure(ctx, item, lifecycle); err != nil || snapshot() != before {
			t.Fatal("terminal anonymization replay mutated evidence", err)
		}
	})
	t.Run("completion-requires-published-anonymization", func(t *testing.T) {
		before := snapshot()
		if err := r.CompleteAccountClosure(ctx, item); !errors.Is(err, ErrConflict) || snapshot() != before {
			t.Fatal("unpublished completion changed facts", err)
		}
	})
	t.Run("published-completion-preserves-original-closure", func(t *testing.T) {
		// Controlled publication acknowledgement, not an actual NATS delivery.
		if _, err := r.pool.Exec(ctx, `UPDATE outbox_events SET published_at=clock_timestamp() WHERE aggregate_id=$1 AND event_type='account.user.anonymized.v1'`, item.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.CompleteAccountClosure(ctx, item); err != nil {
			t.Fatal(err)
		}
		var complete bool
		if err := r.pool.QueryRow(ctx, `SELECT status='COMPLETED' AND stage='COMPLETED' AND completed_at IS NOT NULL AND lease_until IS NULL FROM account_closure_requests WHERE id=$1`, item.ID).Scan(&complete); err != nil || !complete {
			t.Fatal("published completion did not reach terminal state", err)
		}
	})
	t.Run("retention-not-yet-due", func(t *testing.T) {
		before := snapshot()
		if count, err := r.PurgeExpiredAccountClosureRetention(ctx, 0); err != nil || count != 0 || snapshot() != before {
			t.Fatal("future retention was purged", count, err)
		}
	})
	t.Run("retention-legal-hold", func(t *testing.T) {
		if _, err := r.pool.Exec(ctx, `UPDATE account_subject_retention_records SET retained_until=clock_timestamp()-interval '1 second',legal_hold=true WHERE user_id=$1`, user); err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		if count, err := r.PurgeExpiredAccountClosureRetention(ctx, 1); err != nil || count != 0 || snapshot() != before {
			t.Fatal("legal hold was purged", count, err)
		}
	})
	t.Run("retention-purge-is-metadata-only", func(t *testing.T) {
		if _, err := r.pool.Exec(ctx, `UPDATE account_subject_retention_records SET legal_hold=false WHERE user_id=$1`, user); err != nil {
			t.Fatal(err)
		}
		withoutRetention := func(facts string) string {
			rows := []string{}
			for _, line := range strings.Split(facts, "\n") {
				if !strings.HasPrefix(line, "account_subject_retention_records=") {
					rows = append(rows, line)
				}
			}
			return strings.Join(rows, "\n")
		}
		before := withoutRetention(snapshot())
		var recordBefore, recordAfter string
		query := `SELECT (to_jsonb(t)-ARRAY['status','purged_at'])::text FROM account_subject_retention_records t WHERE user_id=$1`
		if err := r.pool.QueryRow(ctx, query, user).Scan(&recordBefore); err != nil {
			t.Fatal(err)
		}
		if count, err := r.PurgeExpiredAccountClosureRetention(ctx, 1); err != nil || count != 1 {
			t.Fatal("due record was not marked purged", count, err)
		}
		var purged bool
		if err := r.pool.QueryRow(ctx, `SELECT status='PURGED' AND purged_at IS NOT NULL FROM account_subject_retention_records WHERE user_id=$1`, user).Scan(&purged); err != nil || !purged {
			t.Fatal("purge marker missing", err)
		}
		if err := r.pool.QueryRow(ctx, query, user).Scan(&recordAfter); err != nil || recordAfter != recordBefore || withoutRetention(snapshot()) != before {
			t.Fatal("metadata purge destroyed retained evidence", err)
		}
		if count, err := r.PurgeExpiredAccountClosureRetention(ctx, 1); err != nil || count != 0 {
			t.Fatal("metadata purge replay changed result", count, err)
		}
	})
}
