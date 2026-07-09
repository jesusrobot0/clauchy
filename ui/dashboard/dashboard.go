// Package dashboard provides the Bubbletea TUI for Claude Code usage statistics.
//
// Key design decisions (§5):
//   - Elm architecture: Model + Init/Update/View. Concurrency is delegated to
//     tea.Cmd (tea.Batch for parallel fetches); no hand-rolled goroutines.
//   - In-flight guard: on tickMsg, a fetch is re-issued ONLY when its loading flag
//     is clear, preventing stacking of slow fetches across 5s ticks.
//   - Per-panel error states: limitsErr and statsErr are stored independently;
//     each panel renders its own degraded message (no global failure screen).
//   - Injected now func() time.Time for deterministic golden tests.
//   - Deps struct with FetchLimits/FetchStats seams for test injection.
package dashboard

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/jesusrobot0/clauchy/internal/limits"
	"github.com/jesusrobot0/clauchy/internal/severity"
	"github.com/jesusrobot0/clauchy/internal/status"
	"github.com/jesusrobot0/clauchy/internal/transcript"
	"github.com/jesusrobot0/clauchy/ui/theme"
)

// ─── Deps & message types ─────────────────────────────────────────────────────

// Deps holds the injectable data-fetching functions. main wires real closures;
// tests inject stubs.
type Deps struct {
	FetchLimits func() (limits.Usage, error)
	FetchStats  func() (transcript.Stats, error)
	// FetchStatus fetches the Claude status page summary. It should use
	// status.Cached with a 180s TTL so the 5s dashboard tick mostly cache-hits.
	// On fetch failure, return the error; the dashboard renders nothing on the
	// left side of the footer in that case.
	FetchStatus func() (status.Status, error)
}

// LimitsMsg carries the result of a FetchLimits call.
// Exported so tests can send it directly to Model.Update.
type LimitsMsg struct {
	Usage limits.Usage
	Err   error
}

// StatsMsg carries the result of a FetchStats call.
// Exported so tests can send it directly to Model.Update.
type StatsMsg struct {
	Stats transcript.Stats
	Err   error
}

// StatusMsg carries the result of a FetchStatus call.
// Exported so tests can send it directly to Model.Update.
type StatusMsg struct {
	Status status.Status
	Err    error
}

// TickMsg triggers a periodic refresh. Exported for tests.
type TickMsg time.Time

// AnimTickMsg triggers a header brand-animation frame advance.
// Issued every ~150ms; no in-flight guard needed (it is cheap and idempotent).
// Exported so tests can send it directly to Model.Update.
type AnimTickMsg struct{}

const tickInterval = 5 * time.Second
const animTickInterval = 150 * time.Millisecond

// ─── Model ───────────────────────────────────────────────────────────────────

// Model is the Bubbletea model for the clauchy dashboard.
// All fields are unexported; tests access state via exported accessor methods.
type Model struct {
	deps    Deps
	palette theme.Palette
	scheme  Scheme
	now     func() time.Time
	plan    string // optional plan label, e.g. "Max 20x"; empty → not shown

	width, height int
	frame         int // animation frame counter, incremented by each AnimTickMsg

	// syncPulseFrames is a countdown that keeps the header animation alive for
	// at least ~600ms (4 anim frames × 150ms) after a sync starts. Set to 4
	// whenever a fetch is issued; decremented on each AnimTickMsg; animation
	// is active while loadingLimits || loadingStats || syncPulseFrames > 0.
	// This prevents an ugly single-frame flicker when a cache hit resolves
	// before the first AnimTickMsg arrives.
	syncPulseFrames int

	loadingLimits bool
	loadingStats  bool
	loadingStatus bool

	limits    limits.Usage
	limitsErr error
	hasLimits bool

	stats    transcript.Stats
	statsErr error
	hasStats bool

	// statusData holds the latest Claude status page result.
	// hasStatus is true once a StatusMsg has been received without error.
	statusData status.Status
	statusErr  error
	hasStatus  bool

	lastUpdated time.Time
}

// New creates a new dashboard Model using the default MonochromeScheme.
// plan is an optional human-readable plan label (e.g. "Max 20x") derived from
// the OAuth credentials; pass "" when credentials are unavailable.
// loadingLimits and loadingStats start as true because Init() issues fetches
// immediately; setting them here (not in Init) satisfies the in-flight guard
// on the very first tick even before the fetches complete.
func New(d Deps, p theme.Palette, now func() time.Time, plan string) Model {
	return newWithScheme(d, p, MonochromeScheme(), now, plan)
}

// NewColorful creates a new dashboard Model using the ColorfulScheme (Sky
// accents, severity-mapped bars). Activated via the --colorful CLI flag.
func NewColorful(d Deps, p theme.Palette, now func() time.Time, plan string) Model {
	return newWithScheme(d, p, ColorfulScheme(), now, plan)
}

// newWithScheme is the internal constructor shared by New and NewColorful.
// loadingStatus starts true only when a FetchStatus seam is wired: with a nil
// dep, Init never issues a status fetch, so a true flag would never clear and
// the header animation would run forever.
func newWithScheme(d Deps, p theme.Palette, s Scheme, now func() time.Time, plan string) Model {
	return Model{
		deps:            d,
		palette:         p,
		scheme:          s,
		now:             now,
		plan:            plan,
		loadingLimits:   true,
		loadingStats:    true,
		loadingStatus:   d.FetchStatus != nil,
		syncPulseFrames: 4, // guarantee at least 4 anim frames of animation from the initial fetch pair
	}
}

// ─── Exported accessors for testing ──────────────────────────────────────────

func (m Model) LimitsErr() error     { return m.limitsErr }
func (m Model) StatsErr() error      { return m.statsErr }
func (m Model) HasLimits() bool      { return m.hasLimits }
func (m Model) HasStats() bool       { return m.hasStats }
func (m Model) HasStatus() bool      { return m.hasStatus }
func (m Model) Width() int           { return m.width }
func (m Model) Height() int          { return m.height }
func (m Model) Frame() int           { return m.frame }
func (m Model) SyncPulseFrames() int { return m.syncPulseFrames }

// ─── Bubbletea interface ──────────────────────────────────────────────────────

// Init issues the initial fetch commands and starts the 5s tick timer
// and the 150ms animation tick timer for the header brand animation.
// The loading flags are already set by New(); Init only issues the commands.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		fetchLimitsCmd(m.deps),
		fetchStatsCmd(m.deps),
		fetchStatusCmd(m.deps),
		tickCmd(),
		animTickCmd(),
	)
}

