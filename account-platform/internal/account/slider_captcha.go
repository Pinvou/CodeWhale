package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	sliderCaptchaWidth     = 280
	sliderCaptchaHeight    = 120
	sliderCaptchaPieceSize = 44
	sliderCaptchaTolerance = 5
)

type SliderCaptcha struct {
	ID        uuid.UUID
	Mobile    string
	Purpose   SMSPurpose
	TargetX   int
	ExpiresAt time.Time
}

type SliderCaptchaResponse struct {
	ID              uuid.UUID `json:"captcha_id"`
	BackgroundImage string    `json:"background_image"`
	PieceImage      string    `json:"piece_image"`
	Width           int       `json:"width"`
	Height          int       `json:"height"`
	PieceWidth      int       `json:"piece_width"`
	PieceHeight     int       `json:"piece_height"`
	PieceY          int       `json:"piece_y"`
	ExpiresAt       time.Time `json:"expires_at"`
}

func (s *MobileAuthService) CreateSliderCaptcha(ctx context.Context, rawMobile string, purpose SMSPurpose) (SliderCaptchaResponse, error) {
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil || purpose != SMSPurposeRegister && purpose != SMSPurposeLogin {
		return SliderCaptchaResponse{}, ErrMobileInvalid
	}
	if purpose == SMSPurposeRegister {
		exists, err := s.repository.MobileExists(ctx, mobile)
		if err != nil {
			return SliderCaptchaResponse{}, err
		}
		if exists {
			recoverable, err := s.repository.MobileRegistrationRecoverable(ctx, s.issuer, mobile)
			if err != nil {
				return SliderCaptchaResponse{}, err
			}
			if !recoverable {
				return SliderCaptchaResponse{}, ErrMobileAlreadyRegistered
			}
		}
	}
	targetX, err := secureRandomInt(72, sliderCaptchaWidth-sliderCaptchaPieceSize-14)
	if err != nil {
		return SliderCaptchaResponse{}, ErrCaptchaInvalid
	}
	pieceY, err := secureRandomInt(14, sliderCaptchaHeight-sliderCaptchaPieceSize-14)
	if err != nil {
		return SliderCaptchaResponse{}, ErrCaptchaInvalid
	}
	background, piece, err := renderSliderCaptcha(targetX, pieceY)
	if err != nil {
		return SliderCaptchaResponse{}, ErrCaptchaInvalid
	}
	challenge := SliderCaptcha{
		ID:        uuid.Must(uuid.NewV7()),
		Mobile:    mobile,
		Purpose:   purpose,
		TargetX:   targetX,
		ExpiresAt: s.now().Add(2 * time.Minute),
	}
	if err := s.repository.CreateSliderCaptcha(ctx, challenge); err != nil {
		return SliderCaptchaResponse{}, err
	}
	return SliderCaptchaResponse{
		ID:              challenge.ID,
		BackgroundImage: "data:image/png;base64," + base64.StdEncoding.EncodeToString(background),
		PieceImage:      "data:image/png;base64," + base64.StdEncoding.EncodeToString(piece),
		Width:           sliderCaptchaWidth,
		Height:          sliderCaptchaHeight,
		PieceWidth:      sliderCaptchaPieceSize,
		PieceHeight:     sliderCaptchaPieceSize,
		PieceY:          pieceY,
		ExpiresAt:       challenge.ExpiresAt,
	}, nil
}

func (r *PostgresRepository) CreateSliderCaptcha(ctx context.Context, challenge SliderCaptcha) error {
	now := r.now()
	_, err := r.pool.Exec(ctx, `INSERT INTO slider_captcha_challenges(id,mobile_e164,purpose,target_x,status,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,'CREATED',$5,$6,$6)`, challenge.ID, challenge.Mobile, challenge.Purpose, challenge.TargetX, challenge.ExpiresAt, now)
	return err
}

