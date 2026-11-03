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

	"github.com/Pinvou/pinvou3/account-platform/internal/f17fixture"
	"github.com/jackc/pgx/v5"
)

// f18OwnedConnectionConfig admits only an explicitly declared, loopback-owned
// disposable PostgreSQL instance for the explicit local-owned profile. The
// integration root retains its existing default dedicated-instance guard.
func f18OwnedConnectionConfig(t *testing.T, raw string) (*pgx.ConnConfig, string, int) {
	t.Helper()
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
