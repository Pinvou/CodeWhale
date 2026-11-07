package billing

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
	"github.com/Pinvou/pinvou3/account-platform/internal/f17fixture"
	"github.com/jackc/pgx/v5"
)

// f18OwnedConnectionConfig admits an explicitly declared native disposable
// instance or this job's already admitted CI service. The integration root
// retains its existing default dedicated-instance guard.
func f18OwnedConnectionConfig(t *testing.T, raw string) (*pgx.ConnConfig, string, int) {
	t.Helper()
	if os.Getenv("F18_TEST_DATABASE_PROFILE") == "ci-owned" {
		config, err := pgx.ParseConfig(raw)
		port, system, directory := os.Getenv("PINVOU_ACCOUNT_CI_PG_PORT"), os.Getenv("PINVOU_ACCOUNT_CI_PG_SYSTEM_ID"), os.Getenv("PINVOU_ACCOUNT_CI_PG_DIRECTORY")
		if err != nil || !f18CIConfigurationAllowed(config, port, system, directory, os.Getenv("GITHUB_ACTIONS") == "true") {
			t.Fatal("F18 PostgreSQL URL is not the admitted CI coordinator")
		}
		// The admitted client port is mapped to the container's server port.
		return config, directory, 5432
	}
	if os.Getenv("F18_TEST_DATABASE_PROFILE") != f17fixture.OwnedProfile {
		t.Fatal("unknown F18_TEST_DATABASE_PROFILE")
	}
	expectedUser := strings.TrimSpace(os.Getenv("F18_TEST_DATABASE_USER"))
	expectedDirectory := filepath.Clean(strings.TrimSpace(os.Getenv("F18_TEST_DATA_DIRECTORY")))
	portText := strings.TrimSpace(os.Getenv("F18_TEST_DATABASE_PORT"))
	instance := f17fixture.OwnedInstance{User: expectedUser, Port: portText, Directory: expectedDirectory, Prefix: "pinvou-f18-billing-"}
	config, err := pgx.ParseConfig(raw)
	if err != nil || !instance.ConfigurationAllowed(config) {
		t.Fatal("F18 PostgreSQL URL is not the declared local-owned instance")
	}
	return config, expectedDirectory, int(config.Port)
}

func f18CIConfigurationAllowed(config *pgx.ConnConfig, port, system, directory string, githubActions bool) bool {
	return githubActions && cifixture.ConfigurationAllowed(config, port, system, directory) && config.Database == "postgres" && directory == "/var/lib/postgresql/data"
}

func f18ConnectOwned(ctx context.Context, config *pgx.ConnConfig) (*pgx.Conn, error) {
	if os.Getenv("F18_TEST_DATABASE_PROFILE") == "ci-owned" {
		// Verify system identifier, server directory/port and both principals on
		// this very backend before the integration root can create any actor.
		return cifixture.ConnectConfig(ctx, config)
	}
	return pgx.ConnectConfig(ctx, config)
}

func TestF18CIConfigurationBindsAdmittedService(t *testing.T) {
	raw := "postgres://postgres@127.0.0.1:49173/postgres?sslmode=disable"
	config, err := pgx.ParseConfig(raw)
	if err != nil || !f18CIConfigurationAllowed(config, "49173", "1234567", "/var/lib/postgresql/data", true) {
		t.Fatal("admitted PostgreSQL16 client configuration rejected", err)
	}
	for _, candidate := range []string{
		strings.Replace(raw, "127.0.0.1", "localhost", 1), strings.Replace(raw, "49173", "49174", 1),
		strings.Replace(raw, "postgres@", "other@", 1), strings.Replace(raw, "postgres@", "postgres:secret@", 1),
		strings.Replace(raw, "/postgres?", "/deployment?", 1), strings.Replace(raw, "disable", "require", 1),
		raw + "&application_name=extra", raw + "&host=192.0.2.1",
	} {
		parsed, err := pgx.ParseConfig(candidate)
		if err == nil && f18CIConfigurationAllowed(parsed, "49173", "1234567", "/var/lib/postgresql/data", true) {
			t.Fatal("unadmitted CI client configuration accepted")
		}
	}
	for _, fields := range [][3]string{{"49174", "1234567", "/var/lib/postgresql/data"}, {"49173", "0", "/var/lib/postgresql/data"}, {"49173", "", "/var/lib/postgresql/data"}, {"49173", "1234567", "/other/data"}} {
		if f18CIConfigurationAllowed(config, fields[0], fields[1], fields[2], true) {
			t.Fatal("incomplete or wrong CI identity accepted")
		}
	}
	if f18CIConfigurationAllowed(config, "49173", "1234567", "/var/lib/postgresql/data", false) {
		t.Fatal("CI profile accepted outside GitHub Actions")
	}
}

func f18OwnedDirectoryMatches(actual, expected string) bool {
	return (f17fixture.OwnedInstance{Directory: expected, Prefix: "pinvou-f18-billing-"}).DirectoryMatches(actual)
}

// Exercise the real integration root without a database: the original default
// must attempt its explicitly configured connection, not silently skip it.
func TestF18ConfiguredDefaultDoesNotSkip(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	attempted := make(chan bool, 1)
	go func() {
		connection, err := listener.Accept()
		attempted <- err == nil
		if err == nil {
			connection.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestF18HoldRecoveryUsesTrustedFence$", "-test.v")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "PINVOU_TEST_BILLING_DATABASE_URL") || strings.HasPrefix(strings.ToUpper(key), "F18_") || strings.HasPrefix(strings.ToUpper(key), "PG") {
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "F18_TEST_DATABASE_PROFILE=", "PINVOU_TEST_BILLING_DATABASE_URL=postgres://legacy_probe@"+listener.Addr().String()+"/postgres?sslmode=disable")
	output, err := command.CombinedOutput()
	listener.Close()
	connected := <-attempted
	var exit *exec.ExitError
	if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 1 || !connected || !strings.Contains(string(output), "owned coordinator unavailable") {
		t.Fatalf("configured default must reach the owned connection and fail, not skip: connected=%v err=%v output=%s", connected, err, output)
	}
}