// Update handles all incoming messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case LimitsMsg:
		m.loadingLimits = false
		m.limits = msg.Usage
		m.limitsErr = msg.Err
		m.hasLimits = msg.Err == nil && !msg.Usage.CachedAt.IsZero()
		m.lastUpdated = m.now()
		return m, nil

	case StatsMsg:
		m.loadingStats = false
		m.stats = msg.Stats
		m.statsErr = msg.Err
		m.hasStats = msg.Err == nil
		m.lastUpdated = m.now()
		return m, nil

	case StatusMsg:
		m.loadingStatus = false
		m.statusData = msg.Status
		m.statusErr = msg.Err
		m.hasStatus = msg.Err == nil
		return m, nil

	case TickMsg:
		var cmds []tea.Cmd
		// In-flight guard: only re-issue a fetch when its loading flag is clear.
		var issuedFetch bool
		if !m.loadingLimits {
			m.loadingLimits = true
			issuedFetch = true
			cmds = append(cmds, fetchLimitsCmd(m.deps))
		}
		if !m.loadingStats {
			m.loadingStats = true
			issuedFetch = true
			cmds = append(cmds, fetchStatsCmd(m.deps))
		}
		// The status branch also requires a wired FetchStatus dep: with a nil
		// dep there is no fetch to issue, so neither the loading flag nor the
		// sync pulse may be touched for it.
		if !m.loadingStatus && m.deps.FetchStatus != nil {
			m.loadingStatus = true
			issuedFetch = true
			cmds = append(cmds, fetchStatusCmd(m.deps))
		}
		// Reset the minimum-pulse countdown whenever a new sync starts.
		if issuedFetch {
			m.syncPulseFrames = 4
		}
		cmds = append(cmds, tickCmd())
		return m, tea.Batch(cmds...)

	case AnimTickMsg:
		// Advance the animation frame counter and re-issue the tick.
		// No in-flight guard needed — the tick is cheap and idempotent.
		m.frame++
		// Decrement the minimum-pulse countdown. When a sync started just before
		// a fast cache-hit response, syncPulseFrames keeps the animation alive
		// for at least ~600ms (4 frames × 150ms) so there is no single-frame flicker.
		if m.syncPulseFrames > 0 {
			m.syncPulseFrames--
		}
		return m, animTickCmd()

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyRunes:
			if string(msg.Runes) == "q" {
				return m, tea.Quit
			}
		case tea.KeyEsc, tea.KeyCtrlC:
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}

	return m, nil
}

// ─── Scheme ───────────────────────────────────────────────────────────────────

// Scheme holds every color token used by the dashboard View.
// Two built-in constructors are provided: MonochromeScheme (the default,
// matching the user's original black-and-white reference design) and
// ColorfulScheme (the original Sky-accented look, activated with --colorful).
//
// Colors are expressed as Lipgloss-compatible hex strings ("#RRGGBB").
// An empty string ("") means "use the terminal's default foreground".
type Scheme struct {
	// SectionTitle is the color for "Today ↘", "Models (7d)", "This Week".
	// Empty string = terminal default (white/bold only).
	SectionTitle string
	// Accent is the fill color for model bars and Total digit rows.
	// Empty string = terminal default foreground.
	Accent string
	// BarFill is the fill color for limit bars (Session / Week / per-model).
	// In ColorfulScheme this is severity-mapped per item; in MonochromeScheme
	// it is always this fixed color.
	// Empty string = terminal default foreground.
	BarFill string
	// Subtle is the color for reset times, plan label, footer text.
	Subtle string
	// Border is the color for column dividers and horizontal rules.
	Border string
	// Error is the color for error messages (kept red in both schemes —
	// errors must alarm regardless of color mode).
	Error string
	// Warning is the color for minor-incident status text in the footer
	// (kept yellow in both schemes — incident colors are semantic, like Error).
	Warning string
	// Total is the color for the big-digit Total block in Today.
	Total string
	// ColorfulBars reports whether limit-bar fill should be severity-mapped
	// (ColorfulScheme) or fixed to BarFill (MonochromeScheme).
	ColorfulBars bool
	// GrayscaleAnim switches the header "clauchy" animation from the rainbow
	// hue wave to a white/gray lightness wave, so the animation matches the
	// monochrome look while keeping the product signature alive.
	GrayscaleAnim bool
}

// MonochromeScheme returns the default scheme: white/gray tones, no Sky hue,
// matching the user's original black-and-white reference design.
// Errors keep red because they SHOULD alarm regardless of color mode.
// The header "clauchy" animation is sync-gated: active only while data is
// loading (loadingLimits || loadingStats || loadingStatus || syncPulseFrames > 0);
// when idle the title renders as plain bold terminal-default (Change 19).
func MonochromeScheme() Scheme {
	return Scheme{
		SectionTitle:  "",        // terminal default bold-white
		Accent:        "",        // terminal default foreground
		BarFill:       "#ffffff", // white fill for all limit / model bars
		Subtle:        "#6c7086", // Catppuccin Overlay0 — dim gray hints
		Border:        "#585b70", // Catppuccin Surface2 — dividers
		Error:         "#f38ba8", // Catppuccin Red — keep alarm color
		Warning:       "#f9e2af", // Catppuccin Yellow — semantic incident color, kept in mono
		Total:         "",        // terminal default foreground (white)
		ColorfulBars:  false,
		GrayscaleAnim: true, // header wave in white/gray, not rainbow
	}
}

// ColorfulScheme returns the original Sky-accented color scheme,
// with severity-mapped limit bars and Sky (#89dceb) accents throughout.
func ColorfulScheme() Scheme {
	return Scheme{
		SectionTitle: "#89dceb", // Sky
		Accent:       "#89dceb", // Sky
		BarFill:      "#89dceb", // Sky (overridden per-bar by severity in ColorfulBars mode)
		Subtle:       "#6c7086", // Catppuccin Overlay0
		Border:       "#585b70", // Catppuccin Surface2
		Error:        "#f38ba8", // Catppuccin Red
		Warning:      "#f9e2af", // Catppuccin Yellow
		Total:        "#89dceb", // Sky
		ColorfulBars: true,
	}
}

// ─── View ─────────────────────────────────────────────────────────────────────

