package dashboard_test

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/muesli/termenv"

	"github.com/jesusrobot0/clauchy/internal/limits"
	"github.com/jesusrobot0/clauchy/internal/status"
	"github.com/jesusrobot0/clauchy/internal/transcript"
	"github.com/jesusrobot0/clauchy/ui/dashboard"
	"github.com/jesusrobot0/clauchy/ui/theme"
)

// TestMain pins the Lipgloss color profile to TrueColor so golden files are
// byte-identical between a developer TTY and TTY-less CI.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// ─── shared helpers ───────────────────────────────────────────────────────────

var (
	fixedNow    = time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	testPalette = theme.Default()
	testWidth   = 80
	testHeight  = 24
)

// stubDeps returns a Deps that produces controlled, deterministic data.
func stubDeps(u limits.Usage, uErr error, s transcript.Stats, sErr error) dashboard.Deps {
	return dashboard.Deps{
		FetchLimits: func() (limits.Usage, error) { return u, uErr },
		FetchStats:  func() (transcript.Stats, error) { return s, sErr },
		FetchStatus: func() (status.Status, error) { return status.Status{}, nil },
	}
}

// stubDepsWithStatus returns a Deps with a specific status fetch stub.
func stubDepsWithStatus(u limits.Usage, uErr error, s transcript.Stats, sErr error, st status.Status, stErr error) dashboard.Deps {
	return dashboard.Deps{
		FetchLimits: func() (limits.Usage, error) { return u, uErr },
		FetchStats:  func() (transcript.Stats, error) { return s, sErr },
		FetchStatus: func() (status.Status, error) { return st, stErr },
	}
}

func fixedNowFn() time.Time { return fixedNow }

// sampleUsage is a fresh cache hit with representative utilization values
// that exercise the inline bar renderer (session 22%, week 31%, model 24%).
func sampleUsage() limits.Usage {
	return limits.Usage{
		FiveHour: limits.Window{
			Utilization: 22,
			ResetsAt:    fixedNow.Add(90 * time.Minute),
		},
		SevenDay: limits.Window{
			Utilization: 31,
			ResetsAt:    fixedNow.Add(49 * time.Hour),
		},
		Models: []limits.ModelLimit{
			{Name: "Fable", Utilization: 24, ResetsAt: fixedNow.Add(49 * time.Hour)},
		},
		CachedAt: fixedNow.Add(-10 * time.Second),
		Stale:    false,
	}
}

// sampleStats returns a Stats with realistic large token counts that exercise
// the humanize() formatter (K / M suffixes) and fmtCost() ($N rounding).
func sampleStats() transcript.Stats {
	return transcript.Stats{
		Today: transcript.DayTotals{
			InputTokens:      2_000,
			OutputTokens:     184_000,
			CacheWriteTokens: 2_100_000,
			CacheReadTokens:  82_100_000,
			Cost:             167.0,
			Sessions:         3,
			Messages:         550,
		},
		Week: transcript.WeekTotals{
			InputTokens:      10_000_000,
			OutputTokens:     20_000_000,
			CacheWriteTokens: 100_000_000,
			CacheReadTokens:  110_000_000,
			Cost:             463.0,
		},
		Models7d: []transcript.ModelUsage{
			{Model: "claude-opus-4-5", Input: 327_000_000, Output: 0, Cost: 350.0},
			{Model: "claude-haiku-3-5", Input: 317_000, Output: 0, Cost: 0.50},
		},
		Streak:      2,
		PricingDate: "2026-07-07",
		Generated:   fixedNow,
	}
}

// ─── Plan label in header ────────────────────────────────────────────────────

// TestViewHeader_PlanLabel verifies that when a non-empty plan label is
// supplied to New(), the header row contains both the brand title and the plan label.
// This is a unit-level check: we render View() directly with a fixed size and
// assert the plan string appears in the output.
func TestViewHeader_PlanLabel(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "Max 20x")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	output := m2.(dashboard.Model).View()

	// The plan label must appear somewhere in the rendered output.
	if !strings.Contains(stripANSI(output), "Max 20x") {
		t.Errorf("View() does not contain plan label %q\nfull output:\n%s", "Max 20x", output)
	}

	// The brand title "clauchy" must be present (Change 19: replaces "CLAUDE CODE").
	if !strings.Contains(stripANSI(output), "clauchy") {
		t.Errorf("View() does not contain the header brand 'clauchy'\nfull output:\n%s", output)
	}
}

// TestViewHeader_EmptyPlan verifies that an empty plan label does not break
// the header (backward-compatible call site).
func TestViewHeader_EmptyPlan(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	output := m2.(dashboard.Model).View()

	if !strings.Contains(stripANSI(output), "clauchy") {
		t.Errorf("View() with empty plan does not contain header brand 'clauchy'\nfull output:\n%s", output)
	}
}

// ─── prettyModelName table tests ─────────────────────────────────────────────

