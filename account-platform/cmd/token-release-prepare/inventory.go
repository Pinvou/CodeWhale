package main

import (
	"context"
	"slices"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

type writerIdentity struct {
	Name         string
	Login, Super bool
	Sessions     int
}
type installation struct {
	State          string
	Epoch, Version int64
	Roles          []string
}

// Includes NOINHERIT members able to SET ROLE, direct grants and executable
// SECURITY DEFINER functions. Roles are NOT silently altered from this query.
const writerInventorySQL = `SELECT r.rolname,r.rolcanlogin,r.rolsuper,
 (SELECT count(*) FROM pg_catalog.pg_stat_activity a WHERE a.usesysid=r.oid AND a.datname=current_database() AND a.pid<>pg_backend_pid())
 FROM pg_catalog.pg_roles r WHERE r.rolsuper OR EXISTS(SELECT 1 FROM pg_catalog.pg_roles gateway WHERE gateway.rolname='token_gateway' AND pg_catalog.pg_has_role(r.oid,gateway.oid,'MEMBER')) OR EXISTS (
 SELECT 1 FROM pg_catalog.pg_roles g WHERE (g.oid=r.oid OR pg_catalog.pg_has_role(r.oid,g.oid,'MEMBER')) AND (
 EXISTS(SELECT 1 FROM pg_catalog.pg_namespace n WHERE n.nspname='public' AND pg_catalog.has_schema_privilege(g.oid,n.oid,'CREATE')) OR
 EXISTS (SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relkind IN ('r','p') AND pg_catalog.has_table_privilege(g.oid,c.oid,'INSERT,UPDATE,DELETE,TRUNCATE')) OR
 EXISTS (SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='public' AND p.prosecdef AND pg_catalog.has_function_privilege(g.oid,p.oid,'EXECUTE')))) ORDER BY r.rolname`

func installationSnapshot(ctx context.Context, tx pgx.Tx, directory string, lock bool) (installation, error) {
	result := installation{State: "NOT_INSTALLED"}
	var ledger, activation, transitions bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL,to_regclass('public.token_command_governance_activation') IS NOT NULL,to_regclass('public.token_command_governance_transitions') IS NOT NULL`).Scan(&ledger, &activation, &transitions); err != nil {
		return result, coded("INVENTORY_UNAVAILABLE")
	}
	if !ledger {
		var tables int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p')`).Scan(&tables); err != nil || tables != 0 || activation || transitions {
			return result, coded("MIGRATION_LEDGER_REQUIRED")
		}
		return result, nil // Empty, not-installed database, not a fabricated PRE.
	}
	if lock {
		if _, err := tx.Exec(ctx, `LOCK TABLE public.schema_migrations IN SHARE MODE NOWAIT`); err != nil {
			return result, coded("MIGRATION_BUSY")
		}
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version),0) FROM public.schema_migrations`).Scan(&result.Version); err != nil {
		return result, coded("MIGRATION_LEDGER_INVALID")
	}
	loaded, err := migrations.Load(directory)
	if err != nil {
		return result, coded("MIGRATION_PACKAGE_INVALID")
	}
	var applied []migrations.Migration
	for _, migration := range loaded {
		if migration.Version <= result.Version {
			applied = append(applied, migration)
		}
	}
	if len(applied) == 0 || result.Version != applied[len(applied)-1].Version {
		return result, coded("MIGRATION_LEDGER_INVALID")
	}
	if _, err = migrations.VerifyApplied(ctx, tx.Conn(), applied); err != nil {
		return result, coded("MIGRATION_CHECKSUM_MISMATCH")
	}
	if activation != (result.Version >= 31) || transitions != activation {
		return result, coded("ACTIVATION_INSTALLATION_MISMATCH")
	}
	if !activation {
		return result, nil
	}
	query := `SELECT state,epoch,approved_runtime_roles FROM public.token_command_governance_activation WHERE singleton=TRUE`
	if lock {
		query += " FOR UPDATE"
	}
	if err = tx.QueryRow(ctx, query).Scan(&result.State, &result.Epoch, &result.Roles); err != nil {
		return result, coded("ACTIVATION_UNAVAILABLE")
	}
	if result.State == "PRE_ACTIVATION" {
		if result.Epoch != 0 || len(result.Roles) != 0 {
			return result, coded("ACTIVATION_INVALID")
		}
	} else if !slices.Contains([]string{"ACTIVE", "SAFE_ROLLBACK"}, result.State) || result.Epoch <= 0 || len(result.Roles) == 0 || !slices.IsSorted(result.Roles) {
		return result, coded("ACTIVATION_INVALID")
	}
	return result, nil
}

func writerInventory(ctx context.Context, tx pgx.Tx) ([]writerIdentity, error) {
	rows, err := tx.Query(ctx, writerInventorySQL)
	if err != nil {
		return nil, coded("WRITER_INVENTORY_UNAVAILABLE")
	}
	defer rows.Close()
	var result []writerIdentity
	for rows.Next() {
		var item writerIdentity
		if err = rows.Scan(&item.Name, &item.Login, &item.Super, &item.Sessions); err != nil {
			return nil, coded("WRITER_INVENTORY_UNAVAILABLE")
		}
		result = append(result, item)
	}
	if rows.Err() != nil {
		return nil, coded("WRITER_INVENTORY_UNAVAILABLE")
	}
	return result, nil
}

func noUnexpectedWriters(writers []writerIdentity, sessionUser string, retired []string, quiesced bool) error {
	for _, writer := range writers {
		if writer.Name == sessionUser {
			if writer.Sessions != 0 {
				return coded("OTHER_DBA_SESSION_PRESENT")
			}
			continue
		}
		if writer.Super {
			if writer.Sessions != 0 {
				return coded("OTHER_DBA_SESSION_PRESENT")
			}
			continue // Never ALTER or terminate another cluster administrator.
		}
		if (writer.Login || writer.Sessions != 0) && !slices.Contains(retired, writer.Name) {
			return coded("UNAPPROVED_WRITER_PRESENT")
		}
		if quiesced && (writer.Login || writer.Sessions != 0) {
			return coded("WRITERS_NOT_QUIESCED")
		}
	}
	return nil
}

func lockDomainTables(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT c.relname,pg_get_userbyid(c.relowner) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p') ORDER BY c.relname`)
	if err != nil {
		return coded("DOMAIN_INVENTORY_UNAVAILABLE")
	}
	var tables []string
	for rows.Next() {
		var name, owner string
		if err = rows.Scan(&name, &owner); err != nil {
			rows.Close()
			return coded("DOMAIN_INVENTORY_UNAVAILABLE")
		}
		if !slices.Contains([]string{"token_gateway", "token_schema_owner", "token_cap_policy_deployer"}, owner) {
			rows.Close()
			return coded("DOMAIN_OWNER_UNEXPECTED")
		}
		tables = append(tables, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return coded("DOMAIN_INVENTORY_UNAVAILABLE")
	}
	for _, name := range tables {
		if _, err = tx.Exec(ctx, "LOCK TABLE "+pgx.Identifier{"public", name}.Sanitize()+" IN ACCESS EXCLUSIVE MODE NOWAIT"); err != nil {
			return coded("DOMAIN_WRITER_BUSY")
		}
	}
	return nil
}

