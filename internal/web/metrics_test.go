package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInstrumentHandler_CapturesStatusCode(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})

	handler := InstrumentHandler(inner)
	req := httptest.NewRequest(http.MethodPost, "/reports/1/resolve", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", rec.Code)
	}
}

func TestInstrumentHandler_DefaultsTo200(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	handler := InstrumentHandler(inner)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestNormalizePath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/", "/"},
		{"/dashboard", "/"},
		{"/events", "/events"},
		{"/partials/reports", "/partials/reports"},
		{"/partials/stats", "/partials/stats"},
		{"/partials/incidents", "/partials/incidents"},
		{"/reports/123", "/reports/:id"},
		{"/reports/abc-def", "/reports/:id"},
		{"/static/css/app.css", "/static/*"},
		{"/unknown/path", "/other"},
	}
	for _, tt := range tests {
		got := normalizePath(tt.path)
		if got != tt.want {
			t.Errorf("normalizePath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestResponseWriter_WriteHeader_OnlyOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, statusCode: http.StatusOK}
	rw.WriteHeader(http.StatusNotFound)
	rw.WriteHeader(http.StatusOK) // should be ignored

	if rw.statusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rw.statusCode)
	}
}

func TestResponseWriter_Write_SetsWritten(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	_, err := rw.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if !rw.written {
		t.Error("expected written=true after Write")
	}
}

func TestResponseWriter_Unwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	if rw.Unwrap() != rec {
		t.Error("Unwrap should return the underlying ResponseWriter")
	}
}

func TestSetSSEClientCount(t *testing.T) {
	// Should not panic.
	SetSSEClientCount(5)
	SetSSEClientCount(0)
}

func TestSetReportStateCount(t *testing.T) {
	// Should not panic.
	SetReportStateCount("open", 10)
	SetReportStateCount("resolved", 5)
}

func TestMetricsHandler_ServesMetrics(t *testing.T) {
	handler := MetricsHandler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") == "" {
		t.Error("expected Content-Type header")
	}
}
