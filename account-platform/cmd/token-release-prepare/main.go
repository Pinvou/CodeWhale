// token-release-prepare is a short-lived, explicit DBA utility, never a service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var roleName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
var evidenceReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type releaseRequest struct {
	Operation     string
	Database      string
	Approval      string
	Maintenance   string
	ExpectedEpoch int64
	Retired       []string
	Candidates    []string
	Apply         bool
	Terminate     bool
	Migrations    string
}

type releaseEvidence struct {
	Operation         string `json:"operation"`
	Approval          string `json:"approval_reference,omitempty"`
	Decision          string `json:"decision"`
	State             string `json:"state"`
	Epoch             int64  `json:"epoch"`
	MigrationVersion  int64  `json:"migration_version"`
	WriterLogins      int    `json:"writer_logins"`
	WriterSessions    int    `json:"writer_sessions"`
	Applied           bool   `json:"applied"`
	EnvironmentStatus string `json:"environment_status"`
}

func coded(code string) error { return errors.New("TOKEN_RELEASE_" + code) }

func (r releaseRequest) validate() error {
	if !slices.Contains([]string{"inventory", "fence", "prepare", "enable", "account-policy-check"}, r.Operation) || !roleName.MatchString(r.Database) {
		return coded("INPUT_INVALID")
	}
	if r.Apply && slices.Contains([]string{"inventory", "account-policy-check"}, r.Operation) || r.Terminate && (r.Operation != "fence" || !r.Apply) {
		return coded("OPERATION_INVALID")
	}
	if slices.Contains([]string{"inventory", "account-policy-check"}, r.Operation) && (len(r.Retired) != 0 || len(r.Candidates) != 0 || r.Approval != "" || r.Maintenance != "" || r.ExpectedEpoch > 0) {
		return coded("OPERATION_INVALID")
	}
	if r.Operation == "fence" && len(r.Candidates) != 0 || r.Operation == "enable" && len(r.Retired) != 0 || r.Operation != "enable" && r.ExpectedEpoch > 0 {
		return coded("OPERATION_INVALID")
	}
	if r.Operation != "inventory" && r.Operation != "account-policy-check" && (!evidenceReference.MatchString(r.Approval) || !evidenceReference.MatchString(r.Maintenance)) {
		return coded("APPROVAL_REQUIRED")
	}
	for _, names := range [][]string{r.Retired, r.Candidates} {
		if !slices.IsSorted(names) {
			return coded("ROLE_SET_INVALID")
		}
		for i, name := range names {
			if !roleName.MatchString(name) || i > 0 && names[i-1] == name {
				return coded("ROLE_SET_INVALID")
			}
		}
	}
	for _, name := range r.Candidates {
		if slices.Contains(r.Retired, name) || protectedRole(name) || name == "token_gateway" {
			return coded("ROLE_SET_INVALID")
		}
	}
	for _, name := range r.Retired {
		if protectedRole(name) {
			return coded("RETIRED_ROLE_PROTECTED")
		}
	}
	if r.Operation == "fence" && len(r.Retired) == 0 || slices.Contains([]string{"prepare", "enable"}, r.Operation) && len(r.Candidates) != 2 || r.Operation == "enable" && r.ExpectedEpoch < 1 {
		return coded("ROLE_SET_INVALID")
	}
	return nil
}

func protectedRole(name string) bool {
	return strings.HasPrefix(name, "pg_") || slices.Contains([]string{"token_schema_owner", "token_cap_policy_deployer", "token_migrator", "token_policy_operator"}, name)
}

func parseRequest(args []string) (releaseRequest, error) {
	r := releaseRequest{}
	flags := flag.NewFlagSet("token-release-prepare", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var retired, candidates string
	flags.StringVar(&r.Operation, "operation", "inventory", "inventory, fence, prepare, enable, account-policy-check")
	flags.StringVar(&r.Database, "expected-database", "", "exact target database")
	flags.StringVar(&r.Approval, "approval-reference", "", "approved operation reference")
	flags.StringVar(&r.Maintenance, "maintenance-reference", "", "exclusive DBA maintenance window reference")
	flags.StringVar(&r.Migrations, "migrations", "migrations/token", "packaged Token migrations")
	flags.StringVar(&retired, "retired-roles", "", "exact sorted retired roles")
	flags.StringVar(&candidates, "candidate-roles", "", "exact sorted gateway and credential roles")
	flags.Int64Var(&r.ExpectedEpoch, "expected-epoch", -1, "ACTIVE epoch for enable")
	flags.BoolVar(&r.Apply, "apply", false, "perform this operation only")
	flags.BoolVar(&r.Terminate, "terminate-sessions", false, "after fencing, terminate only approved retired sessions")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return r, coded("INPUT_INVALID")
	}
	if retired != "" {
		r.Retired = strings.Split(retired, ",")
	}
	if candidates != "" {
		r.Candidates = strings.Split(candidates, ",")
	}
	return r, r.validate()
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Never expose pgx/SQL/connection errors or flag values.
		message := err.Error()
		if !regexp.MustCompile(`^TOKEN_RELEASE_[A-Z_]+$`).MatchString(message) {
			message = "TOKEN_RELEASE_FAILED"
		}
		fmt.Fprintln(os.Stderr, message)
		os.Exit(1)
	}
}

func run(args []string) error {
	r, err := parseRequest(args)
	if err != nil {
		return err
	}
	environment := "TOKEN_RELEASE_DBA_DATABASE_URL"
	if r.Operation == "account-policy-check" {
		environment = "ACCOUNT_POLICY_DATABASE_URL"
	}
	if strings.TrimSpace(os.Getenv(environment)) == "" {
		return coded("CONNECTION_REQUIRED")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(os.Getenv(environment))
	if err != nil {
		return coded("CONNECTION_INVALID")
	}
	config.RuntimeParams["search_path"] = "pg_catalog,public"
	config.RuntimeParams["application_name"] = "pinvou-token-release-prepare"
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return coded("CONNECTION_UNAVAILABLE")
	}
	defer connection.Close(context.Background())
	evidence, err := executeRelease(ctx, connection, r)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(evidence)
}
