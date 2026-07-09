package pricing_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jesusrobot0/clauchy/internal/pricing"
)

// TestPricingDate asserts that the embedded pricing date matches the design specification.
func TestPricingDate(t *testing.T) {
	t.Parallel()

	const want = "2026-07-07"
	if pricing.PricingDate != want {
		t.Errorf("PricingDate = %q, want %q", pricing.PricingDate, want)
	}
}

// TestBuiltin_NonEmpty asserts that Builtin() returns a populated table.
func TestBuiltin_NonEmpty(t *testing.T) {
	t.Parallel()

	table := pricing.Builtin()
	if len(table) == 0 {
		t.Fatal("Builtin() returned an empty table")
	}
}

// TestBuiltin_KnownModels asserts that the July 2026 rates for key models are correct.
func TestBuiltin_KnownModels(t *testing.T) {
	t.Parallel()

	table := pricing.Builtin()

	cases := []struct {
		model      string
		wantInput  float64
		wantOutput float64
	}{
		{"claude-opus-4-8", 5, 25},
		{"claude-sonnet-4-6", 3, 15},
		{"claude-haiku-4-5", 1, 5},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			r, ok := table[tc.model]
			if !ok {
				t.Fatalf("Builtin(): model %q not found in table", tc.model)
			}
			if r.Input != tc.wantInput {
				t.Errorf("Input = %v, want %v", r.Input, tc.wantInput)
			}
			if r.Output != tc.wantOutput {
				t.Errorf("Output = %v, want %v", r.Output, tc.wantOutput)
			}
		})
	}
}

