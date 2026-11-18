package portal

import (
	"context"
	"errors"
	"time"
)

// RunTransientInventory progresses independently of new login traffic. It also
// recovers inventory after a Redis restoration without dropping original data.
func (s *RedisStore) RunTransientInventory(ctx context.Context) {
	runTransientInventory(ctx, s.prepareTransientCapacity)
}

func runTransientInventory(ctx context.Context, prepare func(context.Context, transientKind) error) {
	var reported inventoryFailureReporter
	for ctx.Err() == nil {
		ready, err := transientInventoryPass(ctx, prepare)
		if ctx.Err() == nil {
			reported.report(ctx, err, time.Now())
		}
		pause := 10 * time.Millisecond
		if ready || err != nil {
			pause = time.Second
		}
		if errors.Is(err, ErrCapacityInventoryRequired) {
			pause = 10 * time.Second
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// InventoryTransientCapacity is the explicit operations command. It only
// builds metadata from original native expiry times, and never changes payloads.
func (s *RedisStore) InventoryTransientCapacity(ctx context.Context) error {
	for ctx.Err() == nil {
		ready, err := transientInventoryPass(ctx, s.prepareTransientCapacity)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

type CapacityInventoryState struct {
	FlowReady          bool  `json:"flow_ready"`
	FlowActiveSlots    int64 `json:"flow_active_slots"`
	HandoffReady       bool  `json:"handoff_ready"`
	HandoffActiveSlots int64 `json:"handoff_active_slots"`
}

func (s *RedisStore) InspectTransientCapacity(ctx context.Context) (out CapacityInventoryState, err error) {
	for i, k := range []transientKind{flowTransient, handoffTransient} {
		values, e := s.transientClient.Eval(ctx, "#!lua flags=no-writes\n"+admissionCommonScript+`
expect(KEYS[1], 'zset')
expect(KEYS[2], 'hash')
need('HGET', KEYS[2], 'ready')
need('ZCOUNT', KEYS[1], '-inf', '+inf')
local now=milliseconds()
local ready=0
if redis.call('HGET', KEYS[2], 'ready')=='1' then ready=1 end
return {ready,redis.call('ZCOUNT', KEYS[1], '('..tostring(now), '+inf')}
`, []string{transientIndex(k), transientInventory(k)}).Int64Slice()
		if e != nil {
			return out, &capacityInventoryError{kind: k, stage: "inspect", err: e}
		}
		if len(values) != 2 {
			return out, &capacityInventoryError{kind: k, stage: "inspect", err: ErrAdmissionUnavailable}
		}
		if i == 0 {
			out.FlowReady = values[0] == 1
			out.FlowActiveSlots = values[1]
		} else {
			out.HandoffReady = values[0] == 1
			out.HandoffActiveSlots = values[1]
		}
	}
	return out, nil
}

func transientInventoryPass(ctx context.Context, prepare func(context.Context, transientKind) error) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ready := [2]bool{}
	kinds := [2]transientKind{flowTransient, handoffTransient}
	for page := 0; page < 32; page++ {
		for i, k := range kinds {
			if ready[i] {
				continue
			}
			err := prepare(ctx, k)
			if err == nil {
				ready[i] = true
			} else if !errors.Is(err, ErrCapacityPreparing) {
				return false, &capacityInventoryError{kind: k, stage: "prepare", err: err}
			}
		}
		if ready[0] && ready[1] {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
	}
	return false, nil
}
