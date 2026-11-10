package billingschema

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Opt-in uses the same exclusively owned public-role fixture as sparse schema
// acceptance. It never exports the coordinator database or a production site.
func TestHistoryExportPublicDomainsPostgres(t *testing.T) {
	shell := os.Getenv("PINVOU_HISTORY_EXPORT_TEST_PWSH")
	if shell == "" {
		t.Skip("explicit PowerShell 7 history-export fixture is required")
	}
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Fatal("history export requires its isolated PostgreSQL fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	roles := []string{"token_gateway", "token_schema_owner", "token_cap_policy_deployer", "token_migrator", "token_policy_operator", "history_export_reader"}
	var existing int
	if err := f.admin.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&existing); err != nil || existing != 0 {
		t.Fatal("history fixture actors must be absent")
	}
	for _, role := range roles {
		login := "LOGIN"
		if strings.HasSuffix(role, "_owner") || role == "token_cap_policy_deployer" {
			login = "NOLOGIN"
		}
		if _, err := f.admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" "+login+" NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal(err)
		}
		f.roles = append(f.roles, role)
	}
	if _, err := f.admin.Exec(ctx, `GRANT token_gateway,token_schema_owner,token_cap_policy_deployer TO token_migrator`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, err := f.admin.Exec(clean, `REVOKE pg_read_all_stats FROM token_migrator`); err != nil {
			t.Error("owned Token bootstrap membership cleanup failed")
		}
	})
	root := t.TempDir()
	// Make the custodial parent private before any archive bytes are written.
	privacy := `param($p) if($IsWindows){$sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value;$a=[Security.AccessControl.DirectorySecurity]::new();$a.SetSecurityDescriptorSddlForm(('O:{0}D:P(A;OICI;FA;;;{0})(A;OICI;FA;;;SY)' -f $sid));Set-Acl -LiteralPath $p -AclObject $a}else{[IO.File]::SetUnixFileMode($p,[IO.UnixFileMode]448)}`
	cmd := exec.CommandContext(ctx, shell, "-NoProfile", "-Command", "& {"+privacy+"} '"+strings.ReplaceAll(root, "'", "''")+"'")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("private fixture directory: %v %s", err, output)
	}
	script, err := filepath.Abs("../../../scripts/export-account-history.ps1")
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"account", "billing", "token"} {
		t.Run(domain, func(t *testing.T) {
			owner := domain + "_service"
			if domain == "billing" {
				owner = "billing_schema_owner"
			}
			if domain == "token" {
				owner = f.config.User
			}
			conn := f.database(t, ctx, domain, owner)
			loaded, err := migrations.Load("../../migrations/" + domain)
			if err != nil {
				t.Fatal(err)
			}
			switch domain {
			case "account":
				err = accountschema.Up(ctx, conn, loaded)
			case "billing":
				err = Up(ctx, conn, loaded)
			case "token":
				bootstrap, e := os.ReadFile("../../deploy/production/token-database-bootstrap.sql")
				if e != nil {
					t.Fatal(e)
				}
				if _, e = conn.Exec(ctx, string(bootstrap)); e != nil {
					t.Fatal(e)
				}
				cfg := conn.Config().Copy()
				cfg.User = "token_migrator"
				cfg.Password = ""
				migrator, e := pgx.ConnectConfig(ctx, cfg)
				if e != nil {
					t.Fatal(e)
				}
				f.connections = append(f.connections, migrator)
				if _, e = migrator.Exec(ctx, `SET ROLE token_gateway`); e != nil {
					t.Fatal(e)
				}
				err = tokenschema.Up(ctx, migrator, loaded)
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg := f.config.Copy()
			cfg.Database = conn.Config().Database
			operator, e := pgx.ConnectConfig(ctx, cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer operator.Close(context.Background())
			event := uuid.NewString()
			if _, e = operator.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at) VALUES($1,'history.fixture.v1',$1,1,'{"evidence":"preserve-original"}','history-fixture',now())`, event); e != nil {
				t.Fatal(e)
			}
			tables := "public.schema_migrations,public.outbox_events"
			if domain == "billing" {
				tables += ",public.payment_provider_health_observations"
				if _, e = operator.Exec(ctx, `INSERT INTO payment_provider_attempts(id,provider_attempt_id,request_fingerprint,provider_id,provider_version,capability,channel,route,trusted_probe,created_at) VALUES(gen_random_uuid(),'history-attempt',repeat('a',64),'fixture','v1','PAY','fixture','fixture',true,now()); INSERT INTO payment_provider_health_observations(id,provider_attempt_id,provider_id,provider_version,capability,channel,route,trusted_probe,outcome,classification_rule_version,evidence_digest,received_at) VALUES(gen_random_uuid(),'history-attempt','fixture','v1','PAY','fixture','fixture',true,'SUCCESS','fixture/v1',repeat('b',64),now())`); e != nil {
					t.Fatal(e)
				}
			}
			if _, e = operator.Exec(ctx, "GRANT USAGE ON SCHEMA public TO history_export_reader; GRANT SELECT ON "+tables+" TO history_export_reader"); e != nil {
				t.Fatal(e)
			}
			baseEnv := []string{}
			for _, item := range os.Environ() {
				if !strings.HasPrefix(strings.ToUpper(item), "PG") {
					baseEnv = append(baseEnv, item)
				}
			}
			env := append(baseEnv, "PGHOST="+cfg.Host, "PGPORT="+strconv.Itoa(int(cfg.Port)), "PGDATABASE="+cfg.Database, "PGUSER=history_export_reader", "PGPASSWORD=", "PGSSLMODE=disable")
			run := func(dest, selected string, verify bool, timeout string) ([]byte, error) {
				args := []string{"-NoProfile", "-File", script, "-Domain", selected, "-OutputDirectory", dest, "-TimeoutSeconds", timeout}
				if tools := os.Getenv("PINVOU_HISTORY_EXPORT_TEST_PGTOOLS"); tools != "" {
					args = append(args, "-PgToolsDirectory", tools)
				}
				if verify {
					args = append(args, "-Verify")
				}
				c := exec.CommandContext(ctx, shell, args...)
				c.Env = env
				return c.CombinedOutput()
			}
			dest := filepath.Join(root, domain)
			if output, e := run(dest, domain, false, "30"); e != nil || strings.TrimSpace(string(output)) != "ACCOUNT_HISTORY_EXPORT_COMPLETE" {
				t.Fatalf("real read-only export failed: %v %s", e, output)
			}
			manifestBytes, e := os.ReadFile(filepath.Join(dest, "manifest.json"))
			if e != nil {
				t.Fatal(e)
			}
			var manifest struct {
				Domain string `json:"domain"`
				Source struct {
					Role string `json:"database_role"`
				} `json:"source"`
				Tables []string `json:"tables"`
			}
			if e = json.Unmarshal(manifestBytes, &manifest); e != nil || manifest.Domain != domain || manifest.Source.Role != "history_export_reader" {
				t.Fatal("source identity missing")
			}
			restore := "pg_restore"
			if tools := os.Getenv("PINVOU_HISTORY_EXPORT_TEST_PGTOOLS"); tools != "" {
				restore = filepath.Join(tools, restore)
			}
			decoded := filepath.Join(dest, "fixture-data.sql")
			decode := exec.CommandContext(ctx, restore, "--file", decoded, filepath.Join(dest, "history.dump"))
			decode.Env = env
			if _, e := decode.CombinedOutput(); e != nil {
				t.Fatal("real archive extraction failed")
			}
			data, e := os.ReadFile(decoded)
			if e != nil || !strings.Contains(string(data), event) || !strings.Contains(string(data), "preserve-original") || !strings.Contains(string(data), "COPY public.schema_migrations") {
				t.Fatal("archive lost original event or migration evidence")
			}
			if domain == "billing" && (!strings.Contains(string(data), "COPY public.payment_provider_health_observations") || !strings.Contains(string(data), "history-attempt")) {
				t.Fatal("archive lost health observation evidence")
			}
			if output, e := run(dest, domain, true, "30"); e != nil || strings.TrimSpace(string(output)) != "ACCOUNT_HISTORY_EXPORT_VERIFIED" {
				t.Fatalf("verify: %v %s", e, output)
			}
			assertFailure := func(dest, selected string, verify bool, timeout string) {
				t.Helper()
				out, e := run(dest, selected, verify, timeout)
				if e == nil || strings.Contains(string(out), "COMPLETE") || strings.Contains(string(out), "VERIFIED") || strings.Contains(string(out), "preserve-original") {
					t.Fatalf("unsafe success or disclosure: %v %s", e, out)
				}
			}
			assertFailure(dest, domain, false, "30") // Never overwrite an accepted archive.
			weak := filepath.Join(root, domain+"-unprotected-parent")
			if e = os.Mkdir(weak, 0755); e != nil {
				t.Fatal(e)
			}
			assertFailure(filepath.Join(weak, "export"), domain, false, "30")
			wrong := "billing"
			if domain == "billing" {
				wrong = "account"
			}
			assertFailure(filepath.Join(root, domain+"-wrong"), wrong, false, "30")
			assertFailure(dest, wrong, true, "30")
			// Incorrect manifest metadata must fail even with a valid archive.
			changed := strings.Replace(string(manifestBytes), "account-history-export/v1", "account-history-export/unknown", 1)
			if e = os.WriteFile(filepath.Join(dest, "manifest.json"), []byte(changed), 0600); e != nil {
				t.Fatal(e)
			}
			assertFailure(dest, domain, true, "30")
			if e = os.WriteFile(filepath.Join(dest, "manifest.json"), manifestBytes, 0600); e != nil {
				t.Fatal(e)
			}
			// A corrupt archive must fail, then the same valid archive must recover.
			archive := filepath.Join(dest, "history.dump")
			original, e := os.ReadFile(archive)
			if e != nil {
				t.Fatal(e)
			}
			corrupt := append([]byte{}, original...)
			corrupt[len(corrupt)-8] ^= 1 // Leave the table catalog and file length intact.
			if e = os.WriteFile(archive, corrupt, 0600); e != nil {
				t.Fatal(e)
			}
			assertFailure(dest, domain, true, "30")
			if e = os.WriteFile(archive, original, 0600); e != nil {
				t.Fatal(e)
			}
			if output, e := run(dest, domain, true, "30"); e != nil {
				t.Fatalf("next verification after corrupt input: %v %s", e, output)
			}
			if _, e = operator.Exec(ctx, `ALTER TABLE public.outbox_events ENABLE ROW LEVEL SECURITY`); e != nil {
				t.Fatal(e)
			}
			assertFailure(filepath.Join(root, domain+"-rls"), domain, false, "30")
			if _, e = operator.Exec(ctx, `ALTER TABLE public.outbox_events DISABLE ROW LEVEL SECURITY; REVOKE SELECT ON public.outbox_events FROM history_export_reader`); e != nil {
				t.Fatal(e)
			}
			assertFailure(filepath.Join(root, domain+"-denied"), domain, false, "30")
			if _, e = operator.Exec(ctx, `GRANT SELECT ON public.outbox_events TO history_export_reader`); e != nil {
				t.Fatal(e)
			}
			// Block the real SQL entry point; timeout must leave no accepted manifest
			// and the next invocation must work after the transaction releases.
			tx, e := operator.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, `LOCK TABLE public.outbox_events IN ACCESS EXCLUSIVE MODE`); e != nil {
				t.Fatal(e)
			}
			timed := filepath.Join(root, domain+"-timeout")
			assertFailure(timed, domain, false, "1")
			if _, e = os.Stat(filepath.Join(timed, "manifest.json")); !os.IsNotExist(e) {
				t.Fatal("timeout produced accepted manifest")
			}
			if e = tx.Rollback(ctx); e != nil {
				t.Fatal(e)
			}
			if output, e := run(filepath.Join(root, domain+"-next"), domain, false, "30"); e != nil {
				t.Fatalf("next export after dependency recovery: %v %s", e, output)
			}
			var unchanged int
			if e = operator.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_id=$1 AND payload->>'evidence'='preserve-original' AND published_at IS NULL AND attempts=0`, event).Scan(&unchanged); e != nil || unchanged != 1 {
				t.Fatal("export changed delivery evidence")
			}
			if domain == "billing" {
				if e = operator.QueryRow(ctx, `SELECT count(*) FROM payment_provider_health_observations WHERE provider_attempt_id='history-attempt' AND outcome='SUCCESS' AND evidence_digest=repeat('b',64)`).Scan(&unchanged); e != nil || unchanged != 1 {
					t.Fatal("export changed health evidence")
				}
			}
		})
	}
}
