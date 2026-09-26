package main

import (
	"context"
	"slices"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/jackc/pgx/v5"
)

func executeRelease(ctx context.Context, connection *pgx.Conn, r releaseRequest) (releaseEvidence, error) {
	result := releaseEvidence{Operation: r.Operation, Approval: r.Approval, Decision: "CHECKED", EnvironmentStatus: "ENV_PENDING"}
	if err := r.validate(); err != nil {
		return result, err
	}
	options := pgx.TxOptions{}
	if r.Operation == "inventory" {
		options.AccessMode = pgx.ReadOnly
	}
	tx, err := connection.BeginTx(ctx, options)
	if err != nil {
		return result, coded("TRANSACTION_UNAVAILABLE")
	}
	defer tx.Rollback(context.Background())
	var database, session, current string
	var super bool
	if err = tx.QueryRow(ctx, `SELECT current_database(),session_user,current_user,rolsuper FROM pg_catalog.pg_roles WHERE rolname=session_user`).Scan(&database, &session, &current, &super); err != nil || database != r.Database || session != current || session != connection.Config().User {
		return result, coded("DATABASE_OR_IDENTITY_MISMATCH")
	}
	if r.Operation == "account-policy-check" {
		if super {
			return result, coded("ACCOUNT_NON_SUPER_IDENTITY_REQUIRED")
		}
		if _, err = tx.Exec(ctx, `LOCK TABLE public.approval_command_policies IN SHARE MODE NOWAIT`); err != nil {
			return result, coded("ACCOUNT_POLICY_LOCK_DENIED")
		}
		if _, err = approvalpolicy.VerifyPolicySet(ctx, tx); err != nil {
			return result, coded("ACCOUNT_POLICY_SET_INVALID")
		}
		result.State = "ACCOUNT_POLICY_LOCK_VERIFIED"
		return result, nil // Always rollback, including the successful SHARE lock.
	}
	if !super {
		return result, coded("DBA_IDENTITY_REQUIRED")
	}
	if r.Operation != "inventory" {
		if _, err = tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('pinvou_token_release_prepare',0))`); err != nil {
			return result, coded("OPERATION_BUSY")
		}
		// Same lock as migrations.Up; try-only avoids holding a role operation
		// across an already running migration. The lock is transaction-scoped.
		var available bool
		if err = tx.QueryRow(ctx, `SELECT pg_catalog.pg_try_advisory_xact_lock(pg_catalog.hashtext('pinvou_account_platform_migrations'))`).Scan(&available); err != nil || !available {
			return result, coded("MIGRATION_BUSY")
		}
	}
	installed, err := installationSnapshot(ctx, tx, r.Migrations, r.Operation != "inventory")
	if err != nil {
		return result, err
	}
	result.State, result.Epoch, result.MigrationVersion = installed.State, installed.Epoch, installed.Version
	writers, err := writerInventory(ctx, tx)
	if err != nil {
		return result, err
	}
	for _, writer := range writers {
		result.WriterSessions += writer.Sessions // Inventory excludes only the current PID, not its login name.
		if writer.Name != session {
			if writer.Login && !writer.Super {
				result.WriterLogins++
			}
		}
	}
	if r.Operation == "inventory" {
		return result, nil
	}
	if installed.State == "ACTIVE" && r.Operation != "enable" {
		return result, coded("SAFE_REQUIRED")
	}
	if r.Operation == "enable" {
		if installed.State != "ACTIVE" || installed.Epoch != r.ExpectedEpoch || !slices.Equal(installed.Roles, r.Candidates) {
			return result, coded("ACTIVE_EPOCH_OR_ROLES_MISMATCH")
		}
		if err = noUnexpectedWriters(writers, session, r.Candidates, false); err != nil {
			return result, err
		}
	} else if err = noUnexpectedWriters(writers, session, r.Retired, r.Operation == "prepare"); err != nil {
		return result, err
	}
	if slices.Contains(r.Retired, session) || slices.Contains(r.Candidates, session) {
		return result, coded("DBA_TARGET_FORBIDDEN")
	}
	names := slices.Concat(r.Retired, r.Candidates, []string{"token_gateway"})
	if r.Operation == "prepare" {
		names = append(names, "token_schema_owner", "token_cap_policy_deployer", "token_migrator", "token_policy_operator")
	}
	if err = rejectCrossDatabaseRoles(ctx, connection, names); err != nil {
		return result, err
	}
	switch r.Operation {
	case "fence":
		err = fenceRoles(ctx, tx, r)
	case "prepare":
		err = prepareRoles(ctx, tx, r, installed)
	case "enable":
		var already bool
		already, err = enableRoles(ctx, tx, r)
		if already && err == nil {
			result.Decision = "ALREADY_ENABLED"
			return result, nil
		}
	}
	if err != nil {
		return result, err
	}
	if !r.Apply {
		return result, nil
	} // DDL was never issued in check mode.
	if err = tx.Commit(ctx); err != nil {
		return result, coded("COMMIT_UNKNOWN_QUERY_INVENTORY")
	}
	result.Applied = true
	if r.Operation == "fence" {
		if r.Terminate {
			// Role-name-bound fresh lookup, not stale PIDs; refuse other databases.
			if _, err = connection.Exec(ctx, `SELECT pg_catalog.pg_terminate_backend(pid) FROM pg_catalog.pg_stat_activity WHERE usename=ANY($1::text[]) AND datname=current_database() AND pid<>pg_backend_pid()`, r.Retired); err != nil {
				return result, coded("FENCED_SESSION_CLEANUP_INCOMPLETE")
			}
		}
		cleanupDeadline := time.Now().Add(2 * time.Second)
		for {
			var sessions int
			if err = connection.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE usename=ANY($1::text[]) AND pid<>pg_backend_pid()`, r.Retired).Scan(&sessions); err != nil {
				return result, coded("FENCED_SESSIONS_REMAIN")
			}
			if sessions == 0 {
				break
			}
			if !r.Terminate {
				return result, coded("FENCED_SESSIONS_REMAIN")
			}
			if time.Now().After(cleanupDeadline) {
				return result, coded("FENCED_SESSION_CLEANUP_INCOMPLETE")
			}
			select {
			case <-ctx.Done():
				return result, coded("FENCED_SESSION_CLEANUP_INCOMPLETE")
			case <-time.After(25 * time.Millisecond):
			}
		}
		result.WriterLogins, result.WriterSessions = 0, 0
	}
	return result, nil
}

