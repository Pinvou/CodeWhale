package h3cmall

import (
	"encoding/base64"
	"errors"
	"testing"
)

func TestValidQRDataOnlyAcceptsPNGData(t *testing.T) {
	png := append([]byte{137, 80, 78, 71, 13, 10, 26, 10}, []byte("test")...)
	valid := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	if !ValidQRData(valid) {
		t.Fatal("valid PNG data URL was rejected")
	}
	for _, value := range []string{"https://example.com/qr", "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(png), "data:image/png;base64,broken"} {
		if ValidQRData(value) {
			t.Fatalf("invalid QR data accepted: %q", value)
		}
	}
}

func TestMallRequestMobileConvertsE164ToMallFormat(t *testing.T) {
	for input, expected := range map[string]string{
		"+8613800138000": "13800138000",
		"13800138000":    "13800138000",
	} {
		actual, err := mallRequestMobile(input)
		if err != nil || actual != expected {
			t.Fatalf("mallRequestMobile(%q) = %q, %v; want %q", input, actual, err, expected)
		}
	}
	for _, input := range []string{"+85213800138000", "1380013800", "1380013800x"} {
		if _, err := mallRequestMobile(input); !errors.Is(err, ErrRejected) {
			t.Fatalf("mallRequestMobile(%q) error = %v; want ErrRejected", input, err)
		}
	}
}

func TestCanonicalMainlandMobileUsesE164(t *testing.T) {
	actual, err := canonicalMainlandMobile("13800138000")
	if err != nil || actual != "+8613800138000" {
		t.Fatalf("canonicalMainlandMobile() = %q, %v", actual, err)
	}
}
