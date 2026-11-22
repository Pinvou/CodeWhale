package manualreview

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const onCallSeparator = ","

type Deadlines struct {
	Claim         time.Time
	FirstResponse time.Time
	Mitigation    time.Time
	Resolution    time.Time
}

// DeadlinesFor freezes the default NFR-45 clock at case creation. Assignment
// and escalation callers must persist these values and never recalculate them.
func DeadlinesFor(createdAt time.Time, severity string) (Deadlines, error) {
	switch strings.ToUpper(strings.TrimSpace(severity)) {
	case "P0":
		return Deadlines{
			Claim:         createdAt.Add(15 * time.Minute),
			FirstResponse: createdAt.Add(30 * time.Minute),
			Mitigation:    createdAt.Add(2 * time.Hour),
			Resolution:    createdAt.Add(4 * time.Hour),
		}, nil
	case "P1":
		return Deadlines{
			Claim:         createdAt.Add(30 * time.Minute),
			FirstResponse: createdAt.Add(time.Hour),
			Mitigation:    createdAt.Add(4 * time.Hour),
			Resolution:    createdAt.Add(24 * time.Hour),
		}, nil
	case "P2":
		return Deadlines{
			Claim:         AddBusinessHours(createdAt, 4),
			FirstResponse: AddBusinessHours(createdAt, 8),
			Mitigation:    AddBusinessHours(createdAt, 8),
			Resolution:    AddBusinessHours(createdAt, 16),
		}, nil
	default:
		return Deadlines{}, errors.New("manual review severity must be P0, P1, or P2")
	}
}

func AddBusinessHours(start time.Time, hours int) time.Time {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	current := start.In(location)
	remaining := time.Duration(hours) * time.Hour
	for remaining > 0 {
		if current.Weekday() == time.Saturday || current.Weekday() == time.Sunday || current.Hour() < 9 || current.Hour() >= 17 {
			current = nextBusinessHour(current)
			continue
		}
		endOfDay := time.Date(current.Year(), current.Month(), current.Day(), 17, 0, 0, 0, location)
		available := endOfDay.Sub(current)
		if remaining <= available {
			return current.Add(remaining).UTC()
		}
		remaining -= available
		current = nextBusinessHour(endOfDay)
	}
	return current.UTC()
}

func nextBusinessHour(current time.Time) time.Time {
	location := current.Location()
	if current.Hour() < 9 && current.Weekday() != time.Saturday && current.Weekday() != time.Sunday {
		return time.Date(current.Year(), current.Month(), current.Day(), 9, 0, 0, 0, location)
	}
	current = time.Date(current.Year(), current.Month(), current.Day(), 9, 0, 0, 0, location).AddDate(0, 0, 1)
	for current.Weekday() == time.Saturday || current.Weekday() == time.Sunday {
		current = current.AddDate(0, 0, 1)
	}
	return current
}

type OnCallSchedule struct {
	owners []uuid.UUID
}

// ParseOnCallSchedule accepts an ordered escalation chain. Production callers
// must require a non-empty schedule so an SLA breach cannot degrade to a mere
// notification.
func ParseOnCallSchedule(raw string, required bool) (OnCallSchedule, error) {
	parts := strings.Split(strings.TrimSpace(raw), onCallSeparator)
	seen := map[uuid.UUID]struct{}{}
	owners := make([]uuid.UUID, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		owner, err := uuid.Parse(part)
		if err != nil || owner == uuid.Nil {
			return OnCallSchedule{}, errors.New("MANUAL_REVIEW_ON_CALL_USER_IDS must contain ordered UUIDs")
		}
		if _, exists := seen[owner]; exists {
			return OnCallSchedule{}, errors.New("MANUAL_REVIEW_ON_CALL_USER_IDS must not repeat an owner")
		}
		seen[owner] = struct{}{}
		owners = append(owners, owner)
	}
	if required && len(owners) == 0 {
		return OnCallSchedule{}, errors.New("MANUAL_REVIEW_ON_CALL_USER_IDS is required in production")
	}
	return OnCallSchedule{owners: owners}, nil
}

func (s OnCallSchedule) Next(escalationLevel int) (uuid.UUID, bool) {
	if len(s.owners) == 0 {
		return uuid.Nil, false
	}
	if escalationLevel < 0 {
		escalationLevel = 0
	}
	if escalationLevel >= len(s.owners) {
		escalationLevel = len(s.owners) - 1
	}
	return s.owners[escalationLevel], true
}

func RequiresStopLoss(severity, overdueStage string) bool {
	severity = strings.ToUpper(strings.TrimSpace(severity))
	return (severity == "P0" || severity == "P1") && strings.ToUpper(strings.TrimSpace(overdueStage)) == "MITIGATION_OVERDUE"
}
