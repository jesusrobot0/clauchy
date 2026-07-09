package waybar_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"clauchy/internal/limits"
	"clauchy/internal/oauth"
	"clauchy/ui/waybar"
)

var (
	// fixed "now" for deterministic tooltip rendering
	fixedNow = time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
)

// makeUsage builds a non-stale Usage with the given 5h and 7d utilization values.
// ResetsAt is set relative to fixedNow for predictable tooltip strings.
func makeUsage(fiveHourPct, sevenDayPct float64) limits.Usage {
	return limits.Usage{
		FiveHour: limits.Window{
			Utilization: fiveHourPct,
			ResetsAt:    fixedNow.Add(90 * time.Minute), // 1h 30m from now
		},
		SevenDay: limits.Window{
			Utilization: sevenDayPct,
			ResetsAt:    fixedNow.Add(76 * time.Hour), // 3d 4h from now
		},
		CachedAt: fixedNow.Add(-10 * time.Second),
		Stale:    false,
	}
}

// --- JSON contract -----------------------------------------------------------

// TestOutput_AlwaysThreeKeys verifies that Render always emits exactly the
// keys "text", "tooltip", and "class" — even when values are empty strings.
// The spec mandates NO omitempty; Waybar ignores class/tooltip when missing.
func TestOutput_AlwaysThreeKeys(t *testing.T) {
	cases := []struct {
		name string
		u    limits.Usage
		err  error
	}{
		{"normal", makeUsage(42, 18), nil},
		{"no_creds", limits.Usage{}, oauth.ErrNoCredentials},
		{"loading", limits.Usage{}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := waybar.Render(tc.u, tc.err, fixedNow)
			b, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}

			var m map[string]json.RawMessage
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}

			for _, key := range []string{"text", "tooltip", "class"} {
				if _, ok := m[key]; !ok {
					t.Errorf("key %q missing from JSON output %s", key, b)
				}
			}
			if len(m) != 3 {
				t.Errorf("expected exactly 3 JSON keys, got %d: %s", len(m), b)
			}
		})
	}
}

// --- Error states ------------------------------------------------------------

func TestRender_NoCredentials_Critical(t *testing.T) {
	out := waybar.Render(limits.Usage{}, oauth.ErrNoCredentials, fixedNow)
	if out.Class != "critical" {
		t.Errorf("class = %q, want %q", out.Class, "critical")
	}
	if !strings.Contains(out.Tooltip, "Run claude to log in") {
		t.Errorf("tooltip = %q, want it to contain \"Run claude to log in\"", out.Tooltip)
	}
	if out.Text != " " {
		t.Errorf("text = %q, want \" \" (single space; icon painted by CSS background-image)", out.Text)
	}
}

func TestRender_InvalidCredentials_Critical(t *testing.T) {
	out := waybar.Render(limits.Usage{}, oauth.ErrInvalidCredentials, fixedNow)
	if out.Class != "critical" {
		t.Errorf("class = %q, want %q", out.Class, "critical")
	}
	if !strings.Contains(out.Tooltip, "Run claude to log in") {
		t.Errorf("tooltip = %q, want it to contain \"Run claude to log in\"", out.Tooltip)
	}
}

// TestRender_RefreshRejected_NoCacheOver7d verifies the persistent-rejection path:
// >7d stale + ErrRefreshRejected → re-auth guidance, class critical.
func TestRender_RefreshRejected_NoCacheOver7d(t *testing.T) {
	out := waybar.Render(limits.Usage{}, oauth.ErrRefreshRejected, fixedNow)
	if out.Class != "critical" {
		t.Errorf("class = %q, want %q", out.Class, "critical")
	}
	if !strings.Contains(out.Tooltip, "Run claude to log in") {
		t.Errorf("tooltip = %q, want it to contain \"Run claude to log in\"", out.Tooltip)
	}
}

// TestRender_OtherError_Loading verifies that an unexpected error falls back
// to the Loading… / low state.
func TestRender_OtherError_Loading(t *testing.T) {
	out := waybar.Render(limits.Usage{}, errors.New("some unexpected error"), fixedNow)
	if out.Class != "low" {
		t.Errorf("class = %q, want %q", out.Class, "low")
	}
	if !strings.Contains(out.Tooltip, "Loading") {
		t.Errorf("tooltip = %q, want it to contain \"Loading\"", out.Tooltip)
	}
}

