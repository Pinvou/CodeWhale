package h3cmall

import (
	"encoding/base64"
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