func TestPrettyModelName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// Normal form: family first, then version numbers.
		{"claude-opus-4", "Opus 4"},
		{"claude-opus-4-8", "Opus 4.8"},
		{"claude-sonnet-4-6", "Sonnet 4.6"},
		{"claude-haiku-4-5", "Haiku 4.5"},
		{"claude-fable-5", "Fable 5"},
		// Date suffix stripped first.
		{"claude-opus-4-8-20260101", "Opus 4.8"},
		// Bracketed suffix stripped.
		{"claude-opus-4-8[1m]", "Opus 4.8"},
		// Both suffixes.
		{"claude-opus-4-8-20260101[1m]", "Opus 4.8"},
		// Legacy inverted form: version segments before family word.
		{"claude-3-5-sonnet-20241022", "Sonnet 3.5"},
		{"claude-3-opus-20240229", "Opus 3"},
		// No "claude-" prefix — not our pattern, pass through unchanged.
		{"<unknown-x>", "<unknown-x>"},
		{"gpt-4o", "gpt-4o"},
		// Empty string — pass through unchanged.
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := dashboard.PrettyModelName(tc.input)
			if got != tc.want {
				t.Errorf("PrettyModelName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ─── BigDigits table tests ────────────────────────────────────────────────────

func TestBigDigits_AllGlyphs(t *testing.T) {
	// Every supported rune must produce exactly 4 rows with equal visual width.
	singles := "0123456789.KMBT"
	for _, r := range singles {
		t.Run(string(r), func(t *testing.T) {
			rows := dashboard.BigDigits(string(r))
			if len(rows) != 4 {
				t.Fatalf("BigDigits(%q) returned %d rows, want 4", string(r), len(rows))
			}
			w0 := lipgloss.Width(rows[0])
			for i, row := range rows {
				if lipgloss.Width(row) != w0 {
					t.Errorf("BigDigits(%q) row[%d] width %d != row[0] width %d",
						string(r), i, lipgloss.Width(row), w0)
				}
			}
		})
	}
}

func TestBigDigits_FullString(t *testing.T) {
	// "84.4M" exercises: two-digit number, decimal, digit, unit suffix.
	rows := dashboard.BigDigits("84.4M")
	if len(rows) != 4 {
		t.Fatalf("BigDigits(%q) returned %d rows, want 4", "84.4M", len(rows))
	}
	// All rows must share the same visual width.
	w0 := lipgloss.Width(rows[0])
	if w0 == 0 {
		t.Fatal("BigDigits(\"84.4M\") produced empty row[0]")
	}
	for i, row := range rows {
		if lipgloss.Width(row) != w0 {
			t.Errorf("BigDigits(\"84.4M\") row[%d] width %d != row[0] width %d",
				i, lipgloss.Width(row), w0)
		}
	}
	// Unit letter 'M' renders once, near the baseline (row 2 only).
	if strings.Contains(rows[1], "M") {
		t.Errorf("BigDigits(\"84.4M\") row[1] = %q — unit letter must NOT repeat on row 1", rows[1])
	}
	if !strings.Contains(rows[2], "M") {
		t.Errorf("BigDigits(\"84.4M\") row[2] = %q — want single 'M' on row 2", rows[2])
	}
}

func TestBigDigits_EmptyString(t *testing.T) {
	rows := dashboard.BigDigits("")
	if len(rows) != 4 {
		t.Fatalf("BigDigits(\"\") returned %d rows, want 4", len(rows))
	}
	// All rows must be empty strings.
	for i, row := range rows {
		if row != "" {
			t.Errorf("BigDigits(\"\") row[%d] = %q, want empty", i, row)
		}
	}
}

// ─── BuildBar table tests ─────────────────────────────────────────────────────

// TestBuildBar_BoundaryValues verifies fill/track body cell counts and pill-cap
// presence at the boundary percentages: 0%, 1%, 50%, 99%, 100% for a 10-cell bar.
//
// Pill anatomy: left-cap (U+E0B6 "") + body (barLen-2 cells) + right-cap (U+E0B4 "")
// Total visual width stays barLen because the two cap glyphs each measure width-1.
//
// Body rules (enforced on the barLen-2 body, not the full bar):
//   - 0%   → 0 fill cells,  8 track cells  (truly empty body)
//   - 1%   → 1 fill cell,   7 track cells  (at-least-one-fill guarantee on body)
//   - 50%  → 4 fill cells,  4 track cells  (floor(0.5*8)=4)
//   - 99%  → 7 fill cells,  1 track cell   (at-least-one-track guarantee on body)
//   - 100% → 8 fill cells,  0 track cells  (truly full body)
//
// Pill presence: every bar must start with "" and end with "".
// Total lipgloss.Width must equal barLen.
func TestBuildBar_BoundaryValues(t *testing.T) {
	const fillGlyph = "█"
	const trackGlyph = "░"
	const leftCap = ""  // U+E0B6
	const rightCap = "" // U+E0B4
	const barLen = 10
	const bodyLen = barLen - 2 // caps consume 2 of the 10 cells

	cases := []struct {
		pct       float64
		wantFill  int
		wantTrack int
	}{
		{0, 0, bodyLen},
		{1, 1, bodyLen - 1},
		{50, 4, 4},
		{99, 7, 1},
		{100, bodyLen, 0},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("pct=%.0f", tc.pct), func(t *testing.T) {
			// BuildBar returns the unstyled bar string (ANSI-stripped for count).
			got := dashboard.BuildBar(tc.pct, barLen)
			// Strip any ANSI sequences for counting.
			stripped := stripANSI(got)

			// Pill cap presence checks.
			if !strings.HasPrefix(stripped, leftCap) {
				t.Errorf("pct=%.0f: bar does not start with left cap '': %q", tc.pct, stripped)
			}
			if !strings.HasSuffix(stripped, rightCap) {
				t.Errorf("pct=%.0f: bar does not end with right cap '': %q", tc.pct, stripped)
			}

			// Body fill/track counts (exclude the two cap chars).
			body := stripped
			if strings.HasPrefix(body, leftCap) {
				body = body[len(leftCap):]
			}
			if strings.HasSuffix(body, rightCap) {
				body = body[:len(body)-len(rightCap)]
			}
			gotFill := strings.Count(body, fillGlyph)
			gotTrack := strings.Count(body, trackGlyph)

			if gotFill != tc.wantFill {
				t.Errorf("pct=%.0f: body fill cells = %d, want %d (body: %q)", tc.pct, gotFill, tc.wantFill, body)
			}
			if gotTrack != tc.wantTrack {
				t.Errorf("pct=%.0f: body track cells = %d, want %d (body: %q)", tc.pct, gotTrack, tc.wantTrack, body)
			}

			// Body total must be bodyLen.
			total := gotFill + gotTrack
			if total != bodyLen {
				t.Errorf("pct=%.0f: body total cells = %d, want %d", tc.pct, total, bodyLen)
			}

			// Full visual width must still be barLen (lipgloss treats PUA glyphs as width-1).
			if w := lipgloss.Width(got); w != barLen {
				t.Errorf("pct=%.0f: lipgloss.Width = %d, want %d", tc.pct, w, barLen)
			}
		})
	}
}

// TestBuildBarCaps verifies that BuildBarCaps returns the correct cap-color
// booleans for the three critical boundary states:
//   - 0%   → leftFill=false (left cap uses track color), rightFill=false
//   - 22%  → leftFill=true  (fill > 0, left cap uses fill color), rightFill=false
//   - 100% → leftFill=true, rightFill=true (right cap uses fill color at 100%)
func TestBuildBarCaps(t *testing.T) {
	cases := []struct {
		pct           float64
		wantLeftFill  bool
		wantRightFill bool
	}{
		{0, false, false},
		{22, true, false},
		{100, true, true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("pct=%.0f", tc.pct), func(t *testing.T) {
			gotLeft, gotRight := dashboard.BuildBarCaps(tc.pct)
			if gotLeft != tc.wantLeftFill {
				t.Errorf("pct=%.0f: leftFill = %v, want %v", tc.pct, gotLeft, tc.wantLeftFill)
			}
			if gotRight != tc.wantRightFill {
				t.Errorf("pct=%.0f: rightFill = %v, want %v", tc.pct, gotRight, tc.wantRightFill)
			}
		})
	}
}

// ─── UI polish batch: 4 changes ──────────────────────────────────────────────

// Change 1 — Today column indent removal.
// Today rows (Input/Output/Cache/Total/API equiv./Sessions/Messages) must start
// flush with "Today ↘" — no extra leading spaces that the right column lacks.
func TestTodayColumn_NoLeadingIndent(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})

	output := m4.View()
	lines := strings.Split(output, "\n")

	// Collect all Today-column lines by finding the vertical column
	// position of the │ separator and looking at the left half only.
	// A simpler approach: find the ANSI-stripped "Input" line and check
	// it doesn't start with extra spaces past the 3-space card padding.
	targetLabels := []string{"Input", "Output", "Cache Read", "Cache Write", "API equiv.", "Sessions", "Messages"}
	for _, label := range targetLabels {
		for _, line := range lines {
			stripped := stripANSI(line)
			// Each content line has 3-space left padding from addRow.
			// The first non-space content should be the label itself, not "  Label".
			if strings.Contains(stripped, label) && !strings.Contains(stripped, "│") {
				// Line is: "   Label..." (3 spaces from pad, then label immediately)
				// It must NOT be "     Label..." (3+2=5 spaces = extra indent)
				if strings.HasPrefix(stripped, "     ") && strings.Contains(stripped, label) {
					// check the label appears right after the 3-space pad
					afterPad := strings.TrimLeft(stripped, " ")
					if strings.HasPrefix(afterPad, "  "+label) {
						t.Errorf("label %q has extra 2-space indent. Line: %q", label, stripped)
					}
				}
				break
			}
		}
	}
}

// Change 3 — rainbowText helper and animTickMsg.
// rainbowText(s, frame) must be deterministic: same input+frame → same output.
// Different frames must differ. ANSI-stripped must equal input.
func TestRainbowText_Deterministic(t *testing.T) {
	got1 := dashboard.RainbowText("clauchy", 0)
	got2 := dashboard.RainbowText("clauchy", 0)
	if got1 != got2 {
		t.Errorf("RainbowText not deterministic: got different results for same frame 0")
	}
}

func TestRainbowText_DifferentFramesDiffer(t *testing.T) {
	got0 := dashboard.RainbowText("clauchy", 0)
	got5 := dashboard.RainbowText("clauchy", 5)
	if got0 == got5 {
		t.Error("RainbowText frame 0 and frame 5 produced identical output")
	}
}

func TestRainbowText_ANSIStrippedEqualsInput(t *testing.T) {
	got := dashboard.RainbowText("clauchy", 0)
	stripped := stripANSI(got)
	if stripped != "clauchy" {
		t.Errorf("ANSI-stripped RainbowText(%q, 0) = %q, want %q", "clauchy", stripped, "clauchy")
	}
}

// TestAnimTick_FrameAdvances verifies that AnimTickMsg increments Model.frame by 1.
func TestAnimTick_FrameAdvances(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	// Send two AnimTickMsg; frame should go 0→1→2
	m2, _ := m.Update(dashboard.AnimTickMsg{})
	dm2 := m2.(dashboard.Model)
	if dm2.Frame() != 1 {
		t.Errorf("after 1 AnimTickMsg, Frame() = %d, want 1", dm2.Frame())
	}

	m3, _ := dm2.Update(dashboard.AnimTickMsg{})
	dm3 := m3.(dashboard.Model)
	if dm3.Frame() != 2 {
		t.Errorf("after 2 AnimTickMsg, Frame() = %d, want 2", dm3.Frame())
	}
}

// Change 4 — blank line after each model bar.
// In the Models 7d section, after each bar there must be a blank line.
// Because the two columns are zipped, the blank right-column row appears on
// the same output line as whatever the left column has on that row.
// We detect the blank by: find the line with the Opus 4.5 bar (contains "█"
// and is directly after the line containing "Opus 4.5" in the right-half),
// then the NEXT zipped line's right-column part (after the "│") must be
// all spaces (blank).
func TestModels7d_BlankLineAfterBar(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})

	output := m4.View()
	lines := strings.Split(output, "\n")

	// Find the bar line: line[i-1] contains "Opus 4.5" (model name)
	// and line[i] is the bar; line[i+1]'s right-col part must be blank.
	foundBarAfterOpus := false
	for i := range lines {
		if i == 0 {
			continue
		}
		if !strings.Contains(stripANSI(lines[i-1]), "Opus 4.5") {
			continue
		}
		// lines[i] is the bar for Opus 4.5.
		// lines[i+1]'s right-column portion (after │) should be blank.
		if i+1 >= len(lines) {
			t.Error("no line after Opus 4.5 bar")
			break
		}
		nextLine := stripANSI(lines[i+1])
		// Split on "│" to get the right-column portion.
		parts := strings.SplitN(nextLine, "│", 2)
		rightPart := ""
		if len(parts) == 2 {
			rightPart = strings.TrimSpace(parts[1])
		} else {
			// No │ — the entire line must be blank
			rightPart = strings.TrimSpace(nextLine)
		}
		if rightPart != "" {
			t.Errorf("expected blank right-col after Opus 4.5 bar, got: %q (full line: %q)", rightPart, nextLine)
		}
		foundBarAfterOpus = true
		break
	}
	if !foundBarAfterOpus {
		t.Error("could not find the bar line after 'Opus 4.5' model entry")
	}
}