// View renders the full dashboard as a borderless content block that flows
// edge-to-edge with 1-space uniform horizontal padding so text does not kiss
// the terminal window edge.
//
// Card anatomy (no outer frame — the floating terminal window provides one):
//
//	clauchy                                                          Max 20x
//
//	Session 22%   Week 31%   Fable 24%         Resets Jul 9, 4:00pm
//	───────────────────────────────────┬──────────────────────────────────
//	Today ↘                             │ Models (7d)
//	Input                   2K         │ claude-opus-4-5   327.0M · 99%
//	...                                │ ...
//	                                   │ This Week
//	Total               84.4M          │ 240.0M tokens              ~$463
//	API equiv.          ~$167          │                             2d streak
//
//	● operational                est. costs · pricing from Jul 7, 2026 · updated 15:04
//
// Internal structure:
//   - Header: animated-while-syncing "clauchy" brand, plan label right-aligned.
//   - Vertical column divider "│" between Today and the right column.
//   - Column divider line uses ─ with a ┬ at the divider column.
//   - Footer: Claude status on the left, pricing/updated info on the right.
func (m Model) View() string {
	outerWidth := m.width
	if m.width == 0 || m.width < 72 {
		outerWidth = 80
	}
	// contentWidth = visible text area with 1-space padding on each side.
	// No border glyphs to subtract — content is 2 chars wider than the old
	// bordered layout (which subtracted 4: 2 for │ chars + 2 for spaces).
	const pad = 3
	contentWidth := outerWidth - pad*2

	// rowStyle pads content to exactly contentWidth visual chars so every
	// line fills the card uniformly.
	rowStyle := lipgloss.NewStyle().Width(contentWidth)

	var lines []string

	// addRow pads each sub-line of a (possibly multi-line) string to
	// contentWidth and prepends/appends the 3-space horizontal padding.
	addRow := func(s string) {
		for _, sub := range strings.Split(s, "\n") {
			padded := rowStyle.Render(sub)
			lines = append(lines, strings.Repeat(" ", pad)+padded)
		}
	}

	// ── Top margin + Header row ───────────────────────────────────────────────
	addRow("") // blank top margin above header
	addRow(m.viewHeader(contentWidth))
	addRow("") // blank spacer

	// ── Inline limit bars (1–2 rows) ─────────────────────────────────────────
	for _, lr := range m.viewLimitBarsInline(contentWidth) {
		addRow(lr)
	}

	// ── Column divider ────────────────────────────────────────────────────────
	addRow(m.viewColDivider(contentWidth))

	// ── Two-column body ───────────────────────────────────────────────────────
	addRow(m.viewTwoColumns(contentWidth))

	// ── Footer row: plain subtle text, full-width, bottom-anchored ──────────────
	// When height is known, pad with blank lines so the footer sits on the last
	// row of the window (content top-anchored, footer bottom-anchored).
	// The footer itself counts as 1 line; the blank separator before it counts
	// as 1 more, so we need (height - len(lines) - 2) blank lines in between.
	footerStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color(m.scheme.Subtle)).
		Width(outerWidth).
		Padding(0, pad)
	footer := footerStyle.Render(m.viewFooterContent(contentWidth))

	if m.height > 0 {
		// Total lines so far + 1 blank separator + footer = m.height target.
		blankCount := m.height - len(lines) - 2
		if blankCount < 0 {
			blankCount = 0
		}
		for i := 0; i < blankCount; i++ {
			lines = append(lines, "")
		}
	}
	lines = append(lines, "")
	lines = append(lines, footer)

	return strings.Join(lines, "\n")
}

// viewHeader renders the "clauchy" brand title (Change 19).
// While syncing (loadingLimits || loadingStats || loadingStatus || syncPulseFrames > 0)
// the title is animated via RainbowText (colorful scheme) or GrayscaleText (mono scheme).
// When idle it renders static BOLD terminal-default (no per-letter color codes).
// When a plan label (e.g. "Max 20x") is set it is shown right-aligned on
// the same row in the scheme's Subtle color. The two sides are separated by
// enough spaces to fill contentWidth exactly.
func (m Model) viewHeader(contentWidth int) string {
	const brand = "clauchy"
	syncing := m.loadingLimits || m.loadingStats || m.loadingStatus || m.syncPulseFrames > 0

	var title string
	if syncing {
		// Animated: per-letter color wave matching the scheme.
		if m.scheme.GrayscaleAnim {
			title = GrayscaleText(brand, m.frame)
		} else {
			title = RainbowText(brand, m.frame)
		}
	} else {
		// Idle: plain bold, terminal-default foreground.
		title = lipgloss.NewStyle().Bold(true).Render(brand)
	}

	if m.plan == "" {
		return title
	}
	planRendered := lipgloss.NewStyle().Foreground(lipgloss.Color(m.scheme.Subtle)).Render(m.plan)
	titleWidth := len(brand) // brand is ASCII-only, so byte length == visual width
	planWidth := lipgloss.Width(planRendered)
	gap := contentWidth - titleWidth - planWidth
	if gap < 1 {
		gap = 1
	}
	return title + strings.Repeat(" ", gap) + planRendered
}

