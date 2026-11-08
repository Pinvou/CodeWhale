package billingschema

import (
	"context"
	_ "embed"
	"errors"
	"slices"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// The original fixed 026 is retained only as exact installed history. Fresh
// installations still execute the original mainline 026, then new 900024.
//
//go:embed retained/000026_hold_operations_evidence_oldfix.up.sql
var oldWaiverEvidenceSQL string

const oldWaiverEvidenceChecksum = "2aac5d4c84d798c61e30ea960bd351914c815c8602c14894c92d50753c27ed87"
const oldWaiverEvidenceCRLFChecksum = "e43525148d30007f77fe009bfcc8181c4d33f62d19c15f002ec3d759fe26cffe"
const mainlineWaiverEvidenceChecksum = "6a1985280bad6704730786d142e5224fe53a3c242759e6b4d67914d788d6d2a4"
const mainlineWaiverEvidenceCRLFChecksum = "d1e48b1e99d52ec1723fdb156d92a4bc7c76a136486b8b3f1d76f1f42230806b"

func knownLegacyWaiverHistory(ctx context.Context, q walletReceiptHistoryQueryer) (bool, error) {
	if sqlChecksum(oldWaiverEvidenceSQL) != oldWaiverEvidenceChecksum {
		return false, errors.New("legacy waiver history resource changed")
	}
	var ledger, commands bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL,to_regclass('usage_hold_admin_commands') IS NOT NULL`).Scan(&ledger, &commands); err != nil {
		return false, err
	}
	if !ledger {
		if commands {
			return false, errors.New("legacy waiver table has no migration identity")
		}
		return false, nil
	}
	var name, checksum string
	err := q.QueryRow(ctx, `SELECT name,checksum FROM schema_migrations WHERE version=26`).Scan(&name, &checksum)
	if errors.Is(err, pgx.ErrNoRows) {
		if commands {
			return false, errors.New("legacy waiver table has no 026 identity")
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if name != "hold_operations_evidence" {
		return false, errors.New("installed Billing 026 has a different identity")
	}
	switch checksum {
	case mainlineWaiverEvidenceChecksum, mainlineWaiverEvidenceCRLFChecksum:
		if commands {
			return false, errors.New("unregistered legacy waiver command table")
		}
		return false, nil
	case oldWaiverEvidenceChecksum, oldWaiverEvidenceCRLFChecksum:
		if !commands {
			return false, errors.New("original fixed 026 command table missing")
		}
	default:
		return false, errors.New("installed Billing 026 is not a recognized immutable history")
	}
	var compatible bool
	err = q.QueryRow(ctx, `SELECT
 (SELECT jsonb_agg(jsonb_build_array(attname,format_type(atttypid,atttypmod),attnotnull,attidentity,attgenerated) ORDER BY attnum) FROM pg_attribute WHERE attrelid='usage_hold_admin_commands'::regclass AND attnum>0 AND NOT attisdropped)
 ='[["idempotency_key","text",true,"",""],["hold_id","uuid",true,"",""],["request_fingerprint","text",true,"",""],["reason","text",true,"",""],["actor","text",true,"",""],["before_status","text",true,"",""],["created_at","timestamp with time zone",true,"",""]]'::jsonb
 AND NOT EXISTS(SELECT 1 FROM pg_attrdef WHERE adrelid='usage_hold_admin_commands'::regclass)
 AND (SELECT count(*)=2 FROM pg_constraint WHERE conrelid='usage_hold_admin_commands'::regclass)
 AND EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='usage_hold_admin_commands'::regclass AND contype='p' AND conkey=ARRAY[1]::smallint[] AND NOT condeferrable)
 AND EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='usage_hold_admin_commands'::regclass AND contype='f' AND conkey=ARRAY[2]::smallint[] AND confrelid='usage_holds'::regclass AND confkey=ARRAY[1]::smallint[] AND confupdtype='a' AND confdeltype='a' AND convalidated AND NOT condeferrable)
 AND (SELECT count(*) BETWEEN 1 AND 2 FROM pg_trigger WHERE tgrelid='usage_hold_admin_commands'::regclass AND NOT tgisinternal)
 AND NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='usage_hold_admin_commands'::regclass AND NOT tgisinternal AND tgname NOT IN ('usage_hold_admin_commands_immutable','billing_no_truncate'))
 AND NOT EXISTS(SELECT 1 FROM pg_trigger trigger JOIN pg_proc function ON function.oid=trigger.tgfoid JOIN pg_namespace namespace ON namespace.oid=function.pronamespace WHERE trigger.tgrelid='usage_hold_admin_commands'::regclass AND trigger.tgname='billing_no_truncate' AND (trigger.tgtype<>34 OR trigger.tgenabled<>'O' OR trigger.tgnargs<>0 OR trigger.tgqual IS NOT NULL OR namespace.nspname<>'public' OR function.proname<>'reject_billing_truncate' OR function.prosecdef))
 AND EXISTS(SELECT 1 FROM pg_trigger trigger JOIN pg_proc function ON function.oid=trigger.tgfoid JOIN pg_namespace namespace ON namespace.oid=function.pronamespace
 WHERE trigger.tgrelid='usage_hold_admin_commands'::regclass AND trigger.tgname='usage_hold_admin_commands_immutable' AND trigger.tgtype=27 AND trigger.tgenabled='O' AND trigger.tgnargs=0 AND trigger.tgqual IS NULL
 AND namespace.nspname='public' AND function.proname='reject_usage_hold_evidence_mutation' AND NOT function.prosecdef
 AND replace(replace(function.prosrc,E'\r\n',E'\n'),E'\r',E'\n')=E'\nBEGIN\n    RAISE EXCEPTION ''usage hold operational evidence is immutable'';\nEND ')
 AND (SELECT pg_get_userbyid(relowner) IN ('billing_service','billing_schema_owner') AND relkind='r' AND NOT relrowsecurity AND NOT relforcerowsecurity FROM pg_class WHERE oid='usage_hold_admin_commands'::regclass)`).Scan(&compatible)
	if err != nil {
		return false, err
	}
	if !compatible {
		return false, errors.New("original fixed 026 command shape is not compatible")
	}
	return true, nil
}

// Validate delivered mainline SQL before consulting installed history. The
// adapter may replace026 only after its original delivery has been verified.
func validateUsageWaiverHistoryDelivery(input []migrations.Migration) error {
	found, requires026 := false, false
	for _, m := range input {
		if m.Version == usageWaiverVersion {
			requires026 = true
		}
		if m.Version != 26 {
			continue
		}
		found = true
		if m.Name != "hold_operations_evidence" || m.Checksum != mainlineWaiverEvidenceChecksum || sqlChecksum(m.UpSQL) != mainlineWaiverEvidenceChecksum {
			return errors.New("mainline Billing 026 source changed")
		}
	}
	if requires026 && !found {
		return errors.New("usage waiver delivery requires original Billing 026")
	}
	return nil
}

// expectedUsageWaiverHistory is used before public Up and by the Billing-only
// verifier. It never edits schema_migrations, table ownership, or historical rows.
func expectedUsageWaiverHistory(ctx context.Context, q walletReceiptHistoryQueryer, input []migrations.Migration) ([]migrations.Migration, error) {
	if err := validateUsageWaiverHistoryDelivery(input); err != nil {
		return nil, err
	}
	old, err := knownLegacyWaiverHistory(ctx, q)
	if err != nil {
		return nil, err
	}
	result := slices.Clone(input)
	found := false
	for i, m := range result {
		if m.Version != 26 {
			continue
		}
		found = true
		if old {
			result[i].UpSQL = oldWaiverEvidenceSQL
			result[i].Checksum = oldWaiverEvidenceChecksum
		}
	}
	if old && !found {
		return nil, errors.New("legacy waiver identity requires original 026 in the package")
	}
	return result, nil
}
