// Package severity maps a usage percentage to a severity level.
// It is a pure leaf package with zero external dependencies.
package severity

// Severity represents a usage severity level.
type Severity int

const (
	// Low is 0–49% utilization.
	Low Severity = iota
	// Mid is 50–74% utilization.
	Mid
	// High is 75–89% utilization.
	High
	// Critical is ≥ 90% utilization.
	Critical
)

// Classify returns the Severity for a given utilization percentage using
// half-open intervals: [0,50) → Low, [50,75) → Mid, [75,90) → High, [90,∞) → Critical.
func Classify(pct float64) Severity {
	switch {
	case pct < 50:
		return Low
	case pct < 75:
		return Mid
	case pct < 90:
		return High
	default:
		return Critical
	}
}
