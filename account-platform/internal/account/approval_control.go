package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const adminApprovalControlID = "ADMIN_APPROVAL"

type AdminApprovalMode string

const (
	AdminApprovalModeLegacy        AdminApprovalMode = "LEGACY"
	AdminApprovalModeQuiescing     AdminApprovalMode = "QUIESCING"
	AdminApprovalModeProofRequired AdminApprovalMode = "PROOF_REQUIRED"
	AdminApprovalModeSafeRollback  AdminApprovalMode = "SAFE_ROLLBACK"
)

type AdminApprovalRelayState string

const (
	AdminApprovalRelayRunning AdminApprovalRelayState = "RUNNING"
	AdminApprovalRelayFenced  AdminApprovalRelayState = "FENCED"
)

// AdminApprovalControl is the complete, closed read model of the single
// ADMIN_APPROVAL control row. GSA-04 deliberately exposes no mutation helper:
// later cutover work must first supply the facts required by each transition.
type AdminApprovalControl struct {
	ControlID               string
	SchemaVersion           int
	Mode                    AdminApprovalMode
	ApprovalWireVersion     int
	ProofRequired           bool
	AuthorizationRelayState AdminApprovalRelayState
	ApprovalEpoch           int64
	AuthorizationRelayEpoch int64
	Version                 int64
	UpdatedAt               time.Time
}

var ErrAdminApprovalControlInvalid = errors.New("admin approval control is invalid")

const adminApprovalControlColumns = `control_id,schema_version,mode,approval_wire_version,proof_required,authorization_relay_state,approval_epoch,authorization_relay_epoch,version,updated_at`

func scanAdminApprovalControl(row pgx.Row) (AdminApprovalControl, error) {
	var control AdminApprovalControl
	if err := row.Scan(
		&control.ControlID,
		&control.SchemaVersion,
		&control.Mode,
		&control.ApprovalWireVersion,
		&control.ProofRequired,
		&control.AuthorizationRelayState,
		&control.ApprovalEpoch,
		&control.AuthorizationRelayEpoch,
		&control.Version,
		&control.UpdatedAt,
	); err != nil {
		return AdminApprovalControl{}, err
	}
	if err := validateAdminApprovalControl(control); err != nil {
		return AdminApprovalControl{}, err
	}
	return control, nil
}

