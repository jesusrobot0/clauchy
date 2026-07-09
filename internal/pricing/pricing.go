// Package pricing provides the embedded Claude model rate table, a cost formula,
// and an optional per-model override loaded from ~/.config/clauchy/pricing.json.
//
// Rate lookup uses chained normalization to handle model IDs that include an
// 8-digit date suffix (e.g. -20260101), a trailing bracketed suffix (e.g. [1m]),
// or both. A shorter table key never matches a longer model ID.
package pricing

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
)

// PricingDate is the publication date of the embedded rate table (ISO 8601).
const PricingDate = "2026-07-07"

// Rate holds the USD input and output prices per 1 million tokens for one model.
type Rate struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// Table maps canonical model IDs (without date or bracket suffixes) to their Rate.
type Table map[string]Rate

// Usage holds the per-message token counts as reported by the Claude API.
// Each field maps to a distinct cost bucket in the Cost formula.
type Usage struct {
	Input        int // plain prompt tokens
	CacheWrite1h int // ephemeral 1-hour cache-write tokens (×2.0 × Ri)
	CacheWrite5m int // ephemeral 5-minute cache-write tokens (×1.25 × Ri)
	CacheRead    int // cache-read tokens (×0.1 × Ri)
	Output       int // output / completion tokens (× Ro)
}

// Cost returns the total USD cost for u at rate r.
// The multipliers (×2.0, ×1.25, ×0.1) apply to the respective cache token BUCKETS
// (each multiplied by r.Input), not to the plain input count.
func Cost(u Usage, r Rate) float64 {
	return (float64(u.Input)*r.Input +
		float64(u.CacheWrite1h)*r.Input*2.0 +
		float64(u.CacheWrite5m)*r.Input*1.25 +
		float64(u.CacheRead)*r.Input*0.1 +
		float64(u.Output)*r.Output) / 1_000_000
}

// Builtin returns the embedded pricing table for July 2026 Claude model rates.
// Keys are canonical model IDs (without date or bracket suffixes).
func Builtin() Table {
	return Table{
		// Opus 4.x — $5 input / $25 output per million tokens
		"claude-opus-4-5": {Input: 5, Output: 25},
		"claude-opus-4-8": {Input: 5, Output: 25},

		// Sonnet 4.x — $3 input / $15 output per million tokens
		"claude-sonnet-4-5": {Input: 3, Output: 15},
		"claude-sonnet-4-6": {Input: 3, Output: 15},

		// Sonnet 5 — $2 input / $10 output (intro pricing valid until 2026-08-31)
		"claude-sonnet-5": {Input: 2, Output: 10},

		// Haiku 4.5 — $1 input / $5 output per million tokens
		"claude-haiku-4-5": {Input: 1, Output: 5},

		// Fable 5 / Mythos 5 — $10 input / $50 output per million tokens
		"claude-fable-5":  {Input: 10, Output: 50},
		"claude-mythos-5": {Input: 10, Output: 50},
	}
}

// dateSuffixRe matches a trailing 8-digit date segment, e.g. "-20260101".
var dateSuffixRe = regexp.MustCompile(`-\d{8}$`)

// bracketSuffixRe matches a trailing bracketed segment, e.g. "[1m]".
var bracketSuffixRe = regexp.MustCompile(`\[[^\]]+\]$`)

// RateFor looks up the Rate for model in t using chained normalization:
//  1. Exact match.
//  2. Strip trailing 8-digit date suffix, retry.
//  3. Strip trailing bracket suffix, retry.
//  4. Strip bracket suffix then date suffix (chained), retry.
//
// A shorter table key never matches a longer model ID because only the specific
// patterns above are stripped — no arbitrary prefix truncation is performed.
// Returns (Rate{}, false) for unrecognized models.
func RateFor(t Table, model string) (Rate, bool) {
	// 1. Exact match.
	if r, ok := t[model]; ok {
		return r, true
	}

	// 2. Strip 8-digit date suffix (e.g. "claude-opus-4-8-20260101" → "claude-opus-4-8").
	noDate := dateSuffixRe.ReplaceAllString(model, "")
	if noDate != model {
		if r, ok := t[noDate]; ok {
			return r, true
		}
	}

	// 3. Strip trailing bracket suffix (e.g. "claude-opus-4-8[1m]" → "claude-opus-4-8").
	noBracket := bracketSuffixRe.ReplaceAllString(model, "")
	if noBracket != model {
		if r, ok := t[noBracket]; ok {
			return r, true
		}
	}

	// 4. Chained: strip bracket first, then strip date from the result.
	// Handles "claude-opus-4-8-20260101[1m]" → "claude-opus-4-8-20260101" → "claude-opus-4-8".
	if noBracket != model {
		noDateNoBracket := dateSuffixRe.ReplaceAllString(noBracket, "")
		if noDateNoBracket != noBracket {
			if r, ok := t[noDateNoBracket]; ok {
				return r, true
			}
		}
	}

	return Rate{}, false
}

// LoadOverride reads a JSON file at path and merges its entries over base,
// returning the merged table. The file must be a JSON object mapping model IDs
// to {"input": N, "output": N} objects — the same shape as the builtin table.
// If the file does not exist, base is returned unchanged and no error is raised.
func LoadOverride(path string, base Table) (Table, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return base, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pricing override read %s: %w", path, err)
	}

	var overrides Table
	if err := json.Unmarshal(data, &overrides); err != nil {
		return nil, fmt.Errorf("pricing override parse %s: %w", path, err)
	}

	// Build result from base, then apply overrides.
	result := make(Table, len(base))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range overrides {
		result[k] = v
	}
	return result, nil
}
