package portal

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/redisconnection"
	"github.com/redis/go-redis/v9"
)

type capacityInventoryError struct {
	kind  transientKind
	stage string
	err   error
}

func (e *capacityInventoryError) Error() string { return "Portal capacity inventory failed" }
func (e *capacityInventoryError) Unwrap() error { return e.err }

// CapacityInventoryDiagnostic contains only fixed, non-secret operational labels.
type CapacityInventoryDiagnostic struct{ Code, Component, Stage string }

// DescribeCapacityInventoryError never includes an error message, key or address.
// Wrapped errors retain their identity for the existing retry/fail-closed paths.
func DescribeCapacityInventoryError(err error) CapacityInventoryDiagnostic {
	d := CapacityInventoryDiagnostic{Code: "PORTAL_CAPACITY_INVENTORY_FAILED", Component: "unknown", Stage: "inventory"}
	var inventory *capacityInventoryError
	if errors.As(err, &inventory) {
		switch inventory.kind {
		case flowTransient, handoffTransient:
			d.Component = string(inventory.kind)
		}
		switch inventory.stage {
		case "prepare", "inspect":
			d.Stage = inventory.stage
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		d.Code = "PORTAL_INVENTORY_CANCELED"
	case errors.Is(err, context.DeadlineExceeded):
		d.Code = "PORTAL_INVENTORY_TIMEOUT"
	case errors.Is(err, ErrCapacityInventoryRequired):
		d.Code = "PORTAL_INVENTORY_OPERATIONS_REQUIRED"
	case errors.Is(err, ErrCapacityPreparing):
		d.Code = "PORTAL_INVENTORY_PREPARING"
	case errors.Is(err, redisconnection.ErrConfiguration):
		d.Code = "PORTAL_INVENTORY_CONFIGURATION_INVALID"
	default:
		var server redis.Error
		var network net.Error
		if errors.As(err, &server) {
			// Lua errors include a script location before the fixed error token.
			for _, code := range []string{"PORTAL_TRANSIENT_TTL_REQUIRED", "PORTAL_ADMISSION_STATE_INVALID", "PORTAL_INVENTORY_PAGE_TOO_LARGE", "PORTAL_ADMISSION_COMMANDS_REQUIRED"} {
				if strings.Contains(server.Error(), code) {
					d.Code = code
					return d
				}
			}
			switch {
			case redis.HasErrorPrefix(err, "NOPERM"):
				d.Code = "PORTAL_ADMISSION_COMMANDS_REQUIRED"
			case redis.HasErrorPrefix(err, "NOAUTH"), redis.HasErrorPrefix(err, "WRONGPASS"):
				d.Code = "PORTAL_INVENTORY_AUTHENTICATION_FAILED"
			case redis.HasErrorPrefix(err, "OOM"):
				d.Code = "PORTAL_INVENTORY_REDIS_OOM"
			case redis.HasErrorPrefix(err, "WRONGTYPE"):
				d.Code = "PORTAL_ADMISSION_STATE_INVALID"
			}
		} else if errors.As(err, &network) {
			d.Code = "PORTAL_INVENTORY_REDIS_UNAVAILABLE"
			if network.Timeout() {
				d.Code = "PORTAL_INVENTORY_TIMEOUT"
			}
		}
	}
	return d
}

type inventoryFailureReporter map[CapacityInventoryDiagnostic]time.Time

func (reported *inventoryFailureReporter) report(ctx context.Context, err error, now time.Time) {
	if err == nil {
		*reported = nil // A recovered pass starts a new reporting episode.
		return
	}
	d := DescribeCapacityInventoryError(err)
	if last, ok := (*reported)[d]; ok && !now.Before(last) && now.Sub(last) < time.Minute {
		return
	}
	if *reported == nil {
		*reported = make(inventoryFailureReporter)
	}
	(*reported)[d] = now // Finite codes/kinds/stages bound this map independently of error text.
	slog.ErrorContext(ctx, "portal_capacity_inventory_failed", "error_code", d.Code, "component", d.Component, "stage", d.Stage)
}
