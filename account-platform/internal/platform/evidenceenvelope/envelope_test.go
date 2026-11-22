package evidenceenvelope

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type testWrapper struct {
	wrappedDEKs, wrapAADs      [][]byte
	current                    string
	keys                       map[string][]byte
	wrappedInput, returnedDEK  []byte
	seenKey                    string
	encryptCalls, decryptCalls int
	encryptHook                func([]byte) ([]byte, error)
	decryptHook                func() ([]byte, error)
}

func newWrapper() *testWrapper {
	return &testWrapper{current: "current", keys: map[string][]byte{"current": bytes.Repeat([]byte{1}, 32), "previous": bytes.Repeat([]byte{2}, 32)}}
}
func (w *testWrapper) KeyID() string { return w.current }
func (w *testWrapper) EncryptWithKeyIDContext(ctx context.Context, id string, data, aad []byte) ([]byte, error) {
	w.encryptCalls++
	w.wrappedInput = data // Retain reference to verify the caller clears its DEK.
	// Copies survive production clear; the original reference above still checks
	// erasure. Direct file-AEAD assertions must not rely on wrapper decryption.
	w.wrappedDEKs = append(w.wrappedDEKs, bytes.Clone(data))
	w.wrapAADs = append(w.wrapAADs, bytes.Clone(aad))
	w.seenKey = id
	if w.encryptHook != nil {
		return w.encryptHook(data)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(w.keys[id])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, data, aad), nil
}
func (w *testWrapper) DecryptWithKeyIDContext(ctx context.Context, id string, data, aad []byte) ([]byte, error) {
	w.decryptCalls++
	w.seenKey = id
	if w.decryptHook != nil {
		var err error
		w.returnedDEK, err = w.decryptHook()
		return w.returnedDEK, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(w.keys[id])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < 12 {
		return nil, ErrInvalid
	}
	w.returnedDEK, err = aead.Open(nil, data[:12], data[12:], aad)
	return w.returnedDEK, err
}
func binding() Binding {
	return Binding{"billing.payment-evidence", uuid.MustParse("11111111-1111-4111-8111-111111111111"), uuid.MustParse("22222222-2222-4222-8222-222222222222"), 1, "application/pdf"}
}
func sealed(t *testing.T, w *testWrapper, b Binding, data []byte) Envelope {
	t.Helper()
	e, err := Seal(context.Background(), w, b, data)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func assertZero(t *testing.T, b []byte) {
	t.Helper()
	if !bytes.Equal(b, make([]byte, len(b))) {
		t.Fatal("DEK was not cleared")
	}
}
func assertInvalid(t *testing.T, got []byte, err error) {
	t.Helper()
	if got != nil || err != ErrInvalid {
		t.Fatalf("expected nil output and fixed error, got output size %d, error %v", len(got), err)
	}
}

func TestRoundTripBoundariesAndHistoricalKey(t *testing.T) {
	for _, size := range []int{1, MaxPlaintextBytes} {
		for _, media := range []string{"image/png", "image/jpeg", "application/pdf"} {
			t.Run(media+"/"+strconv.Itoa(size), func(t *testing.T) {
				w := newWrapper()
				w.current = "previous"
				b := binding()
				b.MediaType = media
				data := bytes.Repeat([]byte{7}, size)
				e := sealed(t, w, b, data)
				if len(w.wrappedInput) != 32 || e.KeyID != "previous" || e.PlaintextSHA256 != sha256.Sum256(data) || e.PlaintextSize != int64(size) {
					t.Fatal("wrong envelope facts or wrapper payload")
				}
				assertZero(t, w.wrappedInput)
				w.current = "current"
				got, err := Open(context.Background(), w, b, e)
				if err != nil || !bytes.Equal(got, data) || w.seenKey != "previous" {
					t.Fatalf("historical round trip failed: %v", err)
				}
				assertZero(t, w.returnedDEK)
			})
		}
	}
}

func TestFreshDEKAndNonce(t *testing.T) {
	w := newWrapper()
	b := binding()
	data := []byte("synthetic fixture")
	a := sealed(t, w, b, data)
	c := sealed(t, w, b, data)
	if len(w.wrappedDEKs) != 2 || len(w.wrappedDEKs[0]) != 32 || len(w.wrappedDEKs[1]) != 32 || bytes.Equal(w.wrappedDEKs[0], w.wrappedDEKs[1]) {
		t.Fatal("per-file DEKs must be distinct 32-byte values")
	}
	if bytes.Equal(a.Nonce, c.Nonce) || bytes.Equal(a.Ciphertext, c.Ciphertext) || bytes.Equal(a.WrappedDEK, c.WrappedDEK) {
		t.Fatal("reused file encryption material")
	}
}

func TestFileAEADAuthenticatesWrapAAD(t *testing.T) {
	w := newWrapper()
	b := binding()
	data := []byte("synthetic fixture")
	e := sealed(t, w, b, data)
	if len(w.wrappedDEKs) != 1 || len(w.wrappedDEKs[0]) != 32 || len(w.wrapAADs) != 1 || len(w.wrapAADs[0]) == 0 {
		t.Fatal("expected one captured 32-byte DEK and nonempty wrap AAD")
	}
	// Decrypt the file directly, without asking the authenticated wrapper. This
	// prevents wrapper AAD checks from masking missing file-layer AAD checks.
	block, err := aes.NewCipher(w.wrappedDEKs[0])
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	aad := w.wrapAADs[0]
	got, err := aead.Open(nil, e.Nonce, e.Ciphertext, aad)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("file must authenticate the same nonempty AAD as the wrapped DEK")
	}
	tamperedAAD := bytes.Clone(aad)
	tamperedAAD[0] ^= 1
	for name, wrongAAD := range map[string][]byte{"nil": nil, "tampered": tamperedAAD} {
		t.Run(name, func(t *testing.T) {
			if got, err := aead.Open(nil, e.Nonce, e.Ciphertext, wrongAAD); err == nil || got != nil {
				t.Fatal("file accepted incorrect AAD")
			}
		})
	}
	// Keep wrapper AAD valid but replace only the file with a ciphertext under
	// nil AAD. Open must reject this independent file-layer downgrade.
	e.Ciphertext = aead.Seal(nil, e.Nonce, data, nil)
	got, err = Open(context.Background(), w, b, e)
	assertInvalid(t, got, err)
	assertZero(t, w.returnedDEK)
}

func TestRejectMalformedEnvelopeBeforeUnwrap(t *testing.T) {
	mutations := map[string]func(*Envelope){
		"empty wrapped":    func(e *Envelope) { e.WrappedDEK = nil },
		"large wrapped":    func(e *Envelope) { e.WrappedDEK = make([]byte, MaxWrappedKeyBytes+1) },
		"nonce":            func(e *Envelope) { e.Nonce = e.Nonce[:11] },
		"ciphertext":       func(e *Envelope) { e.Ciphertext = e.Ciphertext[:len(e.Ciphertext)-1] },
		"zero size":        func(e *Envelope) { e.PlaintextSize = 0 },
		"negative size":    func(e *Envelope) { e.PlaintextSize = -1 },
		"large size":       func(e *Envelope) { e.PlaintextSize = MaxPlaintextBytes + 1 },
		"large ciphertext": func(e *Envelope) { e.Ciphertext = make([]byte, MaxPlaintextBytes+17) },
		"key empty":        func(e *Envelope) { e.KeyID = "" },
		"key path":         func(e *Envelope) { e.KeyID = "../key" },
		"key large":        func(e *Envelope) { e.KeyID = strings.Repeat("a", 129) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			w := newWrapper()
			e := sealed(t, w, binding(), []byte("fixture"))
			mutate(&e)
			got, err := Open(context.Background(), w, binding(), e)
			assertInvalid(t, got, err)
			if w.decryptCalls != 0 {
				t.Fatal("unwrapped malformed envelope")
			}
		})
	}
}

