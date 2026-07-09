package waybar_test

// Feature A: Output.Text must be a single space " " so Waybar renders a
// dimensionless box that CSS background-image paints the icon into.
//
// Feature B: Full tooltip string golden tests pin the exact byte content of the
// tooltip for each significant state. This prevents accidental drift of separator
// strings (e.g. "  ·  ") or label copy.

import (
	"testing"
	"time"

	"github.com/jesusrobot0/clauchy/internal/limits"
	"github.com/jesusrobot0/clauchy/internal/oauth"
	"github.com/jesusrobot0/clauchy/internal/status"
	"github.com/jesusrobot0/clauchy/ui/waybar"
)

var (
	textTestNow = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	// tooltipNow is used for tooltip golden tests so expected strings can be
	// computed precisely. It matches the fixedNow in waybar_test.go.
	tooltipNow = time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
)

// TestRender_Text_IsSpace verifies that Output.Text is always a single space
// regardless of usage state. The module box must exist for CSS background-image
// to paint the icon; the legacy glyph is replaced by the background image.
func TestRender_Text_IsSpace(t *testing.T) {
	cases := []struct {
		name string
		u    limits.Usage
		err  error
	}{
		{
			name: "normal_low",
			u: limits.Usage{
				FiveHour: limits.Window{Utilization: 20, ResetsAt: textTestNow.Add(90 * time.Minute)},
				SevenDay: limits.Window{Utilization: 10, ResetsAt: textTestNow.Add(76 * time.Hour)},
				CachedAt: textTestNow.Add(-5 * time.Second),
			},
		},
		{
			name: "normal_critical",
			u: limits.Usage{
				FiveHour: limits.Window{Utilization: 95, ResetsAt: textTestNow.Add(10 * time.Minute)},
				SevenDay: limits.Window{Utilization: 80, ResetsAt: textTestNow.Add(24 * time.Hour)},
				CachedAt: textTestNow.Add(-5 * time.Second),
			},
		},
		{
			name: "no_credentials",
			u:    limits.Usage{},
			err:  oauth.ErrNoCredentials,
		},
		{
			name: "loading_no_cache",
			u:    limits.Usage{},
		},
		{
			name: "stale",
			u: limits.Usage{
				FiveHour: limits.Window{Utilization: 50, ResetsAt: textTestNow.Add(60 * time.Minute)},
				SevenDay: limits.Window{Utilization: 30, ResetsAt: textTestNow.Add(48 * time.Hour)},
				CachedAt: textTestNow.Add(-5 * time.Minute),
				Stale:    true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := waybar.Render(tc.u, tc.err, textTestNow, status.Status{})
			if out.Text != " " {
				t.Errorf("Text = %q, want \" \" (single space); the icon is now painted by CSS background-image", out.Text)
			}
		})
	}
}

// ─── Tooltip full-string golden tests (Fix 5) ────────────────────────────────
//
// These tests pin the EXACT tooltip bytes for each significant rendering state.
// Any change to separator strings ("  ·  "), label copy ("resets in", "(weekly)"),
// or Pango tags (<b>, <i>) will cause a failure here, making drift impossible to
// miss.
//
// Reference time: tooltipNow = 2026-07-07 12:00:00 UTC
//
//   FiveHour.ResetsAt = tooltipNow + 90min → sessionReset = 1h 30m
//   SevenDay.ResetsAt = tooltipNow + 76h   → weeklyReset  = 3d 4h  (76/24=3 days, 76%24=4 hours)
//   stale CachedAt    = tooltipNow - 5min  → ageMin = 5

func makeTooltipUsage(fiveHourPct, sevenDayPct float64) limits.Usage {
	return limits.Usage{
		FiveHour: limits.Window{
			Utilization: fiveHourPct,
			ResetsAt:    tooltipNow.Add(90 * time.Minute),
		},
		SevenDay: limits.Window{
			Utilization: sevenDayPct,
			ResetsAt:    tooltipNow.Add(76 * time.Hour),
		},
		CachedAt: tooltipNow.Add(-10 * time.Second),
		Stale:    false,
	}
}

// TestTooltip_ExactString is a table-driven test pinning the full tooltip bytes
// for four distinct states. Any drift in separators or copy fails immediately.
func TestTooltip_ExactString(t *testing.T) {
	cases := []struct {
		name        string
		u           limits.Usage
		err         error
		wantTooltip string
	}{
		{
			name: "normal_session_weekly_model",
			u: func() limits.Usage {
				u := makeTooltipUsage(42, 18)
				u.Models = []limits.ModelLimit{
					{Name: "claude-opus-4-8", Utilization: 30, ResetsAt: tooltipNow.Add(76 * time.Hour)},
				}
				return u
			}(),
			err: nil,
			wantTooltip: "<b>Session (5h)</b>: 42%  ·  resets in 1h 30m" +
				"\n<b>Weekly</b>: 18%  ·  resets in 3d 4h" +
				"\n<b>claude-opus-4-8 (weekly)</b>: 30%  ·  resets in 3d 4h",
		},
		{
			name: "stale_suffix",
			u: func() limits.Usage {
				u := makeTooltipUsage(42, 18)
				u.CachedAt = tooltipNow.Add(-5 * time.Minute)
				u.Stale = true
				return u
			}(),
			err: nil,
			wantTooltip: "<b>Session (5h)</b>: 42%  ·  resets in 1h 30m" +
				"\n<b>Weekly</b>: 18%  ·  resets in 3d 4h" +
				"\n<i>(cached 5 min ago)</i>",
		},
		{
			name:        "missing_credentials",
			u:           limits.Usage{},
			err:         oauth.ErrNoCredentials,
			wantTooltip: "Run claude to log in",
		},
		{
			name:        "loading",
			u:           limits.Usage{}, // CachedAt.IsZero() → Loading…
			err:         nil,
			wantTooltip: "Loading…", // "Loading…" (Unicode ellipsis U+2026)
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := waybar.Render(tc.u, tc.err, tooltipNow, status.Status{})
			if out.Tooltip != tc.wantTooltip {
				t.Errorf("Tooltip mismatch for %q:\ngot:  %q\nwant: %q", tc.name, out.Tooltip, tc.wantTooltip)
			}
		})
	}
}
