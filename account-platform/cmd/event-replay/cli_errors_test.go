package main

import (
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestEventReplayCLIErrors(t *testing.T) {
	if os.Getenv("F18_SYNTHETIC_CLI_PROCESS") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
				break
			}
		}
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		main()
		return
	}
	cases := []struct {
		name, dsn, want string
		args            []string
	}{
		{"invalid_url_redacted", "postgres://synthetic_identity:synthetic_password@synthetic_address.invalid/synthetic_database?sslmode=synthetic_invalid", "EVENT_REPLAY_STORE_UNAVAILABLE", []string{"--event-id", "00000000-0000-4000-8000-000000000001", "--inspect"}},
		{"invalid_keywords_redacted", "host=synthetic_address.invalid user=synthetic_identity password=synthetic_password dbname=synthetic_database sslmode=synthetic_invalid", "EVENT_REPLAY_STORE_UNAVAILABLE", []string{"--event-id", "00000000-0000-4000-8000-000000000001", "--inspect"}},
		{"invalid_arguments_preserved", "", "--event-id must be a UUID", []string{"--event-id", "bad-id"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			args := append([]string{"-test.run=^TestEventReplayCLIErrors$", "--"}, tc.args...)
			cmd := exec.Command(exe, args...)
			for _, e := range os.Environ() {
				if !strings.HasPrefix(e, "DATABASE_URL=") && !strings.HasPrefix(e, "F18_SYNTHETIC_CLI_PROCESS=") {
					cmd.Env = append(cmd.Env, e)
				}
			}
			cmd.Env = append(cmd.Env, "DATABASE_URL="+tc.dsn, "F18_SYNTHETIC_CLI_PROCESS=1")
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatal("failed operation exited successfully")
			}
			text := string(output)
			for _, marker := range []string{"synthetic_identity", "synthetic_password", "synthetic_address", "synthetic_database", "postgres://"} {
				if strings.Contains(text, marker) {
					t.Errorf("CLI exposed synthetic connection field %s", marker)
				}
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("expected bounded diagnostic %q, got %q", tc.want, text)
			}
		})
	}
}
