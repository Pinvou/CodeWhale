package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func encoded(t *testing.T, kind byte, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	var err error
	if kind == 1 {
		err = png.Encode(&b, img)
	} else {
		err = jpeg.Encode(&b, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func frame(kind byte, data []byte) []byte {
	h := make([]byte, headerSize)
	copy(h, "PIV1")
	h[4] = kind
	copy(h[8:24], "fixed-test-nonce")
	sum := sha256.Sum256(data)
	copy(h[24:56], sum[:])
	binary.BigEndian.PutUint32(h[56:], uint32(len(data)))
	return append(h, data...)
}
func TestDimensionsAndHeaders(t *testing.T) {
	for _, tc := range []struct {
		w, h int
		want bool
	}{{8000, 5000, true}, {40000000, 1, true}, {40000001, 1, false}, {0, 1, false}, {-1, 1, false}, {1, 0, false}, {int(^uint(0) >> 1), 2, false}} {
		if dimensionsOK(tc.w, tc.h) != tc.want {
			t.Fatal("pixel bounds")
		}
	}
	h := frame(1, []byte("a"))[:headerSize]
	if _, n, err := parseHeader(h); err != nil || n != 1 {
		t.Fatal("valid header")
	}
	for _, index := range []int{0, 4, 5, 6, 7} {
		v := append([]byte(nil), h...)
		v[index] = 255
		if _, _, err := parseHeader(v); err == nil {
			t.Fatal("invalid header accepted")
		}
	}
	for _, n := range []uint32{0, maxBytes + 1, ^uint32(0)} {
		v := append([]byte(nil), h...)
		binary.BigEndian.PutUint32(v[56:], n)
		if _, _, err := parseHeader(v); err == nil {
			t.Fatal("invalid length accepted")
		}
	}
}

func TestPNGInvalidCRCAndOversizedDimensions(t *testing.T) {
	body := encoded(t, 1, image.NewGray(image.Rect(0, 0, 2, 2)))
	badCRC := append([]byte(nil), body...)
	badCRC[29] ^= 1
	if validateImage(1, badCRC) == nil {
		t.Fatal("PNG with invalid CRC accepted")
	}
	tooMany := append([]byte(nil), body...)
	binary.BigEndian.PutUint32(tooMany[16:20], 40000001)
	binary.BigEndian.PutUint32(tooMany[20:24], 1)
	binary.BigEndian.PutUint32(tooMany[29:33], crc32.ChecksumIEEE(tooMany[12:29]))
	if validateImage(1, tooMany) == nil {
		t.Fatal("oversized PNG accepted")
	}
}

func pngChunk(kind string, data []byte) []byte {
	out := make([]byte, 12+len(data))
	binary.BigEndian.PutUint32(out[:4], uint32(len(data)))
	copy(out[4:8], kind)
	copy(out[8:], data)
	binary.BigEndian.PutUint32(out[len(out)-4:], crc32.ChecksumIEEE(out[4:len(out)-4]))
	return out
}

func TestRejectAPNGChunksWithoutFalsePositive(t *testing.T) {
	realAPNG, err := os.ReadFile("testdata/animated.png")
	if err != nil || validateImage(1, realAPNG) == nil {
		t.Fatal("valid APNG animation accepted")
	}
	static := encoded(t, 1, image.NewGray(image.Rect(0, 0, 2, 2)))
	// IHDR is a fixed 25-byte chunk immediately following the signature.
	insert := func(chunk []byte) []byte {
		out := append([]byte(nil), static[:33]...)
		out = append(out, chunk...)
		return append(out, static[33:]...)
	}
	textChunk := insert(pngChunk("tEXt", []byte("comment\x00acTL fcTL fdAT")))
	before := append([]byte(nil), textChunk...)
	if validateImage(1, textChunk) != nil || !bytes.Equal(textChunk, before) {
		t.Fatal("static PNG with marker text rejected or changed")
	}
	for _, kind := range []string{"acTL", "fcTL", "fdAT"} {
		animated := insert(pngChunk(kind, make([]byte, 8)))
		if validateImage(1, animated) == nil {
			t.Fatalf("%s APNG chunk accepted", kind)
		}
		if validateImage(1, animated[:38]) == nil {
			t.Fatalf("%s truncated chunk accepted", kind)
		}
	}
}

func TestPNGStrictChunksAndCompressedStream(t *testing.T) {
	static := encoded(t, 1, image.NewGray(image.Rect(0, 0, 8, 8)))
	var pos, end int
	for i := 8; i+12 <= len(static); {
		n := int(binary.BigEndian.Uint32(static[i : i+4]))
		j := i + n + 12
		if string(static[i+4:i+8]) == "IDAT" {
			pos, end = i, j
			break
		}
		i = j
	}
	if pos == 0 {
		t.Fatal("no fixture IDAT")
	}
	data := static[pos+8 : end-4]
	build := func(chunks ...[]byte) []byte {
		b := append([]byte(nil), static[:pos]...)
		for _, c := range chunks {
			b = append(b, c...)
		}
		return append(b, static[end:]...)
	}
	mid := len(data) / 2
	validSplit := build(pngChunk("IDAT", data[:mid]), pngChunk("IDAT", data[mid:]))
	if validateImage(1, validSplit) != nil {
		t.Fatal("valid multi-IDAT PNG rejected")
	}
	trailing := append(append([]byte(nil), static...), []byte("compatible trailing content acTL")...)
	before := append([]byte(nil), trailing...)
	if _, err := png.Decode(bytes.NewReader(trailing)); err != nil || validateImage(1, trailing) != nil || !bytes.Equal(trailing, before) {
		t.Fatal("decodable static PNG with trailing content rejected or changed")
	}
	for _, compatible := range [][]byte{
		build(pngChunk("ABCD", []byte("unknown critical")), pngChunk("IDAT", data)),
		build(pngChunk("IDAT", append(append([]byte(nil), data...), 0x42))),
		build(pngChunk("IDAT", data), pngChunk("IDAT", []byte("extra"))),
		build(pngChunk("IDAT", data), pngChunk("tEXt", []byte("a\x00b")), pngChunk("IDAT", []byte("extra"))),
	} {
		before := append([]byte(nil), compatible...)
		if _, err := png.Decode(bytes.NewReader(compatible)); err != nil || validateImage(1, compatible) != nil || !bytes.Equal(compatible, before) {
			t.Fatal("decodable static PNG rejected or changed")
		}
	}
	for _, bad := range [][]byte{
		build(pngChunk("IDAT", data[:mid]), pngChunk("tEXt", []byte("a\x00b")), pngChunk("IDAT", data[mid:])),
	} {
		if validateImage(1, bad) == nil {
			t.Fatal("malformed PNG accepted")
		}
	}
}
func TestCompleteImageValidationAndOriginalBytes(t *testing.T) {
	img := image.NewNRGBA64(image.Rect(0, 0, 3, 2))
	img.SetNRGBA64(1, 1, color.NRGBA64{R: 65535, G: 2000, B: 33000, A: 50000})
	samples := map[byte][][]byte{1: {encoded(t, 1, img)}, 2: {encoded(t, 2, img)}}
	for _, file := range []string{"testdata/progressive.jpg", "testdata/cmyk.jpg"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		samples[2] = append(samples[2], b)
	}
	for kind, all := range samples {
		for _, body := range all {
			before := append([]byte(nil), body...)
			if validateImage(kind, body) != nil || !bytes.Equal(body, before) {
				t.Fatal("valid image rejected or changed")
			}
			if validateImage(kind, body[:len(body)/2]) == nil {
				t.Fatal("truncated image accepted")
			}
			other := byte(1)
			if kind == 1 {
				other = 2
			}
			if validateImage(other, body) == nil {
				t.Fatal("wrong media accepted")
			}
		}
	}
}
func TestFortyMillionPixelImage(t *testing.T) {
	if testing.Short() {
		t.Skip("large valid pixel-boundary decode")
	}
	original := image.NewGray(image.Rect(0, 0, 8000, 5000))
	body := encoded(t, 1, original)
	if len(body) > maxBytes {
		t.Fatal("fixture too large")
	}
	before := sha256.Sum256(body)
	if validateImage(1, body) != nil || sha256.Sum256(body) != before {
		t.Fatal("valid 40 million pixel image rejected or changed")
	}
}
func buildBinary(t *testing.T, source string) string {
	t.Helper()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	out := filepath.Join(t.TempDir(), "validator"+ext)
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go"+ext)
	args := []string{"build", "-o", out}
	if source == "" {
		args = append(args, ".")
	} else {
		file := filepath.Join(t.TempDir(), "main.go")
		if err := os.WriteFile(file, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		args = append(args, file)
	}
	cmd := exec.Command(goBin, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatal("test binary build failed")
	}
	return out
}
func TestWireAndDisconnectCancel(t *testing.T) {
	binary := buildBinary(t, "")
	body := encoded(t, 1, image.NewGray(image.Rect(0, 0, 2, 2)))
	a, b := net.Pipe()
	finished := make(chan struct{})
	go func() { handle(a, binary); close(finished) }()
	if _, err := b.Write(frame(1, body)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, responseSize)
	if _, err := io.ReadFull(b, response); err != nil {
		t.Fatal("no fixed response")
	}
	sum := sha256.Sum256(body)
	if string(response[:4]) != "PIV1" || response[4] != 1 || !bytes.Equal(response[8:24], []byte("fixed-test-nonce")) || !bytes.Equal(response[24:], sum[:]) {
		t.Fatal("incorrect bound response")
	}
	b.Close()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("handler not released")
	}
	// The fixture writes only a readiness marker, never image bytes. Wait for
	// the child to start before disconnecting so this proves termination.
	marker := filepath.Join(t.TempDir(), "child-started")
	sleeper := buildBinary(t, fmt.Sprintf(`package main
import ("os"; "time")
func main(){_ = os.WriteFile(%q, []byte("ready"), 0600);time.Sleep(60*time.Second)}`, marker))
	a, b = net.Pipe()
	finished = make(chan struct{})
	go func() { handle(a, sleeper); close(finished) }()
	if _, err := b.Write(frame(1, body)); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("child never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.Close()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not stop and reap child")
	}
}

func TestWireRejectsMismatchedDigest(t *testing.T) {
	data := encoded(t, 1, image.NewGray(image.Rect(0, 0, 2, 2)))
	wire := frame(1, data)
	wire[24] ^= 1
	a, b := net.Pipe()
	finished := make(chan struct{})
	go func() { handle(a, "unavailable-executable"); close(finished) }()
	if _, err := b.Write(wire); err != nil {
		t.Fatal(err)
	}
	var response [responseSize]byte
	if _, err := io.ReadFull(b, response[:]); err == nil {
		t.Fatal("bad digest received a response")
	}
	_ = b.Close()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("bad digest held the handler")
	}
}
func TestDisabledByDefault(t *testing.T) {
	t.Setenv("EVIDENCE_IMAGE_VALIDATOR_ENABLED", "")
	if run() != 0 {
		t.Fatal("disabled default failed")
	}
}

func TestTimeoutKillsAndReapsChild(t *testing.T) {
	var childCmd *exec.Cmd
	marker := filepath.Join(t.TempDir(), "child-started")
	sleeper := buildBinary(t, fmt.Sprintf(`package main
import ("os"; "time")
func main(){_ = os.WriteFile(%q, []byte("ready"), 0600);time.Sleep(60*time.Second)}`, marker))
	data := encoded(t, 1, image.NewGray(image.Rect(0, 0, 2, 2)))
	a, b := net.Pipe()
	finished := make(chan struct{})
	// Allow the process ample startup time on a loaded host, then require the
	// readiness marker before accepting timeout termination as a passing test.
	started := time.Now()
	go func() {
		handleWithPDFCandidate(a, "unused", 10*time.Second, func(ctx context.Context) *exec.Cmd {
			childCmd = exec.CommandContext(ctx, sleeper)
			return childCmd
		})
		close(finished)
	}()
	wire := frame(3, data)
	copy(wire[:4], "PIV2")
	if _, err := b.Write(wire); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case <-finished:
			t.Fatal("handler finished before child readiness")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("timeout did not terminate child")
	}
	if time.Since(started) < 10*time.Second {
		t.Fatal("child exited before the timeout")
	}
	if childCmd == nil || childCmd.Process == nil || childCmd.ProcessState == nil || childCmd.ProcessState.Pid() != childCmd.Process.Pid || childCmd.ProcessState.Success() {
		t.Fatal("child was not waited for after termination")
	}
	var response [1]byte
	if _, err := b.Read(response[:]); err != io.EOF {
		t.Fatal("timed-out request produced a response")
	}
	_ = b.Close()
}

