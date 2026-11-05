package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

const maxBytes = 10 << 20
const maxPixels = 40000000
const headerSize = 60
const responseSize = 56
const deadline = 45 * time.Second

var errInvalid = errors.New("invalid image")

// A request is magic PIV1 (images) or PIV2 (images and type 3 PDF), three zero bytes,
// a 16-byte caller nonce, SHA-256, then a big-endian uint32 body length.
// Response PIV1 contains status (1 valid, 2 invalid, 3 unavailable), three
// zero bytes, and the original nonce and computed digest. Never log frames.
func parseHeader(h []byte) (byte, int, error) {
	if len(h) != headerSize || (string(h[:4]) != "PIV1" && string(h[:4]) != "PIV2") || h[4] < 1 || h[4] > 3 || (string(h[:4]) == "PIV1" && h[4] == 3) || h[5] != 0 || h[6] != 0 || h[7] != 0 {
		return 0, 0, errInvalid
	}
	n := binary.BigEndian.Uint32(h[56:60])
	if n == 0 || n > maxBytes {
		return 0, 0, errInvalid
	}
	return h[4], int(n), nil
}
func dimensionsOK(w, h int) bool { return w > 0 && h > 0 && uint64(w) <= uint64(maxPixels)/uint64(h) }

// Inspect actual PNG chunk boundaries; matching bytes inside compressed image
// data or metadata are not chunk types. The standard decoder then validates
// ordering, CRCs and the complete default image.
func staticPNG(body []byte) bool {
	if len(body) < 8 || !bytes.Equal(body[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return false
	}
	for offset := 8; ; {
		if len(body)-offset < 12 {
			return false
		}
		size := uint64(binary.BigEndian.Uint32(body[offset : offset+4]))
		if size > uint64(len(body)-offset-12) {
			return false
		}
		kind := string(body[offset+4 : offset+8])
		if kind == "acTL" || kind == "fcTL" || kind == "fdAT" {
			return false
		}
		end := offset + int(size) + 12
		if crc32.ChecksumIEEE(body[offset+4:end-4]) != binary.BigEndian.Uint32(body[end-4:end]) {
			return false
		}
		offset += int(size) + 12
		if kind == "IEND" {
			return size == 0
		}
	}
}

func validateImage(kind byte, body []byte) error {
	var cfg image.Config
	var err error
	switch kind {
	case 1:
		cfg, err = png.DecodeConfig(bytes.NewReader(body))
	case 2:
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(body))
	default:
		return errInvalid
	}
	if err != nil || !dimensionsOK(cfg.Width, cfg.Height) {
		return errInvalid
	}
	if kind == 1 && !staticPNG(body) {
		return errInvalid
	}
	switch kind {
	case 1:
		_, err = png.Decode(bytes.NewReader(body))
	case 2:
		_, err = jpeg.Decode(bytes.NewReader(body))
	}
	if err != nil {
		return errInvalid
	}
	return nil
}
func child() int {
	h := make([]byte, headerSize)
	if _, err := io.ReadFull(os.Stdin, h); err != nil {
		return 1
	}
	kind, n, err := parseHeader(h)
	if err != nil {
		return 1
	}
	body := make([]byte, n)
	defer clear(body)
	if _, err = io.ReadFull(os.Stdin, body); err != nil {
		return 1
	}
	sum := sha256.Sum256(body)
	if subtle.ConstantTimeCompare(sum[:], h[24:56]) != 1 {
		return 1
	}
	status := byte(1)
	if validateImage(kind, body) != nil {
		status = 2
	}
	_, err = os.Stdout.Write([]byte{status})
	if err != nil {
		return 1
	}
	return 0
}

func handle(c net.Conn, executable string) { handleWithTimeout(c, executable, deadline) }

func handleWithTimeout(c net.Conn, executable string, timeout time.Duration) {
	handleWithPDFCandidate(c, executable, timeout, nil)
}

// Production passes a factory only when the separate PDF candidate is opted
// in; its executable and script paths are compile-time constants.
func handleWithPDFCandidate(c net.Conn, executable string, timeout time.Duration, pdfFactory func(context.Context) *exec.Cmd) {
	handleWithSlot(c, executable, timeout, pdfFactory, func() {})
}

