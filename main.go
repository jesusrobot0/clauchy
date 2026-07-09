// clauchy — zero-config Claude Code usage monitor.
//
// Usage:
//
//	clauchy              # open the TUI dashboard (monochrome, default)
//	clauchy --colorful   # open the TUI dashboard with Sky-accented colors
//	clauchy waybar       # emit one JSON line for a Waybar custom module
//	clauchy install      # add/repair the Waybar module config idempotently
//	clauchy --version    # print the build version and exit
//
// The --colorful flag may appear anywhere before or after the mode word.
// Dashboard mode is the default when no mode word is provided.
//
// The binary is intentionally thin: every domain concern lives in an internal
// package. main's only job is composition — wiring injected values (HTTP clients,
// paths, token func, clock) into the domain packages and dispatching to the
// correct entry point.
//
// Clock seam re-sampling invariant (CRITICAL — §1): the TokenFunc closure and
// the FetchStats closure both re-sample time.Now() on EVERY call. A captured
// fixed time would freeze "today" on a long-running dashboard. This is a wiring
// invariant enforced here by code review, not a unit test.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jesusrobot0/clauchy/internal/cache"
	"github.com/jesusrobot0/clauchy/internal/limits"
	"github.com/jesusrobot0/clauchy/internal/oauth"
	"github.com/jesusrobot0/clauchy/internal/paths"
	"github.com/jesusrobot0/clauchy/internal/pricing"
	"github.com/jesusrobot0/clauchy/internal/status"
	"github.com/jesusrobot0/clauchy/internal/transcript"
	"github.com/jesusrobot0/clauchy/ui/dashboard"
	"github.com/jesusrobot0/clauchy/ui/theme"
	"github.com/jesusrobot0/clauchy/ui/waybar"

	"github.com/jesusrobot0/clauchy/internal/install"
)

// version is set by the build system via:
//
//	go build -ldflags "-X main.version=$(git describe --tags)"
//
// It defaults to "dev" for local builds.
var version = "dev"

// parseArgs parses os.Args[1:] (or any equivalent slice) into a mode string
// and a colorful flag. This is a pure function so it can be unit-tested without
// spawning a process.
//
// Recognized modes: "dashboard" (default), "waybar", "install", "version".
// The --colorful flag may appear anywhere in the args slice.
func parseArgs(args []string) (mode string, colorful bool) {
	mode = "dashboard"
	for _, a := range args {
		switch a {
		case "--colorful":
			colorful = true
		case "waybar":
			mode = "waybar"
		case "install":
			mode = "install"
		case "--version":
			mode = "version"
		}
	}
	return
}

func main() {
	mode, colorful := parseArgs(os.Args[1:])
	switch mode {
	case "version":
		fmt.Println("clauchy " + version)
	case "waybar":
		runWaybar()
	case "install":
		runInstall(colorful)
	default:
		runDashboard(colorful)
	}
}

// ─── Waybar mode ─────────────────────────────────────────────────────────────

