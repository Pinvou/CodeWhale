//go:build linux && evidencecontainer

package billing

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// This opt-in test runs inside the disposable CI client container against the
// real worker and exercises the actual Billing client, not a protocol mock.
func TestEvidenceValidatorContainer(t *testing.T) {
	mode := os.Getenv("EVIDENCE_CONTAINER_TEST_MODE")
	if mode != "allowed" && mode != "wrong-caller" {
		t.Fatal("missing or unknown container test mode")
	}
	v, err := newEvidenceImageValidator("/run/evidence/validator.sock", 65533)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("png", func(t *testing.T) {
		var data bytes.Buffer
		if err := png.Encode(&data, image.NewGray(image.Rect(0, 0, 2, 2))); err != nil {
			t.Fatal(err)
		}
		want := error(nil)
		if mode == "wrong-caller" {
			want = errImageValidatorUnavailable
		}
		if got := v.Validate(context.Background(), "image/png", data.Bytes()); !errors.Is(got, want) {
			t.Fatalf("got %v; want %v", got, want)
		}
	})
	cases := []struct {
		file, media string
		want        error
	}{
		{"progressive.jpg", "image/jpeg", nil},
		{"animated.png", "image/png", errImageValidatorInvalid},
		{"pdf/plain-compressed.pdf", "application/pdf", nil},
		{"pdf/truncated.pdf", "application/pdf", errImageValidatorInvalid},
		{"pdf/user-password.pdf", "application/pdf", errEvidencePDFPasswordRequired},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			data, readErr := os.ReadFile(filepath.Join("/ci/fixtures", tc.file))
			if readErr != nil {
				t.Fatal(readErr)
			}
			got := v.Validate(context.Background(), tc.media, data)
			want := tc.want
			if mode == "wrong-caller" {
				want = errImageValidatorUnavailable
			}
			if !errors.Is(got, want) {
				t.Fatalf("got %v; want %v", got, want)
			}
		})
	}
	if mode == "allowed" {
		// Correctly authenticated socket, deliberately wrong expected server UID:
		// the real client must refuse it before sending any request bytes.
		wrong, err := newEvidenceImageValidator("/run/evidence/validator.sock", 65534)
		if err != nil {
			t.Fatal(err)
		}
		if got := wrong.Validate(context.Background(), "image/png", []byte("synthetic")); !errors.Is(got, errImageValidatorUnavailable) {
			t.Fatalf("wrong server UID: got %v", got)
		}
	}
}
