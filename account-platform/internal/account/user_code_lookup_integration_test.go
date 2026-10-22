package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Only the disposable loopback admin database is eligible. Existing named
// databases are never connected or cleaned; the retained fixture owns UUID DBs
// and insists all of its runtime actors are absent before creating any.
func userCodeLookupTestGuard(t *testing.T) {
	t.Helper()
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("dedicated PostgreSQL required")
	}
	c, e := pgx.ParseConfig(raw)
	if e != nil || c.Host != "127.0.0.1" || c.User != "postgres" || c.Password != "" || c.Database != "postgres" || c.Port < 1024 {
		t.Fatal("isolated loopback admin postgres URL required before any fixture writes")
	}
}
func userCodeActor(t *testing.T, r *PostgresRepository, label string) (identity.Principal, uuid.UUID, string) {
	t.Helper()
	a := seedEntitlementApprovalActor(t, context.Background(), r.pool, "PLATFORM_SUPER_ADMIN", label)
	p := entitlementApprovalPrincipal(a)
	var code string
	if e := r.pool.QueryRow(context.Background(), `SELECT user_code FROM users WHERE id=$1`, a.ID).Scan(&code); e != nil {
		t.Fatal(e)
	}
	return p, a.ID, FormatUserCode(code)
}
func userCodeHandler(r *PostgresRepository, principals map[string]identity.Principal) http.Handler {
	return NewService(r, nil).Handler(func(_ context.Context, token string) (identity.Principal, error) {
		p, ok := principals[token]
		if !ok {
			return p, errors.New("untrusted controlled credential")
		}
		return p, nil
	})
}
func userCodeHTTP(h http.Handler, token, code, source string) *httptest.ResponseRecorder {
	q := httptest.NewRequest(http.MethodGet, "/admin/v1/user-code-lookups/"+code, nil)
	q.Header.Set("Authorization", "Bearer "+token)
	q.Header.Set("X-Device-ID", "lookup-device-"+token)
	q.RemoteAddr = source + ":1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, q)
	return w
}
func TestUserCodeLookupRuntimePostgres(t *testing.T) {
	userCodeLookupTestGuard(t)
	f := retainedWalletFenceActors(t)
	t.Run("actual-route-normalization-deleted-and-digest-audit", func(t *testing.T) {
		r := f.repository(t)
		p, actor, _ := userCodeActor(t, r, "lookup-reader")
		_, target, code := userCodeActor(t, r, "lookup-target")
		ctx := context.Background()
		h := userCodeHandler(r, map[string]identity.Principal{"reader": p})
		raw := strings.ToLower(strings.ReplaceAll(code, "-", ""))
		w := userCodeHTTP(h, "reader", raw, "192.0.2.13")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var item AdminUser
		if e := json.Unmarshal(w.Body.Bytes(), &item); e != nil || item.ID != target || item.UserCode != code || item.Email != nil {
			t.Fatal("original exact lookup projection", item, e)
		}
		if _, e := r.pool.Exec(ctx, `UPDATE users SET status='DELETED' WHERE id=$1`, target); e != nil {
			t.Fatal(e)
		}
		w = userCodeHTTP(h, "reader", code, "192.0.2.13")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"DELETED"`) || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("permanent deleted lookup lost", w.Code, w.Body.String())
		}
		hash := sha256.Sum256([]byte(strings.ReplaceAll(code, "-", "")))
		var attempts, audits int
		var summary string
		if e := r.pool.QueryRow(ctx, `SELECT count(*) FROM user_code_lookup_attempts WHERE actor_user_id=$1 AND target_code_hash=$2 AND octet_length(source_hash)=32 AND octet_length(device_hash)=32 AND result='SUCCEEDED'`, actor, hash[:]).Scan(&attempts); e != nil {
			t.Fatal(e)
		}
		if e := r.pool.QueryRow(ctx, `SELECT count(*),min(change_summary::text) FROM audit_logs WHERE actor_user_id=$1 AND action='user.code.lookup' AND resource_id=$2`, actor, hex.EncodeToString(hash[:])).Scan(&audits, &summary); e != nil || attempts != 2 || audits != 2 || strings.Contains(summary, code) || strings.Contains(summary, "192.0.2.13") {
			t.Fatal("missing/redaction audit", attempts, audits, summary, e)
		}
		q := httptest.NewRequest("GET", "/admin/v1/user-code-lookups/"+code, nil)
		out := httptest.NewRecorder()
		h.ServeHTTP(out, q)
		if out.Code == 200 {
			t.Fatal("anonymous lookup")
		}
	})
	t.Run("permission-invalid-absent-unified-and-current-revocation", func(t *testing.T) {
		r := f.repository(t)
		p, actor, _ := userCodeActor(t, r, "lookup-denial")
		_, _, code := userCodeActor(t, r, "lookup-denial-target")
		h := userCodeHandler(r, map[string]identity.Principal{"reader": p})
		missing, e := GenerateUserCode()
		if e != nil {
			t.Fatal(e)
		}
		invalid := userCodeHTTP(h, "reader", "not-a-user-code", "192.0.2.14")
		absent := userCodeHTTP(h, "reader", missing, "192.0.2.14")
		if invalid.Code != 404 || absent.Code != 404 || invalid.Body.String() != absent.Body.String() {
			t.Fatal("invalid/absent enumeration", invalid.Code, absent.Code)
		}
		if _, e = r.pool.Exec(context.Background(), `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE user_id=$1 AND role_code='PLATFORM_SUPER_ADMIN'`, actor); e != nil {
			t.Fatal(e)
		}
		denied := userCodeHTTP(h, "reader", code, "192.0.2.14")
		if denied.Code != 404 || denied.Body.String() != absent.Body.String() {
			t.Fatal("revocation/permission enumeration", denied.Code, denied.Body.String())
		}
		var results string
		if e = r.pool.QueryRow(context.Background(), `SELECT string_agg(result,',' ORDER BY result) FROM user_code_lookup_attempts WHERE actor_user_id=$1`, actor).Scan(&results); e != nil || results != "DENIED,DENIED,NOT_FOUND" {
			t.Fatal("outcome attempts", results, e)
		}
	})
	t.Run("concurrent-shared-target-quota-blocks-before-audit-and-survives-new-handler", func(t *testing.T) {
		r := f.repository(t)
		ctx := context.Background()
		p1, _, _ := userCodeActor(t, r, "lookup-concurrent-one")
		p2, _, _ := userCodeActor(t, r, "lookup-concurrent-two")
		_, _, code := userCodeActor(t, r, "lookup-concurrent-target")
		for i := 0; i < 9; i++ {
			if _, e := r.LookupUserByCode(ctx, p1, code, "seed-source", "seed-device"); e != nil {
				t.Fatal(e)
			}
		}
		barrier, e := r.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer barrier.Rollback(ctx)
		if _, e = barrier.Exec(ctx, `LOCK TABLE audit_logs IN SHARE MODE`); e != nil {
			t.Fatal(e)
		}
		type answer struct{ err error }
		done := make(chan answer, 2)
		var wg sync.WaitGroup
		for i, p := range []identity.Principal{p1, p2} {
			wg.Add(1)
			go func(i int, p identity.Principal) {
				defer wg.Done()
				_, e := r.LookupUserByCode(ctx, p, code, fmt.Sprint("separate-source-", i), fmt.Sprint("separate-device-", i))
				done <- answer{e}
			}(i, p)
		}
		observed := false
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			var pending, serialized bool
			e = f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock' AND query LIKE '%INSERT INTO audit_logs%'),EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock%')`, r.pool.Config().ConnConfig.Database).Scan(&pending, &serialized)
			if e != nil {
				t.Fatal(e)
			}
			if pending && serialized {
				observed = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		_ = barrier.Rollback(ctx)
		wg.Wait()
		close(done)
		success, limited := 0, 0
		for a := range done {
			if a.err == nil {
				success++
			} else if errors.Is(a.err, ErrUserCodeLookupRateLimited) {
				limited++
			} else {
				t.Fatal(a.err)
			}
		}
		if !observed || success != 1 || limited != 1 {
			t.Fatal("quota barrier/once admission", observed, success, limited)
		}
		h := userCodeHandler(r, map[string]identity.Principal{"new-reader": p2})
		w := userCodeHTTP(h, "new-reader", code, "192.0.2.16")
		if w.Code != 429 {
			t.Fatal("persisted minute quota lost across handler", w.Code, w.Body.String())
		}
	})
	t.Run("permission-is-read-after-waiting-for-actor-row", func(t *testing.T) {
		r := f.repository(t)
		ctx := context.Background()
		p, actor, _ := userCodeActor(t, r, "lookup-wait-reader")
		_, _, code := userCodeActor(t, r, "lookup-wait-target")
		blocker, e := r.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer blocker.Rollback(ctx)
		var locked uuid.UUID
		if e = blocker.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, actor).Scan(&locked); e != nil {
			t.Fatal(e)
		}
		done := make(chan error, 1)
		go func() { _, e := r.LookupUserByCode(ctx, p, code, "wait-source", "wait-device"); done <- e }()
		waiting := false
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if e = f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock' AND query LIKE '%FOR UPDATE OF u%')`, r.pool.Config().ConnConfig.Database).Scan(&waiting); e != nil {
				t.Fatal(e)
			}
			if waiting {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if _, e = blocker.Exec(ctx, `UPDATE role_assignments SET revoked_at=clock_timestamp() WHERE user_id=$1 AND role_code='PLATFORM_SUPER_ADMIN'`, actor); e != nil {
			t.Fatal(e)
		}
		if e = blocker.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		if e = <-done; !waiting || !errors.Is(e, ErrNotFound) {
			t.Fatal("lookup used pre-lock permission", waiting, e)
		}
	})
	t.Run("original-actor-source-device-thresholds", func(t *testing.T) {
		for _, dimension := range []string{"actor", "source", "device"} {
			t.Run(dimension, func(t *testing.T) {
				r := f.repository(t)
				ctx := context.Background()
				p, id, _ := userCodeActor(t, r, "lookup-dimension")
				code, e := GenerateUserCode()
				if e != nil {
					t.Fatal(e)
				}
				source, device := "source-final", "device-final"
				sh, dh := sha256.Sum256([]byte(source)), sha256.Sum256([]byte(device))
				limit := 30
				if dimension == "source" {
					limit = 60
				}
				// Controlled existing history is produced by the same durable writer,
				// including its audit. Distinct target digests avoid the independent cap10.
				tx, e := r.pool.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				defer tx.Rollback(ctx)
				for i := 0; i < limit; i++ {
					a := id
					s, d := sha256.Sum256([]byte(fmt.Sprint("seed-source", i))), sha256.Sum256([]byte(fmt.Sprint("seed-device", i)))
					if dimension != "actor" {
						_, a, _ = userCodeActor(t, r, fmt.Sprint("lookup-seed", i))
					}
					if dimension == "source" {
						s = sh
					}
					if dimension == "device" {
						d = dh
					}
					target := sha256.Sum256([]byte(fmt.Sprint("seed-target", i)))
					if e = recordUserCodeLookup(ctx, tx, p, a, s, d, target, "DENIED"); e != nil {
						t.Fatal(e)
					}
				}
				if e = tx.Commit(ctx); e != nil {
					t.Fatal(e)
				}
				if _, e = r.LookupUserByCode(ctx, p, code, source, device); !errors.Is(e, ErrUserCodeLookupRateLimited) {
					t.Fatal("original dimension limit", dimension, e)
				}
			})
		}
	})
}