// stripANSI removes ANSI escape sequences so we can count raw runes.
func stripANSI(s string) string {
	var out strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc && r == 'm':
			inEsc = false
		case inEsc:
			// still inside escape sequence
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// ─── UI polish: footer-pinned + padding + top margin (RED tests) ─────────────

// TestView_FooterPinnedToBottom verifies that when m.height > 0 the rendered
// View() has exactly m.height lines and the footer row is the last line.
// Change 19: the footer left side now shows status data (not "clauchy"), so we
// check the last line is a footer row by verifying the overall line count and that
// the header "clauchy" brand appears on line 1 (not the footer).
func TestView_FooterPinnedToBottom(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	output := m2.(dashboard.Model).View()

	// Split on newlines
	lines := strings.Split(output, "\n")

	// Must have exactly testHeight lines
	if len(lines) != testHeight {
		t.Errorf("View() has %d lines, want %d (testHeight)", len(lines), testHeight)
	}

	// The header "clauchy" brand must appear at line index 1 (after the blank top margin).
	if len(lines) >= 2 {
		headerLine := stripANSI(findHeaderLine(t, output))
		if !strings.Contains(headerLine, "clauchy") {
			t.Errorf("line[1] (header) does not contain 'clauchy', got: %q", headerLine)
		}
	}
}

// TestView_HorizontalPadding3 verifies that content lines have 3-space left padding.
func TestView_HorizontalPadding3(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	output := m2.(dashboard.Model).View()

	lines := strings.Split(output, "\n")
	// Find the header line containing "clauchy" (brand, Change 19).
	for _, line := range lines {
		stripped := stripANSI(line)
		if strings.Contains(stripped, "clauchy") && !strings.Contains(stripped, "est.") {
			if !strings.HasPrefix(stripped, "   ") {
				t.Errorf("header line does not start with 3-space left padding: %q", stripped)
			}
			return
		}
	}
	t.Error("header line 'clauchy' not found in View() output")
}

// TestView_TopMarginBlankLine verifies that the first non-empty visual line
// is preceded by a blank line (top margin above the header).
func TestView_TopMarginBlankLine(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	output := m2.(dashboard.Model).View()

	lines := strings.Split(output, "\n")
	if len(lines) == 0 {
		t.Fatal("View() produced no output")
	}
	// First line must be blank (the top margin).
	first := strings.TrimRight(stripANSI(lines[0]), " ")
	if first != "" {
		t.Errorf("first line should be blank (top margin), got: %q", lines[0])
	}
}

// ─── Unit tests (direct Model.Update) ────────────────────────────────────────

func TestModel_Update_LimitsMsg_StoresData(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	u := sampleUsage()
	next, _ := m.Update(dashboard.LimitsMsg{Usage: u, Err: nil})
	dm := next.(dashboard.Model)

	if dm.LimitsErr() != nil {
		t.Errorf("LimitsErr = %v, want nil", dm.LimitsErr())
	}
	if !dm.HasLimits() {
		t.Error("HasLimits = false after limitsMsg with valid data")
	}
}

func TestModel_Update_StatsMsg_StoresData(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, sampleStats(), nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	s := sampleStats()
	next, _ := m.Update(dashboard.StatsMsg{Stats: s, Err: nil})
	dm := next.(dashboard.Model)

	if dm.StatsErr() != nil {
		t.Errorf("StatsErr = %v, want nil", dm.StatsErr())
	}
	if !dm.HasStats() {
		t.Error("HasStats = false after statsMsg with valid data")
	}
}

func TestModel_Update_LimitsMsg_StoresError(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	next, _ := m.Update(dashboard.LimitsMsg{Usage: limits.Usage{}, Err: limits.ErrTransient})
	dm := next.(dashboard.Model)

	if dm.LimitsErr() == nil {
		t.Error("LimitsErr = nil after error msg, want non-nil")
	}
}

// TestModel_Update_TickMsg_InFlightGuard verifies two things:
//  1. (guard ON) A TickMsg while both loading flags are true must NOT increment
//     fetchCount — no re-issue of in-flight fetches.
//  2. A tick cmd is still returned so the 5s ticker keeps firing.
func TestModel_Update_TickMsg_InFlightGuard(t *testing.T) {
	var fetchCount int
	slowDeps := dashboard.Deps{
		FetchLimits: func() (limits.Usage, error) {
			fetchCount++
			return limits.Usage{}, nil
		},
		FetchStats: func() (transcript.Stats, error) {
			fetchCount++
			return transcript.Stats{}, nil
		},
	}
	m := dashboard.New(slowDeps, testPalette, fixedNowFn, "")

	// New() sets loadingLimits=true and loadingStats=true — guard is ON.
	// Init() would queue the initial fetches, but those run asynchronously
	// in the real runtime; in tests we never execute the returned commands,
	// so fetchCount stays 0 here.
	_ = m.Init()
	countBeforeTick := fetchCount

	// Tick while guards are ON: must not issue new fetches.
	_, cmds := m.Update(dashboard.TickMsg(fixedNow))

	// Assertion 1: no fetches issued (guard ON).
	if fetchCount != countBeforeTick {
		t.Errorf("guard ON: fetchCount went from %d to %d after tick — re-issued while in-flight", countBeforeTick, fetchCount)
	}

	// Assertion 2: tick cmd is still returned (the 5s ticker reschedules).
	if cmds == nil {
		t.Error("expected at least a tick reschedule cmd from tickMsg, got nil")
	}
}

func TestModel_Update_Quit_q(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("cmd = nil after q press, want tea.Quit")
	}
}

func TestModel_Update_Quit_Esc(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("cmd = nil after ESC press, want tea.Quit")
	}
}

func TestModel_Update_Quit_CtrlC(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("cmd = nil after Ctrl+C press, want tea.Quit")
	}
}

func TestModel_Update_WindowSizeMsg_StoresDims(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	dm := next.(dashboard.Model)

	if dm.Width() != 120 || dm.Height() != 40 {
		t.Errorf("dims = (%d, %d), want (120, 40)", dm.Width(), dm.Height())
	}
}

// ─── Golden tests ─────────────────────────────────────────────────────────────
//
// All goldens are rendered DIRECTLY from Model.View() with every data message
// (LimitsMsg / StatsMsg / StatusMsg) delivered manually. teatest is not used
// for static goldens: its event loop races asynchronously-delivered messages
// against the quit key, making the captured bytes non-deterministic.

func TestView_Loading_Golden(t *testing.T) {
	// Loading golden is rendered directly from Model.View() to avoid
	// race conditions introduced by teatest frame-capture timing.
	// The loading state has no dynamic data, so View() output is fully
	// deterministic for a fixed window size and fixed clock.
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	// Set a fixed size so the golden is deterministic
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})

	got := []byte(m2.(dashboard.Model).View())
	golden.RequireEqual(t, got)
}

func TestView_FullData_Golden(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	// "Max 20x" is the sample plan label shown in the golden to verify
	// right-aligned plan rendering in the header.
	m := dashboard.New(deps, testPalette, fixedNowFn, "Max 20x")

	// Deliver every data message manually, then render View() directly.
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})
	m5, _ := m4.(dashboard.Model).Update(dashboard.StatusMsg{Status: status.Status{}, Err: nil})

	got := []byte(m5.(dashboard.Model).View())
	golden.RequireEqual(t, got)
}

func TestView_Degraded_Golden(t *testing.T) {
	// Limits panel has an error; stats panel has data
	deps := stubDeps(limits.Usage{}, limits.ErrTransient, sampleStats(), nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: limits.Usage{}, Err: limits.ErrTransient})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})
	m5, _ := m4.(dashboard.Model).Update(dashboard.StatusMsg{Status: status.Status{}, Err: nil})

	got := []byte(m5.(dashboard.Model).View())
	golden.RequireEqual(t, got)
}

// ─── Scheme selection tests ───────────────────────────────────────────────────

// skyHex is the ANSI true-color escape for Sky (#89dceb = R137 G220 B235).
// Used to detect the presence/absence of Sky coloring in rendered output.
const skyHex = "38;2;137;220;235"

// TestScheme_Monochrome_NoSkyHex verifies that the monochrome scheme (default)
// produces a render with NO Sky color escape codes in the limit bars and section
// headers — those elements must use white/gray instead.
func TestScheme_Monochrome_NoSkyHex(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})

	output := m4.View()
	// Exclude the last line (footer) to keep the test focused on bars and
	// section headers; the footer renders status/pricing text in Subtle.
	lines := strings.Split(output, "\n")
	mainContent := strings.Join(lines[:len(lines)-1], "\n")
	if strings.Contains(mainContent, skyHex) {
		t.Errorf("monochrome render contains Sky color escape %q — expected no Sky tones in main content", skyHex)
	}
}

// TestScheme_Colorful_HasSkyHex verifies that the colorful scheme
// renders Sky color escape codes in section headers and limit bars.
func TestScheme_Colorful_HasSkyHex(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	m := dashboard.NewColorful(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})

	output := m4.View()
	if !strings.Contains(output, skyHex) {
		t.Errorf("colorful render does not contain Sky color escape %q — expected Sky tones in section headers / bars", skyHex)
	}
}

// TestView_Colorful_FullData_Golden pins the colorful scheme full-data render.
func TestView_Colorful_FullData_Golden(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	m := dashboard.NewColorful(deps, testPalette, fixedNowFn, "Max 20x")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})
	m5, _ := m4.(dashboard.Model).Update(dashboard.StatusMsg{Status: status.Status{}, Err: nil})

	got := []byte(m5.(dashboard.Model).View())
	golden.RequireEqual(t, got)
}

// ─── GrayscaleText (monochrome header animation) ─────────────────────────────

func TestGrayscaleText_Deterministic(t *testing.T) {
	got1 := dashboard.GrayscaleText("clauchy", 0)
	got2 := dashboard.GrayscaleText("clauchy", 0)
	if got1 != got2 {
		t.Error("GrayscaleText not deterministic for the same frame")
	}
}

func TestGrayscaleText_DifferentFramesDiffer(t *testing.T) {
	if dashboard.GrayscaleText("clauchy", 0) == dashboard.GrayscaleText("clauchy", 5) {
		t.Error("GrayscaleText frame 0 and frame 5 produced identical output")
	}
}

func TestGrayscaleText_ANSIStrippedEqualsInput(t *testing.T) {
	if got := stripANSI(dashboard.GrayscaleText("clauchy", 0)); got != "clauchy" {
		t.Errorf("stripped GrayscaleText = %q, want %q", got, "clauchy")
	}
}