func fenceRoles(ctx context.Context, tx pgx.Tx, r releaseRequest) error {
	for _, name := range r.Retired {
		var safe bool
		if err := tx.QueryRow(ctx, `SELECT NOT rolsuper AND NOT rolcreaterole AND NOT rolcreatedb AND NOT rolreplication AND NOT rolbypassrls FROM pg_catalog.pg_roles WHERE rolname=$1`, name).Scan(&safe); err != nil || !safe {
			return coded("RETIRED_ROLE_UNSAFE")
		}
	}
	if !r.Apply {
		return nil
	}
	for _, name := range r.Retired {
		if _, err := tx.Exec(ctx, "ALTER ROLE "+pgx.Identifier{name}.Sanitize()+" NOLOGIN"); err != nil {
			return coded("FENCE_FAILED")
		}
	}
	return nil
}

func prepareRoles(ctx context.Context, tx pgx.Tx, r releaseRequest, installed installation) error {
	if err := lockDomainTables(ctx, tx); err != nil {
		return err
	}
	var databaseOwner, schemaOwner string
	if err := tx.QueryRow(ctx, `SELECT pg_get_userbyid(datdba),(SELECT pg_get_userbyid(nspowner) FROM pg_catalog.pg_namespace WHERE nspname='public') FROM pg_catalog.pg_database WHERE datname=current_database()`).Scan(&databaseOwner, &schemaOwner); err != nil {
		return coded("OWNER_INVENTORY_UNAVAILABLE")
	}
	if !slices.Contains([]string{"token_gateway", "token_schema_owner"}, databaseOwner) || !slices.Contains([]string{"token_gateway", "token_schema_owner", "pg_database_owner"}, schemaOwner) {
		return coded("CONTROL_OWNER_UNEXPECTED")
	}
	type desiredRole struct {
		name    string
		inherit bool
		parents []string
	}
	desired := []desiredRole{{"token_gateway", true, nil}, {"token_schema_owner", true, nil}, {"token_cap_policy_deployer", true, nil}, {"token_migrator", false, []string{"pg_read_all_stats", "token_cap_policy_deployer", "token_gateway", "token_schema_owner"}}, {"token_policy_operator", false, nil}}
	for _, name := range r.Candidates {
		desired = append(desired, desiredRole{name, true, []string{"token_gateway"}})
	}
	missing := make(map[string]bool)
	for _, role := range desired {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=$1)`, role.name).Scan(&exists); err != nil {
			return coded("ROLE_INVENTORY_UNAVAILABLE")
		}
		if !exists {
			missing[role.name] = true
			continue
		}
		var safe bool
		if err := tx.QueryRow(ctx, `SELECT NOT rolcanlogin AND NOT rolsuper AND NOT rolcreaterole AND NOT rolcreatedb AND NOT rolreplication AND NOT rolbypassrls AND rolinherit=$2 FROM pg_catalog.pg_roles WHERE rolname=$1`, role.name, role.inherit).Scan(&safe); err != nil || !safe {
			return coded("EXISTING_ROLE_CONFLICT")
		}
		var parents []string
		if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(parent.rolname::text ORDER BY parent.rolname),'{}'::text[]) FROM pg_catalog.pg_auth_members m JOIN pg_catalog.pg_roles child ON child.oid=m.member JOIN pg_catalog.pg_roles parent ON parent.oid=m.roleid WHERE child.rolname=$1`, role.name).Scan(&parents); err != nil {
			return coded("ROLE_INVENTORY_UNAVAILABLE")
		}
		for _, parent := range parents {
			if !slices.Contains(role.parents, parent) {
				return coded("EXISTING_ROLE_MEMBERSHIP_CONFLICT")
			}
		}
		var delegated bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members m JOIN pg_catalog.pg_roles child ON child.oid=m.member WHERE child.rolname=$1 AND m.admin_option)`, role.name).Scan(&delegated); err != nil || delegated {
			return coded("EXISTING_ROLE_MEMBERSHIP_CONFLICT")
		}
		if slices.Contains(r.Candidates, role.name) {
			if len(parents) != 1 || parents[0] != "token_gateway" {
				return coded("CANDIDATE_ROLE_CONFLICT")
			}
			var inherited bool
			if err := tx.QueryRow(ctx, `SELECT pg_has_role($1::text,'token_gateway','USAGE') AND EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members m JOIN pg_catalog.pg_roles child ON child.oid=m.member JOIN pg_catalog.pg_roles parent ON parent.oid=m.roleid WHERE child.rolname=$1 AND parent.rolname='token_gateway' AND m.inherit_option AND m.set_option AND NOT m.admin_option)`, role.name).Scan(&inherited); err != nil || !inherited {
				return coded("CANDIDATE_ROLE_CONFLICT")
			}
			if installed.Version >= 31 {
				var used bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.token_command_governance_transitions WHERE state='ACTIVE' AND $1::text=ANY(approved_runtime_roles))`, role.name).Scan(&used); err != nil || used {
					return coded("CANDIDATE_ROLE_RETIRED")
				}
			}
		}
	}
	var extensionSchema string
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT n.nspname FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='pgcrypto'),'')`).Scan(&extensionSchema); err != nil || extensionSchema != "" && extensionSchema != "public" {
		return coded("EXTENSION_PLACEMENT_INVALID")
	}
	if !r.Apply {
		return nil
	}
	for _, role := range desired {
		if missing[role.name] {
			inherit := " INHERIT"
			if !role.inherit {
				inherit = " NOINHERIT"
			}
			if _, err := tx.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role.name}.Sanitize()+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"+inherit); err != nil {
				return coded("ROLE_PREPARATION_FAILED")
			}
		}
	}
	if _, err := tx.Exec(ctx, `GRANT token_gateway,token_cap_policy_deployer,token_schema_owner TO token_migrator`); err != nil {
		return coded("MIGRATOR_PREPARATION_FAILED")
	}
	for _, name := range r.Candidates {
		if _, err := tx.Exec(ctx, "GRANT token_gateway TO "+pgx.Identifier{name}.Sanitize()); err != nil {
			return coded("CANDIDATE_PREPARATION_FAILED")
		}
	}
	if _, err := tx.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{r.Database}.Sanitize()+" OWNER TO token_schema_owner"); err != nil {
		return coded("DATABASE_OWNER_PREPARATION_FAILED")
	}
	for _, sql := range []string{`ALTER SCHEMA public OWNER TO token_schema_owner`, `REVOKE CREATE ON SCHEMA public FROM PUBLIC`, `GRANT USAGE ON SCHEMA public TO token_gateway`, `CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public`} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return coded("SCHEMA_PREPARATION_FAILED")
		}
	}
	create := `REVOKE CREATE ON SCHEMA public FROM token_gateway`
	if installed.Version < 29 {
		create = `GRANT CREATE ON SCHEMA public TO token_gateway`
	}
	if _, err := tx.Exec(ctx, create); err != nil {
		return coded("SCHEMA_PREPARATION_FAILED")
	}
	return nil
}

func enableRoles(ctx context.Context, tx pgx.Tx, r releaseRequest) (bool, error) {
	logins := 0
	sessions := 0
	for _, name := range r.Candidates {
		var safe, login, credential bool
		// Every installed privileged entry point must still have its reviewed
		// catalog identity. This positive check intentionally runs separately from
		// the catch-all scan below: changing an expected function to SECURITY
		// INVOKER, moving its ownership, or dropping it must fail closed too.
		if err := tx.QueryRow(ctx, `SELECT r.rolcanlogin,
 r.rolinherit AND NOT r.rolsuper AND NOT r.rolcreaterole AND NOT r.rolcreatedb AND NOT r.rolreplication AND NOT r.rolbypassrls
 AND pg_has_role(r.oid,'token_gateway','USAGE')
 AND (SELECT count(*) FROM pg_catalog.pg_auth_members m JOIN pg_catalog.pg_roles p ON p.oid=m.roleid WHERE m.member=r.oid AND p.rolname='token_gateway' AND m.inherit_option AND m.set_option AND NOT m.admin_option)=1
 AND (SELECT count(*) FROM pg_catalog.pg_auth_members m WHERE m.member=r.oid)=1
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_roles p WHERE p.rolname IN ('token_schema_owner','token_cap_policy_deployer','token_migrator','token_policy_operator') AND pg_has_role(r.oid,p.oid,'MEMBER'))
 AND NOT pg_has_role(r.oid,(SELECT datdba FROM pg_catalog.pg_database WHERE datname=current_database()),'MEMBER')
 AND NOT has_schema_privilege(r.oid,'public','CREATE')
 AND NOT has_table_privilege(r.oid,'public.token_command_governance_activation','INSERT,UPDATE,DELETE,TRUNCATE')
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace JOIN pg_catalog.pg_roles owner ON owner.oid=c.relowner CROSS JOIN (VALUES('INSERT'),('UPDATE'),('DELETE'),('TRUNCATE')) AS rights(privilege) WHERE n.nspname='public' AND c.relkind IN ('r','p') AND owner.rolname IN ('token_schema_owner','token_cap_policy_deployer') AND has_table_privilege(r.oid,c.oid,rights.privilege) AND NOT (owner.rolname='token_cap_policy_deployer' AND c.relname IN ('token_account_provisioning_reservations','token_account_provisioning_recovery_actions') AND rights.privilege IN ('INSERT','UPDATE')))
 AND (NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=31) OR EXISTS(
   SELECT 1 FROM pg_catalog.pg_proc p
   JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
   JOIN pg_catalog.pg_roles owner ON owner.oid=p.proowner
   JOIN pg_catalog.pg_language language ON language.oid=p.prolang
   WHERE p.oid=to_regprocedure('public.lock_token_command_governance_activation()')
     AND n.nspname='public' AND owner.rolname='token_schema_owner'
     AND p.prokind='f' AND p.prosecdef AND language.lanname='sql'
     AND p.proconfig=ARRAY['search_path=pg_catalog']::text[]
     AND p.proretset AND p.prorettype=to_regtype('public.token_command_governance_activation')
     AND has_function_privilege(r.oid,p.oid,'EXECUTE')
 ))
 AND (NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=32) OR EXISTS(
   SELECT 1 FROM pg_catalog.pg_proc p
   JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
   JOIN pg_catalog.pg_roles owner ON owner.oid=p.proowner
   JOIN pg_catalog.pg_language language ON language.oid=p.prolang
   WHERE p.oid=to_regprocedure('public.lock_token_account_owner_cap_policy()')
     AND n.nspname='public' AND owner.rolname='token_cap_policy_deployer'
     AND p.prokind='f' AND p.prosecdef AND language.lanname='sql'
     AND p.proconfig=ARRAY['search_path=pg_catalog']::text[]
     AND NOT p.proretset AND p.prorettype=to_regtype('pg_catalog.int8')
     AND has_function_privilege(r.oid,p.oid,'EXECUTE')
 ))
 AND (NOT EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=34) OR EXISTS(
   SELECT 1 FROM pg_catalog.pg_proc p
   JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
   JOIN pg_catalog.pg_roles owner ON owner.oid=p.proowner
   JOIN pg_catalog.pg_language language ON language.oid=p.prolang
   WHERE p.oid=to_regprocedure('public.backfill_usage_history_projection(integer)')
     AND n.nspname='public' AND owner.rolname='token_schema_owner'
     AND p.prokind='f' AND p.prosecdef AND language.lanname='plpgsql'
     AND p.proconfig=ARRAY['search_path=pg_catalog']::text[]
     AND p.proretset AND p.prorettype=to_regtype('pg_catalog.record')
     AND pg_catalog.pg_get_function_result(p.oid)='TABLE(processed integer, complete boolean)'
     AND has_function_privilege(r.oid,p.oid,'EXECUTE')
 ))
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace JOIN pg_catalog.pg_roles owner ON owner.oid=p.proowner WHERE p.prosecdef AND owner.rolname IN ('token_schema_owner','token_cap_policy_deployer') AND NOT (
   n.nspname='public'
   AND p.proconfig=ARRAY['search_path=pg_catalog']::text[]
   AND (
     (owner.rolname='token_schema_owner'
       AND COALESCE(p.oid=to_regprocedure('public.lock_token_command_governance_activation()'),false)
       AND EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=31))
     OR (owner.rolname='token_cap_policy_deployer'
       AND COALESCE(p.oid=to_regprocedure('public.lock_token_account_owner_cap_policy()'),false)
       AND EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=32))
     OR (owner.rolname='token_schema_owner'
       AND COALESCE(p.oid=to_regprocedure('public.backfill_usage_history_projection(integer)'),false)
       AND EXISTS(SELECT 1 FROM public.schema_migrations WHERE version=34))
   )
 ) AND has_function_privilege(r.oid,p.oid,'EXECUTE'))
 AND NOT EXISTS(SELECT 1 FROM public.token_command_governance_transitions WHERE state='ACTIVE' AND epoch<>$2 AND $1::text=ANY(approved_runtime_roles)),
 coalesce(a.rolpassword ~ '^SCRAM-SHA-256\$[1-9][0-9]*:[A-Za-z0-9+/]+={0,2}\$[A-Za-z0-9+/]+={0,2}:[A-Za-z0-9+/]+={0,2}$' AND a.rolvaliduntil IS NOT NULL AND isfinite(a.rolvaliduntil) AND a.rolvaliduntil>clock_timestamp(),false)
 FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_authid a ON a.oid=r.oid WHERE r.rolname=$1`, name, r.ExpectedEpoch).Scan(&login, &safe, &credential); err != nil || !safe {
			return false, coded("CANDIDATE_ROLE_UNSAFE")
		}
		if !credential {
			return false, coded("CANDIDATE_CREDENTIAL_UNCONFIRMED")
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE usename=$1 AND pid<>pg_backend_pid()`, name).Scan(&count); err != nil {
			return false, coded("CANDIDATE_SESSION_UNCONFIRMED")
		}
		sessions += count
		if login {
			logins++
		}
	}
	if logins == len(r.Candidates) {
		return true, nil
	}
	if logins != 0 {
		return false, coded("CANDIDATE_LOGIN_STATE_MIXED")
	}
	if sessions != 0 {
		return false, coded("CANDIDATE_SESSION_UNCONFIRMED")
	}
	if !r.Apply {
		return false, nil
	}
	for _, name := range r.Candidates {
		if _, err := tx.Exec(ctx, "ALTER ROLE "+pgx.Identifier{name}.Sanitize()+" LOGIN"); err != nil {
			return false, coded("ENABLE_FAILED")
		}
	}
	return false, nil
}