// TestRender_ZeroUsage_NoError_Loading covers the ErrTransient + no-cache path:
// limits.Cached returns (zero Usage, nil) when there is no stale data.
func TestRender_ZeroUsage_NoError_Loading(t *testing.T) {
	out := waybar.Render(limits.Usage{}, nil, fixedNow)
	if out.Class != "low" {
		t.Errorf("class = %q, want %q", out.Class, "low")
	}
	if !strings.Contains(out.Tooltip, "Loading") {
		t.Errorf("tooltip = %q, want it to contain \"Loading\"", out.Tooltip)
	}
}

// --- Class thresholds --------------------------------------------------------

func TestRender_ClassThresholds(t *testing.T) {
	cases := []struct {
		pct   float64
		class string
	}{
		{0, "low"},
		{49.9, "low"},
		{50, "mid"},
		{74.9, "mid"},
		{75, "high"},
		{89.9, "high"},
		{90, "critical"},
		{100, "critical"},
	}

	for _, tc := range cases {
		u := makeUsage(tc.pct, 0)
		out := waybar.Render(u, nil, fixedNow)
		if out.Class != tc.class {
			t.Errorf("pct=%.1f → class=%q, want %q", tc.pct, out.Class, tc.class)
		}
	}
}

// --- Tooltip content ---------------------------------------------------------

func TestRender_NormalTooltip_TwoLines(t *testing.T) {
	u := makeUsage(42, 18)
	out := waybar.Render(u, nil, fixedNow)

	// Must contain both required lines
	if !strings.Contains(out.Tooltip, "<b>Session (5h)</b>") {
		t.Errorf("tooltip missing session line: %q", out.Tooltip)
	}
	if !strings.Contains(out.Tooltip, "<b>Weekly</b>") {
		t.Errorf("tooltip missing weekly line: %q", out.Tooltip)
	}
	// Must contain percentages
	if !strings.Contains(out.Tooltip, "42%") {
		t.Errorf("tooltip missing 5h pct: %q", out.Tooltip)
	}
	if !strings.Contains(out.Tooltip, "18%") {
		t.Errorf("tooltip missing 7d pct: %q", out.Tooltip)
	}
	// Must contain reset times
	if !strings.Contains(out.Tooltip, "resets in") {
		t.Errorf("tooltip missing 'resets in': %q", out.Tooltip)
	}
	// No stale suffix when not stale
	if strings.Contains(out.Tooltip, "cached") {
		t.Errorf("tooltip should not contain 'cached' for fresh data: %q", out.Tooltip)
	}
}

func TestRender_SessionResetFormat_HhMm(t *testing.T) {
	// 1h 30m from fixedNow
	u := makeUsage(42, 18)
	out := waybar.Render(u, nil, fixedNow)
	if !strings.Contains(out.Tooltip, "1h 30m") {
		t.Errorf("session reset not in Hh Mm format: %q", out.Tooltip)
	}
}

func TestRender_WeeklyResetFormat_DdHh(t *testing.T) {
	// 3d 4h from fixedNow (76h = 3d 4h)
	u := makeUsage(42, 18)
	out := waybar.Render(u, nil, fixedNow)
	if !strings.Contains(out.Tooltip, "3d 4h") {
		t.Errorf("weekly reset not in Dd Hh format: %q", out.Tooltip)
	}
}

func TestRender_PerModelLines_WhenModelsPresent(t *testing.T) {
	u := makeUsage(42, 18)
	u.Models = []limits.ModelLimit{
		{Name: "claude-opus-4-8", Utilization: 30, ResetsAt: fixedNow.Add(76 * time.Hour)},
	}
	out := waybar.Render(u, nil, fixedNow)

	if !strings.Contains(out.Tooltip, "claude-opus-4-8") {
		t.Errorf("tooltip missing per-model line: %q", out.Tooltip)
	}
	if !strings.Contains(out.Tooltip, "(weekly)") {
		t.Errorf("tooltip missing '(weekly)' label in per-model line: %q", out.Tooltip)
	}
	if !strings.Contains(out.Tooltip, "30%") {
		t.Errorf("tooltip missing per-model pct: %q", out.Tooltip)
	}
}

