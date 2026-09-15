package account

import (
	"context"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestApprovalAuthorizationFunctionMetadataIntegration(t *testing.T) {
	database := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	type expectedFunction struct {
		signature       string
		owner           string
		securityDefiner bool
	}
	expected := []expectedFunction{
		{"account_approval_authorization.reject_sidecar_mutation()", "account_approval_authorization_owner", true},
		{"account_approval_authorization.reject_unfinalized_pending()", "account_approval_authorization_owner", true},
		{"account_approval_authorization.guard_outbox_insert()", "account_approval_authorization_owner", true},
		{"account_approval_authorization.guard_outbox_update()", "account_approval_authorization_owner", true},
		{"account_approval_authorization.guard_outbox_delete()", "account_approval_authorization_owner", true},
		{"account_approval_authorization.guard_outbox_truncate()", "account_approval_authorization_owner", true},
		{"account_approval_authorization.prepare_authorization_event(uuid,uuid,bigint,jsonb,text,bigint,text)", "account_approval_authorization_owner", true},
		{"account_approval_authorization.finalize_authorization_event(uuid,text,bytea)", "account_approval_authorization_owner", true},
		{"account_approval_authorization.backfill_authorization_sidecar(uuid,bytea,text)", "account_approval_authorization_owner", true},
		{"account_approval_authorization.authorization_commit_outcome(uuid,uuid,bigint,text,text,bigint,uuid,uuid)", "account_approval_authorization_owner", true},
		{"account_approval_authorization.authorization_backfill_outcome(uuid,text,text)", "account_approval_authorization_owner", true},
		{"account_approval_authorization.approval_terminal_commit_outcome(uuid,bigint,text,uuid,text)", "account_approval_authorization_owner", true},
		{"account_approval_authorization.require_authorization_sidecar_at_commit()", "account_approval_authorization_owner", true},
		{"account_approval_authorization.require_approved_request_authorization_at_commit()", "account_approval_authorization_owner", true},
		{"account_approval_authorization.authorization_delivery_matches(uuid,text,uuid,bigint,jsonb,text,timestamp with time zone,bigint,text,bytea)", "account_approval_authorization_owner", false},
		{"account_approval_authorization.authorization_sidecar_summary()", "account_approval_authorization_owner", true},
		{"account_approval_authorization.authorization_backfill_candidates(timestamp with time zone,uuid,timestamp with time zone,uuid,integer)", "account_approval_authorization_owner", true},
		{"account_approval_authorization.authorization_backfill_readiness()", "account_approval_authorization_owner", true},
		{"account_approval_authorization.validate_authorization_outbox_for_quiescing(bigint)", "account_approval_authorization_owner", true},
		{"account_approval_authorization.authorization_validation_candidates(timestamp with time zone,uuid,integer)", "account_approval_authorization_owner", true},
		{"account_approval_authorization.validate_control_transition_sidecars()", "account_approval_authorization_owner", true},
		{"account_approval_control.lock_admin_approval_control_exclusive_for_authorization()", "account_approval_control_owner", true},
		{"public.lock_approval_writer_authorization(uuid,text)", "account_approval_authorization_owner", true},
	}
	for _, function := range expected {
		var owner string
		var securityDefiner bool
		var settings []string
		if err := database.adminPool.QueryRow(ctx, `SELECT pg_catalog.pg_get_userbyid(p.proowner),p.prosecdef,p.proconfig
			FROM pg_catalog.pg_proc p WHERE p.oid=$1::text::regprocedure`, function.signature).Scan(&owner, &securityDefiner, &settings); err != nil {
			t.Fatalf("read %s metadata: %v", function.signature, err)
		}
		if owner != function.owner || securityDefiner != function.securityDefiner || len(settings) != 1 || settings[0] != "search_path=pg_catalog" {
			t.Fatalf("unsafe %s metadata: owner=%s secdef=%v settings=%v", function.signature, owner, securityDefiner, settings)
		}
	}
	var authorizationFunctions int
	if err := database.adminPool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_proc p
		JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='account_approval_authorization'`).Scan(&authorizationFunctions); err != nil {
		t.Fatal(err)
	}
	if authorizationFunctions != 21 {
		t.Fatalf("authorization function inventory drifted: %d", authorizationFunctions)
	}
	var helperExposed bool
	if err := database.adminPool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_proc p,
		LATERAL pg_catalog.aclexplode(COALESCE(p.proacl,pg_catalog.acldefault('f',p.proowner))) acl
		WHERE p.oid='account_approval_authorization.authorization_delivery_matches(uuid,text,uuid,bigint,jsonb,text,timestamp with time zone,bigint,text,bytea)'::regprocedure
		  AND acl.privilege_type='EXECUTE'
		  AND acl.grantee IN (0,'account_service'::regrole,'account_approval_writer'::regrole,'account_approval_backfill'::regrole)
	)`).Scan(&helperExposed); err != nil {
		t.Fatal(err)
	}
	if helperExposed {
		t.Fatal("internal delivery matcher is executable by an untrusted role")
	}
}

