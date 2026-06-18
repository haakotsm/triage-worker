package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/lib/pq"
)

const (
	maxSSEClients       = 100
	sseReconnectTimeout = 3 * time.Second
	channelReports      = "report_changes"
	debounceWindow      = 500 * time.Millisecond
)

// SSEBroker manages Server-Sent Events with PG LISTEN/NOTIFY fan-out.
type SSEBroker struct {
	mu        sync.RWMutex
	clients   map[chan SSEEvent]struct{}
	logger    *slog.Logger
	db        *sql.DB
	dsn       string
	listener  *pq.Listener
	cancelCtx context.CancelFunc
	wg        sync.WaitGroup // tracks dispatchLoop goroutine

	// Debounce: coalesce rapid PG NOTIFY events into a single broadcast.
	debounceMu    sync.Mutex
	pendingEvents map[string]SSEEvent // keyed by event name
	debounceTimer *time.Timer
}

// SSEEvent represents a named event sent to clients.
type SSEEvent struct {
	Name string // SSE event name (e.g. "incident-update", "report-update")
	Data string // JSON payload
}

// IncidentNotification is the payload from PG NOTIFY on incident_changes.
type IncidentNotification struct {
	ID         int64  `json:"id"`
	WorkflowID string `json:"workflow_id"`
	State      string `json:"state"`
	Namespace  string `json:"namespace"`
	Workload   string `json:"workload"`
	Severity   string `json:"severity"`
	UpdatedAt  string `json:"updated_at"`
}

// NewSSEBroker creates a broker. Call Start() to begin listening.
func NewSSEBroker(db *sql.DB, dsn string, logger *slog.Logger) *SSEBroker {
	return &SSEBroker{
		clients:       make(map[chan SSEEvent]struct{}),
		logger:        logger,
		db:            db,
		dsn:           dsn,
		pendingEvents: make(map[string]SSEEvent),
	}
}

// Start begins PG LISTEN and event dispatch. Call Stop() to clean up.
func (b *SSEBroker) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	b.cancelCtx = cancel

	reportProblem := func(ev pq.ListenerEventType, err error) {
		if err != nil {
			b.logger.Error("pg listener error", "event", ev, "error", err)
		}
	}

	b.listener = pq.NewListener(b.dsn, 10*time.Second, time.Minute, reportProblem)

	if err := b.listener.Listen(channelReports); err != nil {
		cancel()
		return fmt.Errorf("listen %s: %w", channelReports, err)
	}

	b.logger.Info("SSE broker started", "channel", channelReports)

	b.wg.Add(1)
	go b.dispatchLoop(ctx)
	return nil
}

// Stop shuts down the PG listener and closes all client channels.
func (b *SSEBroker) Stop() {
	if b.cancelCtx != nil {
		b.cancelCtx()
	}
	b.wg.Wait() // wait for dispatchLoop to exit before closing channels
	if b.listener != nil {
		_ = b.listener.Close()
	}
	b.mu.Lock()
	for ch := range b.clients {
		close(ch)
	}
	b.clients = nil // signal broadcast() to stop
	b.mu.Unlock()
}

func (b *SSEBroker) dispatchLoop(ctx context.Context) {
	defer b.wg.Done()
	for {
		select {
		case <-ctx.Done():
			// Stop the debounce timer to prevent post-shutdown callback races.
			b.debounceMu.Lock()
			if b.debounceTimer != nil {
				b.debounceTimer.Stop()
			}
			b.debounceMu.Unlock()
			// Flush any pending debounced events before exit.
			b.flushPendingEvents()
			return
		case n := <-b.listener.Notify:
			if n == nil {
				// PG listener reconnected — notifications during disconnect are lost.
				// Broadcast refresh immediately (not debounced) so frontends refetch.
				b.broadcast(SSEEvent{Name: "refresh", Data: `{"reason":"reconnect"}`})
				continue
			}
			if n.Channel != channelReports {
				continue
			}
			// Determine event name from payload state for targeted frontend refresh.
			eventName := "report-update"
			var payload struct {
				State string `json:"state"`
			}
			if json.Unmarshal([]byte(n.Extra), &payload) == nil {
				switch payload.State {
				case "processing", "reported", "acknowledged", "resolved":
					eventName = "incident-update"
				}
			}
			b.debouncedBroadcast(SSEEvent{Name: eventName, Data: n.Extra})
		}
	}
}