// A role is cluster-global. Inspect other connectable databases read-only;
// inability to inspect is a blocker, never permission to mutate a shared role.
func rejectCrossDatabaseRoles(ctx context.Context, connection *pgx.Conn, names []string) error {
	var unavailable bool
	if err := connection.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_database WHERE NOT datistemplate AND NOT datallowconn)`).Scan(&unavailable); err != nil || unavailable {
		return coded("CROSS_DATABASE_INVENTORY_UNAVAILABLE")
	}
	rows, err := connection.Query(ctx, `SELECT datname FROM pg_catalog.pg_database WHERE datname<>current_database() AND NOT datistemplate AND datallowconn ORDER BY datname`)
	if err != nil {
		return coded("CROSS_DATABASE_INVENTORY_UNAVAILABLE")
	}
	var databases []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return coded("CROSS_DATABASE_INVENTORY_UNAVAILABLE")
		}
		databases = append(databases, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return coded("CROSS_DATABASE_INVENTORY_UNAVAILABLE")
	}
	for _, database := range databases {
		config := connection.Config().Copy()
		config.Database = database
		other, err := pgx.ConnectConfig(ctx, config)
		if err != nil {
			return coded("CROSS_DATABASE_INVENTORY_UNAVAILABLE")
		}
		tx, err := other.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			other.Close(ctx)
			return coded("CROSS_DATABASE_INVENTORY_UNAVAILABLE")
		}
		var shared bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_roles r WHERE r.rolname=ANY($1::text[]) AND (
 pg_catalog.pg_has_role(r.oid,(SELECT datdba FROM pg_catalog.pg_database WHERE datname=current_database()),'MEMBER') OR
 EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity a WHERE a.usesysid=r.oid AND a.datname=current_database()) OR
 EXISTS(SELECT 1 FROM pg_catalog.pg_roles g WHERE (g.oid=r.oid OR pg_catalog.pg_has_role(r.oid,g.oid,'MEMBER')) AND (
 EXISTS(SELECT 1 FROM pg_catalog.pg_shdepend d WHERE d.refclassid='pg_catalog.pg_authid'::regclass AND d.refobjid=g.oid AND d.deptype IN ('o','a','i','r') AND (d.dbid=(SELECT oid FROM pg_catalog.pg_database WHERE datname=current_database()) OR (d.dbid=0 AND d.classid='pg_catalog.pg_database'::regclass AND d.objid=(SELECT oid FROM pg_catalog.pg_database WHERE datname=current_database())))) OR
 EXISTS(SELECT 1 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND c.relkind IN ('r','p') AND pg_catalog.has_table_privilege(g.oid,c.oid,'INSERT,UPDATE,DELETE,TRUNCATE')) OR
 EXISTS(SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND p.prosecdef AND pg_catalog.has_function_privilege(g.oid,p.oid,'EXECUTE'))))))`, names).Scan(&shared)
		_ = tx.Rollback(ctx)
		_ = other.Close(ctx)
		if err != nil {
			return coded("CROSS_DATABASE_INVENTORY_UNAVAILABLE")
		}
		if shared {
			return coded("CROSS_DATABASE_ROLE_SHARED")
		}
	}
	return nil
}
