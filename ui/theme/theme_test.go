package theme_test

import (
	"strings"
	"testing"

	"github.com/jesusrobot0/clauchy/internal/install"
	"github.com/jesusrobot0/clauchy/internal/severity"
	"github.com/jesusrobot0/clauchy/ui/theme"
)

func TestDefault_HasDistinctHexPerSeverity(t *testing.T) {
	p := theme.Default()

	got := []struct {
		name string
		hex  string
	}{
		{"Low", p.Hex(severity.Low)},
		{"Mid", p.Hex(severity.Mid)},
		{"High", p.Hex(severity.High)},
		{"Critical", p.Hex(severity.Critical)},
	}

	for _, g := range got {
		if !strings.HasPrefix(g.hex, "#") || len(g.hex) != 7 {
			t.Errorf("Hex(%s) = %q is not a valid #rrggbb color", g.name, g.hex)
		}
	}

	// all four must be distinct
	for i := range got {
		for j := range got {
			if i != j && got[i].hex == got[j].hex {
				t.Errorf("Hex(%s) == Hex(%s) = %q (must be distinct)", got[i].name, got[j].name, got[i].hex)
			}
		}
	}
}

func TestDefault_IconGlyph(t *testing.T) {
	p := theme.Default()
	icon := p.Icon()
	if icon == "" {
		t.Fatal("Icon() returned empty string")
	}
	// Must be the Claude Nerd Font glyph used in the waybar text field.
	const wantIcon = "󰚩"
	if icon != wantIcon {
		t.Errorf("Icon() = %q, want %q", icon, wantIcon)
	}
}

// TestCrossConsistency_ThemeMatchesInstallCSS verifies that theme.Default()'s
// hex colors are byte-identical to the ones used in install's CSS color: overrides.
// The cross-package import is test-only; it prevents the two sources of truth
// from drifting silently (ADR-7).
//
// Change 17 — theme-adaptive symbolic icon: the CSS block now uses
// -gtk-recolor(url("claude-symbolic.svg")) so the icon inherits the theme
// foreground for the low/calm state. Severity states (mid/high/critical) override
// `color:` with the Catppuccin Mocha hex values — the same values used by the
// dashboard severity bars. install.IconSeverityColors() is the single source of
// truth for these three hex values within the install package.
func TestCrossConsistency_ThemeMatchesInstallCSS(t *testing.T) {
	p := theme.Default()
	iconColors := install.IconSeverityColors()

	cases := []struct {
		sev  severity.Severity
		name string
		want string
	}{
		// Low: inherits theme foreground via -gtk-recolor (no color: override).
		// Cross-consistency only applies to mid/high/critical where both the theme
		// severity bars and the CSS color: override must use the same hex.
		{severity.Mid, "mid", "#f9e2af"},
		{severity.High, "high", "#fab387"},
		{severity.Critical, "critical", "#f38ba8"},
	}

	for _, tc := range cases {
		got := p.Hex(tc.sev)
		if got != tc.want {
			t.Errorf("theme.Default().Hex(%s) = %q, want %q", tc.name, got, tc.want)
		}
		iconHex, ok := iconColors[tc.name]
		if !ok {
			t.Errorf("install.IconSeverityColors() missing key %q", tc.name)
			continue
		}
		if iconHex != tc.want {
			t.Errorf("install.IconSeverityColors()[%q] = %q, want %q (theme and install are inconsistent)", tc.name, iconHex, tc.want)
		}
	}
}

// TestCrossConsistency_IconSeverityColors_HasAllSeverities verifies that
// install.IconSeverityColors() has entries for the three non-low severity levels.
// Change 17: "low" is intentionally absent — with -gtk-recolor the low/calm state
// inherits the theme foreground automatically, so no CSS color: override is needed.
func TestCrossConsistency_IconSeverityColors_HasAllSeverities(t *testing.T) {
	colors := install.IconSeverityColors()
	for _, key := range []string{"mid", "high", "critical"} {
		if v, ok := colors[key]; !ok || v == "" {
			t.Errorf("install.IconSeverityColors() missing or empty key %q", key)
		}
	}
	// "low" must NOT be present.
	if v, ok := colors["low"]; ok {
		t.Errorf("install.IconSeverityColors() must not contain 'low' key (got %q); low inherits theme foreground", v)
	}
}