type closeObservedConn struct {
	net.Conn
	closed   chan bool
	released <-chan struct{}
}

func (c *closeObservedConn) Close() error {
	select {
	case <-c.released:
		c.closed <- true
	default:
		c.closed <- false
	}
	return c.Conn.Close()
}

func TestSlotReleasedBeforeResponseEOF(t *testing.T) {
	data := encoded(t, 1, image.NewGray(image.Rect(0, 0, 2, 2)))
	validator := buildBinary(t, "")
	for _, tc := range []struct {
		name       string
		pdf        bool
		executable string
		status     byte
	}{{"successful child", false, validator, 1}, {"failed child", false, "unavailable-executable", 3}, {"disabled PDF", true, "unused", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			wire := frame(1, data)
			if tc.pdf {
				wire = frame(3, data)
				copy(wire[:4], "PIV2") // disabled PDF returns unavailable without a child
			}
			a, b := net.Pipe()
			released := make(chan struct{})
			closed := make(chan bool, 1)
			go handleWithSlot(&closeObservedConn{Conn: a, closed: closed, released: released}, tc.executable, 10*time.Second, nil, func() { close(released) })
			if _, err := b.Write(wire); err != nil {
				t.Fatal(err)
			}
			var response [responseSize]byte
			if _, err := io.ReadFull(b, response[:]); err != nil || response[4] != tc.status {
				t.Fatal("missing or incorrect response")
			}
			var trailing [1]byte
			if _, err := b.Read(trailing[:]); err != io.EOF {
				t.Fatal("response was not followed by EOF")
			}
			if !<-closed {
				t.Fatal("connection closed before slot release")
			}
			_ = b.Close()
		})
	}
}

