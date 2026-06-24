package telemetry

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestNewTemporalMetricsHandlerRegistersMetrics verifies that metrics emitted
// through the Temporal SDK MetricsHandler are reported into the supplied
// Prometheus registry, so they appear on the worker's /metrics endpoint.
func TestNewTemporalMetricsHandlerRegistersMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()

	handler, closer := NewTemporalMetricsHandler(reg)
	if handler == nil {
		t.Fatal("NewTemporalMetricsHandler returned a nil handler")
	}

	handler.Counter("test_workflow_completed").Inc(1)

	// Closing the tally scope flushes buffered metrics into the registry.
	if err := closer.Close(); err != nil {
		t.Fatalf("closer.Close: %v", err)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("registry.Gather: %v", err)
	}

	found := false
	for _, f := range families {
		if strings.Contains(f.GetName(), "test_workflow_completed") {
			found = true
			break
		}
	}
	if !found {
		var names []string
		for _, f := range families {
			names = append(names, f.GetName())
		}
		t.Errorf("emitted counter not registered in Prometheus registry; got families %v", names)
	}
}

// TestNewTemporalMetricsHandlerWithTags ensures tagging returns a usable child
// handler (the SDK calls WithTags heavily) without panicking or losing the
// backing scope.
func TestNewTemporalMetricsHandlerWithTags(t *testing.T) {
	reg := prometheus.NewRegistry()
	handler, closer := NewTemporalMetricsHandler(reg)
	defer func() { _ = closer.Close() }()

	tagged := handler.WithTags(map[string]string{"namespace": "default"})
	if tagged == nil {
		t.Fatal("WithTags returned nil")
	}
	tagged.Gauge("test_worker_task_slots_available").Update(5)
}

// TestNewTemporalMetricsHandlerDuplicateRegistryDoesNotPanic ensures that
// building a second handler against the same registry and re-emitting a metric
// degrades gracefully (logs) instead of panicking — tally's default register
// error behaviour is to panic, which would take down the worker.
func TestNewTemporalMetricsHandlerDuplicateRegistryDoesNotPanic(t *testing.T) {
	reg := prometheus.NewRegistry()

	h1, c1 := NewTemporalMetricsHandler(reg)
	defer func() { _ = c1.Close() }()
	h1.Counter("test_dup_metric").Inc(1)
	_ = c1.Close() // flush h1's registration into reg

	h2, c2 := NewTemporalMetricsHandler(reg)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second handler panicked on duplicate registration: %v", r)
		}
	}()
	h2.Counter("test_dup_metric").Inc(1)
	if err := c2.Close(); err != nil {
		t.Fatalf("c2.Close: %v", err)
	}
}