// TestCost_Buckets verifies that Cost() applies the correct multipliers to each
// cache bucket (each multiplied by Ri), NOT to the plain input count.
func TestCost_Buckets(t *testing.T) {
	t.Parallel()

	opusRate := pricing.Rate{Input: 5, Output: 25}
	sonnetRate := pricing.Rate{Input: 3, Output: 15}

	tests := []struct {
		name  string
		usage pricing.Usage
		rate  pricing.Rate
		want  float64
	}{
		{
			// spec scenario: 1h cache write: 1M × $5 × 2.0 = $10.00
			name:  "1h cache write contribution",
			usage: pricing.Usage{CacheWrite1h: 1_000_000},
			rate:  opusRate,
			want:  10.00,
		},
		{
			// spec scenario: 5m cache write (explicit bucket): 1M × $3 × 1.25 = $3.75
			name:  "5m cache write contribution",
			usage: pricing.Usage{CacheWrite5m: 1_000_000},
			rate:  sonnetRate,
			want:  3.75,
		},
		{
			name:  "cache read contribution",
			usage: pricing.Usage{CacheRead: 1_000_000},
			rate:  opusRate,
			want:  0.50, // 1M × $5 × 0.1
		},
		{
			name:  "output contribution",
			usage: pricing.Usage{Output: 1_000_000},
			rate:  opusRate,
			want:  25.00, // 1M × $25
		},
		{
			name:  "input contribution",
			usage: pricing.Usage{Input: 1_000_000},
			rate:  opusRate,
			want:  5.00, // 1M × $5
		},
		{
			name: "all buckets combined",
			usage: pricing.Usage{
				Input:        1_000_000, // 1M × $5     = $5.00
				CacheWrite1h: 1_000_000, // 1M × $5 × 2 = $10.00
				CacheWrite5m: 1_000_000, // 1M × $5 × 1.25 = $6.25
				CacheRead:    1_000_000, // 1M × $5 × 0.1 = $0.50
				Output:       1_000_000, // 1M × $25    = $25.00
			},
			rate: opusRate,
			want: 46.75, // 5 + 10 + 6.25 + 0.5 + 25
		},
		{
			name:  "zero usage",
			usage: pricing.Usage{},
			rate:  opusRate,
			want:  0,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := pricing.Cost(tt.usage, tt.rate)
			if got != tt.want {
				t.Errorf("Cost() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRateFor tests the chained model ID normalization.
// Order: exact → strip 8-digit date suffix → strip bracket suffix → chained (both).
func TestRateFor(t *testing.T) {
	t.Parallel()

	table := pricing.Table{
		"claude-opus-4-8":   {Input: 5, Output: 25},
		"claude-sonnet-4-6": {Input: 3, Output: 15},
	}

	tests := []struct {
		name      string
		model     string
		wantRate  pricing.Rate
		wantFound bool
	}{
		{
			name:      "exact match",
			model:     "claude-opus-4-8",
			wantRate:  pricing.Rate{Input: 5, Output: 25},
			wantFound: true,
		},
		{
			name:      "strip 8-digit date suffix",
			model:     "claude-opus-4-8-20260101",
			wantRate:  pricing.Rate{Input: 5, Output: 25},
			wantFound: true,
		},
		{
			name:      "strip trailing bracket suffix",
			model:     "claude-opus-4-8[1m]",
			wantRate:  pricing.Rate{Input: 5, Output: 25},
			wantFound: true,
		},
		{
			name:      "chained: date suffix then bracket suffix",
			model:     "claude-opus-4-8-20260101[1m]",
			wantRate:  pricing.Rate{Input: 5, Output: 25},
			wantFound: true,
		},
		{
			name:      "unknown model returns false",
			model:     "claude-unknown-99",
			wantRate:  pricing.Rate{},
			wantFound: false,
		},
		{
			name:      "empty string returns false",
			model:     "",
			wantRate:  pricing.Rate{},
			wantFound: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := pricing.RateFor(table, tt.model)
			if ok != tt.wantFound {
				t.Errorf("RateFor(%q): found=%v, want found=%v", tt.model, ok, tt.wantFound)
			}
			if got != tt.wantRate {
				t.Errorf("RateFor(%q): rate=%v, want %v", tt.model, got, tt.wantRate)
			}
		})
	}
}

// TestRateFor_NoShorterMatchesLonger ensures a shorter table key never matches
// a longer model ID (prefix matching is forbidden by the design).
func TestRateFor_NoShorterMatchesLonger(t *testing.T) {
	t.Parallel()

	// Table has "claude-opus-4" (short); model is "claude-opus-4-8" (longer).
	table := pricing.Table{
		"claude-opus-4": {Input: 5, Output: 25},
	}

	_, ok := pricing.RateFor(table, "claude-opus-4-8")
	if ok {
		t.Errorf("RateFor: shorter key %q matched longer model %q — shorter-matches-longer is forbidden",
			"claude-opus-4", "claude-opus-4-8")
	}
}

// TestLoadOverride_MissingFile asserts that a missing override file is not an error
// and that the base table is returned unchanged.
func TestLoadOverride_MissingFile(t *testing.T) {
	t.Parallel()

	base := pricing.Table{
		"claude-opus-4-8": {Input: 5, Output: 25},
	}

	result, err := pricing.LoadOverride("/nonexistent/clauchy/pricing.json", base)
	if err != nil {
		t.Fatalf("LoadOverride with missing file should not error: %v", err)
	}
	if len(result) != len(base) {
		t.Errorf("result len=%d, want %d (base unchanged)", len(result), len(base))
	}
	if r := result["claude-opus-4-8"]; r.Input != 5 {
		t.Errorf("base entry mutated: Input=%v, want 5", r.Input)
	}
}

// TestLoadOverride_MergesOverBase asserts that override entries replace base entries
// while non-overridden entries remain unchanged.
func TestLoadOverride_MergesOverBase(t *testing.T) {
	t.Parallel()

	base := pricing.Table{
		"claude-opus-4-8":   {Input: 5, Output: 25},
		"claude-sonnet-4-6": {Input: 3, Output: 15},
	}

	dir := t.TempDir()
	overrideFile := filepath.Join(dir, "pricing.json")

	// Override only claude-opus-4-8 with new rates.
	overrideData := map[string]pricing.Rate{
		"claude-opus-4-8": {Input: 4.5, Output: 22},
	}
	b, err := json.Marshal(overrideData)
	if err != nil {
		t.Fatalf("marshal override: %v", err)
	}
	if err := os.WriteFile(overrideFile, b, 0o644); err != nil {
		t.Fatalf("write override file: %v", err)
	}

	result, err := pricing.LoadOverride(overrideFile, base)
	if err != nil {
		t.Fatalf("LoadOverride: %v", err)
	}

	// Overridden entry uses new rates.
	opus, ok := result["claude-opus-4-8"]
	if !ok {
		t.Fatal("claude-opus-4-8 missing from result")
	}
	if opus.Input != 4.5 {
		t.Errorf("overridden Input = %v, want 4.5", opus.Input)
	}
	if opus.Output != 22 {
		t.Errorf("overridden Output = %v, want 22", opus.Output)
	}

	// Non-overridden entry unchanged.
	sonnet, ok := result["claude-sonnet-4-6"]
	if !ok {
		t.Fatal("claude-sonnet-4-6 missing from result")
	}
	if sonnet.Input != 3 {
		t.Errorf("non-overridden Input = %v, want 3", sonnet.Input)
	}
	if sonnet.Output != 15 {
		t.Errorf("non-overridden Output = %v, want 15", sonnet.Output)
	}
}