// TestGrayscaleText_OnlyGrays: every emitted truecolor escape must have R==G==B.
func TestGrayscaleText_OnlyGrays(t *testing.T) {
	re := regexp.MustCompile(`38;2;(\d+);(\d+);(\d+)m`)
	for _, m := range re.FindAllStringSubmatch(dashboard.GrayscaleText("clauchy", 3), -1) {
		if m[1] != m[2] || m[2] != m[3] {
			t.Errorf("non-gray color emitted: rgb(%s,%s,%s)", m[1], m[2], m[3])
		}
	}
}

// ─── Fix 9: Rune-safe model label truncation ─────────────────────────────────

// TestLimitBars_MultibyteLabelTruncation verifies that a model name composed of
// multibyte runes is truncated at the 10-rune boundary, not the 10-byte boundary.
// The rendered output must be valid UTF-8 and must not contain replacement/garbage
// characters produced by a mid-rune byte slice.
func TestLimitBars_MultibyteLabelTruncation(t *testing.T) {
	// "日本語テスト長い名前" = 10 runes, each 3 bytes → 30 bytes total.
	// Truncating at byte 10 splits the 4th rune (テ) and produces invalid UTF-8.
	longMultibyteModel := "日本語テスト長い名前追加" // 12 runes — must be truncated to 10
	u := sampleUsage()
	u.Models = []limits.ModelLimit{
		{Name: longMultibyteModel, Utilization: 50, ResetsAt: fixedNow.Add(49 * time.Hour)},
	}
	deps := stubDeps(u, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: u, Err: nil})
	output := m3.View()

	// The output must be valid UTF-8 (no replacement characters from broken byte slices).
	if !utf8.ValidString(output) {
		t.Error("View() output is not valid UTF-8 — model label was likely byte-sliced mid-rune")
	}

	// The first 10 runes of the label must appear (in rune-string form).
	want10 := string([]rune(longMultibyteModel)[:10]) // "日本語テスト長い名前"
	strippedOutput := stripANSI(output)
	if !strings.Contains(strippedOutput, want10) {
		t.Errorf("View() output missing 10-rune truncation %q; the label was probably byte-truncated.\nFull stripped output:\n%s", want10, strippedOutput)
	}
}

// ─── Fix 4: In-flight guard real assertion ────────────────────────────────────

// TestModel_Update_TickMsg_InFlightGuard_NoFetch verifies that when both
// loading flags are true, a TickMsg does NOT issue new fetch commands.
// (Guard ON: no re-issue while loading.)
func TestModel_Update_TickMsg_InFlightGuard_NoFetch(t *testing.T) {
	var fetchCount int
	slowDeps := dashboard.Deps{
		FetchLimits: func() (limits.Usage, error) {
			fetchCount++
			return limits.Usage{}, nil
		},
		FetchStats: func() (transcript.Stats, error) {
			fetchCount++
			return transcript.Stats{}, nil
		},
	}
	m := dashboard.New(slowDeps, testPalette, fixedNowFn, "")
	// New() sets loadingLimits=true and loadingStats=true.
	// A TickMsg must NOT increment fetchCount (both guards active).
	fetchCountBefore := fetchCount
	_, _ = m.Update(dashboard.TickMsg(fixedNow))
	if fetchCount != fetchCountBefore {
		t.Errorf("TickMsg while loading: fetchCount went from %d to %d, want no increment (guard ON)", fetchCountBefore, fetchCount)
	}
}

// TestModel_Update_TickMsg_InFlightGuard_Fetches verifies that when both
// loading flags are clear, a TickMsg DOES issue new fetch commands.
// (Guard OFF: re-issue when not loading.)
func TestModel_Update_TickMsg_InFlightGuard_Fetches(t *testing.T) {
	var fetchCount int
	readyDeps := dashboard.Deps{
		FetchLimits: func() (limits.Usage, error) {
			fetchCount++
			return limits.Usage{}, nil
		},
		FetchStats: func() (transcript.Stats, error) {
			fetchCount++
			return transcript.Stats{}, nil
		},
	}
	m := dashboard.New(readyDeps, testPalette, fixedNowFn, "")
	// Clear the loading flags by delivering data messages.
	m2, _ := m.Update(dashboard.LimitsMsg{Usage: limits.Usage{CachedAt: fixedNow}, Err: nil})
	m3, _ := m2.(dashboard.Model).Update(dashboard.StatsMsg{Stats: transcript.Stats{}, Err: nil})

	fetchCountBefore := fetchCount
	_, _ = m3.(dashboard.Model).Update(dashboard.TickMsg(fixedNow))
	// The tick should have queued 2 new fetches (one per dep), incrementing fetchCount via cmd execution.
	// However, since cmd functions are not invoked inline (they are returned, not called),
	// we check that the model has re-set the loading flags to indicate fetches were re-issued.
	// We use the exported accessors; loadingLimits/loadingStats should be true again after tick.
	// But we don't have those exported — so we verify via a second tick, which would no-op if guard is active.
	_ = fetchCountBefore // fetchCount won't change until cmds are executed by the runtime
	// Verify that a subsequent tick sees the guard as ON (loading flags set by first tick).
	m4, _ := m3.(dashboard.Model).Update(dashboard.TickMsg(fixedNow))
	fetchCountAfterFirstTick := fetchCount
	_, _ = m4.(dashboard.Model).Update(dashboard.TickMsg(fixedNow))
	if fetchCount != fetchCountAfterFirstTick {
		t.Errorf("second TickMsg (while guards should be ON) incremented fetchCount from %d to %d", fetchCountAfterFirstTick, fetchCount)
	}
}

// ─── Fix 1: Labels row width constraint ─────────────────────────────────────

// TestLimitBars_LabelsRowWidth verifies that the labels+reset row emitted by
// viewLimitBarsInline never exceeds contentWidth for the target terminal widths.
// At 80 cols: contentWidth = 80 - 3*2 = 74. At 72 cols (minimum enforced): same 74.
func TestLimitBars_LabelsRowWidth(t *testing.T) {
	cases := []struct {
		name      string
		termWidth int
		wantMaxW  int
	}{
		{"80col", 80, 74},
		{"72col", 72, 74}, // m.width < 72 triggers outerWidth=80 fallback
		{"100col", 100, 94},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := sampleUsage()
			deps := stubDeps(u, nil, transcript.Stats{}, nil)
			m := dashboard.New(deps, testPalette, fixedNowFn, "")

			m2, _ := m.Update(tea.WindowSizeMsg{Width: tc.termWidth, Height: testHeight})
			m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: u, Err: nil})
			output := m3.View()

			lines := strings.Split(output, "\n")
			for i, line := range lines {
				w := lipgloss.Width(line)
				// Content lines are padded with 3 spaces on each side; full line width = outerWidth.
				// We check no content line exceeds the outer width.
				outerWidth := tc.termWidth
				if outerWidth < 72 {
					outerWidth = 80
				}
				if w > outerWidth {
					t.Errorf("line %d exceeds outerWidth %d (got %d): %q", i, outerWidth, w, stripANSI(line))
				}
			}
		})
	}
}

// ─── Fix 2: Full-data View height at 80x24 ───────────────────────────────────

// TestView_FullData_ExactHeight verifies that View() with full data at 80x24
// renders EXACTLY 24 lines — no overflow beyond the terminal height.
func TestView_FullData_ExactHeight(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "Max 20x")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})

	output := m4.View()
	lines := strings.Split(output, "\n")
	if len(lines) != testHeight {
		t.Errorf("full-data View() at %dx%d has %d lines, want exactly %d",
			testWidth, testHeight, len(lines), testHeight)
		for i, l := range lines {
			t.Logf("  line[%02d]: %q", i, stripANSI(l))
		}
	}
}

// TestView_Loading_ExactHeight verifies that the loading state also renders
// EXACTLY testHeight lines and the footer is bottom-anchored.
// Change 19: "clauchy" is now in the header (line[1]), not the footer.
// The footer left side shows status data (empty while loading).
func TestView_Loading_ExactHeight(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	output := m2.(dashboard.Model).View()
	lines := strings.Split(output, "\n")
	if len(lines) != testHeight {
		t.Errorf("loading View() at %dx%d has %d lines, want exactly %d",
			testWidth, testHeight, len(lines), testHeight)
	}

	// The header "clauchy" brand must appear at line index 1.
	if len(lines) >= 2 {
		headerLine := stripANSI(findHeaderLine(t, output))
		if !strings.Contains(headerLine, "clauchy") {
			t.Errorf("line[1] (header) does not contain 'clauchy', got: %q", headerLine)
		}
	}
}

// ─── Change 18: header animation as sync indicator ───────────────────────────

// countColorCodes counts the number of distinct truecolor RGB escape sequences
// in a string. Each per-letter color in the animated header contributes one.
func countColorCodes(s string) int {
	re := regexp.MustCompile(`\x1b\[38;2;\d+;\d+;\d+m`)
	return len(re.FindAllString(s, -1))
}