func runWaybar() {
	p, err := paths.Resolve()
	if err != nil {
		// Even a path resolution failure must emit a valid JSON line (spec).
		emitWaybarError(fmt.Errorf("resolve paths: %w", err))
		return
	}

	c := cache.New(p.CacheDir)

	// Three separate HTTP clients per the critical wiring invariant (§1 / design):
	// - oauthClient: 20s timeout for the token refresh cold-boot path.
	// - limitsClient: bounded by the ctx deadline (FetchTimeout = 2.5s).
	// - statusClient: bounded by status.FetchTimeout (2.5s).
	oauthClient := &http.Client{Timeout: oauth.RefreshTimeout}
	limitsClient := &http.Client{}
	statusClient := &http.Client{}

	oauthCfg := oauth.Config{
		CredentialsPath: p.CredentialsFile,
		TokenURL:        "https://platform.claude.com/v1/oauth/token",
	}

	// TokenFunc re-samples time.Now() on every call (clock seam invariant).
	tokenFunc := func() (string, error) {
		return oauth.Token(oauthCfg, oauthClient, time.Now())
	}

	u, limitsErr := limits.Cached(
		context.Background(),
		c,
		limitsClient,
		"https://api.anthropic.com",
		tokenFunc,
		time.Now, // re-sampled closure — NOT a captured value
	)

	// Fetch Claude status page. The 180s TTL means the 60s Waybar poll mostly
	// cache-hits. A status failure must NEVER break waybar output — on error
	// pass a zero Status so Render omits the status tooltip line.
	// Skipped entirely on credential-class limits errors: Render early-returns
	// the "Run claude to log in" payload without a tooltip, so fetching status
	// would be pure latency waste.
	var st status.Status
	if !isCredentialErr(limitsErr) {
		var stErr error
		st, stErr = status.Cached(
			context.Background(),
			c,
			statusClient,
			"https://status.claude.com",
			time.Now,
		)
		if stErr != nil {
			st = status.Status{} // zero → Render appends no status line
		}
	}

	out := waybar.Render(u, limitsErr, time.Now(), st)
	if encErr := json.NewEncoder(os.Stdout).Encode(out); encErr != nil {
		// Encoding failure is vanishingly unlikely; exit non-zero for debuggability.
		fmt.Fprintf(os.Stderr, "clauchy waybar: encode: %v\n", encErr)
		os.Exit(1)
	}
}

// isCredentialErr reports whether err is a credential-class limits error —
// the same set waybar.Render maps to its "Run claude to log in" early return.
func isCredentialErr(err error) bool {
	return errors.Is(err, oauth.ErrNoCredentials) ||
		errors.Is(err, oauth.ErrInvalidCredentials) ||
		errors.Is(err, oauth.ErrRefreshRejected)
}

// emitWaybarError writes a safe fallback JSON line to stdout and exits 0.
// Used when a pre-render error occurs so Waybar never sees empty output.
func emitWaybarError(_ error) {
	out := waybar.Render(limits.Usage{}, nil, time.Now(), status.Status{})
	json.NewEncoder(os.Stdout).Encode(out) //nolint:errcheck
}

// ─── Dashboard mode ───────────────────────────────────────────────────────────

func runDashboard(colorful bool) {
	p, err := paths.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "clauchy: resolve paths: %v\n", err)
		os.Exit(1)
	}

	c := cache.New(p.CacheDir)
	palette := theme.Default()

	oauthClient := &http.Client{Timeout: oauth.RefreshTimeout}
	limitsClient := &http.Client{}
	statusClient := &http.Client{}

	table, err := pricing.LoadOverride(p.PricingOverride, pricing.Builtin())
	if err != nil {
		table = pricing.Builtin()
	}

	oauthCfg := oauth.Config{
		CredentialsPath: p.CredentialsFile,
		TokenURL:        "https://platform.claude.com/v1/oauth/token",
	}

	tokenFunc := func() (string, error) {
		return oauth.Token(oauthCfg, oauthClient, time.Now())
	}

	deps := dashboard.Deps{
		FetchLimits: func() (limits.Usage, error) {
			return limits.Cached(
				context.Background(),
				c,
				limitsClient,
				"https://api.anthropic.com",
				tokenFunc,
				time.Now, // re-sampled per call
			)
		},
		FetchStats: func() (transcript.Stats, error) {
			// time.Now() and time.Local are re-sampled on each call so that a
			// long-running dashboard always uses the current date/location.
			return transcript.Aggregate(p.TranscriptRoots, table, time.Now(), time.Local)
		},
		FetchStatus: func() (status.Status, error) {
			// Shares the same cache instance as limits — the 180s TTL means
			// the 5s dashboard tick mostly cache-hits.
			return status.Cached(
				context.Background(),
				c,
				statusClient,
				"https://status.claude.com",
				time.Now,
			)
		},
	}

	// Derive the plan label from OAuth credentials.
	// Missing or unreadable credentials → empty string (dashboard opens normally;
	// the limits panel already reports credential problems).
	var planLabel string
	if creds, err := oauth.Load(p.CredentialsFile); err == nil {
		planLabel = oauth.PlanLabel(creds.SubscriptionType, creds.RateLimitTier)
	}

	var m dashboard.Model
	if colorful {
		m = dashboard.NewColorful(deps, palette, time.Now, planLabel)
	} else {
		m = dashboard.New(deps, palette, time.Now, planLabel)
	}

	prog := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := prog.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "clauchy: %v\n", err)
		os.Exit(1)
	}
}

