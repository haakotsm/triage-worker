package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetEnv(t *testing.T) {
	t.Run("returns fallback when unset", func(t *testing.T) {
		got := getEnv("TRIAGE_TEST_NONEXISTENT_VAR_XYZ", "default")
		if got != "default" {
			t.Errorf("getEnv = %q, want 'default'", got)
		}
	})
	t.Run("returns env value when set", func(t *testing.T) {
		t.Setenv("TRIAGE_TEST_VAR", "custom")
		got := getEnv("TRIAGE_TEST_VAR", "default")
		if got != "custom" {
			t.Errorf("getEnv = %q, want 'custom'", got)
		}
	})
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseLogLevel(tt.input)
			if got != tt.want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestTemporalLogger(t *testing.T) {
	logger := slog.Default()
	tl := newTemporalLogger(logger)
	// Verify it doesn't panic
	tl.Debug("test debug", "key", "val")
	tl.Info("test info", "key", "val")
	tl.Warn("test warn", "key", "val")
	tl.Error("test error", "key", "val")
}

// TestMetricsMuxServesMetrics verifies the dedicated metrics router exposes
// Prometheus metrics on /metrics (the endpoint scraped on METRICS_ADDR:9090).
func TestMetricsMuxServesMetrics(t *testing.T) {
	srv := httptest.NewServer(newMetricsMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want a Prometheus text exposition type", ct)
	}
}

// TestPublicMuxDoesNotServeMetrics guards the actual security boundary: the
// public dashboard/webhook router (served on LISTEN_ADDR, exposed via ingress)
// must NOT serve Prometheus /metrics. We register a sentinel as the dashboard
// handler and assert that GET /metrics reaches the sentinel — proving the
// Prometheus handler is not registered on the public mux. If someone re-adds
// /metrics to newPublicMux, the request would be intercepted before the
// sentinel and this test fails.
func TestPublicMuxDoesNotServeMetrics(t *testing.T) {
	const sentinel = "dashboard-sentinel"
	dashboard := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sentinel))
	})

	srv := httptest.NewServer(newPublicMux(dashboard))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), sentinel) {
		t.Errorf("GET /metrics on public mux was not routed to the dashboard handler; "+
			"body = %q. /metrics must not be served on the public (ingress-exposed) port", string(body))
	}
}

// TestMetricsMuxDoesNotServeDashboard verifies the metrics router only exposes
// /metrics; any other path (i.e. dashboard routes) is not served here. This
// keeps the dedicated metrics listener minimal.
func TestMetricsMuxDoesNotServeDashboard(t *testing.T) {
	srv := httptest.NewServer(newMetricsMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET / on metrics mux status = %d, want 404", resp.StatusCode)
	}
}
