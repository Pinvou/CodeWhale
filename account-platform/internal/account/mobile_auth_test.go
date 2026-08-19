package account

import "testing"

func TestNormalizeMainlandMobile(t *testing.T) {
	for _, input := range []string{"13800138000", "+8613800138000", "8613800138000"} {
		mobile, err := NormalizeMainlandMobile(input)
		if err != nil || mobile != "+8613800138000" {
			t.Fatalf("normalize %q: mobile=%q err=%v", input, mobile, err)
		}
	}
	for _, input := range []string{"", "12800138000", "+85213800138000"} {
		if _, err := NormalizeMainlandMobile(input); err == nil {
			t.Fatalf("expected %q to be rejected", input)
		}
	}
}

func TestRegistrationPasswordPolicy(t *testing.T) {
	if !validPassword("pinvou2026") || validPassword("password") || validPassword("12345678") || validPassword("a1") {
		t.Fatal("registration password policy mismatch")
	}
}
