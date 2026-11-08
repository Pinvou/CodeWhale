package f06fixture

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestPrivateF06OwnerOptInBoundary(t *testing.T) {
	for _, scenario := range []string{"shared-billing-only", "invalid-explicit-owner", "remote-explicit-owner", "missing-owner-metadata"} {
		t.Run(scenario, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestPrivateF06OrganizationFundsPG$", "-test.v")
			for _, value := range os.Environ() {
				upper := strings.ToUpper(value)
				if !strings.HasPrefix(upper, "PINVOU_TEST_F06_OWNER_DATABASE_URL=") && !strings.HasPrefix(upper, "PINVOU_TEST_BILLING_DATABASE_URL=") && !strings.HasPrefix(upper, "PINVOU_PRIVATE_OWNER_DATA_DIRECTORY=") && !strings.HasPrefix(upper, "PINVOU_PRIVATE_OWNER_USER=") {
					command.Env = append(command.Env, value)
				}
			}
			command.Env = append(command.Env, "PINVOU_TEST_BILLING_DATABASE_URL=invalid-shared-url")
			if scenario == "invalid-explicit-owner" {
				command.Env = append(command.Env, "PINVOU_TEST_F06_OWNER_DATABASE_URL=invalid-owner-url")
			}
			if scenario == "remote-explicit-owner" {
				command.Env = append(command.Env, "PINVOU_TEST_F06_OWNER_DATABASE_URL=postgres://fixture@192.0.2.1/postgres?sslmode=disable")
			}
			if scenario == "missing-owner-metadata" {
				command.Env = append(command.Env, "PINVOU_TEST_F06_OWNER_DATABASE_URL=postgres://fixture@localhost/postgres?sslmode=disable")
			}
			output, err := command.CombinedOutput()
			if scenario == "shared-billing-only" {
				if err != nil || !strings.Contains(string(output), "--- SKIP: TestPrivateF06OrganizationFundsPG") {
					t.Fatal("shared billing configuration enabled private owner orchestration")
				}
			} else if err == nil || !strings.Contains(string(output), "--- FAIL: TestPrivateF06OrganizationFundsPG") || strings.Contains(string(output), "--- SKIP:") {
				t.Fatal("invalid explicit owner configuration did not fail closed")
			}
		})
	}
}
