package main

import (
	"log/slog"
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
