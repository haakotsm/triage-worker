package activity

import (
	"encoding/json"
	"testing"
)

func TestFixSingleQuotes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "python dict style",
			input: `{'findings': [{'category': 'metric_anomaly', 'description': 'High memory', 'evidence': 'usage at 95%', 'severity': 'critical', 'confidence': 0.9}]}`,
			want:  `{"findings": [{"category": "metric_anomaly", "description": "High memory", "evidence": "usage at 95%", "severity": "critical", "confidence": 0.9}]}`,
		},
		{
			name:  "already valid json",
			input: `{"findings": []}`,
			want:  `{"findings": []}`,
		},
		{
			name:  "mixed quotes - single outer",
			input: `{'key': "value with double quotes inside"}`,
			want:  `{"key": "value with double quotes inside"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fixSingleQuotes(tt.input)
			if got != tt.want {
				t.Errorf("fixSingleQuotes()\ngot:  %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestRepairJSON(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantValid bool
	}{
		{
			name:      "valid json unchanged",
			input:     `{"findings": [{"category": "resource_state", "description": "test", "evidence": "data", "severity": "info", "confidence": 0.5}]}`,
			wantValid: true,
		},
		{
			name:      "single quotes repaired",
			input:     `{'findings': [{'category': 'resource_state', 'description': 'test', 'evidence': 'data', 'severity': 'info', 'confidence': 0.5}]}`,
			wantValid: true,
		},
		{
			name:      "trailing comma repaired",
			input:     `{"findings": [{"category": "resource_state", "description": "test", "evidence": "data", "severity": "info", "confidence": 0.5},]}`,
			wantValid: true,
		},
		{
			name:      "truncated json repaired",
			input:     `{"findings": [{"category": "resource_state", "description": "test"`,
			wantValid: true,
		},
		{
			name:      "empty string",
			input:     "",
			wantValid: false,
		},
		{
			name:      "prose before json",
			input:     "Here is my analysis of the metrics:\n{\"findings\": [{\"category\": \"metric_anomaly\", \"description\": \"test\", \"evidence\": \"data\", \"severity\": \"info\", \"confidence\": 0.8}]}",
			wantValid: true,
		},
		{
			name:      "prose before single-quoted json",
			input:     "Based on my investigation:\n{'findings': [{'category': 'metric_anomaly', 'description': 'High memory', 'evidence': '95%', 'severity': 'critical', 'confidence': 0.9}]}",
			wantValid: true,
		},
		{
			name:      "bare single quotes in findings",
			input:     `[{'category': 'metric_anomaly', 'description': 'OOM detected', 'evidence': 'container killed', 'severity': 'critical', 'confidence': 0.95}]`,
			wantValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := repairJSON(tt.input)
			if tt.wantValid {
				if got == "" {
					t.Fatalf("repairJSON returned empty string")
				}
				// Verify it's valid JSON
				if !isValidJSON(got) {
					t.Errorf("repairJSON() produced invalid JSON:\n%s", got)
				}
			}
		})
	}
}

func isValidJSON(s string) bool {
	var js interface{}
	return json.Unmarshal([]byte(s), &js) == nil
}
