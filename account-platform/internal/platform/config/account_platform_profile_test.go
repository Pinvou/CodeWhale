package config

import "testing"

func TestAccountPlatformProfileExplicitSelection(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  AccountPlatformProfile
	}{
		{"", AccountPlatformProfileModels}, {"models", AccountPlatformProfileModels},
		{"personal", AccountPlatformProfilePersonal}, {" personal ", AccountPlatformProfilePersonal},
	} {
		t.Run("valid-"+tc.value, func(t *testing.T) {
			got, err := ParseAccountPlatformProfile(tc.value)
			if err != nil || got != tc.want {
				t.Fatalf("selection=%q err=%v", got, err)
			}
		})
	}
	for _, value := range []string{"unknown", "PERSONAL", "model", "personal,models"} {
		t.Run("invalid-"+value, func(t *testing.T) {
			if got, err := ParseAccountPlatformProfile(value); err == nil || got != "" {
				t.Fatalf("unknown selector accepted=%q err=%v", got, err)
			}
		})
	}
}