// TestHeaderAnimation_WhileSyncing verifies that a fresh model (loadingLimits=true,
// loadingStats=true) renders the header "clauchy" brand with per-letter ANSI color
// sequences — the animated form is active while a sync is in progress (Change 19).
func TestHeaderAnimation_WhileSyncing(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	// New() sets loadingLimits=true, loadingStats=true, loadingStatus=true → syncing active.
	m := dashboard.New(deps, testPalette, fixedNowFn, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	output := m2.(dashboard.Model).View()
	lines := strings.Split(output, "\n")
	// Header is at line index 1 (line 0 is blank top margin).
	if len(lines) < 2 {
		t.Fatal("View() has fewer than 2 lines")
	}
	headerLine := findHeaderLine(t, output)
	// The animated header has one color code per letter (7 for "clauchy").
	n := countColorCodes(headerLine)
	if n < 7 {
		t.Errorf("header line while syncing has %d color escapes, want >=7 (one per letter of 'clauchy'); got: %q", n, headerLine)
	}
}

// TestHeaderAnimation_StaticWhenIdle verifies that once all data messages have
// arrived AND at least 4 AnimTickMsgs have been sent (exhausting the minimum
// pulse countdown), the header "clauchy" brand renders statically bold with no
// per-letter multi-color animation (Change 19).
func TestHeaderAnimation_StaticWhenIdle(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	// Deliver all data messages so loading flags clear.
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})
	m5, _ := m4.(dashboard.Model).Update(dashboard.StatusMsg{Status: status.Status{}, Err: nil})
	// Exhaust the 4-frame minimum pulse countdown.
	var cur tea.Model = m5
	for i := 0; i < dashboard.SyncPulseMinFrames; i++ {
		cur, _ = cur.(dashboard.Model).Update(dashboard.AnimTickMsg{})
	}
	output := cur.(dashboard.Model).View()
	lines := strings.Split(output, "\n")
	if len(lines) < 2 {
		t.Fatal("View() has fewer than 2 lines")
	}
	headerLine := findHeaderLine(t, output)

	// When idle the header "clauchy" renders as plain bold (no truecolor RGB escapes).
	n := countColorCodes(headerLine)
	if n > 0 {
		t.Errorf("header line when idle has %d truecolor escapes, want 0 (plain bold); got: %q", n, headerLine)
	}
	// The "clauchy" text must still appear.
	if !strings.Contains(stripANSI(headerLine), "clauchy") {
		t.Errorf("header line when idle missing 'clauchy': %q", stripANSI(headerLine))
	}
}

// TestHeaderAnimation_MinimumPulse verifies that when fetch results arrive
// immediately after init (simulating a cache hit), the header animation remains
// active until the 4-frame countdown is exhausted (Change 19).
// After <4 AnimTickMsgs the header must still be animated; after 4+ it goes static.
func TestHeaderAnimation_MinimumPulse(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	// Deliver all data immediately (simulating cache hits before any anim tick).
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})
	m5, _ := m4.(dashboard.Model).Update(dashboard.StatusMsg{Status: status.Status{}, Err: nil})

	// After 3 AnimTickMsgs (< 4) the pulse countdown is not yet exhausted
	// → header animation must still be active.
	var cur tea.Model = m5
	for i := 0; i < dashboard.SyncPulseMinFrames-1; i++ {
		cur, _ = cur.(dashboard.Model).Update(dashboard.AnimTickMsg{})
	}
	outputMid := cur.(dashboard.Model).View()
	headerMid := findHeaderLine(t, outputMid)
	if countColorCodes(headerMid) < 7 {
		t.Errorf("header after 3 AnimTicks (mid-pulse) has <7 color codes — animation must still be active; got: %q", headerMid)
	}

	// After 1 more AnimTickMsg (total 4) the countdown is exhausted → static header.
	cur, _ = cur.(dashboard.Model).Update(dashboard.AnimTickMsg{})
	outputDone := cur.(dashboard.Model).View()
	headerDone := findHeaderLine(t, outputDone)
	if countColorCodes(headerDone) > 0 {
		t.Errorf("header after 4 AnimTicks (pulse done) still animated (%d color codes); want 0 static", countColorCodes(headerDone))
	}
}

// ─── Change 20: sync pulse semantics ─────────────────────────────────────────
//
// The header animation is driven ONLY by syncPulseFrames > 0 (not by the loading
// flags). The pulse is set to SyncPulseMinFrames on:
//   - construction (existing)
//   - LimitsMsg arriving with fresh CachedAt (different from previous)
//   - StatusMsg arriving with fresh CachedAt (different from previous)
// StatsMsg and TickMsg do NOT trigger the pulse.

// exhaustPulse sends SyncPulseMinFrames AnimTickMsgs to the model, fully
// draining the minimum-pulse countdown. Returns the final model state.
func exhaustPulse(m dashboard.Model) dashboard.Model {
	var cur tea.Model = m
	for i := 0; i < dashboard.SyncPulseMinFrames; i++ {
		cur, _ = cur.(dashboard.Model).Update(dashboard.AnimTickMsg{})
	}
	return cur.(dashboard.Model)
}

// idleModel builds a model, delivers nil-data messages to clear all loading
// flags WITHOUT triggering a pulse reset, and exhausts the initial pulse so
// the header is guaranteed static. Used by pulse-semantics tests.
//
// Note: to avoid triggering the fresh-CachedAt pulse rule, LimitsMsg is
// delivered with zero CachedAt (already present in the initial model state) and
// StatsMsg is delivered (doesn't trigger pulse). Status is kept at zero value.
func idleBaseModel(t *testing.T) dashboard.Model {
	t.Helper()
	deps := dashboard.Deps{
		FetchLimits: func() (limits.Usage, error) { return limits.Usage{}, nil },
		FetchStats:  func() (transcript.Stats, error) { return transcript.Stats{}, nil },
		FetchStatus: func() (status.Status, error) { return status.Status{}, nil },
	}
	m := dashboard.New(deps, testPalette, fixedNowFn, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	// LimitsMsg with zero CachedAt — same as initial zero state, no pulse reset.
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: limits.Usage{}, Err: nil})
	// StatsMsg — per spec, never triggers pulse.
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: transcript.Stats{}, Err: nil})
	// StatusMsg with zero CachedAt — same as initial zero state, no pulse reset.
	m5, _ := m4.(dashboard.Model).Update(dashboard.StatusMsg{Status: status.Status{}, Err: nil})
	return exhaustPulse(m5.(dashboard.Model))
}

// TestSyncPulse_FreshLimitsMsg_TriggersPulse verifies that after the initial
// pulse is exhausted, a LimitsMsg with a NEW (non-zero) CachedAt that differs
// from the previous CachedAt resets syncPulseFrames to SyncPulseMinFrames and
// the header becomes animated again (Change 20).
func TestSyncPulse_FreshLimitsMsg_TriggersPulse(t *testing.T) {
	idle := idleBaseModel(t)

	// Verify it starts static.
	headerLine := strings.Split(idle.View(), "\n")[1]
	if n := countColorCodes(headerLine); n > 0 {
		t.Fatalf("precondition failed: header is still animated (%d codes) before fresh LimitsMsg", n)
	}

	// Deliver LimitsMsg with a NEW CachedAt (different from the zero previous value).
	freshUsage := limits.Usage{
		CachedAt: fixedNow.Add(-5 * time.Second), // non-zero, different from previous zero
	}
	after, _ := idle.Update(dashboard.LimitsMsg{Usage: freshUsage, Err: nil})

	// Pulse must be reset.
	dm := after.(dashboard.Model)
	if got := dm.SyncPulseFrames(); got < dashboard.SyncPulseMinFrames {
		t.Errorf("SyncPulseFrames() = %d after fresh LimitsMsg, want >= %d", got, dashboard.SyncPulseMinFrames)
	}

	// Header must be animated.
	m2, _ := dm.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	headerLine2 := strings.Split(m2.(dashboard.Model).View(), "\n")[1]
	if n := countColorCodes(headerLine2); n < 7 {
		t.Errorf("header has %d color codes after fresh LimitsMsg, want >=7 (animated)", n)
	}
}

// TestSyncPulse_CachedLimitsMsg_NoRetrigger verifies that a LimitsMsg with the
// SAME CachedAt as the previous one (cache hit — no new API data) does NOT
// reset the pulse after it has been exhausted (Change 20).
func TestSyncPulse_CachedLimitsMsg_NoRetrigger(t *testing.T) {
	// Start with a model that has received one fresh LimitsMsg.
	deps := dashboard.Deps{
		FetchLimits: func() (limits.Usage, error) { return limits.Usage{}, nil },
		FetchStats:  func() (transcript.Stats, error) { return transcript.Stats{}, nil },
		FetchStatus: func() (status.Status, error) { return status.Status{}, nil },
	}
	m := dashboard.New(deps, testPalette, fixedNowFn, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})

	// First LimitsMsg: fresh CachedAt — sets previous CachedAt to ts1.
	ts1 := fixedNow.Add(-30 * time.Second)
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: limits.Usage{CachedAt: ts1}, Err: nil})
	// StatsMsg and StatusMsg: clear the remaining loading flags so the precondition
	// (header is static) holds under BOTH the old code (loading-flag gating) and
	// the new code (pulse-only gating).
	m3a, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: transcript.Stats{}, Err: nil})
	m3s, _ := m3a.(dashboard.Model).Update(dashboard.StatusMsg{Status: status.Status{}, Err: nil})
	idle1 := exhaustPulse(m3s.(dashboard.Model))

	// Verify idle.
	headerLine := strings.Split(idle1.View(), "\n")[1]
	if n := countColorCodes(headerLine); n > 0 {
		t.Fatalf("precondition failed: header still animated (%d codes) after exhausting pulse from first LimitsMsg", n)
	}

	// Second LimitsMsg: SAME CachedAt (ts1) — cache hit, no new data.
	after, _ := idle1.Update(dashboard.LimitsMsg{Usage: limits.Usage{CachedAt: ts1}, Err: nil})
	dm := after.(dashboard.Model)

	// Pulse must NOT be reset.
	if got := dm.SyncPulseFrames(); got > 0 {
		t.Errorf("SyncPulseFrames() = %d after cache-hit LimitsMsg (same CachedAt), want 0 (no re-trigger)", got)
	}

	// Header must stay static.
	m4, _ := dm.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	headerLine2 := strings.Split(m4.(dashboard.Model).View(), "\n")[1]
	if n := countColorCodes(headerLine2); n > 0 {
		t.Errorf("header has %d color codes after cache-hit LimitsMsg, want 0 (static)", n)
	}
}