// viewLimitBarsInline renders the compact limit bars section.
// Returns 1 row (error / loading) or 2 rows (label+% row + bar row).
func (m Model) viewLimitBarsInline(contentWidth int) []string {
	if m.limitsErr != nil {
		msg := lipgloss.NewStyle().Foreground(lipgloss.Color(m.scheme.Error)).
			Render("Run claude to log in")
		return []string{msg}
	}
	if m.loadingLimits && !m.hasLimits {
		return []string{"Loading…"}
	}

	u := m.limits

	type item struct {
		label string
		pct   float64
		col   lipgloss.Color
	}

	// In ColorfulBars mode each bar gets a severity-mapped color from the palette.
	// In MonochromeScheme every bar gets the scheme's BarFill (white by default).
	barColor := func(pct float64) lipgloss.Color {
		if m.scheme.ColorfulBars {
			return lipgloss.Color(m.palette.Hex(severity.Classify(pct)))
		}
		return lipgloss.Color(m.scheme.BarFill)
	}

	items := []item{
		{"Session", u.FiveHour.Utilization, barColor(u.FiveHour.Utilization)},
		{"Week", u.SevenDay.Utilization, barColor(u.SevenDay.Utilization)},
	}
	for _, ml := range u.Models {
		name := ml.Name
		runes := []rune(name)
		if len(runes) > 10 {
			name = string(runes[:10])
		}
		items = append(items, item{
			name, ml.Utilization, barColor(ml.Utilization),
		})
	}

	// Limit bars are ~2x the old 10-cell width; 20 cells fits cleanly at 80 cols
	// (3 × 20 + 2 × 3 sep = 66 ≤ 76 content width).
	const barLen = 20
	const sep = "   "

	trackStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(barTrackColor))

	var labelParts, barParts []string
	for _, it := range items {
		st := lipgloss.NewStyle().Foreground(it.col)
		// Pad every label to the bar width so the label row and the bar row
		// form true columns — otherwise the rows drift apart and a label ends
		// up hovering over its neighbor's bar.
		label := st.Render(fmt.Sprintf("%s %d%%", it.label, int(it.pct)))
		if w := lipgloss.Width(label); w < barLen {
			label += strings.Repeat(" ", barLen-w)
		}
		labelParts = append(labelParts, label)

		raw := BuildBar(it.pct, barLen)
		// Split the raw bar into cap + body segments so each can be styled
		// independently: fill color for filled cells and the left cap when fill>0,
		// faint/track color for empty cells and the right cap unless at 100%.
		leftFill, rightFill := BuildBarCaps(it.pct)
		fillCount := strings.Count(raw, "█")
		trackCount := strings.Count(raw, "░")
		leftCapStr := barLeftCap
		rightCapStr := barRightCap
		if leftFill {
			leftCapStr = st.Render(leftCapStr)
		} else {
			leftCapStr = trackStyle.Render(leftCapStr)
		}
		if rightFill {
			rightCapStr = st.Render(rightCapStr)
		} else {
			rightCapStr = trackStyle.Render(rightCapStr)
		}
		barStr := leftCapStr +
			st.Render(strings.Repeat("█", fillCount)) +
			trackStyle.Render(strings.Repeat("█", trackCount)) +
			rightCapStr
		barParts = append(barParts, barStr)
	}

	labelsRow := strings.Join(labelParts, sep)
	barsRow := strings.Join(barParts, sep)

	// Append earliest reset time: right-aligned on the labels row when it fits
	// (gap ≥ 1 with at least 1 space between labels and reset text), or on its
	// own right-aligned line ABOVE the labels when it doesn't.
	// "Fits" = leftW + 1 + rightW ≤ contentWidth.
	// This prevents the labels row from overflowing contentWidth, which would
	// trigger lipgloss wrapping and break line-count math in View().
	if t := earliestReset(u); !t.IsZero() {
		resetStr := "Resets " + t.Local().Format("Jan 2, 3:04pm")
		resetStyled := lipgloss.NewStyle().Foreground(lipgloss.Color(m.scheme.Subtle)).Render(resetStr)
		leftW := lipgloss.Width(labelsRow)
		rightW := lipgloss.Width(resetStr)
		g := contentWidth - leftW - rightW
		if g >= 1 {
			// Fits inline: append right-aligned on the same row.
			labelsRow = labelsRow + strings.Repeat(" ", g) + resetStyled
			return []string{labelsRow, barsRow}
		}
		// Does not fit inline: emit reset text on its own right-aligned line above labels.
		resetGap := contentWidth - rightW
		if resetGap < 0 {
			resetGap = 0
		}
		resetLine := strings.Repeat(" ", resetGap) + resetStyled
		return []string{resetLine, labelsRow, barsRow}
	}

	return []string{labelsRow, barsRow}
}

// viewColDivider renders the "──…──┬─…──" line separating the two columns.
// The ┬ sits at the same position as the "│" in viewTwoColumns, and one dash
// follows it (matching the space after │ in the body rows).
func (m Model) viewColDivider(contentWidth int) string {
	const sepWidth = 2
	leftW := (contentWidth - sepWidth) / 2
	rightW := contentWidth - sepWidth - leftW
	// "┬─" = 2 chars (matches "│ " separator width)
	line := strings.Repeat("─", leftW) + "┬" + strings.Repeat("─", rightW+1)
	return lipgloss.NewStyle().Foreground(lipgloss.Color(m.scheme.Border)).Render(line)
}

// viewTwoColumns renders Today (left) and Models+Week (right) side-by-side.
// The inner separator is "│ " (bar + space) so the right column has a visual
// gap matching the left column's leading-space convention.
func (m Model) viewTwoColumns(contentWidth int) string {
	// separator is "│ " (2 chars): 1 for bar, 1 for breathing room
	const sepWidth = 2
	leftWidth := (contentWidth - sepWidth) / 2
	rightWidth := contentWidth - sepWidth - leftWidth

	leftBlock := padLines(m.buildTodayContent(leftWidth), leftWidth)
	rightBlock := padLines(m.buildRightContent(rightWidth), rightWidth)

	barStyled := lipgloss.NewStyle().Foreground(lipgloss.Color(m.scheme.Border)).Render("│")
	sep := barStyled + " "
	return zipColumns(leftBlock, sep, rightBlock)
}

// buildTodayContent returns the raw multi-line string for the Today column.
func (m Model) buildTodayContent(colWidth int) string {
	hdrStyle := lipgloss.NewStyle().Bold(true)
	if m.scheme.SectionTitle != "" {
		hdrStyle = hdrStyle.Foreground(lipgloss.Color(m.scheme.SectionTitle))
	}
	hdr := hdrStyle.Render("Today ↘")

	if m.statsErr != nil {
		return hdr + "\n" + lipgloss.NewStyle().Foreground(lipgloss.Color(m.scheme.Error)).
			Render(fmt.Sprintf("Error: %v", m.statsErr))
	}
	if m.loadingStats && !m.hasStats {
		return hdr + "\n" + "Loading…"
	}

	d := m.stats.Today
	total := d.InputTokens + d.OutputTokens + d.CacheWriteTokens + d.CacheReadTokens

	totalStyle := lipgloss.NewStyle().Bold(true)
	if m.scheme.Total != "" {
		totalStyle = totalStyle.Foreground(lipgloss.Color(m.scheme.Total))
	}

	rows := []string{
		hdr,
		lv("Input", humanize(d.InputTokens), colWidth),
		lv("Output", humanize(d.OutputTokens), colWidth),
		lv("Cache Read", humanize(d.CacheReadTokens), colWidth),
		lv("Cache Write", humanize(d.CacheWriteTokens), colWidth),
		"",
		"Total",
	}
	// 3-row big-digit block for the Total value (btop-clock style).
	// Each big-digit row is accent-colored, no extra indent (flush with header).
	for _, br := range BigDigits(humanize(total)) {
		rows = append(rows, totalStyle.Render(br))
	}
	rows = append(rows,
		lv("API equiv.", fmtCost(d.Cost), colWidth),
		lv("Sessions", fmt.Sprintf("%d", d.Sessions), colWidth),
		lv("Messages", fmt.Sprintf("%d", d.Messages), colWidth),
	)
	return strings.Join(rows, "\n")
}

