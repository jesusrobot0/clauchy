// Package theme provides the single visual palette shared by both the Waybar
// renderer and the Bubbletea dashboard.
//
// Key design decision (ADR-7): severity colors are SEMANTICALLY FIXED using
// Catppuccin Mocha hex values — the same values the install package writes into
// the Waybar CSS block. They must never be sourced from desktop @theme variables
// so that a theme swap can never mute a critical alarm.
//
// The cross-consistency invariant (critical for icon coloring) is enforced by a
// test-only import of internal/install in theme_test.go.
package theme

import "github.com/jesusrobot0/clauchy/internal/severity"

// Palette holds the complete visual configuration for clauchy's renderers.
// All fields are unexported; callers use the accessor methods.
type Palette struct {
	lowHex      string
	midHex      string
	highHex     string
	criticalHex string
	icon        string
}

// Default returns the production palette using Catppuccin Mocha colors.
// These hex values MUST stay in sync with the CSS block in internal/install
// (enforced by TestCrossConsistency_ThemeMatchesInstallCSS in theme_test.go).
func Default() Palette {
	return Palette{
		lowHex:      "#a6e3a1", // Catppuccin Mocha Green
		midHex:      "#f9e2af", // Catppuccin Mocha Yellow
		highHex:     "#fab387", // Catppuccin Mocha Peach
		criticalHex: "#f38ba8", // Catppuccin Mocha Red
		icon:        "󰚩",       // Claude Nerd Font glyph (nf-md-robot)
	}
}

// Hex returns the #rrggbb hex color for the given severity level.
func (p Palette) Hex(s severity.Severity) string {
	switch s {
	case severity.Low:
		return p.lowHex
	case severity.Mid:
		return p.midHex
	case severity.High:
		return p.highHex
	default: // severity.Critical and any future levels default to critical red
		return p.criticalHex
	}
}

// Icon returns the glyph used as the Waybar text field and dashboard header.
func (p Palette) Icon() string {
	return p.icon
}