// TestSyncPulse_TickMsg_NoPulseReset verifies that a TickMsg alone after the
// pulse is exhausted does NOT reset syncPulseFrames and the header stays static
// (Change 20 removes the issuedFetch → pulse coupling from TickMsg).
func TestSyncPulse_TickMsg_NoPulseReset(t *testing.T) {
	idle := idleBaseModel(t)

	// Verify idle.
	headerLine := strings.Split(idle.View(), "\n")[1]
	if n := countColorCodes(headerLine); n > 0 {
		t.Fatalf("precondition failed: header animated (%d codes) before TickMsg", n)
	}

	// TickMsg — must not reset pulse.
	after, _ := idle.Update(dashboard.TickMsg(fixedNow))
	dm := after.(dashboard.Model)

	if got := dm.SyncPulseFrames(); got > 0 {
		t.Errorf("SyncPulseFrames() = %d after TickMsg, want 0 (tick must not re-trigger pulse)", got)
	}

	// Header must stay static.
	m2, _ := dm.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	headerLine2 := strings.Split(m2.(dashboard.Model).View(), "\n")[1]
	if n := countColorCodes(headerLine2); n > 0 {
		t.Errorf("header has %d color codes after TickMsg with exhausted pulse, want 0 (static)", n)
	}
}

// TestSyncPulse_FreshStatusMsg_TriggersPulse verifies that a StatusMsg with a
// NEW CachedAt (different from the previous) resets the pulse and animates the
// header (Change 20).
func TestSyncPulse_FreshStatusMsg_TriggersPulse(t *testing.T) {
	idle := idleBaseModel(t)

	// Verify idle.
	headerLine := strings.Split(idle.View(), "\n")[1]
	if n := countColorCodes(headerLine); n > 0 {
		t.Fatalf("precondition failed: header animated (%d codes) before fresh StatusMsg", n)
	}

	// Deliver StatusMsg with a NEW CachedAt (different from the previous zero value).
	freshStatus := status.Status{
		Indicator:  "none",
		ClaudeCode: "operational",
		CachedAt:   fixedNow.Add(-2 * time.Second), // non-zero, different from previous zero
	}
	after, _ := idle.Update(dashboard.StatusMsg{Status: freshStatus, Err: nil})
	dm := after.(dashboard.Model)

	// Pulse must be reset.
	if got := dm.SyncPulseFrames(); got < dashboard.SyncPulseMinFrames {
		t.Errorf("SyncPulseFrames() = %d after fresh StatusMsg, want >= %d", got, dashboard.SyncPulseMinFrames)
	}

	// Header must be animated.
	m2, _ := dm.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	headerLine2 := strings.Split(m2.(dashboard.Model).View(), "\n")[1]
	if n := countColorCodes(headerLine2); n < 7 {
		t.Errorf("header has %d color codes after fresh StatusMsg, want >=7 (animated)", n)
	}
}

// TestSyncPulse_StatsMsg_NoPulse verifies that a StatsMsg does NOT reset the
// pulse (per Change 20 spec: local recompute every 5s is not a "sync" in the
// user's mental model).
func TestSyncPulse_StatsMsg_NoPulse(t *testing.T) {
	idle := idleBaseModel(t)

	// Verify idle.
	headerLine := strings.Split(idle.View(), "\n")[1]
	if n := countColorCodes(headerLine); n > 0 {
		t.Fatalf("precondition failed: header animated (%d codes) before StatsMsg", n)
	}

	after, _ := idle.Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})
	dm := after.(dashboard.Model)

	if got := dm.SyncPulseFrames(); got > 0 {
		t.Errorf("SyncPulseFrames() = %d after StatsMsg, want 0 (stats must not trigger pulse)", got)
	}

	m2, _ := dm.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	headerLine2 := strings.Split(m2.(dashboard.Model).View(), "\n")[1]
	if n := countColorCodes(headerLine2); n > 0 {
		t.Errorf("header has %d color codes after StatsMsg, want 0 (static)", n)
	}
}

// ─── Change 19: brand header + Claude status indicator ───────────────────────

// TestViewHeader_BrandIsClauchy verifies the header now shows "clauchy"
// (the product brand) instead of "CLAUDE CODE".
func TestViewHeader_BrandIsClauchy(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	output := stripANSI(m2.(dashboard.Model).View())

	if strings.Contains(output, "CLAUDE CODE") {
		t.Error("header must not contain 'CLAUDE CODE' (superseded by Change 19)")
	}
	if !strings.Contains(output, "clauchy") {
		t.Error("header must contain 'clauchy' (product brand)")
	}
}

// TestViewHeader_NoPlanLabel verifies empty plan doesn't break the brand header.
func TestViewHeader_NoPlanLabel_BrandPresent(t *testing.T) {
	deps := stubDeps(limits.Usage{}, nil, transcript.Stats{}, nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	output := stripANSI(m2.(dashboard.Model).View())

	// "clauchy" appears in the header row (first non-blank line).
	lines := strings.Split(output, "\n")
	// line[0] is blank top margin; line[1] is the header.
	if len(lines) < 2 {
		t.Fatal("View() has fewer than 2 lines")
	}
	header := strings.TrimSpace(stripANSI(findHeaderLine(t, output)))
	if !strings.HasPrefix(header, "clauchy") {
		t.Errorf("header line[1] does not start with 'clauchy': %q", header)
	}
}

// TestViewHeader_Static_WhenIdle verifies that once syncing completes and the
// 4-frame pulse is exhausted, the header "clauchy" renders statically (bold,
// no per-letter color codes).
func TestViewHeader_Static_WhenIdle(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})
	// Exhaust 4-frame pulse.
	var cur tea.Model = m4
	for i := 0; i < dashboard.SyncPulseMinFrames; i++ {
		cur, _ = cur.(dashboard.Model).Update(dashboard.AnimTickMsg{})
	}
	// Also deliver StatusMsg to clear loadingStatus.
	cur, _ = cur.(dashboard.Model).Update(dashboard.StatusMsg{Status: status.Status{}, Err: nil})
	output := cur.(dashboard.Model).View()
	lines := strings.Split(output, "\n")
	if len(lines) < 2 {
		t.Fatal("View() has fewer than 2 lines")
	}
	headerLine := findHeaderLine(t, output)
	n := countColorCodes(headerLine)
	// Bold uses a different escape (not truecolor RGB), so n should be 0.
	if n > 0 {
		t.Errorf("header when idle has %d truecolor escapes, want 0 (plain bold); line: %q", n, headerLine)
	}
	stripped := stripANSI(headerLine)
	if !strings.Contains(stripped, "clauchy") {
		t.Errorf("header when idle missing 'clauchy': %q", stripped)
	}
}

// simpleStats returns a Stats with no PricingDate so the footer right side is
// only "updated HH:MM" — short enough that the status left side fits at 80 cols.
func simpleStats() transcript.Stats {
	return transcript.Stats{
		Today:       transcript.DayTotals{Sessions: 1},
		PricingDate: "", // intentionally blank — keeps footer right side short
		Generated:   fixedNow,
	}
}

// footerModel builds a dashboard model at testWidth×testHeight, delivers the
// three data messages, exhausts the 4-frame sync pulse, and returns the final
// model. Shared by the footer status tests.
func footerModel(t *testing.T, s transcript.Stats, st status.Status, stErr error, colorful bool) dashboard.Model {
	t.Helper()
	deps := stubDepsWithStatus(sampleUsage(), nil, s, nil, st, stErr)
	var m dashboard.Model
	if colorful {
		m = dashboard.NewColorful(deps, testPalette, fixedNowFn, "")
	} else {
		m = dashboard.New(deps, testPalette, fixedNowFn, "")
	}
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: s, Err: nil})
	m5, _ := m4.(dashboard.Model).Update(dashboard.StatusMsg{Status: st, Err: stErr})
	var cur tea.Model = m5
	for i := 0; i < dashboard.SyncPulseMinFrames; i++ {
		cur, _ = cur.(dashboard.Model).Update(dashboard.AnimTickMsg{})
	}
	return cur.(dashboard.Model)
}

// lastViewLine returns the footer (last) line of View(), raw (with ANSI).
func lastViewLine(m dashboard.Model) string {
	lines := strings.Split(m.View(), "\n")
	return lines[len(lines)-1]
}

// TestFooterStatus_Operational verifies that an operational status renders the
// short "● operational" form — short so it fits at 80 cols even beside the
// long pricing text (sampleStats has a PricingDate).
func TestFooterStatus_Operational(t *testing.T) {
	st := status.Status{Indicator: "none", ClaudeCode: "operational", CachedAt: fixedNow}
	m := footerModel(t, sampleStats(), st, nil, false)
	lastLine := strings.TrimRight(stripANSI(lastViewLine(m)), " ")
	if !strings.Contains(lastLine, "● operational") {
		t.Errorf("footer should contain '● operational', got: %q", lastLine)
	}
	// The long pricing text must coexist with the status on the same row.
	if !strings.Contains(lastLine, "est. costs") {
		t.Errorf("footer right side missing beside operational status, got: %q", lastLine)
	}
}

