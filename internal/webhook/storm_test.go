package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/haakotsm/triage-worker/internal/types"
)

// TestAlertStorm simulates 50 concurrent webhook requests (the Phase 4 storm test).
// Validates that the handler handles high concurrency without panics or data races.
// This test does NOT need a real Temporal client — it tests the HTTP layer's resilience.
func TestAlertStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping storm test in short mode")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	h := &Handler{
		logger:  logger,
		healthy: &atomic.Bool{},
	}
	// Set unhealthy so we test the rejection path (no real Temporal)
	h.healthy.Store(false)

	srv := httptest.NewServer(h)
	defer srv.Close()

	// Generate 50 unique alerts
	var wg sync.WaitGroup
	var rejected atomic.Int64
	var errors atomic.Int64

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			alertGroup := types.AlertGroup{
				Version:  "4",
				GroupKey: fmt.Sprintf("storm-%d", idx),
				Status:   "firing",
				Alerts: []types.Alert{
					{
						Status:      "firing",
						Fingerprint: fmt.Sprintf("storm-fp-%d", idx),
						Labels: map[string]string{
							"alertname":  "StormAlert",
							"namespace":  "default",
							"deployment": fmt.Sprintf("app-%d", idx%5),
							"severity":   "warning",
						},
						Annotations: map[string]string{
							"description": fmt.Sprintf("Storm alert %d", idx),
						},
						StartsAt: time.Now(),
					},
				},
			}

			body, _ := json.Marshal(alertGroup)
			resp, err := http.Post(srv.URL+"/webhook", "application/json", bytes.NewReader(body))
			if err != nil {
				errors.Add(1)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusServiceUnavailable {
				rejected.Add(1)
			}
		}(i)
	}

	wg.Wait()

	// All should be rejected because healthy=false (Temporal unreachable)
	if rejected.Load() != 50 {
		t.Errorf("rejected = %d, want 50 (handler is unhealthy)", rejected.Load())
	}
	if errors.Load() > 0 {
		t.Errorf("network errors = %d, want 0", errors.Load())
	}
}

// TestAlertStorm_HealthyRejection validates that when healthy, the handler
// attempts to process but handles the nil client gracefully (panics would be caught here).
func TestAlertStorm_ValidPayloads(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping storm test in short mode")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	h := &Handler{
		logger:  logger,
		healthy: &atomic.Bool{},
	}
	h.healthy.Store(true)

	srv := httptest.NewServer(h)
	defer srv.Close()

	// Send resolved-only payloads (should be handled without needing Temporal)
	var wg sync.WaitGroup
	var ok atomic.Int64

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			alertGroup := types.AlertGroup{
				Version:  "4",
				GroupKey: fmt.Sprintf("resolved-storm-%d", idx),
				Status:   "resolved",
				Alerts: []types.Alert{
					{
						Status:      "resolved",
						Fingerprint: fmt.Sprintf("resolved-fp-%d", idx),
						Labels: map[string]string{
							"alertname": "ResolvedAlert",
							"namespace": "default",
						},
						StartsAt: time.Now(),
					},
				},
			}

			body, _ := json.Marshal(alertGroup)
			resp, err := http.Post(srv.URL+"/webhook", "application/json", bytes.NewReader(body))
			if err != nil {
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				ok.Add(1)
			}
		}(i)
	}

	wg.Wait()

	// All resolved-only should return 200 (skipped)
	if ok.Load() != 50 {
		t.Errorf("ok = %d, want 50 (all resolved-only should be skipped)", ok.Load())
	}
}