func validateAdminApprovalControl(control AdminApprovalControl) error {
	if control.ControlID != adminApprovalControlID || control.SchemaVersion != 1 ||
		control.ApprovalEpoch <= 0 || control.AuthorizationRelayEpoch <= 0 || control.Version <= 0 ||
		control.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: invalid identity, schema, epoch, version, or timestamp", ErrAdminApprovalControlInvalid)
	}
	if control.AuthorizationRelayState != AdminApprovalRelayRunning && control.AuthorizationRelayState != AdminApprovalRelayFenced {
		return fmt.Errorf("%w: unknown authorization relay state", ErrAdminApprovalControlInvalid)
	}
	switch control.Mode {
	case AdminApprovalModeLegacy, AdminApprovalModeQuiescing:
		if control.ApprovalWireVersion != 1 || control.ProofRequired {
			return fmt.Errorf("%w: invalid legacy control tuple", ErrAdminApprovalControlInvalid)
		}
	case AdminApprovalModeProofRequired:
		if control.ApprovalWireVersion != 2 || !control.ProofRequired {
			return fmt.Errorf("%w: invalid proof-required control tuple", ErrAdminApprovalControlInvalid)
		}
	case AdminApprovalModeSafeRollback:
		if control.ApprovalWireVersion != 1 || !control.ProofRequired {
			return fmt.Errorf("%w: invalid safe-rollback control tuple", ErrAdminApprovalControlInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown mode", ErrAdminApprovalControlInvalid)
	}
	return nil
}

// ReadAdminApprovalControl reads but does not lock the singleton. The caller
// owns the transaction and its deadline.
func ReadAdminApprovalControl(ctx context.Context, tx pgx.Tx) (AdminApprovalControl, error) {
	return scanAdminApprovalControl(tx.QueryRow(ctx,
		`SELECT `+adminApprovalControlColumns+` FROM account_approval_control.admin_approval_control WHERE control_id='ADMIN_APPROVAL'`,
	))
}

// LockAdminApprovalControlShared takes the fixed SECURITY DEFINER shared lock.
// PostgreSQL retains the row lock until the caller-owned transaction ends.
func LockAdminApprovalControlShared(ctx context.Context, tx pgx.Tx) (AdminApprovalControl, error) {
	return scanAdminApprovalControl(tx.QueryRow(ctx,
		`SELECT `+adminApprovalControlColumns+` FROM account_approval_control.lock_admin_approval_control_shared()`,
	))
}

// LockAdminApprovalControlExclusive directly locks the protected table. It is
// intentionally usable only by the schema owner/migrator; account_service has
// SELECT but no UPDATE privilege and therefore cannot call this successfully.
func LockAdminApprovalControlExclusive(ctx context.Context, tx pgx.Tx) (AdminApprovalControl, error) {
	return scanAdminApprovalControl(tx.QueryRow(ctx,
		`SELECT `+adminApprovalControlColumns+` FROM account_approval_control.admin_approval_control WHERE control_id='ADMIN_APPROVAL' FOR UPDATE`,
	))
}

type AdminApprovalBeginPolicy string

const (
	AdminApprovalBeginLegacyPublished AdminApprovalBeginPolicy = "LEGACY_PUBLISHED"
	AdminApprovalBeginLegacyPreFence  AdminApprovalBeginPolicy = "LEGACY_PRE_FENCE"
	AdminApprovalBeginProofBound      AdminApprovalBeginPolicy = "PROOF_BOUND"
	AdminApprovalBeginExistingOnly    AdminApprovalBeginPolicy = "EXISTING_ONLY"
	AdminApprovalBeginDisabled        AdminApprovalBeginPolicy = "DISABLED"
)

type AdminApprovalRelayPolicy string

const (
	AdminApprovalRelayNormal      AdminApprovalRelayPolicy = "NORMAL"
	AdminApprovalRelayLegacyDrain AdminApprovalRelayPolicy = "LEGACY_DRAIN"
	AdminApprovalRelaySafeDrain   AdminApprovalRelayPolicy = "SAFE_DRAIN"
	AdminApprovalRelayDisabled    AdminApprovalRelayPolicy = "DISABLED"
)

// AdminApprovalActions is a typed classification of the eight frozen
// mode-by-relay tuples. It does not authorize an operation and is not wired to
// an approval, Relay, consumer, or recovery path in GSA-04.
type AdminApprovalActions struct {
	DraftWrites bool
	Approve     bool
	BeginPolicy AdminApprovalBeginPolicy
	SafeClose   bool
	HistoryRead bool
	RelayPolicy AdminApprovalRelayPolicy
}

func AdminApprovalActionsFor(control AdminApprovalControl) (AdminApprovalActions, error) {
	if err := validateAdminApprovalControl(control); err != nil {
		return AdminApprovalActions{}, err
	}
	actions := AdminApprovalActions{SafeClose: true, HistoryRead: true}
	switch {
	case control.Mode == AdminApprovalModeLegacy && control.AuthorizationRelayState == AdminApprovalRelayRunning:
		actions.DraftWrites = true
		actions.Approve = true
		actions.BeginPolicy = AdminApprovalBeginLegacyPublished
		actions.RelayPolicy = AdminApprovalRelayNormal
	case control.Mode == AdminApprovalModeLegacy && control.AuthorizationRelayState == AdminApprovalRelayFenced:
		actions.DraftWrites = true
		actions.BeginPolicy = AdminApprovalBeginLegacyPublished
		actions.RelayPolicy = AdminApprovalRelayDisabled
	case control.Mode == AdminApprovalModeQuiescing && control.AuthorizationRelayState == AdminApprovalRelayRunning:
		actions.DraftWrites = true
		actions.BeginPolicy = AdminApprovalBeginLegacyPreFence
		actions.RelayPolicy = AdminApprovalRelayLegacyDrain
	case control.Mode == AdminApprovalModeQuiescing && control.AuthorizationRelayState == AdminApprovalRelayFenced:
		actions.DraftWrites = true
		actions.BeginPolicy = AdminApprovalBeginExistingOnly
		actions.RelayPolicy = AdminApprovalRelayDisabled
	case control.Mode == AdminApprovalModeProofRequired && control.AuthorizationRelayState == AdminApprovalRelayRunning:
		actions.DraftWrites = true
		actions.Approve = true
		actions.BeginPolicy = AdminApprovalBeginProofBound
		actions.RelayPolicy = AdminApprovalRelayNormal
	case control.Mode == AdminApprovalModeProofRequired && control.AuthorizationRelayState == AdminApprovalRelayFenced:
		actions.DraftWrites = true
		actions.BeginPolicy = AdminApprovalBeginProofBound
		actions.RelayPolicy = AdminApprovalRelayDisabled
	case control.Mode == AdminApprovalModeSafeRollback && control.AuthorizationRelayState == AdminApprovalRelayRunning:
		actions.BeginPolicy = AdminApprovalBeginDisabled
		actions.RelayPolicy = AdminApprovalRelaySafeDrain
	case control.Mode == AdminApprovalModeSafeRollback && control.AuthorizationRelayState == AdminApprovalRelayFenced:
		actions.BeginPolicy = AdminApprovalBeginDisabled
		actions.RelayPolicy = AdminApprovalRelayDisabled
	default:
		return AdminApprovalActions{}, fmt.Errorf("%w: action tuple is not classified", ErrAdminApprovalControlInvalid)
	}
	return actions, nil
}