func TestApprovalAuthorizationMalformedDeliveryIsInvalidIntegration(t *testing.T) {
	database := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, database, true)
	tx, err := database.adminPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE account_approval_authorization_owner;
		ALTER TABLE account_approval_authorization.approval_authorization_event_bytes
		DISABLE TRIGGER approval_authorization_sidecar_update_guard`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `WITH malformed(bytes) AS (VALUES (pg_catalog.convert_to('{"event_id":"00000000-0000-0000-0000-000000000001"}','UTF8')))
		UPDATE account_approval_authorization.approval_authorization_event_bytes s
		SET delivery_bytes=m.bytes,event_digest=pg_catalog.encode(pg_catalog.sha256(m.bytes),'hex')
		FROM malformed m WHERE s.event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE account_approval_authorization.approval_authorization_event_bytes
		ENABLE TRIGGER approval_authorization_sidecar_update_guard`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var authorizationCount, missingEpoch, missingSidecar, invalidSidecar int64
	if err := database.backfillPool.QueryRow(ctx,
		`SELECT authorization_count,missing_epoch_count,missing_sidecar_count,invalid_sidecar_count
		 FROM account_approval_authorization.authorization_backfill_readiness()`,
	).Scan(&authorizationCount, &missingEpoch, &missingSidecar, &invalidSidecar); err != nil {
		t.Fatal(err)
	}
	if authorizationCount != 1 || missingEpoch != 0 || missingSidecar != 0 || invalidSidecar != 1 {
		t.Fatalf("malformed self-digested delivery escaped summary: %d/%d/%d/%d", authorizationCount, missingEpoch, missingSidecar, invalidSidecar)
	}
}

func TestApprovalAuthorizationSidecarRejectsMissingDigestIntegration(t *testing.T) {
	database := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, test := range []struct {
		name       string
		provenance string
		delivery   bool
	}{
		{name: "frozen", provenance: "FROZEN_AT_APPROVAL", delivery: true},
		{name: "reconstructed delivery", provenance: "RECONSTRUCTED_FOR_DELIVERY", delivery: true},
		{name: "reconstructed after publish", provenance: "RECONSTRUCTED_AFTER_PUBLISH"},
	} {
		t.Run(test.name, func(t *testing.T) {
			eventID := uuid.New()
			if _, err := database.runtimePool.Exec(ctx, `INSERT INTO public.outbox_events
				(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at)
				VALUES($1,'account.fixture.digest.v1',$2,1,'{}','digest-check',transaction_timestamp())`, eventID, uuid.New()); err != nil {
				t.Fatal(err)
			}
			tx, err := database.adminPool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE account_approval_authorization_owner`); err != nil {
				t.Fatal(err)
			}
			var delivery any
			if test.delivery {
				delivery = []byte(`{}`)
			}
			_, err = tx.Exec(ctx, `INSERT INTO account_approval_authorization.approval_authorization_event_bytes
				(schema_version,event_id,serialization_version,provenance,delivery_bytes,event_digest,reconstructed_semantic_digest)
				VALUES(1,$1,'LEGACY_TYPED_V1',$2,$3,NULL,NULL)`, eventID, test.provenance, delivery)
			requireControlSQLState(t, err, "23514")
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestApprovalAuthorizationExistingSidecarRejectsNullEpochIntegration(t *testing.T) {
	database := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eventID := seedBackfillAuthorization(t, ctx, database, true)
	var delivery []byte
	if err := database.adminPool.QueryRow(ctx, `SELECT delivery_bytes
		FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=$1`, eventID).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	restoreTriggers := func(restoreCtx context.Context) error {
		restoreTx, err := database.adminPool.Begin(restoreCtx)
		if err != nil {
			return err
		}
		defer restoreTx.Rollback(context.Background())
		if _, err := restoreTx.Exec(restoreCtx, `SET LOCAL ROLE account_approval_authorization_owner;
			ALTER TABLE public.outbox_events ENABLE TRIGGER USER`); err != nil {
			return err
		}
		return restoreTx.Commit(restoreCtx)
	}
	triggersRestored := false
	defer func() {
		if triggersRestored {
			return
		}
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer restoreCancel()
		if err := restoreTriggers(restoreCtx); err != nil {
			t.Errorf("restore authorization outbox triggers after failed fixture injection: %v", err)
		}
	}()
	disableTx, err := database.adminPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disableTx.Exec(ctx, `SET LOCAL ROLE account_approval_authorization_owner;
		ALTER TABLE public.outbox_events DISABLE TRIGGER USER`); err != nil {
		_ = disableTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := disableTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	corruptTx, err := database.adminPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corruptTx.Exec(ctx, `SET LOCAL ROLE account_approval_authorization_owner`); err != nil {
		_ = corruptTx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := corruptTx.Exec(ctx, `UPDATE public.outbox_events SET approval_control_epoch=NULL WHERE event_id=$1`, eventID); err != nil {
		_ = corruptTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := corruptTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restoreTriggers(ctx); err != nil {
		t.Fatal(err)
	}
	triggersRestored = true
	var disabledTriggers, userTriggers int
	if err := database.adminPool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE tgenabled <> 'O'), count(*)
		FROM pg_catalog.pg_trigger
		WHERE tgrelid='public.outbox_events'::pg_catalog.regclass AND NOT tgisinternal`).Scan(&disabledTriggers, &userTriggers); err != nil {
		t.Fatal(err)
	}
	if userTriggers == 0 || disabledTriggers != 0 {
		t.Fatalf("authorization outbox fixture did not restore every user trigger: disabled=%d total=%d", disabledTriggers, userTriggers)
	}
	_, err = database.backfillPool.Exec(ctx,
		`SELECT account_approval_authorization.backfill_authorization_sidecar($1,$2,NULL)`, eventID, delivery)
	requireControlSQLState(t, err, "55000")
	var epoch *int64
	var sidecarCount int
	if err := database.adminPool.QueryRow(ctx, `SELECT o.approval_control_epoch,
		(SELECT count(*) FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=o.event_id)
		FROM public.outbox_events o WHERE o.event_id=$1`, eventID).Scan(&epoch, &sidecarCount); err != nil {
		t.Fatal(err)
	}
	if epoch != nil || sidecarCount != 1 {
		t.Fatalf("rejected existing-sidecar backfill changed facts: epoch=%v sidecars=%d", epoch, sidecarCount)
	}
}

func TestApprovalAuthorizationMigrationRolePreflightIntegration(t *testing.T) {
	database := newApprovalAuthorizationIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	loaded, err := loadApprovalAuthorizationBaselineMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) == 0 || loaded[len(loaded)-1].Version != 51 {
		t.Fatal("role preflight requires migration 51 as the latest migration")
	}

	// Return this otherwise-empty isolated database to the exact upgrade state.
	downConnection := authorizationTestConnection(t, ctx, database.migratorPool)
	if _, err := downConnection.Exec(ctx, `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.DownOne(ctx, downConnection.Conn(), loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, downConnection.Conn())
	if _, err := downConnection.Exec(ctx, `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	assertApprovalAuthorizationMigration51Unapplied(t, ctx, database)

	mutations := []struct {
		name string
		sql  string
	}{
		{name: "owner login", sql: `ALTER ROLE account_approval_authorization_owner LOGIN`},
		{name: "owner inherit", sql: `ALTER ROLE account_approval_authorization_owner INHERIT`},
		{name: "owner superuser", sql: `ALTER ROLE account_approval_authorization_owner SUPERUSER`},
		{name: "owner createdb", sql: `ALTER ROLE account_approval_authorization_owner CREATEDB`},
		{name: "owner createrole", sql: `ALTER ROLE account_approval_authorization_owner CREATEROLE`},
		{name: "owner replication", sql: `ALTER ROLE account_approval_authorization_owner REPLICATION`},
		{name: "owner bypassrls", sql: `ALTER ROLE account_approval_authorization_owner BYPASSRLS`},
		{name: "writer nologin", sql: `ALTER ROLE account_approval_writer NOLOGIN`},
		{name: "writer inherit", sql: `ALTER ROLE account_approval_writer INHERIT`},
		{name: "writer superuser", sql: `ALTER ROLE account_approval_writer SUPERUSER`},
		{name: "writer createdb", sql: `ALTER ROLE account_approval_writer CREATEDB`},
		{name: "writer createrole", sql: `ALTER ROLE account_approval_writer CREATEROLE`},
		{name: "writer replication", sql: `ALTER ROLE account_approval_writer REPLICATION`},
		{name: "writer bypassrls", sql: `ALTER ROLE account_approval_writer BYPASSRLS`},
		{name: "backfill nologin", sql: `ALTER ROLE account_approval_backfill NOLOGIN`},
		{name: "backfill inherit", sql: `ALTER ROLE account_approval_backfill INHERIT`},
		{name: "backfill superuser", sql: `ALTER ROLE account_approval_backfill SUPERUSER`},
		{name: "backfill createdb", sql: `ALTER ROLE account_approval_backfill CREATEDB`},
		{name: "backfill createrole", sql: `ALTER ROLE account_approval_backfill CREATEROLE`},
		{name: "backfill replication", sql: `ALTER ROLE account_approval_backfill REPLICATION`},
		{name: "backfill bypassrls", sql: `ALTER ROLE account_approval_backfill BYPASSRLS`},
		{name: "owner extra member", sql: `GRANT account_approval_authorization_owner TO account_service`},
		{name: "owner reverse membership", sql: `GRANT account_service TO account_approval_authorization_owner`},
		{name: "writer extra member", sql: `GRANT account_approval_writer TO account_service`},
		{name: "writer reverse membership", sql: `GRANT account_service TO account_approval_writer`},
		{name: "backfill extra member", sql: `GRANT account_approval_backfill TO account_service`},
		{name: "backfill reverse membership", sql: `GRANT account_service TO account_approval_backfill`},
		{name: "writer to backfill", sql: `GRANT account_approval_writer TO account_approval_backfill`},
		{name: "backfill to writer", sql: `GRANT account_approval_backfill TO account_approval_writer`},
		{name: "owner migrator loses admin", sql: `REVOKE ADMIN OPTION FOR account_approval_authorization_owner FROM account_migrator`},
		{name: "owner migrator inherits", sql: `GRANT account_approval_authorization_owner TO account_migrator WITH INHERIT TRUE, SET TRUE, ADMIN TRUE`},
	}

	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			configuration := database.adminPool.Config().ConnConfig.Copy()
			connection, err := pgx.ConnectConfig(ctx, configuration)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close(context.Background())
			tx, err := connection.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			// Role changes are transaction-local and are always rolled back. The
			// original production SQL then proves its own fail-closed preflight.
			if _, err := tx.Exec(ctx, test.sql+`; SET SESSION AUTHORIZATION account_migrator; SET ROLE account_service`); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, loaded[len(loaded)-1].UpSQL)
			requireControlSQLState(t, err, "P0001")
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			assertApprovalAuthorizationMigration51Unapplied(t, ctx, database)
			assertApprovalAuthorizationRoleTopology(t, ctx, database)
		})
	}
}

func assertApprovalAuthorizationMigration51Unapplied(t *testing.T, ctx context.Context, database *approvalAuthorizationIntegrationDatabase) {
	t.Helper()
	var latest int64
	var owner string
	var schemaExists, epochColumnExists bool
	if err := database.adminPool.QueryRow(ctx, `SELECT
		(SELECT max(version) FROM public.schema_migrations),
		pg_catalog.pg_get_userbyid((SELECT relowner FROM pg_catalog.pg_class WHERE oid='public.outbox_events'::regclass)),
		pg_catalog.to_regnamespace('account_approval_authorization') IS NOT NULL,
		EXISTS (SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid='public.outbox_events'::regclass AND attname='approval_control_epoch' AND NOT attisdropped)`).Scan(
		&latest, &owner, &schemaExists, &epochColumnExists,
	); err != nil {
		t.Fatal(err)
	}
	if latest != 50 || owner != "account_service" || schemaExists || epochColumnExists {
		t.Fatalf("failed preflight changed database: latest=%d owner=%s schema=%v epoch_column=%v", latest, owner, schemaExists, epochColumnExists)
	}
}

func assertApprovalAuthorizationRoleTopology(t *testing.T, ctx context.Context, database *approvalAuthorizationIntegrationDatabase) {
	t.Helper()
	var unsafeAttributes, unsafeMembership bool
	if err := database.adminPool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_roles
		 WHERE rolname='account_approval_authorization_owner'
		   AND (rolcanlogin OR rolinherit OR rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)
		UNION ALL
		SELECT 1 FROM pg_catalog.pg_roles
		 WHERE rolname IN ('account_approval_writer','account_approval_backfill')
		   AND (NOT rolcanlogin OR rolinherit OR rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)
	), EXISTS (
		SELECT 1
		  FROM pg_catalog.pg_auth_members am
		 WHERE (am.roleid IN ('account_approval_authorization_owner'::regrole,'account_approval_writer'::regrole,'account_approval_backfill'::regrole)
		        OR am.member IN ('account_approval_authorization_owner'::regrole,'account_approval_writer'::regrole,'account_approval_backfill'::regrole))
		   AND NOT (am.roleid='account_approval_authorization_owner'::regrole
		            AND am.member='account_migrator'::regrole
		            AND am.admin_option AND am.set_option AND NOT am.inherit_option)
	)`).Scan(&unsafeAttributes, &unsafeMembership); err != nil {
		t.Fatal(err)
	}
	if unsafeAttributes || unsafeMembership {
		t.Fatalf("rolled-back preflight mutation escaped: attributes=%v membership=%v", unsafeAttributes, unsafeMembership)
	}
}
