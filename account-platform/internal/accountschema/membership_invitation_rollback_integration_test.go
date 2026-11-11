package accountschema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This is a retained original 1..21 database, not a current database with a
// rewritten ledger. Controlled identity/owner setup and a legacy LINK fact are
// followed by actual current writer replay after the public forward upgrade.
func TestMembershipInvitationLegacyRollbackPublicPostgres(t *testing.T) {
	raw := os.Getenv("F17_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("isolated PostgreSQL fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newAuditExportGuardFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var history []migrations.Migration
	for _, migration := range loaded {
		if migration.Version <= 21 {
			history = append(history, migration)
		}
	}
	if len(history) != 21 {
		t.Fatal("original complete 1..21 history required")
	}
	if err := migrations.Up(ctx, c, history); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, c)
	if _, err := migrations.VerifyApplied(ctx, c, history); err != nil {
		t.Fatal(err)
	}
	user, org, membership, invitation := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	principal := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString()}
	code, err := account.GenerateUserCode()
	if err != nil {
		t.Fatal(err)
	}
	setup, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Rollback(context.Background())
	for _, command := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id,user_code,status,created_at,updated_at) VALUES($1,$2,'ACTIVE',now(),now())`, []any{user, code}},
		{`INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,now(),now())`, []any{principal.Issuer, principal.Subject, user}},
		{`INSERT INTO organizations(id,name,status,created_by,created_at,updated_at) VALUES($1,'Retained invitation','ACTIVE',$2,now(),now())`, []any{org, user}},
		{`INSERT INTO memberships(id,organization_id,user_id,status,joined_at,updated_at) VALUES($1,$2,$3,'ACTIVE',now(),now())`, []any{membership, org, user}},
		{`INSERT INTO role_bindings(id,membership_id,role,created_by,created_at,starts_at) VALUES($1,$2,'OWNER',$3,now(),now())`, []any{uuid.New(), membership, user}},
	} {
		if _, err := setup.Exec(ctx, command.sql, command.args...); err != nil {
			t.Fatal(err)
		}
	}
	// Original invitationRequestHash canonical fields, with nil unit scope.
	// The real writer below must accept this exact retained key/fingerprint.
	facts, err := json.Marshal(map[string]any{"version": 1, "target_type": "LINK", "target_digest": "", "roles": []string{"MEMBER"}, "expires_in_hours": 24})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(facts)
	tokenHash := sha256.Sum256([]byte("controlled retained invitation token"))
	key := "retained-link-invitation-key"
	if _, err := setup.Exec(ctx, `INSERT INTO invitations(id,organization_id,email_normalized,token_hash,status,roles,invited_by,expires_at,created_at,updated_at,target_type,target_digest,target_hint,idempotency_key,request_hash) VALUES($1,$2,NULL,$3,'PENDING','["MEMBER"]',$4,now()+interval '24 hours',now(),now(),'LINK',NULL,'',$5,$6)`, invitation, org, tokenHash[:], user, key, hex.EncodeToString(fingerprint[:])); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		rows, err := c.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename`)
		if err != nil {
			t.Fatal(err)
		}
		var tables []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			tables = append(tables, name)
		}
		rows.Close()
		if rows.Err() != nil {
			t.Fatal(rows.Err())
		}
		var result strings.Builder
		for _, name := range tables {
			var value string
			if err := c.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text)::text,'[]') FROM `+pgx.Identifier{"public", name}.Sanitize()+` x`).Scan(&value); err != nil {
				t.Fatal(err)
			}
			result.WriteString(name + "=" + value + "\n")
		}
		var schema string
		if err := c.QueryRow(ctx, `SELECT jsonb_build_object(
 'columns',(SELECT jsonb_agg(to_jsonb(x) ORDER BY table_name,ordinal_position) FROM information_schema.columns x WHERE table_schema='public'),
 'indexes',(SELECT jsonb_agg(to_jsonb(x) ORDER BY indexname) FROM pg_indexes x WHERE schemaname='public'),
 'triggers',(SELECT jsonb_agg(jsonb_build_array(t.tgname,pg_get_triggerdef(t.oid)) ORDER BY c.relname,t.tgname) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND NOT t.tgisinternal),
 'functions',(SELECT jsonb_agg(pg_get_functiondef(p.oid) ORDER BY p.proname) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public' AND p.proname IN ('protect_invitation_identity','protect_membership_governance_history','enforce_single_effective_owner'))
 )::text`).Scan(&schema); err != nil {
			t.Fatal(err)
		}
		return result.String() + schema
	}
	before := snapshot()
	if !t.Run("retained-link-refuses-down-without-fact-or-schema-loss", func(t *testing.T) {
		for attempt := 0; attempt < 2; attempt++ {
			err := DownOne(ctx, c, loaded)
			migrations.Unlock(ctx, c)
			var refused *pgconn.PgError
			if !errors.As(err, &refused) || refused.Code != "23502" || refused.TableName != "invitations" || refused.ColumnName != "email_normalized" {
				t.Fatal("original nullable invitation rollback refusal changed", err)
			}
			if snapshot() != before {
				t.Fatal("failed original Down changed retained data, ledger or schema")
			}
			if _, err := migrations.VerifyApplied(ctx, c, history); err != nil {
				t.Fatal(err)
			}
		}
	}) {
		return
	}
	t.Run("forward-upgrade-replays-original-and-accepts-next-command", func(t *testing.T) {
		if err := Up(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, c)
		if _, err := VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal(err)
		}
		r, err := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		request := account.InvitationCreateRequest{TargetType: "LINK", Roles: []string{"MEMBER"}, ExpiresInHours: 24, IdempotencyKey: key}
		retained := snapshot()
		replay, err := r.CreateInvitation(ctx, principal, org, request)
		if err != nil || replay.ID != invitation || !replay.Replayed || snapshot() != retained {
			t.Fatal("forward upgrade did not retain the original invitation replay", err)
		}
		request.IdempotencyKey = "retained-link-next-command-key"
		next, err := r.CreateInvitation(ctx, principal, org, request)
		if err != nil || next.ID == invitation || next.Replayed || next.Token == "" {
			t.Fatal("legal invitation after refused Down failed", err)
		}
		var count int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM invitations WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 2 {
			t.Fatal("unexpected invitation effects", err)
		}
	})
}
