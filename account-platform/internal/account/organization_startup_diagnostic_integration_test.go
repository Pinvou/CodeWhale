package account

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Run the real account-service product entry against the original public
// Account fixture and its actual non-superuser runtime login. Authorities are
// only configured: no payment/model request or live identity provider is used.
func TestOrganizationStartupDiagnosticsPostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	repository := f.repository(t)
	ctx, cancel := context.WithTimeout(f.ctx, 90*time.Second)
	defer cancel()
	goTool := os.Getenv("PINVOU_ACCOUNT_TEST_GO")
	if goTool == "" {
		goTool = "go"
	}
	product := filepath.Join(t.TempDir(), "account-service")
	if runtime.GOOS == "windows" {
		product += ".exe"
	}
	build := exec.CommandContext(ctx, goTool, "build", "-race", "-tags=nomsgpack", "-mod=readonly", "-o", product, "./cmd/account-service")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build actual account-service: %v\n%s", err, output)
	}
	endpoint, err := url.Parse(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if err != nil {
		t.Fatal("parse owned startup database")
	}
	endpoint.User = url.User("account_service")
	endpoint.Path = "/" + repository.pool.Config().ConnConfig.Database
	// Preserve only OS execution inputs. Inherited application configuration,
	// credentials and telemetry endpoints must not select external services.
	var systemEnvironment []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(key) {
		case "SYSTEMROOT", "WINDIR", "PATH", "PATHEXT", "TEMP", "TMP", "HOME", "USERPROFILE", "COMSPEC", "LD_LIBRARY_PATH":
			systemEnvironment = append(systemEnvironment, value)
		}
	}
	for _, item := range []struct {
		name, profile, billing, token, want string
	}{
		{"profile", "private-invalid-profile", "http://127.0.0.1:1", "http://127.0.0.1:1", "ACCOUNT_ORGANIZATION_PROFILE_INVALID"},
		{"token", "models", "http://127.0.0.1:1", "private-invalid-token", "ACCOUNT_ORGANIZATION_TOKEN_AUTHORITY_INVALID"},
		// The unchanged earlier wallet-authority guard precedes organization
		// assembly for an invalid Billing URL. It must keep its original code.
		{"billing-earlier-wallet-guard", "models", "private-invalid-billing", "http://127.0.0.1:1", "ACCOUNT_WALLET_AUTHORITY_INVALID"},
	} {
		t.Run(item.name, func(t *testing.T) {
			bounded, stop := context.WithTimeout(ctx, 10*time.Second)
			defer stop()
			command := exec.CommandContext(bounded, product)
			command.Env = append(append([]string{}, systemEnvironment...),
				"APP_ENV=test", "DATABASE_URL="+endpoint.String(), "INTERNAL_SERVICE_TOKEN=controlled-startup-workload",
				"OIDC_ISSUER=http://127.0.0.1:1", "OIDC_AUDIENCE=controlled-startup",
				"IDENTITY_HTTP_URL=http://127.0.0.1:1", "OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64="+base64.StdEncoding.EncodeToString(make([]byte, 32)),
				"ACCOUNT_PLATFORM_PROFILE="+item.profile, "BILLING_HTTP_URL="+item.billing, "TOKEN_HTTP_URL="+item.token)
			output, err := command.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 || bounded.Err() != nil || !strings.Contains(string(output), item.want) {
				t.Fatalf("actual startup did not fail with %s: %v\n%s", item.want, err, output)
			}
			if strings.Contains(string(output), "private-invalid") || strings.Contains(string(output), "controlled-startup-workload") || strings.Contains(string(output), "ACCOUNT_ORGANIZATION_AUTHORITY_INVALID") {
				t.Fatal("startup exposed private input or collapsed a known diagnostic category")
			}
			classified := 0
			for _, line := range strings.Split(string(output), "\n") {
				var event map[string]any
				if json.Unmarshal([]byte(line), &event) == nil && event["component"] == "organization-startup" {
					if event["error_code"] != item.want || event["level"] != "ERROR" || event["result"] != "failure" {
						t.Fatal("startup did not preserve the structured failure contract")
					}
					classified++
				}
			}
			if classified != 1 {
				t.Fatal("actual process did not publish one bounded structured error_code")
			}
			var actor string
			var super bool
			var users int
			if err := repository.pool.QueryRow(ctx, `SELECT current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user),(SELECT count(*) FROM users)`).Scan(&actor, &super, &users); err != nil || actor != "account_service" || super || users != 0 {
				t.Fatal("startup validation changed the empty runtime fixture or its authority", err)
			}
		})
	}
}
