package httpserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRequiredSupervisorCancellationStopsHTTP(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	_ = reservation.Close()
	t.Setenv("PORT", strconv.Itoa(port))
	t.Setenv("APP_ENV", "test")
	metricsReservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	metricsAddress := metricsReservation.Addr().String()
	_ = metricsReservation.Close()
	t.Setenv("METRICS_ADDRESS", metricsAddress)
	t.Setenv("SHUTDOWN_TIMEOUT", "1s")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	collector := prometheus.NewGauge(prometheus.GaugeOpts{Name: "required_supervisor_fixture", Help: "Required supervisor collector propagation."})
	collector.Set(7)
	go func() {
		done <- RunWithApplicationContext(ctx, "token-command-executor", http.NotFoundHandler(), nil, collector)
	}()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	endpoint := "http://127.0.0.1:" + strconv.Itoa(port) + "/health/live"
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get(endpoint)
		if err == nil {
			_ = response.Body.Close()
			break
		}
		select {
		case err := <-done:
			t.Fatalf("service failed before supervisor cancellation: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("HTTP listener did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	metricsEndpoint := "http://" + metricsAddress + "/metrics"
	for {
		response, err := client.Get(metricsEndpoint)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "required_supervisor_fixture 7") {
				t.Fatalf("context-owned metrics lost supplied collectors: %v %s", readErr, body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("metrics listener did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// This is the context cancelled on required consumer registration failure.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled supervisor left HTTP alive")
	}
	if response, err := client.Get(endpoint); err == nil {
		_ = response.Body.Close()
		t.Fatal("application listener survived supervisor cancellation")
	}
	if response, err := client.Get(metricsEndpoint); err == nil {
		_ = response.Body.Close()
		t.Fatal("metrics listener survived supervisor cancellation")
	}
}
