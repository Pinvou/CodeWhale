package billingschema

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

const latePaymentQueryGenerationsVersion int64 = 900048
const latePaymentQueryGenerationsName = "late_payment_query_generations"
const latePaymentQueryGenerationsUp = "f233f8a00a12014ce65542e71d10903d16ae58ae2df881fd934c7e47440f8558"
const latePaymentQueryGenerationsDown = "03d2e6a42a8113a6c8d5b4bacdd549aa07226df4b2dd10563b4ad7cf7df480ae"

// This append-only binding does not rewrite the published command/admission
// migrations. Older custom carriers retain their original optional boundary.
func validateLatePaymentQueryGenerations(input []migrations.Migration) error {
	seen := false
	for _, m := range input {
		up := sqlChecksum(m.UpSQL)
		if m.Version != latePaymentQueryGenerationsVersion && m.Name != latePaymentQueryGenerationsName && m.Checksum != latePaymentQueryGenerationsUp && up != latePaymentQueryGenerationsUp {
			continue
		}
		if seen || m.Version != latePaymentQueryGenerationsVersion || m.Name != latePaymentQueryGenerationsName || m.Checksum != latePaymentQueryGenerationsUp || up != latePaymentQueryGenerationsUp || sqlChecksum(m.DownSQL) != latePaymentQueryGenerationsDown {
			return errors.New("late payment query generations require their published migration identity")
		}
		seen = true
	}
	return nil
}

// Generic hardening grants ordinary table DML. This receipt-specific boundary
// must be restored after that pass and after every later migration as well.
func verifyLatePaymentQueryGenerationsRestored(ctx context.Context, tx pgx.Tx) error {
	var present bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.late_payment_query_generations') IS NOT NULL`).Scan(&present); err != nil {
		return err
	}
	if !present {
		return nil
	}
	var safe bool
	err := tx.QueryRow(ctx, `SELECT current_user='billing_service' AND pg_get_userbyid(relowner)='billing_schema_owner'
		AND NOT pg_has_role('billing_service','billing_schema_owner','MEMBER') AND NOT has_schema_privilege('billing_service','public','CREATE')
		AND has_table_privilege('billing_service',oid,'SELECT') AND has_table_privilege('billing_service',oid,'INSERT')
		AND NOT has_table_privilege('billing_service',oid,'UPDATE') AND NOT has_table_privilege('billing_service',oid,'DELETE')
		AND NOT has_table_privilege('billing_service',oid,'TRUNCATE')
		AND (SELECT count(*)=2 AND bool_and(pg_get_userbyid(proowner)='billing_schema_owner' AND NOT prosecdef
		 AND proconfig=ARRAY['search_path=pg_catalog']::text[] AND NOT has_function_privilege('billing_service',p.oid,'EXECUTE'))
		 FROM pg_proc p WHERE p.oid IN (to_regprocedure('public.guard_late_payment_query_generation()'),to_regprocedure('public.require_late_payment_query_generation()')))
		AND (SELECT count(*)=1 AND bool_and(tgdeferrable AND tginitdeferred AND tgenabled='O') FROM pg_trigger
		 WHERE tgrelid='public.late_payment_query_commands'::regclass AND tgname='late_payment_query_generation_required' AND NOT tgisinternal)
		FROM pg_class WHERE oid='public.late_payment_query_generations'::regclass`).Scan(&safe)
	if err != nil {
		return err
	}
	if !safe {
		return errors.New("query generation ownership, immutable identity or runtime authority was not restored")
	}
	return nil
}