func TestAuthenticatedMetadataAndRecordSwaps(t *testing.T) {
	mutations := map[string]func(*Envelope){
		"purpose":    func(e *Envelope) { e.Binding.Purpose = "other" },
		"owner":      func(e *Envelope) { e.Binding.OwnerID = uuid.New() },
		"document":   func(e *Envelope) { e.Binding.DocumentID = uuid.New() },
		"version":    func(e *Envelope) { e.Binding.Version++ },
		"media":      func(e *Envelope) { e.Binding.MediaType = "image/png" },
		"digest":     func(e *Envelope) { e.PlaintextSHA256[0] ^= 1 },
		"size":       func(e *Envelope) { e.PlaintextSize--; e.Ciphertext = e.Ciphertext[:len(e.Ciphertext)-1] },
		"key":        func(e *Envelope) { e.KeyID = "previous" },
		"nonce":      func(e *Envelope) { e.Nonce[0] ^= 1 },
		"ciphertext": func(e *Envelope) { e.Ciphertext[0] ^= 1 },
		"wrapped":    func(e *Envelope) { e.WrappedDEK[0] ^= 1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			w := newWrapper()
			e := sealed(t, w, binding(), []byte("fixture"))
			mutate(&e)
			got, err := Open(context.Background(), w, binding(), e)
			assertInvalid(t, got, err)
			// Even changing the caller's expected binding cannot authenticate a
			// modified envelope: the original AAD is cryptographically committed.
			got, err = Open(context.Background(), w, e.Binding, e)
			assertInvalid(t, got, err)
			assertZero(t, w.returnedDEK)
		})
	}
	t.Run("whole record", func(t *testing.T) {
		w := newWrapper()
		other := binding()
		other.DocumentID = uuid.New()
		e := sealed(t, w, other, []byte("fixture"))
		got, err := Open(context.Background(), w, binding(), e)
		assertInvalid(t, got, err)
		if w.decryptCalls != 0 {
			t.Fatal("record mismatch reached key wrapper")
		}
	})
	t.Run("wrong master key", func(t *testing.T) {
		w := newWrapper()
		e := sealed(t, w, binding(), []byte("fixture"))
		w.keys["current"] = bytes.Repeat([]byte{3}, 32)
		got, err := Open(context.Background(), w, binding(), e)
		assertInvalid(t, got, err)
	})
	for _, field := range []string{"wrapped DEK", "file ciphertext"} {
		t.Run("swap/"+field, func(t *testing.T) {
			w := newWrapper()
			a := sealed(t, w, binding(), []byte("fixture"))
			b := sealed(t, w, binding(), []byte("fixture"))
			if field == "wrapped DEK" {
				a.WrappedDEK = b.WrappedDEK
			} else {
				a.Nonce, a.Ciphertext = b.Nonce, b.Ciphertext
			}
			got, err := Open(context.Background(), w, binding(), a)
			assertInvalid(t, got, err)
			assertZero(t, w.returnedDEK)
		})
	}
}