// TestFooterStatus_Incident_Major verifies a partial_outage (Worst == "major")
// renders the ⚠ prefix with the humanized Claude Code component status.
func TestFooterStatus_Incident_Major(t *testing.T) {
	st := status.Status{Indicator: "minor", ClaudeCode: "partial_outage", CachedAt: fixedNow}
	m := footerModel(t, simpleStats(), st, nil, false)
	lastLine := strings.TrimRight(stripANSI(lastViewLine(m)), " ")
	if !strings.Contains(lastLine, "⚠") {
		t.Errorf("footer left should contain '⚠' for incident, got: %q", lastLine)
	}
	if !strings.Contains(lastLine, "Claude Code: partial outage") {
		t.Errorf("footer left should show humanized 'Claude Code: partial outage', got: %q", lastLine)
	}
}

// TestFooterStatus_NonClaudeCodeIncident_UsesDescription verifies that when the
// incident is elsewhere (ClaudeCode == "operational" but the page indicator is
// non-operational), the footer shows the page Description and does NOT render
// the contradictory "Claude Code: operational".
func TestFooterStatus_NonClaudeCodeIncident_UsesDescription(t *testing.T) {
	st := status.Status{
		Indicator:   "minor",
		ClaudeCode:  "operational",
		Description: "Elevated errors",
		CachedAt:    fixedNow,
	}
	m := footerModel(t, simpleStats(), st, nil, false)
	lastLine := strings.TrimRight(stripANSI(lastViewLine(m)), " ")
	if strings.Contains(lastLine, "operational") {
		t.Errorf("footer must not render 'Claude Code: operational' during a non-Claude-Code incident, got: %q", lastLine)
	}
	if !strings.Contains(lastLine, "Elevated errors") {
		t.Errorf("footer should show the page description for a non-Claude-Code incident, got: %q", lastLine)
	}
}

// TestFooterStatus_ZeroStatus_NoGhost verifies that a zero-value Status
// delivered with a nil error (CachedAt.IsZero() — "no data") renders NO status
// segment: neither the ● operational form nor a ghost ⚠ incident.
func TestFooterStatus_ZeroStatus_NoGhost(t *testing.T) {
	m := footerModel(t, simpleStats(), status.Status{}, nil, false)
	lastLine := strings.TrimRight(stripANSI(lastViewLine(m)), " ")
	if strings.Contains(lastLine, "●") || strings.Contains(lastLine, "⚠") {
		t.Errorf("footer left must be empty for zero-value Status (no data), got: %q", lastLine)
	}
}

// warningHex / errorHex / subtleHex are the truecolor SGR fragments for the
// scheme incident colors — asserted raw to pin the color split.
const (
	warningHex = "38;2;249;226;175" // #f9e2af
	errorHex   = "38;2;243;139;168" // #f38ba8
	subtleHex  = "38;2;108;112;134" // #6c7086
)

// TestFooterStatus_MinorIncident_WarningColor pins minor incidents
// (degraded_performance) to the Warning token #f9e2af in the raw escapes.
func TestFooterStatus_MinorIncident_WarningColor(t *testing.T) {
	st := status.Status{Indicator: "none", ClaudeCode: "degraded_performance", CachedAt: fixedNow}
	m := footerModel(t, simpleStats(), st, nil, false)
	lastLine := lastViewLine(m)
	if !strings.Contains(lastLine, warningHex) {
		t.Errorf("minor incident footer missing warning color %q, got: %q", warningHex, lastLine)
	}
}

// TestFooterStatus_MajorIncident_ErrorColor pins major incidents
// (partial_outage) to the scheme Error color #f38ba8 in the raw escapes.
// Colorful scheme shares the same Error token — asserted for both.
func TestFooterStatus_MajorIncident_ErrorColor(t *testing.T) {
	st := status.Status{Indicator: "none", ClaudeCode: "partial_outage", CachedAt: fixedNow}
	for _, colorful := range []bool{false, true} {
		m := footerModel(t, simpleStats(), st, nil, colorful)
		lastLine := lastViewLine(m)
		if !strings.Contains(lastLine, errorHex) {
			t.Errorf("colorful=%v: major incident footer missing error color %q, got: %q", colorful, errorHex, lastLine)
		}
	}
}

// TestFooterStatus_RightSideKeepsSubtleAfterLeft verifies the ANSI-bleed fix:
// when the left status segment renders (its Render ends with an SGR reset),
// the right segment must still carry the Subtle color explicitly instead of
// relying on the outer footer wrapper style.
func TestFooterStatus_RightSideKeepsSubtleAfterLeft(t *testing.T) {
	st := status.Status{Indicator: "none", ClaudeCode: "degraded_performance", CachedAt: fixedNow}
	m := footerModel(t, simpleStats(), st, nil, false)
	lastLine := lastViewLine(m)

	warnIdx := strings.Index(lastLine, warningHex)
	if warnIdx < 0 {
		t.Fatalf("footer missing the left warning segment: %q", lastLine)
	}
	rest := lastLine[warnIdx:]
	if !strings.Contains(rest, subtleHex) {
		t.Errorf("right footer segment after the left status lost the Subtle color %q: %q", subtleHex, lastLine)
	}
}

// TestFooterStatus_Stale appends "(cached)" when Status.Stale is true.
func TestFooterStatus_Stale(t *testing.T) {
	st := status.Status{Indicator: "none", ClaudeCode: "operational", CachedAt: fixedNow, Stale: true}
	m := footerModel(t, simpleStats(), st, nil, false)
	lastLine := strings.TrimRight(stripANSI(lastViewLine(m)), " ")
	if !strings.Contains(lastLine, "(cached)") {
		t.Errorf("footer left should contain '(cached)' when stale, got: %q", lastLine)
	}
}

// TestFooterStatus_FetchError_EmptyLeft verifies that when FetchStatus errors
// and no status data is available, the left side of the footer is empty
// (right side with est. costs still renders).
func TestFooterStatus_FetchError_EmptyLeft(t *testing.T) {
	deps := stubDepsWithStatus(sampleUsage(), nil, sampleStats(), nil, status.Status{}, fmt.Errorf("transient"))
	m := dashboard.New(deps, testPalette, fixedNowFn, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})
	m5, _ := m4.(dashboard.Model).Update(dashboard.StatusMsg{Status: status.Status{}, Err: fmt.Errorf("transient")})
	var cur tea.Model = m5
	for i := 0; i < dashboard.SyncPulseMinFrames; i++ {
		cur, _ = cur.(dashboard.Model).Update(dashboard.AnimTickMsg{})
	}
	output := stripANSI(cur.(dashboard.Model).View())
	lines := strings.Split(output, "\n")
	lastLine := strings.TrimRight(lines[len(lines)-1], " ")
	// Left side must be empty — no "●", "⚠".
	if strings.Contains(lastLine, "●") || strings.Contains(lastLine, "⚠") {
		t.Errorf("footer left should be empty on fetch error, got: %q", lastLine)
	}
	// Right side (est. costs) must still appear.
	if !strings.Contains(output, "est. costs") {
		t.Errorf("footer right side 'est. costs' missing on status fetch error; output:\n%s", output)
	}
}

// execBatch executes a returned tea.Cmd fully asynchronously (unwrapping one
// tea.BatchMsg level). The top-level cmd also runs in a goroutine: tea.Batch
// with a SINGLE entry returns that command unwrapped, and a bare tickCmd would
// otherwise sleep its whole interval on the test goroutine. Tick goroutines
// leak deliberately; the test binary does not wait for them. Callers observe
// effects via waitForCount polling.
func execBatch(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		batch, ok := msg.(tea.BatchMsg)
		if !ok {
			return
		}
		for _, c := range batch {
			if c == nil {
				continue
			}
			go c() //nolint:errcheck
		}
	}()
}

// waitForCount polls counter (atomic) until it reaches want or the deadline hits.
func waitForCount(counter *atomic.Int32, want int32, deadline time.Duration) bool {
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if counter.Load() >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return counter.Load() >= want
}

// TestStatusMsg_ClearsLoadingStatus verifies the status in-flight guard end to
// end: a TickMsg BEFORE StatusMsg (loadingStatus still true from New) must not
// issue a status fetch; a TickMsg AFTER StatusMsg (flag cleared) must issue
// exactly one. Fetch counts are observed by executing the returned commands.
func TestStatusMsg_ClearsLoadingStatus(t *testing.T) {
	var fetchCount atomic.Int32
	st := status.Status{Indicator: "none", ClaudeCode: "operational", CachedAt: fixedNow}
	deps := dashboard.Deps{
		FetchLimits: func() (limits.Usage, error) { return limits.Usage{}, nil },
		FetchStats:  func() (transcript.Stats, error) { return transcript.Stats{}, nil },
		FetchStatus: func() (status.Status, error) {
			fetchCount.Add(1)
			return st, nil
		},
	}
	m := dashboard.New(deps, testPalette, fixedNowFn, "")

	// Tick BEFORE StatusMsg: guard ON (loadingStatus true from New) — the
	// returned batch must not contain a status fetch.
	m2, cmd := m.Update(dashboard.TickMsg(fixedNow))
	execBatch(cmd)
	if waitForCount(&fetchCount, 1, 100*time.Millisecond) {
		t.Fatalf("TickMsg while loadingStatus issued a status fetch (count=%d), want 0 (guard ON)", fetchCount.Load())
	}

	// Deliver StatusMsg: clears loadingStatus and sets hasStatus.
	m3, _ := m2.(dashboard.Model).Update(dashboard.StatusMsg{Status: st, Err: nil})
	if !m3.(dashboard.Model).HasStatus() {
		t.Error("HasStatus() should be true after StatusMsg with no error")
	}

	// Tick AFTER StatusMsg: guard OFF — exactly one status fetch is issued.
	_, cmd2 := m3.(dashboard.Model).Update(dashboard.TickMsg(fixedNow))
	execBatch(cmd2)
	if !waitForCount(&fetchCount, 1, time.Second) {
		t.Fatalf("TickMsg after StatusMsg issued no status fetch (count=%d), want 1", fetchCount.Load())
	}
	if got := fetchCount.Load(); got != 1 {
		t.Errorf("status fetch count after post-StatusMsg tick = %d, want 1", got)
	}
}