func (r *PostgresRepository) ConsumeSliderCaptcha(ctx context.Context, id uuid.UUID, mobile string, purpose SMSPurpose, offset int) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var target, attempts int
	var status string
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT target_x,status,attempts,expires_at FROM slider_captcha_challenges WHERE id=$1 AND mobile_e164=$2 AND purpose=$3 FOR UPDATE`, id, mobile, purpose).Scan(&target, &status, &attempts, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCaptchaInvalid
	}
	if err != nil {
		return err
	}
	if status != "CREATED" || attempts >= 5 || !expires.After(r.now()) {
		if status == "CREATED" && !expires.After(r.now()) {
			if _, err := tx.Exec(ctx, `UPDATE slider_captcha_challenges SET status='EXPIRED',updated_at=$2 WHERE id=$1`, id, r.now()); err != nil {
				return err
			}
			if err := tx.Commit(ctx); err != nil {
				return err
			}
		}
		return ErrCaptchaInvalid
	}
	delta := target - offset
	if delta < 0 {
		delta = -delta
	}
	if delta > sliderCaptchaTolerance {
		attempts++
		if _, err := tx.Exec(ctx, `UPDATE slider_captcha_challenges SET attempts=$2,status=CASE WHEN $2>=5 THEN 'FAILED' ELSE status END,updated_at=$3 WHERE id=$1`, id, attempts, r.now()); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return ErrCaptchaInvalid
	}
	if _, err := tx.Exec(ctx, `UPDATE slider_captcha_challenges SET status='PASSED',passed_at=$2,updated_at=$2 WHERE id=$1`, id, r.now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func secureRandomInt(minimum, maximum int) (int, error) {
	if maximum <= minimum {
		return 0, errors.New("invalid random range")
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(maximum-minimum+1)))
	if err != nil {
		return 0, err
	}
	return minimum + int(value.Int64()), nil
}

func renderSliderCaptcha(targetX, pieceY int) ([]byte, []byte, error) {
	background := image.NewRGBA(image.Rect(0, 0, sliderCaptchaWidth, sliderCaptchaHeight))
	seed, err := secureRandomInt(0, 255)
	if err != nil {
		return nil, nil, err
	}
	for y := 0; y < sliderCaptchaHeight; y++ {
		for x := 0; x < sliderCaptchaWidth; x++ {
			background.SetRGBA(x, y, color.RGBA{
				R: uint8(42 + (x+seed)%92),
				G: uint8(105 + (y*2+seed)%105),
				B: uint8(128 + (x+y+seed)%92),
				A: 255,
			})
		}
	}
	for index := 0; index < 12; index++ {
		centerX, err := secureRandomInt(8, sliderCaptchaWidth-8)
		if err != nil {
			return nil, nil, err
		}
		centerY, err := secureRandomInt(8, sliderCaptchaHeight-8)
		if err != nil {
			return nil, nil, err
		}
		radius, _ := secureRandomInt(5, 18)
		tint, _ := secureRandomInt(25, 85)
		paintCircle(background, centerX, centerY, radius, uint8(tint))
	}
	piece := image.NewRGBA(image.Rect(0, 0, sliderCaptchaPieceSize, sliderCaptchaPieceSize))
	for y := 0; y < sliderCaptchaPieceSize; y++ {
		for x := 0; x < sliderCaptchaPieceSize; x++ {
			if !sliderPieceMask(x, y) {
				continue
			}
			original := background.RGBAAt(targetX+x, pieceY+y)
			piece.SetRGBA(x, y, original)
			background.SetRGBA(targetX+x, pieceY+y, color.RGBA{
				R: uint8(int(original.R) / 3),
				G: uint8(int(original.G) / 3),
				B: uint8(int(original.B) / 3),
				A: 255,
			})
		}
	}
	var backgroundBuffer, pieceBuffer bytes.Buffer
	if err := png.Encode(&backgroundBuffer, background); err != nil {
		return nil, nil, fmt.Errorf("encode captcha background: %w", err)
	}
	if err := png.Encode(&pieceBuffer, piece); err != nil {
		return nil, nil, fmt.Errorf("encode captcha piece: %w", err)
	}
	return backgroundBuffer.Bytes(), pieceBuffer.Bytes(), nil
}

func sliderPieceMask(x, y int) bool {
	const margin = 3
	inside := x >= margin && x < sliderCaptchaPieceSize-margin && y >= margin && y < sliderCaptchaPieceSize-margin
	center := sliderCaptchaPieceSize / 2
	topBump := (x-center)*(x-center)+(y-margin)*(y-margin) <= 7*7
	rightNotch := (x-(sliderCaptchaPieceSize-margin))*(x-(sliderCaptchaPieceSize-margin))+(y-center)*(y-center) <= 7*7
	return (inside || topBump) && !rightNotch
}

func paintCircle(target *image.RGBA, centerX, centerY, radius int, tint uint8) {
	for y := centerY - radius; y <= centerY+radius; y++ {
		for x := centerX - radius; x <= centerX+radius; x++ {
			if x < 0 || y < 0 || x >= sliderCaptchaWidth || y >= sliderCaptchaHeight || (x-centerX)*(x-centerX)+(y-centerY)*(y-centerY) > radius*radius {
				continue
			}
			pixel := target.RGBAAt(x, y)
			target.SetRGBA(x, y, color.RGBA{R: pixel.R ^ tint, G: pixel.G ^ (tint / 2), B: pixel.B ^ (tint / 3), A: 255})
		}
	}
}
