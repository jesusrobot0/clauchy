package severity_test

import (
	"testing"

	"github.com/jesusrobot0/clauchy/internal/severity"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pct  float64
		want severity.Severity
	}{
		// Low: [0, 50)
		{"zero", 0, severity.Low},
		{"low mid-range", 25, severity.Low},
		{"just below 50 integer", 49, severity.Low},
		{"just below 50 fractional", 49.5, severity.Low},

		// Mid: [50, 75)
		{"exactly 50", 50, severity.Mid},
		{"mid mid-range", 62.5, severity.Mid},
		{"just below 75 integer", 74, severity.Mid},
		{"just below 75 fractional", 74.5, severity.Mid},

		// High: [75, 90)
		{"exactly 75", 75, severity.High},
		{"high mid-range", 82, severity.High},
		{"just below 90 integer", 89, severity.High},
		{"just below 90 fractional", 89.5, severity.High},

		// Critical: [90, ∞)
		{"exactly 90", 90, severity.Critical},
		{"critical mid-range", 95, severity.Critical},
		{"exactly 100", 100, severity.Critical},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := severity.Classify(tt.pct)
			if got != tt.want {
				t.Errorf("Classify(%v) = %v, want %v", tt.pct, got, tt.want)
			}
		})
	}
}

func TestSeverityString(t *testing.T) {
	t.Parallel()

	// Verify the four severity levels are distinct values.
	levels := []severity.Severity{severity.Low, severity.Mid, severity.High, severity.Critical}
	seen := make(map[severity.Severity]bool)
	for _, s := range levels {
		if seen[s] {
			t.Errorf("severity level %v is not unique", s)
		}
		seen[s] = true
	}
}