// buildRightContent returns the raw multi-line string for the right column
// (Models 7d then This Week with streak folded in).
func (m Model) buildRightContent(colWidth int) string {
	var parts []string

	// ── Models (7d) ──────────────────────────────────────────────────────────
	hdrStyle := lipgloss.NewStyle().Bold(true)
	if m.scheme.SectionTitle != "" {
		hdrStyle = hdrStyle.Foreground(lipgloss.Color(m.scheme.SectionTitle))
	}
	modHdr := hdrStyle.Render("Models (7d)")
	parts = append(parts, modHdr)

	if m.statsErr != nil {
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Color(m.scheme.Error)).
			Render(fmt.Sprintf("Error: %v", m.statsErr)))
	} else if m.loadingStats && !m.hasStats {
		parts = append(parts, "Loading…")
	} else if len(m.stats.Models7d) == 0 {
		parts = append(parts, "No model data this week")
	} else {
		var total7d int
		for _, mu := range m.stats.Models7d {
			total7d += mu.Input + mu.Output
		}

		modelBarAccentColor := m.scheme.Accent
		if modelBarAccentColor == "" {
			modelBarAccentColor = "#ffffff"
		}
		barAccent := lipgloss.NewStyle().Foreground(lipgloss.Color(modelBarAccentColor))
		trackStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(barTrackColor))
		// Model bars span the usable column width for visual presence.
		modelBarLen := colWidth
		if modelBarLen < 4 {
			modelBarLen = 4
		}

		for _, mu := range m.stats.Models7d {
			modelTotal := mu.Input + mu.Output
			pct := 0.0
			if total7d > 0 {
				pct = float64(modelTotal) / float64(total7d) * 100
			}
			parts = append(parts, fmt.Sprintf("%s  %s · %d%%", PrettyModelName(mu.Model), humanize(modelTotal), int(pct)))

			raw := BuildBar(pct, modelBarLen)
			leftFill, rightFill := BuildBarCaps(pct)
			fillCount := strings.Count(raw, "█")
			trackCount := strings.Count(raw, "░")
			leftCapStr := barLeftCap
			rightCapStr := barRightCap
			if leftFill {
				leftCapStr = barAccent.Render(leftCapStr)
			} else {
				leftCapStr = trackStyle.Render(leftCapStr)
			}
			if rightFill {
				rightCapStr = barAccent.Render(rightCapStr)
			} else {
				rightCapStr = trackStyle.Render(rightCapStr)
			}
			barStr := leftCapStr +
				barAccent.Render(strings.Repeat("█", fillCount)) +
				trackStyle.Render(strings.Repeat("█", trackCount)) +
				rightCapStr
			parts = append(parts, barStr)
			// Blank line after each bar so model entries breathe
			// (name+numbers / bar / blank — per Change 4).
			parts = append(parts, "")
		}
	}

	parts = append(parts, "")

	// ── This Week ────────────────────────────────────────────────────────────
	weekHdr := hdrStyle.Render("This Week")
	parts = append(parts, weekHdr)

	if m.statsErr != nil {
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Color(m.scheme.Error)).
			Render(fmt.Sprintf("Error: %v", m.statsErr)))
	} else if m.loadingStats && !m.hasStats {
		parts = append(parts, "Loading…")
	} else {
		w := m.stats.Week
		weekTotal := w.InputTokens + w.OutputTokens + w.CacheWriteTokens + w.CacheReadTokens
		parts = append(parts, lv(humanize(weekTotal)+" tokens", fmtCost(w.Cost), colWidth))

		// Streak lives inside This Week.
		if m.stats.Streak > 0 {
			parts = append(parts, lv("", fmt.Sprintf("%dd streak", m.stats.Streak), colWidth))
		}
	}

	return strings.Join(parts, "\n")
}

// viewFooterContent builds the left+right-aligned footer text (Change 19).
// The left side shows Claude status page data:
//   - When FetchStatus errored (statusErr != nil), status has not arrived yet
//     (hasStatus == false), or the Status is a zero value (CachedAt.IsZero() —
//     "no data", same gate as waybar's buildTooltip) → empty left side.
//   - When operational (Worst == "operational") → "● operational" in Subtle
//     color (the short form always fits at 80 cols beside the pricing text).
//   - When incident (Worst == minor/major/critical) → "⚠ " + status.HumanLabel
//     in the scheme Warning color for minor, Error color for major/critical.
//   - When Stale → append " (cached)".
//
// The right side (est. costs · pricing · updated) is styled with Subtle HERE,
// not by the outer footer wrapper: the left segment's Render ends with an SGR
// reset that would otherwise kill the wrapper's style mid-row and leave the
// right side uncolored.
// contentWidth is the available text area inside the footer's padding.
func (m Model) viewFooterContent(contentWidth int) string {
	subtle := lipgloss.NewStyle().Foreground(lipgloss.Color(m.scheme.Subtle))

	var left string
	var leftW int

	var rightParts []string
	if m.stats.PricingDate != "" {
		rightParts = append(rightParts, "est. costs · pricing from "+humanDate(m.stats.PricingDate))
	}
	if !m.lastUpdated.IsZero() {
		rightParts = append(rightParts, "updated "+m.lastUpdated.Format("15:04"))
	}
	rightRaw := strings.Join(rightParts, " · ")
	rightW := lipgloss.Width(rightRaw)
	right := rightRaw
	if rightRaw != "" {
		right = subtle.Render(rightRaw)
	}

	// Build the status left side only when REAL status data arrived: a zero
	// CachedAt means "no data" (zero-value fallback), never an incident.
	if m.hasStatus && m.statusErr == nil && !m.statusData.CachedAt.IsZero() {
		st := m.statusData
		worst := status.Worst(st)

		var raw string
		var leftStyle lipgloss.Style

		switch worst {
		case "operational":
			raw = "● operational"
			leftStyle = subtle
		case "minor":
			raw = "⚠ " + status.HumanLabel(st)
			leftStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(m.scheme.Warning))
		default: // "major" or "critical"
			raw = "⚠ " + status.HumanLabel(st)
			leftStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(m.scheme.Error))
		}

		if st.Stale {
			raw += " (cached)"
		}

		candidateW := lipgloss.Width(raw)
		// Only show the left side when it fits alongside the right side with at least 1 space gap.
		if candidateW+1+rightW <= contentWidth {
			left = leftStyle.Render(raw)
			leftW = candidateW
		}
		// If it doesn't fit, left stays "" and leftW stays 0; right side fills the row.
	}
	// If no status data: left = "" and leftW = 0 → right side fills the row.

	g := contentWidth - leftW - rightW
	if g < 1 {
		g = 1
	}
	return left + strings.Repeat(" ", g) + right
}

