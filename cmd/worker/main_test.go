package main

import (
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

// TestMetricsMuxDoesNotServeDashboard verifies the metrics router only exposes
// /metrics; any other path (i.e. dashboard routes) is not served here. This
// guards the separation that keeps /metrics off the public dashboard ingress.
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
