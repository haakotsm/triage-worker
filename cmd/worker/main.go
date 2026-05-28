package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/haakotsm/triage-worker/internal/activity"
	triageapi "github.com/haakotsm/triage-worker/internal/api"
	"github.com/haakotsm/triage-worker/internal/auth"
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
	correlationDebounce := parseDuration(getEnv("CORRELATION_DEBOUNCE", "60s"), 60*time.Second)
	correlationHardCap := parseDuration(getEnv("CORRELATION_MAX_WINDOW", "5m"), 5*time.Minute)

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
	tc, err := client.Dial(client.Options{
		HostPort:  temporalAddr,
		Namespace: temporalNS,
		Logger:    newTemporalLogger(logger),
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

		// SSE broker for realtime updates (requires DATABASE_URL for PG LISTEN).
		sseBroker := web.NewSSEBroker(db, databaseURL, logger)
		if err := sseBroker.Start(ctx); err != nil {
			logger.Warn("SSE broker failed to start — realtime disabled", "error", err)
		} else {
			wh.SetSSEBroker(sseBroker)
			defer sseBroker.Stop()
			logger.Info("SSE broker started")
		}

		webHandler = wh
		logger.Info("web dashboard enabled")
	}
	handler := webhook.NewHandler(tc, taskQueue, logger, webhookSecret, apiHandler, webHandler, db, correlationDebounce, correlationHardCap)

	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// --- Start ---
	errCh := make(chan error, 2)

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
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "error", err)
	}

	w.Stop()
	logger.Info("shutdown complete")
	return nil
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

func parseDuration(s string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
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
