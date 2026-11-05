package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagedReleaseInputsBindProviderAndPrivateCredentials(t *testing.T) {
	base := []string{"--operation", "inventory", "--expected-database", "token_fixture", "--database-provider", "aliyun-rds", "--expected-dba", "fixture_operator", "--expected-server-host", "database.example.invalid"}
	if _, err := parseRequest(base); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--expected-database", "token_fixture", "--database-provider", "other-cloud"},
		{"--expected-database", "token_fixture", "--database-provider", "aliyun-rds"},
		{"--expected-database", "token_fixture", "--expected-dba", "fixture_operator"},
		append(append([]string{}, base...), "--candidate-credentials-file", "unneeded-file"),
	} {
		if _, err := parseRequest(args); err == nil {
			t.Fatal("incomplete or unrelated managed bindings accepted")
		}
	}
	request := releaseRequest{Operation: "enable", Database: "token_fixture", Provider: "aliyun-rds", ExpectedDBA: "fixture_operator", ExpectedHost: "database.example.invalid", Candidates: []string{"fixture_a", "fixture_b"}, CredentialsFile: filepath.Join(t.TempDir(), "credentials.json")}
	value := managedCredentials{Version: 1, Host: request.ExpectedHost, Database: request.Database, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Second), Credentials: []managedCredential{{Role: "fixture_a", Password: strings.Repeat("synthetic-a-", 3)}, {Role: "fixture_b", Password: strings.Repeat("synthetic-b-", 3)}}}
	write := func(value managedCredentials) {
		body, _ := json.Marshal(value)
		if err := os.WriteFile(request.CredentialsFile, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(value)
	if !privateCredentialPlatformSupported {
		if _, err := readManagedCredentials(request); err == nil || err.Error() != "TOKEN_RELEASE_PRIVATE_CREDENTIAL_PLATFORM_UNSUPPORTED" {
			t.Fatal("unsupported private-file platform was silently accepted")
		}
		return
	}
	if _, err := readManagedCredentials(request); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*managedCredentials){
		func(v *managedCredentials) { v.Host = "other.example.invalid" },
		func(v *managedCredentials) { v.Database = "other_fixture" },
		func(v *managedCredentials) { v.ExpiresAt = time.Now().Add(-time.Minute) },
		func(v *managedCredentials) { v.ExpiresAt = time.Now().Add(91 * 24 * time.Hour) },
		func(v *managedCredentials) { v.Credentials[0].Role = "fixture_b" },
		func(v *managedCredentials) { v.Credentials[0].Password = "short" },
	} {
		invalid := value
		invalid.Credentials = append([]managedCredential(nil), value.Credentials...)
		change(&invalid)
		write(invalid)
		if _, err := readManagedCredentials(request); err == nil {
			t.Fatal("wrong target, credential, role set or validity accepted")
		}
	}
}

func TestManagedBindingRejectsBeforeAnyNetworkAuthentication(t *testing.T) {
	for _, test := range []struct{ name, settings, expectedUser, expectedHost string }{
		{"unverified transport", "host=127.0.0.1 user=fixture_operator sslmode=disable", "fixture_operator", "127.0.0.1"},
		{"wrong login", "host=127.0.0.1 user=other_fixture sslmode=verify-full", "fixture_operator", "127.0.0.1"},
		{"wrong target", "host=127.0.0.1 user=fixture_operator sslmode=verify-full", "fixture_operator", "other.example.invalid"},
		{"alternate host", "host=127.0.0.1,localhost user=fixture_operator sslmode=verify-full", "fixture_operator", "127.0.0.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			accepted := make(chan bool, 1)
			go func() {
				connection, err := listener.Accept()
				if err == nil {
					connection.Close()
				}
				accepted <- err == nil
			}()
			port := listener.Addr().(*net.TCPAddr).Port
			t.Setenv("TOKEN_RELEASE_DBA_DATABASE_URL", fmt.Sprintf("%s port=%d dbname=token_fixture password=synthetic_fixture_only", test.settings, port))
			err = run([]string{"--expected-database", "token_fixture", "--database-provider", "aliyun-rds", "--expected-dba", test.expectedUser, "--expected-server-host", test.expectedHost})
			listener.Close()
			if dialed := <-accepted; dialed || err == nil || err.Error() != "TOKEN_RELEASE_MANAGED_BINDING_MISMATCH" {
				t.Fatal("unbound managed configuration reached the authentication transport")
			}
		})
	}
}
