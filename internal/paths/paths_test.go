package paths_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jesusrobot0/clauchy/internal/paths"
)

// Tests that use t.Setenv cannot call t.Parallel() in Go 1.21+.
// All paths tests set environment variables, so they run sequentially.

func cleanEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
}

func TestResolve_HomeFallback(t *testing.T) {
	home := t.TempDir()
	cleanEnv(t, home)

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	want := filepath.Join(home, ".claude", "projects")
	found := false
	for _, r := range p.TranscriptRoots {
		if r == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("TranscriptRoots = %v, want to contain %q", p.TranscriptRoots, want)
	}
}

func TestResolve_XDGFallback(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	cleanEnv(t, home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	// Create the XDG projects directory so it exists — existence-gating requires this.
	projectsDir := filepath.Join(xdg, "claude", "projects")
	if err := os.MkdirAll(projectsDir, 0755); err != nil {
		t.Fatal(err)
	}

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	want := projectsDir
	found := false
	for _, r := range p.TranscriptRoots {
		if r == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("TranscriptRoots = %v, want to contain %q", p.TranscriptRoots, want)
	}
}

// TestResolve_XDGNoProjectsDir_FallsToHome verifies that when XDG_CONFIG_HOME
// is set but $XDG_CONFIG_HOME/claude/projects does not exist, the resolver falls
// through to ~/.claude/projects rather than returning the non-existent XDG path.
// This is the real-world bug: ~/.config/claude/projects (XDG default) does not
// exist while ~/.claude/projects (179 files) does.
func TestResolve_XDGNoProjectsDir_FallsToHome(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	cleanEnv(t, home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	// Do NOT create xdg/claude/projects → resolver must fall through to home.

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	// The non-existent XDG path must NOT appear in TranscriptRoots.
	xdgPath := filepath.Join(xdg, "claude", "projects")
	for _, r := range p.TranscriptRoots {
		if r == xdgPath {
			t.Errorf("TranscriptRoots contains non-existent XDG dir %q — should fall through", xdgPath)
		}
	}

	// The home fallback MUST be present.
	homeWant := filepath.Join(home, ".claude", "projects")
	found := false
	for _, r := range p.TranscriptRoots {
		if r == homeWant {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("TranscriptRoots = %v, want home fallback %q when XDG dir absent", p.TranscriptRoots, homeWant)
	}
}

func TestResolve_CLAUDEConfigDirCommaSplit(t *testing.T) {
	home := t.TempDir()
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	cleanEnv(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", dir1+","+dir2)

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	want1 := filepath.Join(dir1, "projects")
	want2 := filepath.Join(dir2, "projects")
	for _, want := range []string{want1, want2} {
		found := false
		for _, r := range p.TranscriptRoots {
			if r == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("TranscriptRoots = %v, want to contain %q", p.TranscriptRoots, want)
		}
	}
}

func TestResolve_CredentialsFileExistenceGate(t *testing.T) {
	home := t.TempDir()
	// dir1: no .credentials.json
	dir1 := t.TempDir()
	// dir2: has .credentials.json — later tier should win over dir1
	dir2 := t.TempDir()
	credFile := filepath.Join(dir2, ".credentials.json")
	if err := os.WriteFile(credFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	cleanEnv(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", dir1+","+dir2)

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	if p.CredentialsFile != credFile {
		t.Errorf("CredentialsFile = %q, want %q", p.CredentialsFile, credFile)
	}
}

func TestResolve_CredentialsFileDefaultWhenNoneFound(t *testing.T) {
	home := t.TempDir()
	cleanEnv(t, home)

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	want := filepath.Join(home, ".claude", ".credentials.json")
	if p.CredentialsFile != want {
		t.Errorf("CredentialsFile = %q, want %q", p.CredentialsFile, want)
	}
}

func TestResolve_XDGCredentialsGate(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	// XDG tier has .credentials.json; home tier does not.
	xdgCred := filepath.Join(xdg, "claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(xdgCred), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(xdgCred, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	cleanEnv(t, home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	if p.CredentialsFile != xdgCred {
		t.Errorf("CredentialsFile = %q, want %q", p.CredentialsFile, xdgCred)
	}
}

func TestResolve_WaybarPaths(t *testing.T) {
	home := t.TempDir()
	cleanEnv(t, home)

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	wantConfig := filepath.Join(home, ".config", "waybar", "config.jsonc")
	wantStyle := filepath.Join(home, ".config", "waybar", "style.css")

	if p.WaybarConfig != wantConfig {
		t.Errorf("WaybarConfig = %q, want %q", p.WaybarConfig, wantConfig)
	}
	if p.WaybarStyle != wantStyle {
		t.Errorf("WaybarStyle = %q, want %q", p.WaybarStyle, wantStyle)
	}
}

func TestResolve_WaybarPathsWithXDG(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	cleanEnv(t, home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	wantConfig := filepath.Join(xdg, "waybar", "config.jsonc")
	wantStyle := filepath.Join(xdg, "waybar", "style.css")

	if p.WaybarConfig != wantConfig {
		t.Errorf("WaybarConfig = %q, want %q", p.WaybarConfig, wantConfig)
	}
	if p.WaybarStyle != wantStyle {
		t.Errorf("WaybarStyle = %q, want %q", p.WaybarStyle, wantStyle)
	}
}

func TestResolve_CacheDirAndPricingOverride(t *testing.T) {
	home := t.TempDir()
	cleanEnv(t, home)

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	wantCache := filepath.Join(home, ".cache", "clauchy")
	wantPricing := filepath.Join(home, ".config", "clauchy", "pricing.json")

	if p.CacheDir != wantCache {
		t.Errorf("CacheDir = %q, want %q", p.CacheDir, wantCache)
	}
	if p.PricingOverride != wantPricing {
		t.Errorf("PricingOverride = %q, want %q", p.PricingOverride, wantPricing)
	}
}

func TestResolve_CacheDirWithXDGCacheHome(t *testing.T) {
	home := t.TempDir()
	xdgCache := t.TempDir()
	cleanEnv(t, home)
	t.Setenv("XDG_CACHE_HOME", xdgCache)

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	wantCache := filepath.Join(xdgCache, "clauchy")
	if p.CacheDir != wantCache {
		t.Errorf("CacheDir = %q, want %q", p.CacheDir, wantCache)
	}
}

// ─── DataHome ─────────────────────────────────────────────────────────────────

func TestResolve_DataHome_Default(t *testing.T) {
	home := t.TempDir()
	cleanEnv(t, home)
	t.Setenv("XDG_DATA_HOME", "")

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	want := filepath.Join(home, ".local", "share")
	if p.DataHome != want {
		t.Errorf("DataHome = %q, want %q", p.DataHome, want)
	}
}

func TestResolve_DataHome_XDG(t *testing.T) {
	home := t.TempDir()
	xdgData := t.TempDir()
	cleanEnv(t, home)
	t.Setenv("XDG_DATA_HOME", xdgData)

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	if p.DataHome != xdgData {
		t.Errorf("DataHome = %q, want %q", p.DataHome, xdgData)
	}
}

// ─── HyprlandConf ─────────────────────────────────────────────────────────────

func TestResolve_HyprlandConf_PresentWhenExists(t *testing.T) {
	home := t.TempDir()
	cleanEnv(t, home)

	// Create hyprland.conf in the expected location.
	hyprDir := filepath.Join(home, ".config", "hypr")
	if err := os.MkdirAll(hyprDir, 0755); err != nil {
		t.Fatal(err)
	}
	hyprConf := filepath.Join(hyprDir, "hyprland.conf")
	if err := os.WriteFile(hyprConf, []byte("# hyprland\n"), 0644); err != nil {
		t.Fatal(err)
	}

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	if p.HyprlandConf != hyprConf {
		t.Errorf("HyprlandConf = %q, want %q", p.HyprlandConf, hyprConf)
	}
}

func TestResolve_HyprlandConf_EmptyWhenAbsent(t *testing.T) {
	home := t.TempDir()
	cleanEnv(t, home)
	// Do NOT create ~/.config/hypr/hyprland.conf

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	if p.HyprlandConf != "" {
		t.Errorf("HyprlandConf = %q, want empty string (file absent)", p.HyprlandConf)
	}
}

// ─── Fix 5: CLAUDE_CONFIG_DIR whitespace-only entries fall through ────────────

// TestResolve_CLAUDEConfigDir_WhitespaceEntries_FallThrough verifies that when
// CLAUDE_CONFIG_DIR contains only empty or whitespace-separated entries (e.g.
// " , "), TranscriptRoots must NOT contain any empty-string path or whitespace-
// only path. The resolver must fall through to the XDG/home tier instead.
func TestResolve_CLAUDEConfigDir_WhitespaceEntries_FallThrough(t *testing.T) {
	home := t.TempDir()
	cleanEnv(t, home)
	// CLAUDE_CONFIG_DIR with only whitespace and commas — no valid directories.
	t.Setenv("CLAUDE_CONFIG_DIR", " , , ")

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	// No empty-string paths must appear in TranscriptRoots.
	for _, r := range p.TranscriptRoots {
		if r == "" {
			t.Error("TranscriptRoots contains empty string — whitespace CLAUDE_CONFIG_DIR must be ignored")
		}
		// Also reject pure-whitespace roots.
		trimmed := strings.TrimSpace(r)
		if trimmed == "" {
			t.Errorf("TranscriptRoots contains whitespace-only entry %q", r)
		}
	}

	// Must fall through to the home default (CLAUDE_CONFIG_DIR yielded nothing).
	homeWant := filepath.Join(home, ".claude", "projects")
	found := false
	for _, r := range p.TranscriptRoots {
		if r == homeWant {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("TranscriptRoots = %v; want home fallback %q when CLAUDE_CONFIG_DIR is all-whitespace",
			p.TranscriptRoots, homeWant)
	}
}

func TestResolve_HyprlandConf_UsesXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	cleanEnv(t, home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	// Create hyprland.conf under XDG config home
	hyprDir := filepath.Join(xdg, "hypr")
	if err := os.MkdirAll(hyprDir, 0755); err != nil {
		t.Fatal(err)
	}
	hyprConf := filepath.Join(hyprDir, "hyprland.conf")
	if err := os.WriteFile(hyprConf, []byte("# hyprland\n"), 0644); err != nil {
		t.Fatal(err)
	}

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	if p.HyprlandConf != hyprConf {
		t.Errorf("HyprlandConf = %q, want %q", p.HyprlandConf, hyprConf)
	}
}