func handleWithSlot(c net.Conn, executable string, timeout time.Duration, pdfFactory func(context.Context) *exec.Cmd, release func()) {
	defer c.Close()
	defer release()
	_ = c.SetDeadline(time.Now().Add(timeout))
	h := make([]byte, headerSize)
	if _, err := io.ReadFull(c, h); err != nil {
		return
	}
	kind, n, err := parseHeader(h)
	if err != nil {
		return
	}
	body := make([]byte, n)
	defer clear(body)
	if _, err = io.ReadFull(c, body); err != nil {
		return
	}
	sum := sha256.Sum256(body)
	if subtle.ConstantTimeCompare(sum[:], h[24:56]) != 1 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Any extra bytes, read error, EOF or disconnect cancel the active child.
	done := make(chan struct{})
	go func() { defer close(done); var b [1]byte; _, _ = c.Read(b[:]); cancel() }()
	input := io.MultiReader(bytes.NewReader(h), bytes.NewReader(body))
	var cmd *exec.Cmd
	if kind == 3 {
		if pdfFactory != nil {
			cmd = pdfFactory(ctx)
		}
	} else {
		cmd = exec.CommandContext(ctx, executable, "--decode-child")
	}
	if cmd == nil {
		response := make([]byte, responseSize)
		copy(response[:4], h[:4])
		response[4] = 3
		copy(response[8:24], h[8:24])
		copy(response[24:], sum[:])
		_, _ = c.Write(response)
		_ = c.SetReadDeadline(time.Now())
		<-done
		return
	}
	cmd.Env = []string{}
	cmd.Stdin = input
	var result bytes.Buffer
	cmd.Stdout = &boundedStatusWriter{buffer: &result}
	cmd.Stderr = io.Discard
	configureChild(cmd)
	err = cmd.Run()
	status := byte(3)
	if err == nil && result.Len() == 1 && (result.Bytes()[0] == 1 || result.Bytes()[0] == 2 || (kind == 3 && result.Bytes()[0] == 4)) {
		status = result.Bytes()[0]
	}
	if ctx.Err() == nil {
		response := make([]byte, responseSize)
		copy(response[:4], h[:4])
		response[4] = status
		copy(response[8:24], h[8:24])
		copy(response[24:], sum[:])
		_, _ = c.Write(response)
	}
	// Wake the monitor without exposing EOF until cleanup and slot release finish.
	_ = c.SetReadDeadline(time.Now())
	<-done
}

// A compromised or broken child cannot make the supervisor buffer its output.
type boundedStatusWriter struct{ buffer *bytes.Buffer }

func (w *boundedStatusWriter) Write(p []byte) (int, error) {
	if len(p) > 1-w.buffer.Len() {
		return 0, errInvalid
	}
	return w.buffer.Write(p)
}
func serve(listener net.Listener, executable string, allowedUID uint32, pdfFactory func(context.Context) *exec.Cmd) error {
	var slots = make(chan struct{}, 1)
	var active sync.WaitGroup
	defer func() { listener.Close(); active.Wait() }()
	for {
		c, err := listener.Accept()
		if err != nil {
			return err
		}
		if !allowedPeer(c, allowedUID) {
			c.Close()
			continue
		}
		select {
		case slots <- struct{}{}:
			active.Add(1)
			go func() {
				defer active.Done()
				handleWithSlot(c, executable, deadline, pdfFactory, func() { <-slots })
			}()
		default:
			c.Close()
		}
	}
}
func run() int {
	if len(os.Args) == 2 && os.Args[1] == "--decode-child" {
		return child()
	}
	// No listener exists unless explicitly enabled and all security settings are present.
	if os.Getenv("EVIDENCE_IMAGE_VALIDATOR_ENABLED") != "true" {
		return 0
	}
	path := os.Getenv("EVIDENCE_IMAGE_VALIDATOR_SOCKET")
	uid, err := strconv.ParseUint(os.Getenv("EVIDENCE_IMAGE_VALIDATOR_ALLOWED_UID"), 10, 32)
	if err != nil || uid == 0 || uint32(uid) == uint32(os.Geteuid()) || path == "" || !secureSocketPath(path) {
		return 1
	}
	executable, err := os.Executable()
	if err != nil {
		return 1
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return 1
	}
	// Never unlink a pathname we cannot atomically prove still names our socket.
	// Deployment must safely replace the isolated socket directory on restart.
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	defer listener.Close()
	if os.Chmod(path, 0660) != nil {
		return 1
	}
	var pdfFactory func(context.Context) *exec.Cmd
	if os.Getenv("EVIDENCE_PDF_CANDIDATE_ENABLED") == "true" {
		pdfFactory = fixedPDFCandidateCommand
	}
	_ = serve(listener, executable, uint32(uid), pdfFactory)
	return 1
}

// This path is deliberately fixed; the candidate Dockerfile does not contain
// this runtime. A verified, hash-locked Linux image is a prerequisite to use.
func fixedPDFCandidateCommand(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, "/opt/pinvou/pdf/bin/python3", "-I", "-B", "/opt/pinvou/pdf/pdf_validator.py")
}
func main() { os.Exit(run()) }
