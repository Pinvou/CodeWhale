package portal

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

type PortalOperation string

const (
	LoginStartOperation     PortalOperation = "login-start"
	LoginCallbackOperation  PortalOperation = "login-callback"
	HandoffCreateOperation  PortalOperation = "handoff-create"
	HandoffConsumeOperation PortalOperation = "handoff-consume"
)

type AdmissionLimits struct {
	OperationsPerSecond int
	OperationBurst      int
	FlowCapacity        int
	HandoffCapacity     int
}

func DefaultAdmissionLimits() AdmissionLimits {
	return AdmissionLimits{OperationsPerSecond: 100, OperationBurst: 200, FlowCapacity: 65536, HandoffCapacity: 8192}
}

var ErrAdmissionConfiguration = errors.New("invalid Portal admission configuration")
var ErrAdmissionUnavailable = errors.New("Portal shared admission is unavailable")
var ErrCapacityPreparing = errors.New("Portal transient capacity inventory is not ready")
var ErrCapacityInventoryRequired = errors.New("Portal transient capacity inventory requires the operations principal")
var ErrTransientConflict = errors.New("Portal transient identifier already binds another value")
var ErrInvalidTransientObject = errors.New("invalid Portal transient object")

func (l AdmissionLimits) Valid() bool {
	return l.OperationsPerSecond >= 1 && l.OperationsPerSecond <= 10000 && l.OperationBurst >= 1 && l.OperationBurst <= 1000000 &&
		l.FlowCapacity >= 1 && l.FlowCapacity <= 1000000 && l.HandoffCapacity >= 1 && l.HandoffCapacity <= 1000000
}

func (l AdmissionLimits) fingerprint() string {
	return fmt.Sprintf("v1:%d:%d:%d:%d", l.OperationsPerSecond, l.OperationBurst, l.FlowCapacity, l.HandoffCapacity)
}

type PortalQuotaError struct{ RetryAfter time.Duration }

func (*PortalQuotaError) Error() string { return "Portal shared quota is full" }

// Only four fixed-size rate hashes may grow during OOM; no request/source keys.
// Payload and reservation scripts deliberately retain the normal OOM gate.
//go:embed admission_common.lua
var admissionCommonScript string

//go:embed operation_admission.lua
var operationAdmissionScript string

func (s *RedisStore) AdmitOperation(ctx context.Context, operation PortalOperation) error {
	switch operation {
	case LoginStartOperation, LoginCallbackOperation, HandoffCreateOperation, HandoffConsumeOperation:
	default:
		return ErrAdmissionConfiguration
	}
	result, err := s.transientClient.Eval(ctx, "#!lua flags=allow-oom\n"+admissionCommonScript+operationAdmissionScript,
		[]string{"portal:operation-admission:v1:" + string(operation)}, s.limits.OperationsPerSecond, s.limits.OperationBurst, s.limits.fingerprint()).Int64Slice()
	if err != nil {
		return err
	}
	if err = admissionPair(result); err != nil {
		return err
	}
	switch operation {
	case LoginStartOperation:
		return s.prepareTransientCapacity(ctx, flowTransient)
	case HandoffCreateOperation:
		return s.prepareTransientCapacity(ctx, handoffTransient)
	}
	return nil
}

func admissionPair(result []int64) error {
	if len(result) != 2 {
		return ErrAdmissionUnavailable
	}
	switch result[0] {
	case 1:
		return nil
	case 0:
		if result[1] < 1 || result[1] > int64((24*time.Hour)/time.Millisecond) {
			return ErrAdmissionUnavailable
		}
		return &PortalQuotaError{RetryAfter: time.Duration(result[1]) * time.Millisecond}
	case -2:
		return ErrTransientConflict
	case -3:
		return ErrCapacityPreparing
	case -4:
		return ErrCapacityInventoryRequired
	default:
		return ErrAdmissionUnavailable
	}
}

func writePortalQuotaError(w http.ResponseWriter, r *http.Request, err error) bool {
	var quota *PortalQuotaError
	if !errors.As(err, &quota) {
		return false
	}
	seconds := int(math.Ceil(quota.RetryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 600 {
		seconds = 600
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	api.WriteError(w, http.StatusTooManyRequests, "RATE_LIMITED", "the Portal is busy; retry after the indicated delay", httpserver.RequestID(r.Context()))
	return true
}

func (s *Service) admitPortalOperation(w http.ResponseWriter, r *http.Request, operation PortalOperation, unavailableCode string) bool {
	if err := s.store.AdmitOperation(r.Context(), operation); err != nil {
		if !writePortalQuotaError(w, r, err) {
			if errors.Is(err, ErrCapacityPreparing) || errors.Is(err, ErrCapacityInventoryRequired) {
				w.Header().Set("Retry-After", "1")
			}
			api.WriteError(w, http.StatusServiceUnavailable, unavailableCode, "shared Portal admission is unavailable", httpserver.RequestID(r.Context()))
		}
		return false
	}
	return true
}