// debouncedBroadcast coalesces events within a debounce window.
// Only the latest event per event-name is kept; after the window expires,
// all pending events are broadcast at once.
func (b *SSEBroker) debouncedBroadcast(event SSEEvent) {
	b.debounceMu.Lock()
	defer b.debounceMu.Unlock()

	b.pendingEvents[event.Name] = event

	if b.debounceTimer != nil {
		b.debounceTimer.Stop()
	}
	b.debounceTimer = time.AfterFunc(debounceWindow, func() {
		b.flushPendingEvents()
	})
}

// flushPendingEvents broadcasts all coalesced events and clears the buffer.
func (b *SSEBroker) flushPendingEvents() {
	b.debounceMu.Lock()
	events := make([]SSEEvent, 0, len(b.pendingEvents))
	for _, ev := range b.pendingEvents {
		events = append(events, ev)
	}
	b.pendingEvents = make(map[string]SSEEvent)
	b.debounceMu.Unlock()

	for _, ev := range events {
		b.broadcast(ev)
	}
}

func (b *SSEBroker) broadcast(event SSEEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.clients == nil {
		return // broker stopped
	}
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
			// Slow client, drop event (non-blocking)
		}
	}
}

// subscribe adds a client. Returns the event channel and an unsubscribe func.
func (b *SSEBroker) subscribe() (chan SSEEvent, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.clients) >= maxSSEClients {
		return nil, nil, fmt.Errorf("max SSE clients reached (%d)", maxSSEClients)
	}
	ch := make(chan SSEEvent, 16)
	b.clients[ch] = struct{}{}
	SetSSEClientCount(len(b.clients))
	unsub := func() {
		b.mu.Lock()
		delete(b.clients, ch)
		n := len(b.clients)
		b.mu.Unlock()
		SetSSEClientCount(n)
	}
	return ch, unsub, nil
}

// ClientCount returns the number of active SSE clients.
func (b *SSEBroker) ClientCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.clients)
}

// ServeHTTP handles the /events SSE endpoint.
func (b *SSEBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch, unsub, err := b.subscribe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer unsub()

	// Disable WriteTimeout for this long-lived connection (Go 1.20+).
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx/ingress

	// Send initial connection event.
	fmt.Fprintf(w, "event: connected\ndata: {\"clients\":%d}\n\n", b.ClientCount())
	flusher.Flush()

	// Keepalive ticker to detect dead connections.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return // broker shutting down
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Name, event.Data)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// Incident represents an in-flight incident for the dashboard.
type Incident struct {
	ID         int64     `json:"id"`
	WorkflowID string    `json:"workflow_id"`
	Namespace  string    `json:"namespace"`
	Workload   string    `json:"workload"`
	Kind       string    `json:"kind"`
	AlertName  string    `json:"alert_name"`
	State      string    `json:"state"`
	Severity   string    `json:"severity"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// FetchActiveIncidents queries reports that are still in-flight or awaiting action.
func FetchActiveIncidents(ctx context.Context, db *sql.DB) ([]Incident, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, workflow_id, namespace, workload, kind, alert_name, state, severity, created_at,
		        COALESCE(completed_at, created_at) as updated_at
		 FROM triage.reports
		 WHERE state IN ('processing', 'reported', 'acknowledged')
		 ORDER BY created_at DESC
		 LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var incidents []Incident
	for rows.Next() {
		var inc Incident
		if err := rows.Scan(&inc.ID, &inc.WorkflowID, &inc.Namespace, &inc.Workload,
			&inc.Kind, &inc.AlertName, &inc.State, &inc.Severity,
			&inc.CreatedAt, &inc.UpdatedAt); err != nil {
			return nil, err
		}
		incidents = append(incidents, inc)
	}
	return incidents, rows.Err()
}

// BroadcastStatsUpdate queries fresh stats and broadcasts to all SSE clients.
func (b *SSEBroker) BroadcastStatsUpdate(ctx context.Context) {
	if b.db == nil {
		return
	}
	type statsPayload struct {
		TotalReports int `json:"total_reports"`
		ActiveCount  int `json:"active_incidents"`
	}
	var s statsPayload
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM triage.reports WHERE state = 'reported'`).Scan(&s.TotalReports); err != nil {
		b.logger.Error("broadcast stats: count reported", "error", err)
		return
	}
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM triage.reports WHERE state IN ('processing', 'reported', 'acknowledged')`).Scan(&s.ActiveCount); err != nil {
		b.logger.Error("broadcast stats: count active", "error", err)
		return
	}
	data, err := json.Marshal(s)
	if err != nil {
		b.logger.Error("broadcast stats: marshal", "error", err)
		return
	}
	b.broadcast(SSEEvent{Name: "stats-update", Data: string(data)})
}