// humanDate converts an ISO date ("2026-07-07") into the human form used
// across the card ("Jul 7, 2026"). Unparseable input is returned unchanged —
// showing the raw date beats hiding it.
func humanDate(iso string) string {
	t, err := time.Parse("2006-01-02", iso)
	if err != nil {
		return iso
	}
	return t.Format("Jan 2, 2006")
}

// ─── Layout helpers ───────────────────────────────────────────────────────────

// lv returns "label" + right-aligned "value" filling colWidth visible chars.
func lv(label, value string, colWidth int) string {
	labelW := lipgloss.Width(label)
	valueW := lipgloss.Width(value)
	g := colWidth - labelW - valueW
	if g < 1 {
		g = 1
	}
	return label + strings.Repeat(" ", g) + value
}

// padLines ensures every line of a multi-line string is exactly width visual
// chars wide, padding with spaces where needed.
func padLines(s string, width int) string {
	lines := strings.Split(s, "\n")
	result := make([]string, len(lines))
	for i, line := range lines {
		w := lipgloss.Width(line)
		if w < width {
			result[i] = line + strings.Repeat(" ", width-w)
		} else {
			result[i] = line
		}
	}
	return strings.Join(result, "\n")
}

// zipColumns interleaves lines from two fixed-width blocks with a separator.
// Both blocks must be pre-padded (via padLines) so line widths are uniform;
// zipColumns extends whichever block is shorter with blank lines of the same
// width, ensuring the separator stays at a consistent column in every row.
func zipColumns(left, sep, right string) string {
	leftLines := strings.Split(left, "\n")
	rightLines := strings.Split(right, "\n")

	// Measure column widths from the first line of each block.
	leftColW, rightColW := 0, 0
	if len(leftLines) > 0 {
		leftColW = lipgloss.Width(leftLines[0])
	}
	if len(rightLines) > 0 {
		rightColW = lipgloss.Width(rightLines[0])
	}

	maxLen := len(leftLines)
	if len(rightLines) > maxLen {
		maxLen = len(rightLines)
	}

	// Extend the shorter block with blank lines of the right width.
	for len(leftLines) < maxLen {
		leftLines = append(leftLines, strings.Repeat(" ", leftColW))
	}
	for len(rightLines) < maxLen {
		rightLines = append(rightLines, strings.Repeat(" ", rightColW))
	}

	result := make([]string, maxLen)
	for i := range result {
		result[i] = leftLines[i] + sep + rightLines[i]
	}
	return strings.Join(result, "\n")
}

// earliestReset returns the earliest non-zero ResetsAt from a Usage struct.
func earliestReset(u limits.Usage) time.Time {
	var t time.Time
	for _, w := range []time.Time{u.FiveHour.ResetsAt, u.SevenDay.ResetsAt} {
		if !w.IsZero() && (t.IsZero() || w.Before(t)) {
			t = w
		}
	}
	for _, ml := range u.Models {
		if !ml.ResetsAt.IsZero() && (t.IsZero() || ml.ResetsAt.Before(t)) {
			t = ml.ResetsAt
		}
	}
	return t
}

// ─── Formatting helpers ───────────────────────────────────────────────────────

// humanize converts a token count to a compact human-readable string:
//   - < 1 000                 → plain integer            ("550")
//   - 1 000–999 999           → integer K, no decimal    ("2K", "184K")
//   - 1 000 000–999 999 999   → one-decimal M             ("82.1M", "327.0M")
//   - 1 000 000 000–999 ...   → one-decimal B             ("1.0B", "1.1B")
//   - ≥ 1 000 000 000 000     → one-decimal T             ("1.0T", "1.5T")
func humanize(n int) string {
	switch {
	case n < 1_000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%dK", n/1_000)
	case n < 1_000_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n < 1_000_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	default:
		return fmt.Sprintf("%.1fT", float64(n)/1_000_000_000_000)
	}
}