func TestInvalidBindingsAndPlaintext(t *testing.T) {
	mutations := map[string]func(*Binding){
		"purpose empty":   func(b *Binding) { b.Purpose = "" },
		"purpose large":   func(b *Binding) { b.Purpose = strings.Repeat("a", 129) },
		"purpose control": func(b *Binding) { b.Purpose = "x\ny" },
		"owner":           func(b *Binding) { b.OwnerID = uuid.Nil },
		"document":        func(b *Binding) { b.DocumentID = uuid.Nil },
		"version":         func(b *Binding) { b.Version = 0 },
		"media":           func(b *Binding) { b.MediaType = "application/octet-stream" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			b := binding()
			mutate(&b)
			w := newWrapper()
			e, err := Seal(context.Background(), w, b, []byte("fixture"))
			if err != ErrInvalid || e.Ciphertext != nil || w.encryptCalls != 0 {
				t.Fatal("invalid binding accepted")
			}
		})
	}
	for _, data := range [][]byte{nil, {}, make([]byte, MaxPlaintextBytes+1)} {
		w := newWrapper()
		e, err := Seal(context.Background(), w, binding(), data)
		if err != ErrInvalid || e.Ciphertext != nil || w.encryptCalls != 0 {
			t.Fatal("invalid length accepted")
		}
	}
}

func TestContextAndWrapperFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{nil, ctx} {
		w := newWrapper()
		e := sealed(t, w, binding(), []byte("fixture"))
		out, err := Seal(ctx, w, binding(), []byte("fixture"))
		if err != ErrInvalid || out.Ciphertext != nil {
			t.Fatal("invalid context accepted")
		}
		got, err := Open(ctx, w, binding(), e)
		assertInvalid(t, got, err)
	}
	if e, err := Seal(context.Background(), nil, binding(), []byte("fixture")); err != ErrInvalid || e.Ciphertext != nil {
		t.Fatal("nil wrapper accepted")
	}
	got, err := Open(context.Background(), nil, binding(), Envelope{})
	assertInvalid(t, got, err)
	for _, mode := range []string{"error", "empty", "oversized", "cancel"} {
		t.Run("wrap/"+mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := newWrapper()
			w.encryptHook = func([]byte) ([]byte, error) {
				switch mode {
				case "error":
					return nil, errors.New("synthetic private provider error")
				case "empty":
					return nil, nil
				case "oversized":
					return make([]byte, MaxWrappedKeyBytes+1), nil
				default:
					cancel()
					return []byte{1}, nil
				}
			}
			e, err := Seal(ctx, w, binding(), []byte("fixture"))
			if err != ErrInvalid || e.Ciphertext != nil || e.WrappedDEK != nil {
				t.Fatal("wrap failure exposed output")
			}
			assertZero(t, w.wrappedInput)
		})
	}
	for _, mode := range []string{"error with key", "short key", "long key", "cancel"} {
		t.Run("unwrap/"+mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := newWrapper()
			e := sealed(t, w, binding(), []byte("fixture"))
			w.decryptHook = func() ([]byte, error) {
				switch mode {
				case "error with key":
					return bytes.Repeat([]byte{9}, 32), errors.New("synthetic private provider error")
				case "short key":
					return bytes.Repeat([]byte{9}, 31), nil
				case "long key":
					return bytes.Repeat([]byte{9}, 33), nil
				default:
					cancel()
					return bytes.Repeat([]byte{9}, 32), nil
				}
			}
			got, err := Open(ctx, w, binding(), e)
			assertInvalid(t, got, err)
			assertZero(t, w.returnedDEK)
		})
	}
}

func TestRandomFailures(t *testing.T) {
	// Empty source fails DEK generation; 32 bytes permits DEK but fails nonce.
	for _, n := range []int{0, 32} {
		w := newWrapper()
		e, err := seal(context.Background(), w, binding(), []byte("fixture"), bytes.NewReader(make([]byte, n)))
		if err != ErrInvalid || e.Ciphertext != nil || w.encryptCalls != 0 {
			t.Fatal("random failure accepted")
		}
	}
}

func TestDigestIsCheckedAfterAuthenticatedOpen(t *testing.T) {
	w := newWrapper()
	b := binding()
	e := sealed(t, w, b, []byte("fixture"))
	aad, _ := associatedData(b, e.PlaintextSize, e.PlaintextSHA256, e.KeyID)
	dek, err := w.DecryptWithKeyIDContext(context.Background(), e.KeyID, e.WrappedDEK, aad)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(dek)
	// Construct authenticated but internally inconsistent metadata to exercise
	// the digest check independently of the AEAD authentication failure path.
	e.PlaintextSHA256[0] ^= 1
	aad, _ = associatedData(b, e.PlaintextSize, e.PlaintextSHA256, e.KeyID)
	e.WrappedDEK, err = w.EncryptWithKeyIDContext(context.Background(), e.KeyID, dek, aad)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(dek)
	aead, _ := cipher.NewGCM(block)
	e.Ciphertext = aead.Seal(nil, e.Nonce, []byte("fixture"), aad)
	got, err := Open(context.Background(), w, b, e)
	assertInvalid(t, got, err)
	assertZero(t, w.returnedDEK)
}
