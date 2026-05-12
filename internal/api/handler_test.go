package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestIntParam(t *testing.T) {
	tests := []struct {
		name       string
		s          string
		defaultVal int
		min, max   int
		want       int
	}{
		{"empty returns default", "", 10, 1, 100, 10},
		{"valid in range", "50", 10, 1, 100, 50},
		{"below min returns default", "0", 10, 1, 100, 10},
		{"above max returns max", "200", 10, 1, 100, 100},
		{"non-numeric returns default", "abc", 10, 1, 100, 10},
		{"exact min", "1", 10, 1, 100, 1},
		{"exact max", "100", 10, 1, 100, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := intParam(tt.s, tt.defaultVal, tt.min, tt.max)
			if got != tt.want {
				t.Errorf("intParam(%q, %d, %d, %d) = %d, want %d",
					tt.s, tt.defaultVal, tt.min, tt.max, got, tt.want)
			}
		})
	}
}

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	data := map[string]string{"status": "ok"}
	writeJSON(w, 200, data)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "ok" {
		t.Errorf("body status = %q, want ok", got["status"])
	}
}