// BigDigits renders a compact string (supporting digits 0–9, the decimal
// point '.', and unit suffixes K/M/B/T) as a 4-row slice of strings using
// Unicode half-block characters (▀ ▄ █ and space).
//
// Design: each digit glyph is 3 chars wide × 4 terminal rows tall, encoded
// from a 6×4 pixel bitmap where every terminal row carries 2 pixel rows via
// half-block characters:
//
//	' ' = both pixels off
//	'▀' = top pixel on, bottom off
//	'▄' = top pixel off, bottom on
//	'█' = both pixels on
//
// This gives each digit an effective 8-pixel height, which is enough to render
// all ten digits unambiguously (clear counters, distinct middle bars, etc.).
//
// '.' is 2 chars wide (a bottom-aligned double block for visibility).
// Unit letters (K/M/B/T) are 2 chars wide with the letter on row 2 (baseline-aligned).
//
// A 1-space gap separates every adjacent glyph pair.
// All four returned strings have the same visual width.
//
// Exported so it can be table-tested from the dashboard_test package.
func BigDigits(s string) []string {
	type glyph = [4]string
	glyphMap := map[rune]glyph{
		// Digits: 3 chars wide (compact), pixel-encoded from 6×4 bitmaps.
		// 0 — box with open interior (visible counter)
		'0': {"█▀█", "█ █", "█ █", "▀▀▀"},
		// 1 — centre bar with top serif and base
		'1': {"▀█ ", " █ ", " █ ", "▀▀▀"},
		// 2 — Z-shape: top+right, mid crossover, left+bottom
		'2': {"▀▀█", "▄▄█", "█  ", "▀▀▀"},
		// 3 — right-side staircase
		'3': {"▀▀█", " ▄█", "  █", "▀▀▀"},
		// 4 — open-top fork, crossbar, right arm only
		'4': {"█ █", "█▄█", "  █", "  ▀"},
		// 5 — S-shape: top-left, middle, bottom-right
		'5': {"█▀▀", "█▄▄", "  █", "▀▀▀"},
		// 6 — like 5 but lower half closes into a box
		'6': {"█▀▀", "█▄▄", "█ █", "▀▀▀"},
		// 7 — top bar + straight right descent
		'7': {"▀▀█", "  █", "  █", "  ▀"},
		// 8 — two chambers stacked
		'8': {"█▀█", "█▄█", "█ █", "▀▀▀"},
		// 9 — upper box + right arm
		'9': {"█▀█", "█▄█", "  █", "▀▀▀"},
		// '.' — bottom-aligned block, 1 char wide
		'.': {" ", " ", "▄", "▀"},
		// Unit letters: a single normal-size letter near the baseline (row 2).
		'K': {"  ", "  ", " K", "  "},
		'M': {"  ", "  ", " M", "  "},
		'B': {"  ", "  ", " B", "  "},
		'T': {"  ", "  ", " T", "  "},
	}

	var rows [4]strings.Builder
	first := true
	for _, r := range s {
		g, ok := glyphMap[r]
		if !ok {
			continue
		}
		if !first {
			for i := range rows {
				rows[i].WriteByte(' ')
			}
		}
		first = false
		for i, line := range g {
			rows[i].WriteString(line)
		}
	}
	return []string{rows[0].String(), rows[1].String(), rows[2].String(), rows[3].String()}
}

// prettyModelNameDateRe matches a trailing 8-digit date suffix like "-20241022".
var prettyModelNameDateRe = regexp.MustCompile(`-\d{8}$`)

// prettyModelNameBracketRe matches a trailing bracketed suffix like "[1m]".
var prettyModelNameBracketRe = regexp.MustCompile(`\[[^\]]+\]$`)

// PrettyModelName converts a canonical Claude model ID into a friendly display
// name by structural pattern matching. It never invents a name — any input that
// does not match the expected patterns is returned unchanged.
//
// Rules (applied in order):
//  1. If the ID does not start with "claude-", return it unchanged.
//  2. Strip the leading "claude-" prefix.
//  3. Strip a trailing bracketed suffix (e.g. "[1m]").
//  4. Strip a trailing 8-digit date suffix (e.g. "-20241022").
//  5. Split by "-". Identify the family segment: a segment composed only of
//     Unicode letters (not digits). The family must be the first or last segment.
//  6. Normal form (family first):  "opus-4-8"    → "Opus 4.8"
//     Legacy inverted form (family last): "3-5-sonnet" → "Sonnet 3.5"
//  7. Join version segments (digit-only) with ".". Capitalize family.
//  8. If no valid family is found, return the original input unchanged.
func PrettyModelName(id string) string {
	const prefix = "claude-"
	if !strings.HasPrefix(id, prefix) {
		return id
	}
	// Strip prefix and normalisation suffixes.
	rest := strings.TrimPrefix(id, prefix)
	rest = prettyModelNameBracketRe.ReplaceAllString(rest, "")
	rest = prettyModelNameDateRe.ReplaceAllString(rest, "")

	if rest == "" {
		return id
	}

	parts := strings.Split(rest, "-")
	if len(parts) == 0 {
		return id
	}

	// isWordOnly returns true if every rune in s is a letter (not a digit).
	isWordOnly := func(s string) bool {
		if s == "" {
			return false
		}
		for _, r := range s {
			if !unicode.IsLetter(r) {
				return false
			}
		}
		return true
	}

	var family string
	var versionParts []string

	switch {
	case isWordOnly(parts[0]):
		// Normal form: family first, version numbers follow.
		family = parts[0]
		versionParts = parts[1:]
	case isWordOnly(parts[len(parts)-1]):
		// Legacy inverted form: version numbers first, family last.
		family = parts[len(parts)-1]
		versionParts = parts[:len(parts)-1]
	default:
		// Cannot identify a family word — return unchanged.
		return id
	}

	// Capitalize first letter of family.
	runes := []rune(family)
	runes[0] = unicode.ToUpper(runes[0])
	friendly := string(runes)

	if len(versionParts) > 0 {
		friendly += " " + strings.Join(versionParts, ".")
	}
	return friendly
}

// fmtCost formats a cost as a human-readable estimate:
//   - zero    → "$0"
//   - ≥ $1    → "~$N"    (rounded integer, e.g. "~$167")
//   - < $1    → "~$0.XX" (two decimal places, e.g. "~$0.05")
func fmtCost(c float64) string {
	if c == 0 {
		return "$0"
	}
	if c >= 1.0 {
		return fmt.Sprintf("~$%d", int(math.Round(c)))
	}
	return fmt.Sprintf("~$%.2f", c)
}

// ─── Color / animation helpers ────────────────────────────────────────────────

// hslToHex converts hue [0,360), saturation [0,1], lightness [0,1] to "#RRGGBB".
func hslToHex(h, s, l float64) string {
	// HSL to RGB conversion using the standard formula.
	var r, g, bv float64
	if s == 0 {
		r, g, bv = l, l, l
	} else {
		hue2rgb := func(p, q, t float64) float64 {
			if t < 0 {
				t++
			}
			if t > 1 {
				t--
			}
			switch {
			case t < 1.0/6:
				return p + (q-p)*6*t
			case t < 1.0/2:
				return q
			case t < 2.0/3:
				return p + (q-p)*(2.0/3-t)*6
			default:
				return p
			}
		}
		var q float64
		if l < 0.5 {
			q = l * (1 + s)
		} else {
			q = l + s - l*s
		}
		p := 2*l - q
		h /= 360
		r = hue2rgb(p, q, h+1.0/3)
		g = hue2rgb(p, q, h)
		bv = hue2rgb(p, q, h-1.0/3)
	}
	ri := int(math.Round(r * 255))
	gi := int(math.Round(g * 255))
	bi := int(math.Round(bv * 255))
	return fmt.Sprintf("#%02x%02x%02x", ri, gi, bi)
}

