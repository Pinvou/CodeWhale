package trustedclock

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"
)

func TestSampleNTPValidatesLocalServerResponses(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte) []byte
		valid  bool
	}{
		{"version4", func(packet []byte) []byte { return packet }, true},
		{"version3", func(packet []byte) []byte { packet[0] = 3<<3 | 4; return packet }, true},
		{"unsynchronized", func(packet []byte) []byte { packet[0] |= 3 << 6; return packet }, false},
		{"client-mode", func(packet []byte) []byte { packet[0] = 4<<3 | 3; return packet }, false},
		{"broadcast-mode", func(packet []byte) []byte { packet[0] = 4<<3 | 5; return packet }, false},
		{"old-version", func(packet []byte) []byte { packet[0] = 2<<3 | 4; return packet }, false},
		{"kiss-of-death", func(packet []byte) []byte { packet[1] = 0; return packet }, false},
		{"invalid-stratum", func(packet []byte) []byte { packet[1] = 16; return packet }, false},
		{"short-packet", func(packet []byte) []byte { return packet[:47] }, false},
		{"wrong-request", func(packet []byte) []byte { packet[24] ^= 1; return packet }, false},
		{"zero-receive-time", func(packet []byte) []byte { clear(packet[32:40]); return packet }, false},
		{"zero-transmit-time", func(packet []byte) []byte { clear(packet[40:48]); return packet }, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			if err := server.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				request := make([]byte, 48)
				n, peer, err := server.ReadFrom(request)
				if err != nil {
					done <- err
					return
				}
				if n != 48 || binary.BigEndian.Uint64(request[40:48]) == 0 {
					done <- errors.New("client did not provide a request timestamp")
					return
				}
				response := make([]byte, 48)
				response[0], response[1] = 4<<3|4, 2
				copy(response[24:32], request[40:48])
				now := time.Now().UTC()
				binary.BigEndian.PutUint32(response[32:36], uint32(now.Unix()+ntpEpochOffset))
				binary.BigEndian.PutUint32(response[36:40], uint32((uint64(now.Nanosecond())<<32)/uint64(time.Second)))
				copy(response[40:48], response[32:40])
				_, err = server.WriteTo(test.mutate(response), peer)
				done <- err
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			offset, err := sampleNTP(ctx, server.LocalAddr().String())
			if serverErr := <-done; serverErr != nil {
				t.Fatal(serverErr)
			}
			if test.valid {
				if err != nil || absolute(offset) > time.Second {
					t.Fatalf("valid local response offset=%v err=%v", offset, err)
				}
			} else if err == nil {
				t.Fatal("invalid NTP response was accepted as a trusted sample")
			}
		})
	}
}

func TestMonitorUsesMedianAndEnforcesAlertAndReadinessThresholds(t *testing.T) {
	if _, err := New([]string{"one", "two"}); err == nil {
		t.Fatal("fewer than three sources were accepted")
	}
	monitor, err := New([]string{"one", "two", "three"})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]time.Duration{"one": 90 * time.Millisecond, "two": 260 * time.Millisecond, "three": 2 * time.Second}
	monitor.sample = func(_ context.Context, source string) (time.Duration, error) { return values[source], nil }
	if err := monitor.Check(context.Background()); err != nil {
		t.Fatalf("250ms alert must not remove readiness: %v", err)
	}
	monitor.checkedAt = time.Time{}
	values = map[string]time.Duration{"one": 1100 * time.Millisecond, "two": 1200 * time.Millisecond, "three": 1300 * time.Millisecond}
	if err := monitor.Check(context.Background()); !errors.Is(err, ErrClockUnsafe) {
		t.Fatalf("offset beyond one second must fail readiness: %v", err)
	}
}

func TestMonitorFailsClosedWithoutThreeResponses(t *testing.T) {
	monitor, _ := New([]string{"one", "two", "three"})
	monitor.sample = func(_ context.Context, source string) (time.Duration, error) {
		if source == "three" {
			return 0, errors.New("offline")
		}
		return 0, nil
	}
	if err := monitor.Check(context.Background()); err == nil {
		t.Fatal("monitor accepted fewer than three responses")
	}
}

func TestMonitorDoesNotReuseSafeSampleAfterClockMovesBackwards(t *testing.T) {
	monitor, err := New([]string{"one", "two", "three"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	monitor.now = func() time.Time { return now }
	monitor.sample = func(context.Context, string) (time.Duration, error) { return 0, nil }
	if err := monitor.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	monitor.sample = func(context.Context, string) (time.Duration, error) { return 2 * time.Second, nil }
	now = now.Add(10 * time.Second)
	if err := monitor.Check(context.Background()); err != nil {
		t.Fatalf("unexpired sample should remain cached: %v", err)
	}
	now = now.Add(-11 * time.Second)
	if err := monitor.Check(context.Background()); !errors.Is(err, ErrClockUnsafe) {
		t.Fatalf("clock rollback must resample and detect unsafe offset: %v", err)
	}
}