func TestRender_NoPerModelLines_WhenModelsEmpty(t *testing.T) {
	u := makeUsage(42, 18)
	// Models is nil / empty
	out := waybar.Render(u, nil, fixedNow)

	if strings.Contains(out.Tooltip, "(weekly)") && !strings.Contains(out.Tooltip, "Weekly") {
		// The weekly line itself has no "weekly" in our format, but per-model does
		// so just check that we don't get model names
	}
	// The tooltip should only have two non-suffix lines when Models is empty
	lines := strings.Split(strings.TrimSpace(out.Tooltip), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 tooltip lines with no models, got %d: %q", len(lines), out.Tooltip)
	}
}

// TestRender_StaleSuffix verifies the stale cache tooltip suffix.
func TestRender_StaleSuffix(t *testing.T) {
	staleTime := fixedNow.Add(-5 * time.Minute) // cached 5 min ago
	u := limits.Usage{
		FiveHour: limits.Window{
			Utilization: 42,
			ResetsAt:    fixedNow.Add(90 * time.Minute),
		},
		SevenDay: limits.Window{
			Utilization: 18,
			ResetsAt:    fixedNow.Add(76 * time.Hour),
		},
		CachedAt: staleTime,
		Stale:    true,
	}
	out := waybar.Render(u, nil, fixedNow)

	if !strings.Contains(out.Tooltip, "cached 5 min ago") {
		t.Errorf("stale tooltip missing '(cached 5 min ago)': %q", out.Tooltip)
	}
	if !strings.Contains(out.Tooltip, "<i>") {
		t.Errorf("stale suffix not wrapped in <i>: %q", out.Tooltip)
	}
}

// TestRender_StaleValues_NotAnErrorState verifies that stale data renders with
// real utilization values (not "Loading…") and a class from the utilization.
func TestRender_StaleValues_NotAnErrorState(t *testing.T) {
	u := limits.Usage{
		FiveHour: limits.Window{Utilization: 85, ResetsAt: fixedNow.Add(30 * time.Minute)},
		SevenDay: limits.Window{Utilization: 60, ResetsAt: fixedNow.Add(48 * time.Hour)},
		CachedAt: fixedNow.Add(-3 * time.Minute),
		Stale:    true,
	}
	out := waybar.Render(u, nil, fixedNow)

	if out.Class != "high" { // 85% → high
		t.Errorf("class = %q, want %q", out.Class, "high")
	}
	if strings.Contains(out.Tooltip, "Loading") {
		t.Errorf("stale render should show real values, not Loading…: %q", out.Tooltip)
	}
	if !strings.Contains(out.Tooltip, "85%") {
		t.Errorf("stale render missing utilization pct: %q", out.Tooltip)
	}
}

// TestRender_PangoEscaping verifies that model display names containing Pango
// special characters (&, <, >) are escaped before being embedded in the tooltip.
// API-provided strings must never inject markup into the Pango string.
func TestRender_PangoEscaping(t *testing.T) {
	u := makeUsage(42, 18)
	u.Models = []limits.ModelLimit{
		{Name: "A&B <x>", Utilization: 50, ResetsAt: fixedNow.Add(76 * time.Hour)},
	}
	out := waybar.Render(u, nil, fixedNow)

	// Raw special characters must NOT appear (they would inject Pango markup).
	if strings.Contains(out.Tooltip, "A&B <x>") {
		t.Errorf("tooltip contains raw special chars — Pango injection possible: %q", out.Tooltip)
	}
	// Escaped form must appear (html.EscapeString convention: & → &amp;, < → &lt;, > → &gt;).
	if !strings.Contains(out.Tooltip, "A&amp;B &lt;x&gt;") {
		t.Errorf("tooltip missing Pango-escaped name %q: %q", "A&amp;B &lt;x&gt;", out.Tooltip)
	}
}

// TestRender_Text_SingleSpace verifies that Text is always a single space " ".
// The module box must exist for CSS background-image to paint the Claude SVG;
// the legacy glyph (󰚩) has been replaced by the background-image mechanism.
func TestRender_Text_SingleSpace(t *testing.T) {
	cases := []struct {
		name string
		u    limits.Usage
		err  error
	}{
		{"normal", makeUsage(42, 18), nil},
		{"no_creds", limits.Usage{}, oauth.ErrNoCredentials},
		{"loading", limits.Usage{}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := waybar.Render(tc.u, tc.err, fixedNow)
			if out.Text != " " {
				t.Errorf("Text = %q, want \" \" (single space)", out.Text)
			}
		})
	}
}