// RainbowText renders a string with per-letter HSL hue rotation, offset by the
// animation frame. The formula is deterministic for a given (s, frame) pair:
//
//	hue = (i*30 - frame*12) mod 360
//
// The frame offset is SUBTRACTED so the color wave travels left-to-right
// (clockwise feel) as frames advance. Go's % keeps the sign of the dividend,
// so the result is normalized into [0, 360).
// Saturation=0.90, lightness=0.65 gives vibrant colors readable on dark bg.
// Exported so it can be table-tested from the dashboard_test package.
func RainbowText(s string, frame int) string {
	runes := []rune(s)
	if len(runes) == 0 {
		return ""
	}
	var b strings.Builder
	for i, r := range runes {
		hue := float64(((i*30-frame*12)%360 + 360) % 360)
		hex := hslToHex(hue, 0.90, 0.65)
		st := lipgloss.NewStyle().Foreground(lipgloss.Color(hex))
		b.WriteString(st.Render(string(r)))
	}
	return b.String()
}

// GrayscaleText is the monochrome twin of RainbowText: the same clockwise
// traveling wave, expressed as per-letter LIGHTNESS instead of hue. Letters
// shimmer between dim gray and pure white (saturation 0 → grays only).
// Deterministic for a given (s, frame) pair; exported for table tests.
func GrayscaleText(s string, frame int) string {
	runes := []rune(s)
	if len(runes) == 0 {
		return ""
	}
	var b strings.Builder
	for i, r := range runes {
		phase := float64(((i*30-frame*12)%360+360)%360) * math.Pi / 180
		lightness := 0.65 + 0.35*math.Cos(phase) // range [0.30, 1.00]
		hex := hslToHex(0, 0, lightness)
		st := lipgloss.NewStyle().Foreground(lipgloss.Color(hex))
		b.WriteString(st.Render(string(r)))
	}
	return b.String()
}

// ─── Bar helpers ─────────────────────────────────────────────────────────────

// Powerline half-circle cap glyphs (Unicode Private Use Area, Nerd Fonts /
// Omarchy required — declared dependency; the Waybar icon 󰚩 already requires
// a Nerd Font patched terminal).
const (
	barLeftCap  = "" // U+E0B6  — left half-circle (reads as left rounded end)
	barRightCap = "" // U+E0B4  — right half-circle (reads as right rounded end)
)

// barTrackColor is the solid rail color. The track body and both caps use
// this exact foreground so the pill reads as one continuous shape: the caps
// are solid glyphs, so a Faint(true)-over-default style renders them far
// brighter than a textured ░ track (same color, 4x the lit pixels).
const barTrackColor = "#3f3f3f"

// BuildBarCaps returns the cap-color hint for a given percentage:
//   - leftFill:  true when the left cap should use the FILL color (fill > 0)
//   - rightFill: true when the right cap should use the FILL color (pct == 100)
//
// Both caps default to the TRACK color (foreground-only half-circle glyphs read
// as the bar's rounded ends). Exported for direct testing.
func BuildBarCaps(pct float64) (leftFill bool, rightFill bool) {
	leftFill = pct > 0
	rightFill = pct >= 100
	return
}

// BuildBar returns an unstyled progress bar string of exactly barLen visual
// cells using the "pill" style:
//
//	[left-cap] + [body: barLen-2 cells] + [right-cap]
//
// Cap glyphs:
//   - Left cap:  U+E0B6 "" (Powerline / Nerd Font left half-circle)
//   - Right cap: U+E0B4 "" (Powerline / Nerd Font right half-circle)
//
// Body glyphs:
//   - Fill glyph:  █ (U+2588 FULL BLOCK)
//   - Track glyph: ░ (U+2591 LIGHT SHADE)
//
// lipgloss treats PUA glyphs as width-1, so total visual width == barLen.
//
// Body cell-count rules (applied to bodyLen = barLen-2, boundary-safe):
//   - pct == 0   → 0 fill cells, bodyLen track cells  (truly empty)
//   - pct == 100 → bodyLen fill cells, 0 track cells  (truly full)
//   - 0 < pct < 100 → floor(pct/100*bodyLen) fill cells, THEN:
//   - if fill == 0, clamp to 1 (at-least-one-fill guarantee on body)
//   - if fill == bodyLen, clamp to bodyLen-1 (at-least-one-track guarantee on body)
//
// BuildBarCaps returns the cap-color booleans; callers apply color independently.
// Exported so it can be boundary-tested directly from the dashboard_test package.
func BuildBar(pct float64, barLen int) string {
	if barLen <= 2 {
		// Degenerate: not enough room for caps + at least one body cell.
		// Return caps only for barLen==2, or empty for barLen<=0.
		if barLen == 2 {
			return barLeftCap + barRightCap
		}
		return ""
	}

	bodyLen := barLen - 2 // two cells consumed by the caps

	var filled int
	switch {
	case pct <= 0:
		filled = 0
	case pct >= 100:
		filled = bodyLen
	default:
		filled = int(pct / 100 * float64(bodyLen))
		// At-least-one-fill guarantee (1–99% must never look empty).
		if filled == 0 {
			filled = 1
		}
		// At-least-one-track guarantee (1–99% must never look full).
		if filled == bodyLen {
			filled = bodyLen - 1
		}
	}
	body := strings.Repeat("█", filled) + strings.Repeat("░", bodyLen-filled)
	return barLeftCap + body + barRightCap
}

// ─── Command helpers ──────────────────────────────────────────────────────────

func fetchLimitsCmd(d Deps) tea.Cmd {
	return func() tea.Msg {
		u, err := d.FetchLimits()
		return LimitsMsg{Usage: u, Err: err}
	}
}

func fetchStatsCmd(d Deps) tea.Cmd {
	return func() tea.Msg {
		s, err := d.FetchStats()
		return StatsMsg{Stats: s, Err: err}
	}
}

func fetchStatusCmd(d Deps) tea.Cmd {
	if d.FetchStatus == nil {
		// FetchStatus not wired (e.g. legacy tests without status dep): no-op.
		return nil
	}
	return func() tea.Msg {
		st, err := d.FetchStatus()
		return StatusMsg{Status: st, Err: err}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}

func animTickCmd() tea.Cmd {
	return tea.Tick(animTickInterval, func(_ time.Time) tea.Msg {
		return AnimTickMsg{}
	})
}