func TestAuditSearchOriginalMigrationRuntimePostgres(t *testing.T) {
	userCodeLookupTestGuard(t)
	f := retainedWalletFenceActors(t)
	for _, mode := range []string{"fresh", "original005-ledger", "current-without005-supplement"} {
		t.Run(mode, func(t *testing.T) {
			var prior string
			initial := func(ctx context.Context, c *pgx.Conn, all []migrations.Migration) error {
				if mode == "fresh" {
					return nil
				}
				input := make([]migrations.Migration, 0, len(all))
				for _, m := range all {
					if mode == "current-without005-supplement" && m.Version == 5 {
						continue
					}
					input = append(input, m)
				}
				// Preserve an already-installed exact original005 identity in the
				// supported current schema. This is not an O <=015 whole-release
				// upgrade: that source has other different migration bodies;
				// its whole-schema compatibility is separately unverified.
				if e := accountschema.Up(ctx, c, input); e != nil {
					return e
				}
				return c.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(s) ORDER BY version),'[]'::jsonb)::text FROM schema_migrations s`).Scan(&prior)
			}
			r := f.repositoryWithInitialStage(t, initial)
			var count int
			var unchanged bool
			if e := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname=ANY($1)`, []string{"audit_logs_occurred_idx", "audit_logs_actor_occurred_idx", "audit_logs_result_occurred_idx"}).Scan(&count); e != nil || count != 3 {
				t.Fatal("original005 three indexes absent", count, e)
			}
			if mode != "fresh" {
				if e := r.pool.QueryRow(context.Background(), `SELECT NOT EXISTS(SELECT 1 FROM jsonb_array_elements($1::jsonb) old WHERE NOT EXISTS(SELECT 1 FROM schema_migrations now WHERE to_jsonb(now)=old))`, prior).Scan(&unchanged); e != nil || !unchanged {
					t.Fatal("preexisting ledger rewritten", mode, e)
				}
			}
		})
	}
}
