package account

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"testing"
	"time"
)

func TestPublicMobileAdmissionPrecedesValidationAndGeneration(t *testing.T) {
	for _, operation := range []string{"PRECHECK", "CAPTCHA"} {
		repository := &mobileAuthRepositoryFake{publicAdmissionErr: &SMSRateLimitError{RetryAfter: time.Minute}}
		service := newMobileAuthForTest(t, repository, &smsProviderFake{}, &identityProvisionerFake{})
		ctx := withAuthRisk(context.Background(), "trusted-source", "rotated-device")
		var err error
		if operation == "PRECHECK" {
			_, err = service.PrecheckRegistration(ctx, "invalid", "invalid", "", "")
		} else {
			_, err = service.CreateSliderCaptcha(ctx, "invalid", SMSPurposeLogin)
		}
		if !errors.Is(err, ErrSMSRateLimited) || repository.publicAdmissionSource != "trusted-source" || repository.sliderChallenge.TargetX != 0 {
			t.Fatalf("operation=%s did not fail before business work: %v", operation, err)
		}
	}
}

func TestCaptchaNoLongerDisclosesReversiblePiecePixels(t *testing.T) {
	for range 8 {
		const target, pieceY = 140, 30
		background, piece, err := renderSliderCaptcha(target, pieceY)
		if err != nil {
			t.Fatal(err)
		}
		bg, err := png.Decode(bytes.NewReader(background))
		if err != nil {
			t.Fatal(err)
		}
		p, err := png.Decode(bytes.NewReader(piece))
		if err != nil {
			t.Fatal(err)
		}
		if bg.Bounds().Dx() != sliderCaptchaWidth || p.Bounds().Dx() != sliderCaptchaPieceSize {
			t.Fatal("wire image dimensions changed")
		}
		best, matchAtTarget := 0, 0
		for offset := 72; offset <= sliderCaptchaWidth-sliderCaptchaPieceSize-14; offset++ {
			matches := 0
			for y := 0; y < sliderCaptchaPieceSize; y++ {
				for x := 0; x < sliderCaptchaPieceSize; x++ {
					if !sliderPieceMask(x, y) {
						continue
					}
					pr, pg, pb, _ := p.At(x, y).RGBA()
					br, bg, bb, _ := bg.At(offset+x, pieceY+y).RGBA()
					if (pr>>8)/3 == br>>8 && (pg>>8)/3 == bg>>8 && (pb>>8)/3 == bb>>8 {
						matches++
					}
				}
			}
			if offset == target {
				matchAtTarget = matches
			}
			best = max(best, matches)
		}
		// The previous image guaranteed equality for every opaque piece pixel
		// at the target. No offset may retain that trivial lossless oracle.
		if best > 20 || matchAtTarget > 20 {
			t.Fatalf("reversible oracle remains: best=%d target=%d", best, matchAtTarget)
		}
	}
}
