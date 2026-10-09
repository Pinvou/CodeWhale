package trustedclock

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrClockUnsafe = errors.New("trusted clock offset exceeds the safe threshold")

const ntpEpochOffset = 2208988800

type sampleFunc func(context.Context, string) (time.Duration, error)

type Monitor struct {
	mu        sync.Mutex
	sources   []string
	sample    sampleFunc
	now       func() time.Time
	offset    time.Duration
	checkedAt time.Time
}

func New(sources []string) (*Monitor, error) {
	unique := map[string]bool{}
	clean := make([]string, 0, len(sources))
	for _, source := range sources {
		source = strings.TrimSpace(source)
		if source == "" || unique[source] {
			continue
		}
		unique[source] = true
		clean = append(clean, source)
	}
	if len(clean) < 3 {
		return nil, errors.New("at least three independent trusted time sources are required")
	}
	return &Monitor{sources: clean, sample: sampleNTP, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (m *Monitor) Check(ctx context.Context) error {
	m.mu.Lock()
	age := m.now().Sub(m.checkedAt)
	if !m.checkedAt.IsZero() && age >= 0 && age < 30*time.Second {
		offset := m.offset
		m.mu.Unlock()
		return classify(offset)
	}
	m.mu.Unlock()
	type result struct {
		offset time.Duration
		err    error
	}
	results := make(chan result, len(m.sources))
	for _, source := range m.sources {
		go func() {
			offset, err := m.sample(ctx, source)
			results <- result{offset: offset, err: err}
		}()
	}
	offsets := make([]time.Duration, 0, len(m.sources))
	for range m.sources {
		value := <-results
		if value.err == nil {
			offsets = append(offsets, value.offset)
		}
	}
	if len(offsets) < 3 {
		return errors.New("fewer than three trusted time sources responded")
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	offset := offsets[len(offsets)/2]
	m.mu.Lock()
	m.offset, m.checkedAt = offset, m.now()
	m.mu.Unlock()
	if absolute(offset) > 250*time.Millisecond {
		slog.Warn("trusted clock offset alert", "offset_ms", offset.Milliseconds(), "source_count", len(offsets))
	}
	return classify(offset)
}

func classify(offset time.Duration) error {
	if absolute(offset) > time.Second {
		return fmt.Errorf("%w: offset=%s", ErrClockUnsafe, offset)
	}
	return nil
}

func absolute(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func sampleNTP(ctx context.Context, source string) (time.Duration, error) {
	if _, _, err := net.SplitHostPort(source); err != nil {
		source = net.JoinHostPort(source, "123")
	}
	dialer := net.Dialer{Timeout: 750 * time.Millisecond}
	connection, err := dialer.DialContext(ctx, "udp", source)
	if err != nil {
		return 0, err
	}
	defer connection.Close()
	deadline := time.Now().Add(750 * time.Millisecond)
	_ = connection.SetDeadline(deadline)
	request := make([]byte, 48)
	request[0] = 0x23
	started := time.Now().UTC()
	binary.BigEndian.PutUint32(request[40:44], uint32(started.Unix()+ntpEpochOffset))
	binary.BigEndian.PutUint32(request[44:48], uint32((uint64(started.Nanosecond())<<32)/uint64(time.Second)))
	if _, err := connection.Write(request); err != nil {
		return 0, err
	}
	response := make([]byte, 512)
	n, err := connection.Read(response)
	if err != nil {
		return 0, err
	}
	if n < 48 || response[0]>>6 == 3 || response[0]&7 != 4 ||
		((response[0]>>3)&7 != 3 && (response[0]>>3)&7 != 4) ||
		response[1] == 0 || response[1] > 15 ||
		!bytes.Equal(response[24:32], request[40:48]) ||
		binary.BigEndian.Uint64(response[32:40]) == 0 || binary.BigEndian.Uint64(response[40:48]) == 0 {
		return 0, errors.New("trusted time source returned an invalid or unsynchronized NTP response")
	}
	finished := time.Now().UTC()
	seconds := binary.BigEndian.Uint32(response[40:44])
	fraction := binary.BigEndian.Uint32(response[44:48])
	server := time.Unix(int64(seconds)-ntpEpochOffset, int64(fraction)*int64(time.Second)>>32).UTC()
	localMidpoint := started.Add(finished.Sub(started) / 2)
	return server.Sub(localMidpoint), nil
}
