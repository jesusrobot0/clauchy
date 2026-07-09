// Package waybar builds the JSON payload that Waybar's custom module protocol
// expects: exactly three keys (text, tooltip, class) with no omitempty.
//
// Render is the sole entry point. It maps a (limits.Usage, error) pair from
// limits.Cached to a safe, always-valid Output — even partial or error states
// produce a well-formed JSON line so Waybar never crashes.
//
// Tooltip format (Pango):
//
//	Line 1: <b>Session (5h)</b>: N%  ·  resets in Hh Mm
//	Line 2: <b>Weekly</b>: N%  ·  resets in Dd Hh
//	Line 3+: <b>{Model} (weekly)</b>: N%  ·  resets in Dd Hh (when limits[] non-empty)
//	Suffix (stale): \n<i>(cached N min ago)</i>
package waybar

import (
	"errors"
	"fmt"
	"html"
	"math"
	"strings"
	"time"

	"clauchy/internal/limits"
	"clauchy/internal/oauth"
	"clauchy/internal/severity"
)

// Output is the JSON payload for a Waybar custom module with return-type: json.
// All three fields are always present (no omitempty) per spec.
type Output struct {
	Text    string `json:"text"`
	Tooltip string `json:"tooltip"`
	Class   string `json:"class"`
}

// moduleText is the value emitted as the Waybar "text" field.
// A single space makes the module box exist (Waybar renders it) without drawing
// a glyph; the Claude SVG icon is painted by CSS background-image instead.
// The legacy glyph (󰚩) has been replaced by this mechanism.
const moduleText = " "

// Render produces a Waybar Output from a limits.Cached result.
// It never returns an empty Output — every error state maps to a valid icon +
// message + severity class so Waybar never shows a blank or crashes.
func Render(u limits.Usage, err error, now time.Time) Output {
	// Error states: no credentials, persistent refresh rejection, or other errors.
	if err != nil {
		if errors.Is(err, oauth.ErrNoCredentials) ||
			errors.Is(err, oauth.ErrInvalidCredentials) ||
			errors.Is(err, oauth.ErrRefreshRejected) {
			return Output{
				Text:    moduleText,
				Tooltip: "Run claude to log in",
				Class:   "critical",
			}
		}
		// Any other unexpected error → Loading…
		return Output{
			Text:    moduleText,
			Tooltip: "Loading…",
			Class:   "low",
		}
	}

	// Zero usage (no cache available, ErrTransient with no stale data) → Loading…
	if u.CachedAt.IsZero() {
		return Output{
			Text:    moduleText,
			Tooltip: "Loading…",
			Class:   "low",
		}
	}

	// Normal or stale: build the Pango tooltip and classify from 5h utilization.
	tooltip := buildTooltip(u, now)
	cls := severityClass(severity.Classify(u.FiveHour.Utilization))

	return Output{
		Text:    moduleText,
		Tooltip: tooltip,
		Class:   cls,
	}
}

// buildTooltip assembles the multi-line Pango tooltip from a usage value.
func buildTooltip(u limits.Usage, now time.Time) string {
	var sb strings.Builder

	// Line 1: Session (5h) — format as Hh Mm (the window is at most 5h)
	sessionReset := u.FiveHour.ResetsAt.Sub(now)
	sb.WriteString(fmt.Sprintf(
		"<b>Session (5h)</b>: %d%%  ·  resets in %s",
		pct(u.FiveHour.Utilization),
		fmtHhMm(sessionReset),
	))

	// Line 2: Weekly — format as Dd Hh
	weeklyReset := u.SevenDay.ResetsAt.Sub(now)
	sb.WriteString(fmt.Sprintf(
		"\n<b>Weekly</b>: %d%%  ·  resets in %s",
		pct(u.SevenDay.Utilization),
		fmtDdHh(weeklyReset),
	))

	// Lines 3+: per-model limits (when non-empty).
	// Model display names come from the API and must be Pango-escaped before
	// embedding in the tooltip. html.EscapeString covers &, <, > (same escaping
	// Pango requires for attribute values and text content in markup strings).
	for _, m := range u.Models {
		modelReset := m.ResetsAt.Sub(now)
		sb.WriteString(fmt.Sprintf(
			"\n<b>%s (weekly)</b>: %d%%  ·  resets in %s",
			html.EscapeString(m.Name),
			pct(m.Utilization),
			fmtDdHh(modelReset),
		))
	}

	// Stale suffix
	if u.Stale {
		ageMin := int(math.Round(now.Sub(u.CachedAt).Minutes()))
		sb.WriteString(fmt.Sprintf("\n<i>(cached %d min ago)</i>", ageMin))
	}

	return sb.String()
}

// pct converts a float64 percentage to the nearest integer for display.
func pct(f float64) int {
	return int(math.Round(f))
}

// fmtHhMm formats a duration as "Hh Mm" (used for the 5-hour session window).
// Negative durations (reset already passed) are shown as "0h 0m".
func fmtHhMm(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh %dm", h, m)
}

// fmtDdHh formats a duration as "Dd Hh" (used for 7-day and per-model windows).
// Negative durations are shown as "0d 0h".
func fmtDdHh(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalH := int(d.Hours())
	days := totalH / 24
	hours := totalH % 24
	return fmt.Sprintf("%dd %dh", days, hours)
}

// severityClass maps a Severity to the Waybar CSS class string.
func severityClass(s severity.Severity) string {
	switch s {
	case severity.Low:
		return "low"
	case severity.Mid:
		return "mid"
	case severity.High:
		return "high"
	default:
		return "critical"
	}
}
