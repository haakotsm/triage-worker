// Package telemetry wires the worker's runtime metrics backends. It currently
// exposes the Temporal Go SDK's built-in metrics through the same Prometheus
// registry the rest of the worker uses, so client/worker metrics land on the
// existing /metrics endpoint.
package telemetry

import (
	"io"
	"time"

	prom "github.com/prometheus/client_golang/prometheus"
	tally "github.com/uber-go/tally/v4"
	tallyprom "github.com/uber-go/tally/v4/prometheus"
	"go.temporal.io/sdk/client"
	sdktally "go.temporal.io/sdk/contrib/tally"
)

// reportInterval is how often the tally root scope flushes buffered metrics into
// the Prometheus registry. The Prometheus reporter is pull-based, but tally
// still needs a non-zero interval to move cached values into the registry
// between scrapes.
const reportInterval = time.Second

// NewTemporalMetricsHandler builds a Temporal SDK MetricsHandler that emits the
// SDK's built-in client and worker metrics — workflow/activity execution
// latency and failures, task-queue poll latency, sticky-cache hits, schedule-
// to-start latency, etc. — into the supplied Prometheus registerer. Because the
// worker's /metrics endpoint serves prometheus.DefaultRegisterer, passing that
// registerer surfaces Temporal metrics on the same endpoint with no extra
// listener.
//
// The returned io.Closer stops the underlying tally scope and must be closed on
// shutdown to flush and release its background reporter goroutine.
func NewTemporalMetricsHandler(registerer prom.Registerer) (client.MetricsHandler, io.Closer) {
	reporter := tallyprom.NewReporter(tallyprom.Options{Registerer: registerer})

	scope, closer := tally.NewRootScope(tally.ScopeOptions{
		CachedReporter:  reporter,
		Separator:       tallyprom.DefaultSeparator,
		SanitizeOptions: &sdktally.PrometheusSanitizeOptions,
	}, reportInterval)
	scope = sdktally.NewPrometheusNamingScope(scope)

	return sdktally.NewMetricsHandler(scope), closer
}
