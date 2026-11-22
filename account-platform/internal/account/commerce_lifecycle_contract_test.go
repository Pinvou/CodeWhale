package account

import "testing"

func TestCommerceCallbackLifecycleConflict(t *testing.T) {
	for _, tc := range []struct {
		name, subjectType, status string
		expected, actual          int64
		conflict                  bool
	}{
		{"current_active_user", "USER", "ACTIVE", 2, 2, false},
		{"current_disabled_user", "USER", "DISABLED", 2, 2, false},
		{"current_closing_user_reaches_existing_noop", "USER", "CLOSING", 2, 2, false},
		{"retired_active_user", "USER", "ACTIVE", 2, 3, true},
		{"retired_disabled_user", "USER", "DISABLED", 2, 3, true},
		{"retired_closing_user", "USER", "CLOSING", 2, 3, true},
		{"unexpected_older_user_version", "USER", "ACTIVE", 2, 1, true},
		{"deleted_user", "USER", "DELETED", 2, 2, true},
		{"anonymized_user", "USER", "ANONYMIZED", 2, 2, true},
		{"non_user_ignores_user_lifecycle", "ORGANIZATION", "ACTIVE", 2, 3, false},
		{"non_user_disabled_status_unchanged", "ORGANIZATION", "DISABLED", 2, 3, false},
		{"non_user_closing_status_unchanged", "ORGANIZATION", "CLOSING", 2, 3, false},
		{"non_user_deleted_status_fence_unchanged", "ORGANIZATION", "DELETED", 2, 3, true},
		{"non_user_anonymized_status_fence_unchanged", "ORGANIZATION", "ANONYMIZED", 2, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commerceCallbackLifecycleConflict(tc.subjectType, tc.expected, tc.actual, tc.status); got != tc.conflict {
				t.Fatalf("lifecycle conflict = %t, want %t", got, tc.conflict)
			}
		})
	}
}
