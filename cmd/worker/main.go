package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/haakotsm/triage-worker/internal/activity"
	triageapi "github.com/haakotsm/triage-worker/internal/api"
	"github.com/haakotsm/triage-worker/internal/auth"
	"github.com/haakotsm/triage-worker/internal/settings"
	"github.com/haakotsm/triage-worker/internal/telemetry"
	"github.com/haakotsm/triage-worker/internal/web"
	"github.com/haakotsm/triage-worker/internal/webhook"
	"github.com/haakotsm/triage-worker/internal/workflow"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(getEnv("LOG_LEVEL", "info")),
	}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, logger); err != nil {
		logger.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	// --- Configuration ---
	temporalAddr := getEnv("TEMPORAL_ADDRESS", "temporal-frontend.temporal.svc.cluster.local:7233")
	temporalNS := getEnv("TEMPORAL_NAMESPACE", "default")
	taskQueue := getEnv("TEMPORAL_TASK_QUEUE", "k8s-triage")
	agentURL := getEnv("KAGENT_A2A_URL", "http://agentgateway.agentgateway.svc.cluster.local:3001")
	agentNS := getEnv("KAGENT_AGENT_NAMESPACE", "kagent")
	agentName := getEnv("KAGENT_AGENT_NAME", "error-triage-agent")
	prometheusURL := getEnv("PROMETHEUS_URL", "http://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090")
	lokiURL := getEnv("LOKI_URL", "http://loki.monitoring.svc.cluster.local:3100")
	keycloakTokenURL := getEnv("KEYCLOAK_TOKEN_URL", "http://keycloak.keycloak.svc.cluster.local/realms/bibliotek/protocol/openid-connect/token")
	keycloakClientID := getEnv("KEYCLOAK_CLIENT_ID", "triage-worker")
	keycloakClientSecret := getEnv("KEYCLOAK_CLIENT_SECRET", "")
	databaseURL := getEnv("DATABASE_URL", "")
	webhookSecret := getEnv("WEBHOOK_SECRET", "")
	listenAddr := getEnv("LISTEN_ADDR", ":8080")
	metricsAddr := getEnv("METRICS_ADDR", ":9090")

	fullAgentURL := fmt.Sprintf("%s/api/a2a/%s/%s", agentURL, agentNS, agentName)

	// --- Database ---
	var db *sql.DB
	if databaseURL != "" {
		var err error
		db, err = sql.Open("postgres", databaseURL)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer db.Close()

		db.SetMaxOpenConns(5)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(5 * time.Minute)

		if err := activity.MigrateSchema(ctx, db); err != nil {
			return fmt.Errorf("migrate schema: %w", err)
		}
		logger.Info("database connected and migrated")
	} else {
		logger.Warn("DATABASE_URL not set — reports will not be persisted")
	}

	// --- Kubernetes Client (reused across activity invocations) ---
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		logger.Warn("k8s in-cluster config unavailable — K8s enrichment will be disabled", "error", err)
	}
	var k8sClientset kubernetes.Interface
	if k8sConfig != nil {
		k8sClientset, err = kubernetes.NewForConfig(k8sConfig)
		if err != nil {
			return fmt.Errorf("create k8s clientset: %w", err)
		}
		logger.Info("kubernetes client initialized")
	}
	k8sActivity := &activity.K8sActivity{Clientset: k8sClientset}

	// --- Temporal Client ---
	// Wire the SDK's built-in metrics into the default Prometheus registry so
	// workflow/activity latency, failures, and task-queue health surface on the
	// same /metrics endpoint the worker already serves.
	temporalMetrics, metricsCloser := telemetry.NewTemporalMetricsHandler(prometheus.DefaultRegisterer)
	defer func() {
		if err := metricsCloser.Close(); err != nil {
			logger.Warn("temporal metrics scope close error", "error", err)
		}
	}()

	tc, err := client.Dial(client.Options{
		HostPort:       temporalAddr,
		Namespace:      temporalNS,
		Logger:         newTemporalLogger(logger),
		MetricsHandler: temporalMetrics,
	})
	if err != nil {
		return fmt.Errorf("connect to temporal: %w", err)
	}
	defer tc.Close()
	logger.Info("temporal connected", "address", temporalAddr, "namespace", temporalNS)

	// --- OAuth2 Token Provider ---
	tokenProvider := auth.NewTokenProvider(keycloakTokenURL, keycloakClientID, keycloakClientSecret)

	// --- Activities ---
	httpClient := &http.Client{Timeout: 30 * time.Second}

	enrichActivities := &activity.Activities{
		PrometheusURL: prometheusURL,
		LokiURL:       lokiURL,
		HTTPClient:    httpClient,
	}

	agentActivity := &activity.AgentActivity{
		AgentURL:      fullAgentURL,
		TokenProvider: tokenProvider,
		HTTPClient:    &http.Client{}, // no Client.Timeout — controlled by activity context deadline (300s)
	}

	reportActivity := &activity.ReportActivity{DB: db}

	// --- Temporal Worker ---
	w := worker.New(tc, taskQueue, worker.Options{
		MaxConcurrentActivityExecutionSize:     10,
		MaxConcurrentWorkflowTaskExecutionSize: 10,
	})

	w.RegisterWorkflow(workflow.TriageWorkflow)
	w.RegisterActivity(enrichActivities)
	w.RegisterActivity(agentActivity)
	w.RegisterActivity(reportActivity)
	w.RegisterActivity(k8sActivity)

	// --- Runtime settings (dashboard kill-switch) ---
	// Backed by Postgres so a "pause workflows" toggle survives pod restarts /
	// image redeploys; primed into an atomic cache the webhook reads per alert.
	var settingsStore *settings.Store
	if db != nil {
		settingsStore = settings.New(db, logger)
		if err := settingsStore.Load(ctx); err != nil {
			logger.Warn("load settings — using defaults (workflows enabled)", "error", err)
		}
	}

	// --- HTTP Server (webhook + health + API) ---
	var apiHandler http.Handler
	var webHandler http.Handler
	if db != nil {
		apiHandler = triageapi.NewHandler(db, logger)
		logger.Info("report API enabled")

		wh, err := web.NewHandler(db, logger)
		if err != nil {
			return fmt.Errorf("create web handler: %w", err)
		}
		wh.SetSettings(settingsStore)

		// SSE broker for realtime updates (requires DATABASE_URL for PG LISTEN).
		sseBroker := web.NewSSEBroker(db, databaseURL, logger)
		if err := sseBroker.Start(ctx); err != nil {
			logger.Warn("SSE broker failed to start — realtime disabled", "error", err)
		} else {
			wh.SetSSEBroker(sseBroker)
			defer sseBroker.Stop()
			logger.Info("SSE broker started")
		}

		// Operator-initiated re-triage: start a fresh TriageWorkflow for an
		// existing incident from the dashboard.
		wh.SetRetrieveStarter(workflow.NewRetriageStarter(tc, db, taskQueue, logger))
		logger.Info("re-triage enabled")

		devMode := os.Getenv("DEV_MODE") == "true"
		authMW := web.NewAuthMiddleware(logger, devMode)
		csrfMW := web.NewCSRFMiddleware(logger)
		// InstrumentHandler is outermost so it records every web request
		// (method/route/status + latency) regardless of auth/CSRF outcome.
		webHandler = web.InstrumentHandler(authMW.Wrap(csrfMW.Wrap(wh)))
		logger.Info("web dashboard enabled", "dev_mode", devMode)
	}
	handler := webhook.NewHandler(tc, taskQueue, logger, webhookSecret, apiHandler, webHandler, db)
	if settingsStore != nil {
		handler.SetWorkflowGate(settingsStore)
	}

	// The dashboard/webhook server listens on listenAddr (public via ingress).
	// Prometheus /metrics is served on a SEPARATE listener (metricsAddr) so it
	// is never exposed through the public dashboard ingress, and outside the
	// instrumented web handler so scrapes don't inflate request metrics.
	mux := newPublicMux(handler)

	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	metricsSrv := &http.Server{
		Addr:         metricsAddr,
		Handler:      newMetricsMux(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// --- Start ---
	errCh := make(chan error, 3)

	go func() {
		logger.Info("starting temporal worker", "task_queue", taskQueue)
		errCh <- w.Run(worker.InterruptCh())
	}()

	go func() {
		logger.Info("starting HTTP server", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("http server: %w", err)
		}
	}()

	go func() {
		logger.Info("starting metrics server", "addr", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	// --- Health check loop ---
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Verify Temporal connectivity
				_, err := tc.CheckHealth(ctx, &client.CheckHealthRequest{})
				handler.SetHealthy(err == nil)
				if err != nil {
					logger.Warn("temporal health check failed", "error", err)
				}
			}
		}
	}()

	// --- Wait for shutdown ---
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	handler.SetHealthy(false)
	// Shut both HTTP servers down concurrently so each gets the full grace
	// window. The dashboard server holds long-lived SSE connections and can
	// consume most of shutdownCtx; running them in parallel keeps the metrics
	// server from inheriting a near-expired deadline and avoids serializing the
	// wait before w.Stop().
	var shutdownWG sync.WaitGroup
	for _, s := range []struct {
		name string
		srv  *http.Server
	}{
		{"http", srv},
		{"metrics", metricsSrv},
	} {
		shutdownWG.Add(1)
		go func(name string, srv *http.Server) {
			defer shutdownWG.Done()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				logger.Error("shutdown error", "server", name, "error", err)
			}
		}(s.name, s.srv)
	}
	shutdownWG.Wait()

	w.Stop()
	logger.Info("shutdown complete")
	return nil
}

