package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/postgrescontrol"
	"github.com/jackc/pgx/v5"
)

func (r releaseRequest) managedRDS() bool { return r.Provider == "aliyun-rds" }

func validateManagedConnection(config *pgx.ConnConfig, r releaseRequest) error {
	if config.User != r.ExpectedDBA || config.Host != r.ExpectedHost || len(config.Fallbacks) != 0 || config.TLSConfig == nil ||
		config.TLSConfig.InsecureSkipVerify || config.TLSConfig.ServerName != config.Host {
		return coded("MANAGED_BINDING_MISMATCH")
	}
	return nil
}

func verifyManagedDBA(ctx context.Context, connection *pgx.Conn, tx pgx.Tx, r releaseRequest) error {
	if err := validateManagedConnection(connection.Config(), r); err != nil {
		return err
	}
	var trusted bool
	if err := tx.QueryRow(ctx, `SELECT NOT r.rolsuper AND `+postgrescontrol.TrustedAdministratorSQL+`
 FROM pg_catalog.pg_roles r WHERE r.rolname=session_user`).Scan(&trusted); err != nil || !trusted {
		return coded("MANAGED_DBA_AUTHORITY_UNCONFIRMED")
	}
	return nil
}

// A platform database is not a blanket "cannot connect" exception. Prove its
// protected owner and absence of any access, dependencies or sessions for every
// affected role and all roles it can assume, using cluster-global catalogs.
func verifyRDSPlatformBoundary(ctx context.Context, connection *pgx.Conn, names []string) error {
	var isolated bool
	err := connection.QueryRow(ctx, `SELECT owner.rolsuper AND owner.rolname='aurora'
 AND NOT pg_catalog.has_database_privilege(session_user,d.oid,'CONNECT')
 AND NOT EXISTS (
 SELECT 1 FROM pg_catalog.pg_roles r WHERE r.rolname=ANY($1::text[]) AND EXISTS (
 SELECT 1 FROM pg_catalog.pg_roles g WHERE (r.oid=g.oid OR pg_catalog.pg_has_role(r.oid,g.oid,'MEMBER')) AND (
 pg_catalog.has_database_privilege(g.oid,d.oid,'CONNECT') OR g.oid=d.datdba OR
 EXISTS(SELECT 1 FROM pg_catalog.pg_shdepend dep WHERE dep.refclassid='pg_catalog.pg_authid'::regclass
 AND dep.refobjid=g.oid AND (dep.dbid=d.oid OR (dep.dbid=0 AND dep.classid='pg_catalog.pg_database'::regclass AND dep.objid=d.oid))) OR
 EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity a WHERE a.usesysid=g.oid AND a.datid=d.oid))))
 FROM pg_catalog.pg_database d JOIN pg_catalog.pg_roles owner ON owner.oid=d.datdba WHERE d.datname='rdsadmin'`, names).Scan(&isolated)
	if err != nil || !isolated {
		return coded("PLATFORM_DATABASE_BOUNDARY_UNCONFIRMED")
	}
	return nil
}

type managedCredential struct {
	Role     string `json:"role"`
	Password string `json:"password"`
}

type managedCredentials struct {
	Version     int                 `json:"version"`
	Host        string              `json:"host"`
	Database    string              `json:"database"`
	ExpiresAt   time.Time           `json:"expires_at"`
	Credentials []managedCredential `json:"credentials"`
}

func readManagedCredentials(r releaseRequest) (managedCredentials, error) {
	var value managedCredentials
	if !privateCredentialPlatformSupported {
		return value, coded("PRIVATE_CREDENTIAL_PLATFORM_UNSUPPORTED")
	}
	path := r.CredentialsFile
	resolved, err := filepath.EvalSymlinks(path)
	info, statErr := os.Lstat(path)
	valid := func(info os.FileInfo) bool {
		return info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= 8192 &&
			privateCredentialMetadata(info)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || err != nil || resolved != path || statErr != nil || !valid(info) {
		return value, coded("PRIVATE_CREDENTIAL_FILE_REQUIRED")
	}
	file, err := os.Open(path)
	if err != nil {
		return value, coded("PRIVATE_CREDENTIAL_FILE_REQUIRED")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !valid(opened) {
		return value, coded("PRIVATE_CREDENTIAL_FILE_REQUIRED")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 8193))
	defer clear(raw)
	if err != nil || len(raw) > 8192 {
		return value, coded("PRIVATE_CREDENTIAL_FILE_REQUIRED")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF || value.Version != 1 ||
		value.Host != r.ExpectedHost || value.Database != r.Database || len(value.Credentials) != 2 ||
		value.ExpiresAt.Nanosecond() != 0 ||
		!value.ExpiresAt.After(time.Now()) || value.ExpiresAt.After(time.Now().Add(90*24*time.Hour)) {
		return managedCredentials{}, coded("CREDENTIAL_BINDING_INVALID")
	}
	for i, credential := range value.Credentials {
		if !slices.Contains(r.Candidates, credential.Role) || (i > 0 && value.Credentials[i-1].Role >= credential.Role) ||
			len(credential.Password) < 32 || len(credential.Password) > 128 {
			return managedCredentials{}, coded("CREDENTIAL_BINDING_INVALID")
		}
		for _, c := range credential.Password {
			if c < 33 || c > 126 {
				return managedCredentials{}, coded("CREDENTIAL_BINDING_INVALID")
			}
		}
	}
	return value, nil
}

func installManagedCredentials(ctx context.Context, tx pgx.Tx, credentials managedCredentials) error {
	for _, credential := range credentials.Credentials {
		verifier, err := postgrescontrol.SCRAMVerifier(credential.Password)
		if err != nil {
			return coded("SCRAM_PREPARATION_FAILED")
		}
		// Verifier and UTC timestamp contain no SQL quote. Passwords never enter
		// SQL text, argv, evidence, or errors. Both roles are still NOLOGIN here.
		if _, err = tx.Exec(ctx, "ALTER ROLE "+pgx.Identifier{credential.Role}.Sanitize()+" PASSWORD '"+verifier+
			"' VALID UNTIL '"+credentials.ExpiresAt.UTC().Format(time.RFC3339)+"'"); err != nil {
			return coded("SCRAM_PREPARATION_FAILED")
		}
	}
	return nil
}

func probeManagedCredentials(ctx context.Context, connection *pgx.Conn, credentials managedCredentials) error {
	// Keep exactly the binding/passwords installed by this attempt. Reopening
	// a file after commit or repeating the one-minute admission condition
	// could misclassify a committed operation or prevent read-only recovery.
	for _, credential := range credentials.Credentials {
		if postgrescontrol.ProbeSCRAM(ctx, connection.Config(), credential.Role, credential.Password) != nil {
			return coded("ENABLE_COMMITTED_AUTHENTICATION_UNCONFIRMED")
		}
	}
	return nil
}