func TestPIV2DisabledAndVersionMismatch(t *testing.T) {
	wire := frame(1, []byte("fixture"))
	wire[4] = 3
	if _, _, err := parseHeader(wire[:headerSize]); err == nil {
		t.Fatal("PIV1 accepted PDF")
	}
	copy(wire[:4], "PIV2")
	if kind, _, err := parseHeader(wire[:headerSize]); err != nil || kind != 3 {
		t.Fatal("PIV2 rejected PDF framing")
	}
	a, b := net.Pipe()
	done := make(chan struct{})
	go func() { handle(a, "unavailable-executable"); close(done) }()
	if _, err := b.Write(wire); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, responseSize)
	if _, err := io.ReadFull(b, reply); err != nil {
		t.Fatal("disabled PDF response missing")
	}
	if string(reply[:4]) != "PIV2" || reply[4] != 3 {
		t.Fatal("disabled PDF did not fail unavailable")
	}
	b.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("disabled handler stuck")
	}
}

// This executes the real PDF candidate through the supervisor. Test-only
// paths select a pre-existing isolated Windows prototype runtime; they are not
// production configuration or an approved Linux dependency lock.
func TestPDFCandidateRealSamples(t *testing.T) {
	python, dir := os.Getenv("EVIDENCE_PDF_TEST_PYTHON"), os.Getenv("EVIDENCE_PDF_TEST_FIXTURES")
	if python == "" || dir == "" {
		t.Skip("isolated pikepdf fixture/runtime not configured")
	}
	script, err := filepath.Abs("pdf_validator.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		status byte
	}{{"plain-compressed.pdf", 1}, {"empty-user-owner.pdf", 1}, {"user-password.pdf", 4}, {"truncated.pdf", 2}, {"incremental.pdf", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, tc.name))
			if err != nil {
				t.Fatal("fixture unavailable")
			}
			before := sha256.Sum256(data)
			wire := frame(3, data)
			copy(wire[:4], "PIV2")
			a, b := net.Pipe()
			done := make(chan struct{})
			go func() {
				handleWithPDFCandidate(a, "unused", 10*time.Second, func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, python, "-I", "-B", script) })
				close(done)
			}()
			if _, err = b.Write(wire); err != nil {
				t.Fatal("request failed")
			}
			reply := make([]byte, responseSize)
			if _, err = io.ReadFull(b, reply); err != nil {
				t.Fatal("response missing")
			}
			if string(reply[:4]) != "PIV2" || reply[4] != tc.status || !bytes.Equal(reply[8:24], wire[8:24]) || !bytes.Equal(reply[24:], before[:]) || sha256.Sum256(data) != before {
				t.Fatal("wrong status or input binding")
			}
			b.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("child not reaped")
			}
		})
	}
}
