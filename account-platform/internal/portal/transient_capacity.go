package portal

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/redis/go-redis/v9"
)

type transientKind string

const (
	flowTransient    transientKind = "flow"
	handoffTransient transientKind = "session-handoff"
)

var transientID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

//go:embed capacity_bootstrap.lua
var capacityBootstrapScript string

//go:embed capacity_save.lua
var capacitySaveScript string

//go:embed capacity_take.lua
var capacityTakeScript string

//go:embed capacity_reserve.lua
var capacityReserveScript string

//go:embed capacity_release.lua
var capacityReleaseScript string

const handoffReservationLifetime = 2 * time.Minute

func (s *RedisStore) ReserveSessionHandoff(ctx context.Context, id string, until time.Time) error {
	if !transientID.MatchString(id) {
		return ErrInvalidTransientObject
	}
	ttl := time.Until(until)
	if ttl.Milliseconds() < 1 || ttl > handoffReservationLifetime {
		return ErrInvalidTransientObject
	}
	if err := s.prepareTransientCapacity(ctx, handoffTransient); err != nil {
		return err
	}
	ttl = time.Until(until)
	if ttl.Milliseconds() < 1 {
		return ErrSessionNotFound
	}
	result, err := s.transientClient.Eval(ctx, "#!lua\n"+admissionCommonScript+capacityReserveScript,
		[]string{transientIndex(handoffTransient), transientInventory(handoffTransient)}, id, ttl.Milliseconds(), s.limits.HandoffCapacity).Int64Slice()
	if err != nil {
		return err
	}
	return admissionPair(result)
}

func (s *RedisStore) ReleaseSessionHandoffReservation(ctx context.Context, id string) error {
	if !transientID.MatchString(id) {
		return ErrInvalidTransientObject
	}
	return s.transientClient.Eval(ctx, "#!lua flags=allow-oom\n"+admissionCommonScript+capacityReleaseScript,
		[]string{transientPrefix(handoffTransient) + id, transientIndex(handoffTransient)}, id).Err()
}

func transientIndex(k transientKind) string     { return "portal:transient-capacity:v1:" + string(k) }
func transientInventory(k transientKind) string { return transientIndex(k) + ":inventory" }
func transientPrefix(k transientKind) string    { return "portal:" + string(k) + ":" }

// Bootstrap uses one bounded SCAN page per call. Old producers must be drained
// at the first upgrade; existing consumers remain usable during inventory.
func (s *RedisStore) prepareTransientCapacity(ctx context.Context, k transientKind) error {
	result, err := s.transientClient.Eval(ctx, "#!lua\n"+admissionCommonScript+capacityBootstrapScript,
		[]string{transientIndex(k), transientInventory(k)}, transientPrefix(k)).Int64Slice()
	if err != nil {
		return err
	}
	return admissionPair(result)
}

func (s *RedisStore) saveTransient(ctx context.Context, k transientKind, id string, value any, expires time.Time) error {
	if !transientID.MatchString(id) {
		return ErrInvalidTransientObject
	}
	ttl := time.Until(expires)
	maxTTL, maxBytes, capacity := loginFlowLifetime, 16*1024, s.limits.FlowCapacity
	if k == handoffTransient {
		maxTTL, maxBytes, capacity = time.Minute, 64*1024, s.limits.HandoffCapacity
	}
	if ttl.Milliseconds() < 1 {
		return ErrSessionNotFound
	}
	if ttl > maxTTL {
		return ErrInvalidTransientObject
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(body) > maxBytes {
		return ErrInvalidTransientObject
	}
	if err = s.prepareTransientCapacity(ctx, k); err != nil {
		return err
	}
	ttl = time.Until(expires)
	if ttl.Milliseconds() < 1 {
		return ErrSessionNotFound
	}
	result, err := s.transientClient.Eval(ctx, "#!lua\n"+admissionCommonScript+capacitySaveScript,
		[]string{transientPrefix(k) + id, transientIndex(k), transientInventory(k)}, id, body, ttl.Milliseconds(), capacity).Int64Slice()
	if err != nil {
		return err
	}
	return admissionPair(result)
}

func (s *RedisStore) takeTransient(ctx context.Context, k transientKind, id string, target any) error {
	if !transientID.MatchString(id) {
		return ErrSessionNotFound
	}
	body, err := s.transientClient.Eval(ctx, "#!lua flags=allow-oom\n"+admissionCommonScript+capacityTakeScript,
		[]string{transientPrefix(k) + id, transientIndex(k)}, id).Text()
	if errors.Is(err, redis.Nil) {
		return ErrSessionNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(body), target)
}