// newPublicMux builds the public dashboard/webhook router served on LISTEN_ADDR
// (exposed via ingress). It intentionally does NOT register /metrics: Prometheus
// metrics live on the dedicated metrics listener (see newMetricsMux) so they are
// never reachable through the public ingress.
func newPublicMux(handler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/", handler)
	return mux
}

// newMetricsMux builds the dedicated Prometheus metrics router served on
// METRICS_ADDR (default :9090), kept separate from the public dashboard server
// so /metrics is never reachable through the dashboard ingress.
//
// The endpoint is unauthenticated and binds on all interfaces, so it relies on
// network-level segmentation: a Kubernetes NetworkPolicy must restrict ingress
// on the metrics port to the Prometheus/monitoring namespace. See
// bodils-bibliotek-operations issue #128.
func newMetricsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", web.MetricsHandler())
	return mux
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// temporalLogger adapts slog to Temporal's logger interface.
type temporalLogger struct {
	logger *slog.Logger
}

func newTemporalLogger(l *slog.Logger) *temporalLogger {
	return &temporalLogger{logger: l.With("component", "temporal-sdk")}
}

func (l *temporalLogger) Debug(msg string, keyvals ...interface{}) {
	l.logger.Debug(msg, keyvals...)
}
func (l *temporalLogger) Info(msg string, keyvals ...interface{}) {
	l.logger.Info(msg, keyvals...)
}
func (l *temporalLogger) Warn(msg string, keyvals ...interface{}) {
	l.logger.Warn(msg, keyvals...)
}
func (l *temporalLogger) Error(msg string, keyvals ...interface{}) {
	l.logger.Error(msg, keyvals...)
}