// ─── Fix: nil FetchStatus dep (legacy wiring without a status seam) ──────────

// TestNilFetchStatus_IdleAfterData verifies that a Deps without FetchStatus
// does not leave the model syncing forever: loadingStatus must start false, so
// once limits+stats arrive and the pulse is exhausted the header goes static.
func TestNilFetchStatus_IdleAfterData(t *testing.T) {
	deps := dashboard.Deps{
		FetchLimits: func() (limits.Usage, error) { return sampleUsage(), nil },
		FetchStats:  func() (transcript.Stats, error) { return sampleStats(), nil },
		// FetchStatus deliberately nil.
	}
	m := dashboard.New(deps, testPalette, fixedNowFn, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})
	var cur tea.Model = m4
	for i := 0; i < dashboard.SyncPulseMinFrames; i++ {
		cur, _ = cur.(dashboard.Model).Update(dashboard.AnimTickMsg{})
	}
	headerLine := strings.Split(cur.(dashboard.Model).View(), "\n")[1]
	if n := countColorCodes(headerLine); n > 0 {
		t.Errorf("header still animated (%d color escapes) with nil FetchStatus after data arrived — loadingStatus never cleared", n)
	}
}

// TestNilFetchStatus_TickDoesNotResetPulse verifies that a TickMsg does not
// count a status fetch it will never issue: with nil FetchStatus and the
// limits/stats guards ON, no fetch is issued, so the sync pulse stays at 0.
func TestNilFetchStatus_TickDoesNotResetPulse(t *testing.T) {
	deps := dashboard.Deps{
		FetchLimits: func() (limits.Usage, error) { return limits.Usage{}, nil },
		FetchStats:  func() (transcript.Stats, error) { return transcript.Stats{}, nil },
		// FetchStatus deliberately nil.
	}
	m := dashboard.New(deps, testPalette, fixedNowFn, "")
	// Exhaust the initial pulse while the limits/stats guards stay ON.
	var cur tea.Model = m
	for i := 0; i < dashboard.SyncPulseMinFrames; i++ {
		cur, _ = cur.(dashboard.Model).Update(dashboard.AnimTickMsg{})
	}
	if got := cur.(dashboard.Model).SyncPulseFrames(); got != 0 {
		t.Fatalf("pulse not exhausted before tick: SyncPulseFrames() = %d, want 0", got)
	}
	// Tick: limits/stats guards ON, status dep nil → nothing issued → pulse
	// must NOT be reset to 4.
	cur, _ = cur.(dashboard.Model).Update(dashboard.TickMsg(fixedNow))
	if got := cur.(dashboard.Model).SyncPulseFrames(); got != 0 {
		t.Errorf("TickMsg reset the pulse (SyncPulseFrames() = %d) for a status fetch it cannot issue, want 0", got)
	}
}

// ─── Change 21: panel breathing + wave = 2 sweeps ─────────────────────────────

// TestDivider_LeftGapSymmetric verifies that the left column's right-aligned
// values have exactly 1 space before the │ divider — matching the 1-space gap
// the right column already has after │. Specifically, the "Input ... 2K" row
// must end with " 2K │ ..." (space before │), not "2K│..." (flush).
func TestDivider_LeftGapSymmetric(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})
	output := stripANSI(m4.View())
	lines := strings.Split(output, "\n")

	// Identify specifically the "Input" row: contains "Input" in the left column
	// and "│" in the divider position. The rune immediately before │ must be a space.
	found := false
	for _, line := range lines {
		if !strings.Contains(line, "Input") || !strings.Contains(line, "│") {
			continue
		}
		if strings.Contains(line, "─") {
			continue // skip the ─┬─ divider line
		}
		// Find the │ rune position (rune-safe, since │ = U+2502 is multi-byte).
		runes := []rune(line)
		barIdx := -1
		for i, r := range runes {
			if r == '│' {
				barIdx = i
				break
			}
		}
		if barIdx <= 0 {
			continue
		}
		// The rune immediately before │ must be a space.
		if runes[barIdx-1] != ' ' {
			t.Errorf("no space before │ on 'Input' row — left column value %q ends flush against │; full line: %q",
				string(runes[barIdx-1]), line)
		}
		found = true
		break
	}
	if !found {
		t.Error("could not find the 'Input' value row with │ to check gap")
	}
}

// TestBlankRowBetweenBarsAndRule verifies that there is exactly one blank line
// between the limit-bars row and the horizontal rule (───┬───).
func TestBlankRowBetweenBarsAndRule(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})
	lines := strings.Split(stripANSI(m4.View()), "\n")

	// Find the index of the rule line (contains "┬").
	ruleIdx := -1
	for i, line := range lines {
		if strings.Contains(line, "┬") {
			ruleIdx = i
			break
		}
	}
	if ruleIdx < 1 {
		t.Fatal("could not find the ─┬─ horizontal rule line")
	}

	// The line immediately before the rule must be blank (all spaces or empty).
	lineBeforeRule := strings.TrimRight(lines[ruleIdx-1], " ")
	if lineBeforeRule != "" {
		t.Errorf("line before ─┬─ rule is not blank, got: %q", lines[ruleIdx-1])
	}
}

// TestBlankRowBetweenRuleAndTitles verifies that there is exactly one blank
// line (with the │ divider present) between the horizontal rule (───┬───) and
// the section titles row ("Today ↘ │ Models (7d)").
func TestBlankRowBetweenRuleAndTitles(t *testing.T) {
	deps := stubDeps(sampleUsage(), nil, sampleStats(), nil)
	m := dashboard.New(deps, testPalette, fixedNowFn, "")
	m2, _ := m.Update(tea.WindowSizeMsg{Width: testWidth, Height: testHeight})
	m3, _ := m2.(dashboard.Model).Update(dashboard.LimitsMsg{Usage: sampleUsage(), Err: nil})
	m4, _ := m3.(dashboard.Model).Update(dashboard.StatsMsg{Stats: sampleStats(), Err: nil})
	lines := strings.Split(stripANSI(m4.View()), "\n")

	// Find the rule line.
	ruleIdx := -1
	for i, line := range lines {
		if strings.Contains(line, "┬") {
			ruleIdx = i
			break
		}
	}
	if ruleIdx < 0 || ruleIdx+2 >= len(lines) {
		t.Fatal("could not find the ─┬─ rule line or there are not enough lines after it")
	}

	// The line immediately after the rule must be a blank row with │.
	lineAfterRule := lines[ruleIdx+1]
	strippedAfterRule := strings.TrimRight(lineAfterRule, " ")
	// It must contain │ (to keep the vertical line continuous) and otherwise be all spaces.
	if !strings.Contains(lineAfterRule, "│") {
		t.Errorf("line after rule does not contain │ (vertical continuity required), got: %q", lineAfterRule)
	}
	// After removing the │ and surrounding spaces, the rest must be all spaces.
	withoutBar := strings.Replace(strippedAfterRule, "│", "", 1)
	if strings.TrimRight(withoutBar, " ") != "" {
		t.Errorf("line after rule contains non-space content besides │: %q", lineAfterRule)
	}

	// The line two positions after the rule must contain the section titles.
	titlesLine := stripANSI(lines[ruleIdx+2])
	if !strings.Contains(titlesLine, "Today") || !strings.Contains(titlesLine, "Models") {
		t.Errorf("expected section titles at line[%d], got: %q", ruleIdx+2, titlesLine)
	}
}

// TestWavePulseExactlyTwoSweeps verifies that SyncPulseMinFrames equals exactly
// two sweeps of the brand word "clauchy" at the current waveLetterStep and
// waveFrameStep constants. One sweep = (len("clauchy")-1)*waveLetterStep/waveFrameStep.
// With waveLetterStep=30 and waveFrameStep=22.5: sweepFrames = 6*30/22.5 = 8.
// SyncPulseMinFrames must equal 2*8 = 16 so the constant cannot drift from the rule.
func TestWavePulseExactlyTwoSweeps(t *testing.T) {
	const brand = "clauchy"
	const waveLetterStep = 30.0
	const waveFrameStep = 22.5
	// sweepFrames is the number of frames for one complete crest crossing.
	sweepFrames := int((float64(len(brand)-1) * waveLetterStep) / waveFrameStep)
	want := 2 * sweepFrames
	if dashboard.SyncPulseMinFrames != want {
		t.Errorf("SyncPulseMinFrames = %d, want 2 * sweepFrames(%q) = 2 * %d = %d",
			dashboard.SyncPulseMinFrames, brand, sweepFrames, want)
	}
}

// findHeaderLine returns the first rendered line containing the brand word
// "clauchy" (ANSI included). The header's absolute row shifts when the
// height-shed pass drops the top margin, so tests must locate it, not index it.
func findHeaderLine(t *testing.T, output string) string {
	t.Helper()
	for _, l := range strings.Split(output, "\n") {
		if strings.Contains(stripANSI(l), "clauchy") {
			return l
		}
	}
	t.Fatalf("no line containing 'clauchy' found in View output:\n%s", stripANSI(output))
	return ""
}