// ─── Install mode ─────────────────────────────────────────────────────────────

func runInstall(colorful bool) {
	p, err := paths.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "clauchy install: resolve paths: %v\n", err)
		os.Exit(1)
	}

	// Reloader: reload Waybar (SIGUSR2) and Hyprland (hyprctl reload).
	// Both commands are best-effort; command-not-found is a warning, not an error.
	reloader := func() error {
		_ = sendSIGUSR2ToWaybar()
		_ = runHyprctlReload()
		return nil
	}

	result, err := install.Run(install.RunConfig{
		ConfigPath:   p.WaybarConfig,
		StylePath:    p.WaybarStyle,
		DataHome:     p.DataHome,
		HyprlandConf: p.HyprlandConf,
		Colorful:     colorful,
		Reload:       reloader,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "clauchy install: %v\n", err)
		os.Exit(1)
	}

	anyChange := result.ConfigChanged || result.CSSChanged || result.HyprChanged
	if anyChange {
		fmt.Println("clauchy: installation updated.")
		for _, b := range result.Backups {
			fmt.Printf("  backup: %s\n", b)
		}
		if result.ConfigChanged {
			fmt.Println("  Waybar config.jsonc patched.")
		}
		if result.CSSChanged {
			fmt.Println("  Waybar style.css patched.")
		}
		if result.HyprChanged {
			fmt.Println("  Hyprland window rules appended to hyprland.conf.")
		}
	} else {
		fmt.Println("clauchy: already installed — nothing to do.")
	}
	if result.IconsWritten {
		fmt.Printf("  Icon variants written to %s/clauchy/.\n", p.DataHome)
	}

	for _, w := range result.Warnings {
		fmt.Fprintf(os.Stderr, "clauchy install warning: %s\n", w)
	}
	if !result.OnClickResolved {
		fmt.Fprintln(os.Stderr, "clauchy install: no known terminal found; on-click omitted.")
	}
}

// runHyprctlReload reloads the Hyprland compositor so the new window rules take
// effect immediately. Command-not-found (hyprctl absent — non-Hyprland host) is
// treated as a warning, not a fatal error.
func runHyprctlReload() error {
	cmd := exec.Command("hyprctl", "reload")
	if err := cmd.Run(); err != nil {
		// exec.ErrNotFound or exit status: both are non-fatal for non-Hyprland hosts.
		return fmt.Errorf("hyprctl reload: %w", err)
	}
	return nil
}

// sendSIGUSR2ToWaybar sends SIGUSR2 to all waybar processes, causing them to
// reload their config. ESRCH (no such process) is silently ignored.
func sendSIGUSR2ToWaybar() error {
	// Find waybar PIDs via /proc (Linux-specific; this binary targets Linux Wayland).
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil // /proc unavailable; non-fatal
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		commPath := "/proc/" + e.Name() + "/comm"
		comm, err := os.ReadFile(commPath)
		if err != nil {
			continue
		}
		if string(comm) == "waybar\n" {
			var pid int
			fmt.Sscanf(e.Name(), "%d", &pid)
			if pid > 0 {
				syscall.Kill(pid, syscall.SIGUSR2) //nolint:errcheck
			}
		}
	}
	return nil
}
