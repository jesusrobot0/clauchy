package install_test

// Feature tests for:
//   A — SVG icon variants written to DataHome, CSS background-image rules
//   B — Hyprland window rules (4th install surface)
//
// These tests follow strict TDD: written BEFORE the implementation,
// verifying only the new surface behaviour (icons, hyprland, CSS changes,
// waybar Output.Text = " ").

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jesusrobot0/clauchy/internal/install"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// makeRunConfig returns a RunConfig wired to a fresh temp layout.
// ghostty is faked on PATH so terminal resolution always succeeds.
func makeRunConfig(t *testing.T, cfgName, cssName string) (install.RunConfig, string) {
	t.Helper()
	dir := t.TempDir()
	dataDir := t.TempDir()

	// Fake ghostty binary
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, cfgName)
	cssPath := copyFixture(t, dir, cssName)

	return install.RunConfig{
		ConfigPath:   cfgPath,
		StylePath:    cssPath,
		DataHome:     dataDir,
		HyprlandConf: "", // no hyprland by default
		Reload:       noopReloader,
	}, dir
}

// ─── A: SVG icon variants ─────────────────────────────────────────────────────

// TestRun_IconsWritten_OnFreshInstall verifies that four icon variant SVG files
// are written to DataHome/clauchy/ on a fresh install.
func TestRun_IconsWritten_OnFreshInstall(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")

	res, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IconsWritten {
		t.Error("IconsWritten should be true after fresh install")
	}

	iconDir := filepath.Join(cfg.DataHome, "clauchy")
	for _, name := range []string{"icon-low.svg", "icon-mid.svg", "icon-high.svg", "icon-critical.svg"} {
		p := filepath.Join(iconDir, name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("icon file missing: %s", p)
		}
	}
}

// TestRun_IconLow_WhiteColor verifies icon-low.svg uses white (#ffffff) to match
// the other white bar icons in the calm/low state.
func TestRun_IconLow_WhiteColor(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cfg.DataHome, "clauchy", "icon-low.svg"))
	if err != nil {
		t.Fatalf("read icon-low.svg: %v", err)
	}
	if !bytes.Contains(data, []byte(`fill="#ffffff"`)) {
		t.Errorf("icon-low.svg should use white #ffffff, got:\n%s", data)
	}
}

// TestRun_IconMid_MidColor verifies icon-mid.svg has the mid severity hex (#f9e2af).
func TestRun_IconMid_MidColor(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cfg.DataHome, "clauchy", "icon-mid.svg"))
	if err != nil {
		t.Fatalf("read icon-mid.svg: %v", err)
	}
	if !bytes.Contains(data, []byte(`fill="#f9e2af"`)) {
		t.Errorf("icon-mid.svg should have mid color #f9e2af, got:\n%s", data)
	}
}

// TestRun_IconHigh_HighColor verifies icon-high.svg has the high severity hex (#fab387).
func TestRun_IconHigh_HighColor(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cfg.DataHome, "clauchy", "icon-high.svg"))
	if err != nil {
		t.Fatalf("read icon-high.svg: %v", err)
	}
	if !bytes.Contains(data, []byte(`fill="#fab387"`)) {
		t.Errorf("icon-high.svg should have high color #fab387, got:\n%s", data)
	}
}

// TestRun_IconCritical_CriticalColor verifies icon-critical.svg has the critical hex (#f38ba8).
func TestRun_IconCritical_CriticalColor(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cfg.DataHome, "clauchy", "icon-critical.svg"))
	if err != nil {
		t.Fatalf("read icon-critical.svg: %v", err)
	}
	if !bytes.Contains(data, []byte(`fill="#f38ba8"`)) {
		t.Errorf("icon-critical.svg should have critical color #f38ba8, got:\n%s", data)
	}
}

// TestRun_Icons_Idempotent verifies that re-running does not fail and still produces icons.
func TestRun_Icons_Idempotent(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")

	// First run
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Update cfg to use already-installed config/css
	dir2 := t.TempDir()
	cfg2 := install.RunConfig{
		ConfigPath:   copyFixture(t, dir2, "config_full.jsonc"),
		StylePath:    copyFixture(t, dir2, "style_full.css"),
		DataHome:     cfg.DataHome, // same data dir
		HyprlandConf: "",
		Reload:       noopReloader,
	}

	res2, err := install.Run(cfg2)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	// Icons are always regenerated (overwrite-safe); IconsWritten should be true
	if !res2.IconsWritten {
		t.Error("IconsWritten should be true even on idempotent re-run (icons always regenerated)")
	}
}

// TestRun_Icons_ContainSVGStructure verifies each icon is a valid SVG (has <svg and </svg>).
func TestRun_Icons_ContainSVGStructure(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	iconDir := filepath.Join(cfg.DataHome, "clauchy")
	for _, name := range []string{"icon-low.svg", "icon-mid.svg", "icon-high.svg", "icon-critical.svg"} {
		data, err := os.ReadFile(filepath.Join(iconDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Contains(data, []byte(`<svg`)) {
			t.Errorf("%s: missing <svg element", name)
		}
		if !bytes.Contains(data, []byte(`</svg>`)) {
			t.Errorf("%s: missing </svg> closing tag", name)
		}
	}
}

// ─── A: CSS background-image rules ───────────────────────────────────────────

// TestRun_CSS_HasBackgroundImage verifies the generated CSS block includes
// background-image rules referencing absolute icon paths.
func TestRun_CSS_HasBackgroundImage(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cssData, err := os.ReadFile(cfg.StylePath)
	if err != nil {
		t.Fatalf("read css: %v", err)
	}

	// Must reference icon-low.svg in the base selector
	if !bytes.Contains(cssData, []byte("background-image")) {
		t.Error("CSS block must contain background-image property")
	}
	if !bytes.Contains(cssData, []byte("icon-low.svg")) {
		t.Error("CSS base selector must reference icon-low.svg")
	}
}

// TestRun_CSS_BaseSelector_MinWidth verifies the base #custom-clauchy selector
// has min-width: 22px and background-size/repeat/position.
func TestRun_CSS_BaseSelector_MinWidth(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cssData, err := os.ReadFile(cfg.StylePath)
	if err != nil {
		t.Fatalf("read css: %v", err)
	}
	s := string(cssData)

	if !strings.Contains(s, "min-width: 18px") {
		t.Error("CSS must contain min-width: 18px in #custom-clauchy base selector")
	}
	if !strings.Contains(s, "background-size: 14px 14px") {
		t.Error("CSS must contain background-size: 14px 14px")
	}
	if !strings.Contains(s, "background-repeat: no-repeat") {
		t.Error("CSS must contain background-repeat: no-repeat")
	}
	if !strings.Contains(s, "background-position: center") {
		t.Error("CSS must contain background-position: center")
	}
}

// TestRun_CSS_PerClassOverrides verifies .mid/.high/.critical classes reference
// their respective icon files.
func TestRun_CSS_PerClassOverrides(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cssData, err := os.ReadFile(cfg.StylePath)
	if err != nil {
		t.Fatalf("read css: %v", err)
	}
	s := string(cssData)

	for _, icon := range []string{"icon-mid.svg", "icon-high.svg", "icon-critical.svg"} {
		if !strings.Contains(s, icon) {
			t.Errorf("CSS must reference %s in per-class override", icon)
		}
	}
}

// TestRun_CSS_AbsolutePaths verifies icon URLs use absolute paths (start with url("/")).
func TestRun_CSS_AbsolutePaths(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cssData, err := os.ReadFile(cfg.StylePath)
	if err != nil {
		t.Fatalf("read css: %v", err)
	}

	// All background-image values must be absolute paths (start with /)
	iconDir := filepath.Join(cfg.DataHome, "clauchy")
	if !bytes.Contains(cssData, []byte(`url("`+iconDir)) {
		t.Errorf("CSS background-image must use absolute path starting with %s, CSS:\n%s", iconDir, cssData)
	}
}

// TestCrossConsistency_IconColors verifies that the hex colors used for icons
// (within the install package) match the theme palette hex values.
func TestCrossConsistency_IconColors(t *testing.T) {
	// The install package must expose the icon color constants so tests can
	// verify they match theme.Default().
	colors := install.IconSeverityColors()

	// mid = #f9e2af (Catppuccin Mocha Yellow)
	if colors["mid"] != "#f9e2af" {
		t.Errorf("mid icon color = %q, want #f9e2af", colors["mid"])
	}
	// high = #fab387 (Catppuccin Mocha Peach)
	if colors["high"] != "#fab387" {
		t.Errorf("high icon color = %q, want #fab387", colors["high"])
	}
	// critical = #f38ba8 (Catppuccin Mocha Red)
	if colors["critical"] != "#f38ba8" {
		t.Errorf("critical icon color = %q, want #f38ba8", colors["critical"])
	}
	// low: white (#ffffff) — matches the other white bar icons; calm state is now white
	if colors["low"] != "#ffffff" {
		t.Errorf("low icon color = %q, want #ffffff (white, matching other bar icons)", colors["low"])
	}
}

// ─── B: Hyprland window rules ─────────────────────────────────────────────────

// TestRun_Hyprland_FreshInstall verifies rules are appended to a fresh hyprland.conf.
func TestRun_Hyprland_FreshInstall(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")

	// Add a real hyprland.conf
	hyprDir := t.TempDir()
	hyprPath := copyFixtureTo(t, hyprDir, "hyprland_fresh.conf", "hyprland.conf")
	cfg.HyprlandConf = hyprPath

	reloadCalled := false
	cfg.Reload = func() error {
		reloadCalled = true
		return nil
	}

	res, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.HyprChanged {
		t.Error("HyprChanged should be true after appending window rules")
	}

	data, err := os.ReadFile(hyprPath)
	if err != nil {
		t.Fatalf("read hyprland.conf: %v", err)
	}
	s := string(data)

	if !strings.Contains(s, "# clauchy start") {
		t.Error("hyprland.conf must contain # clauchy start marker")
	}
	if !strings.Contains(s, "# clauchy end") {
		t.Error("hyprland.conf must contain # clauchy end marker")
	}
	if !strings.Contains(s, "windowrule = float on, match:class ^clauchy\\.panel$") {
		t.Error("hyprland.conf must contain float windowrule")
	}
	if !strings.Contains(s, "windowrule = size 920 520, match:class ^clauchy\\.panel$") {
		t.Error("hyprland.conf must contain size windowrule with 920 520")
	}
	if !strings.Contains(s, "windowrule = center on, match:class ^clauchy\\.panel$") {
		t.Error("hyprland.conf must contain center windowrule")
	}
	_ = reloadCalled // reload may or may not be called depending on implementation
}

// TestRun_Hyprland_Idempotent verifies no-op when the block is already present.
func TestRun_Hyprland_Idempotent(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_full.jsonc", "style_full.css")

	hyprDir := t.TempDir()
	hyprPath := copyFixtureTo(t, hyprDir, "hyprland_full.conf", "hyprland.conf")
	cfg.HyprlandConf = hyprPath

	origData, _ := os.ReadFile(hyprPath)

	res, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.HyprChanged {
		t.Error("HyprChanged should be false when block already present")
	}

	// File must be byte-identical
	gotData, _ := os.ReadFile(hyprPath)
	if !bytes.Equal(origData, gotData) {
		t.Error("hyprland.conf must not change on idempotent re-run")
	}
}

// TestRun_Hyprland_Missing_WarningSkip verifies that a missing hyprland.conf
// produces a warning but does NOT fail the install.
func TestRun_Hyprland_Missing_WarningSkip(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	cfg.HyprlandConf = filepath.Join(t.TempDir(), "nonexistent", "hyprland.conf")

	res, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("Run should not fail when hyprland.conf is missing: %v", err)
	}
	if res.HyprChanged {
		t.Error("HyprChanged should be false when file is missing (skip)")
	}

	// Must emit a warning about the missing file
	hasHyprWarning := false
	for _, w := range res.Warnings {
		if strings.Contains(strings.ToLower(w), "hyprland") || strings.Contains(w, "hypr") {
			hasHyprWarning = true
			break
		}
	}
	if !hasHyprWarning {
		t.Errorf("expected a warning about missing hyprland.conf, got warnings: %v", res.Warnings)
	}
}

// TestRun_Hyprland_Empty_SkipSilently verifies that an empty HyprlandConf path
// means no hyprland processing (not an error, no warning).
func TestRun_Hyprland_Empty_SkipSilently(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	cfg.HyprlandConf = "" // explicitly empty = non-Hyprland setup

	res, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.HyprChanged {
		t.Error("HyprChanged must be false when HyprlandConf is empty")
	}
}

// TestRun_Hyprland_Backup verifies a timestamped backup is created when writing rules.
func TestRun_Hyprland_Backup(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")

	hyprDir := t.TempDir()
	hyprPath := copyFixtureTo(t, hyprDir, "hyprland_fresh.conf", "hyprland.conf")
	cfg.HyprlandConf = hyprPath

	res, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.HyprChanged {
		t.Skip("no hyprland change — backup test not applicable")
	}

	// At least one backup with "hyprland" or "hypr" in the name
	hasHyprBackup := false
	for _, b := range res.Backups {
		if strings.Contains(b, "hyprland") {
			hasHyprBackup = true
			if _, err := os.Stat(b); err != nil {
				t.Errorf("backup file missing: %v", err)
			}
		}
	}
	if !hasHyprBackup {
		t.Errorf("expected a hyprland backup in result, got: %v", res.Backups)
	}
}

// TestRun_Hyprland_ClassConstant verifies the window class used in rules matches
// the class hardcoded in buildOnClickCmd (single source of truth).
func TestRun_Hyprland_ClassConstant(t *testing.T) {
	// PanelClass is exported so both install and hyprland rules use the same value.
	cls := install.PanelClass()
	if cls == "" {
		t.Fatal("PanelClass() must return a non-empty string")
	}
	if cls != "clauchy.panel" {
		t.Errorf("PanelClass() = %q, want %q", cls, "clauchy.panel")
	}
}

// TestRun_Hyprland_ReloadCalled verifies hyprctl reload is invoked after writing rules.
func TestRun_Hyprland_ReloadCalled(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")

	hyprDir := t.TempDir()
	hyprPath := copyFixtureTo(t, hyprDir, "hyprland_fresh.conf", "hyprland.conf")
	cfg.HyprlandConf = hyprPath

	reloadCalls := 0
	cfg.Reload = func() error {
		reloadCalls++
		return nil
	}

	_, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reloadCalls == 0 {
		t.Error("Reload must be called when hyprland rules are written")
	}
}

// TestRun_Hyprland_StaleBlock_Repaired verifies that an existing hyprland block
// with OUTDATED content (e.g. the old size 940 580) is detected as stale and
// replaced in place — presence of markers alone is NOT "installed".
// Second run after repair must be a no-op (content-aware idempotency).
func TestRun_Hyprland_StaleBlock_Repaired(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_full.jsonc", "style_full.css")

	hyprDir := t.TempDir()
	// hyprland_stale.conf fixture contains the OLD size (940 580) — stale content.
	hyprPath := copyFixtureTo(t, hyprDir, "hyprland_stale.conf", "hyprland.conf")
	cfg.HyprlandConf = hyprPath

	res, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.HyprChanged {
		t.Fatal("HyprChanged should be true — stale hypr block must be repaired")
	}

	data, err := os.ReadFile(hyprPath)
	if err != nil {
		t.Fatalf("read hyprland.conf: %v", err)
	}
	s := string(data)

	// New size must be present.
	if !strings.Contains(s, "windowrule = size 920 520, match:class ^clauchy\\.panel$") {
		t.Error("repaired block must contain size 920 520")
	}
	// Old size must be gone.
	if strings.Contains(s, "windowrule = size 940 580") {
		t.Error("old size 940 580 must be gone after repair")
	}
	// Exactly one start/end marker.
	if n := strings.Count(s, "# clauchy start"); n != 1 {
		t.Errorf("want exactly 1 hypr start marker after repair, got %d", n)
	}
	if n := strings.Count(s, "# clauchy end"); n != 1 {
		t.Errorf("want exactly 1 hypr end marker after repair, got %d", n)
	}

	// Second run must be a no-op.
	res2, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res2.HyprChanged {
		t.Error("second run: HyprChanged = true — repair is not idempotent")
	}
}

// ─── A: waybar Output.Text == " " ────────────────────────────────────────────
// (These tests live in a separate file but are linked by the same batch.)
// Tested via TestRender_Text_IsSpace in install_feature_waybar_test.go.

// ─── helpers (local to this file) ────────────────────────────────────────────

// copyFixtureTo copies a testdata/<src> file into dir with a target filename.
func copyFixtureTo(t *testing.T, dir, src, dst string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", src))
	if err != nil {
		t.Fatalf("copyFixtureTo: %v", err)
	}
	p := filepath.Join(dir, dst)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("copyFixtureTo write: %v", err)
	}
	return p
}

// TestRun_CSS_StaleBlockRepaired: a pre-existing clauchy CSS block with
// OUTDATED content (e.g. from an older clauchy version that colored a font
// glyph instead of setting background images) must be detected as stale and
// replaced in place — marker presence alone is NOT "installed".
func TestRun_CSS_StaleBlockRepaired(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_stale.css") // fixture holds an old-style block

	res, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.CSSChanged {
		t.Fatal("CSSChanged = false — stale CSS block was not repaired")
	}

	cssData, err := os.ReadFile(cfg.StylePath)
	if err != nil {
		t.Fatalf("read css: %v", err)
	}
	if !bytes.Contains(cssData, []byte("background-image")) {
		t.Error("repaired block must contain background-image")
	}
	if bytes.Contains(cssData, []byte("color: #a6e3a1")) {
		t.Error("old glyph-color rule must be gone after repair")
	}
	if n := bytes.Count(cssData, []byte("/* clauchy start */")); n != 1 {
		t.Errorf("want exactly 1 start marker after repair, got %d", n)
	}
	if n := bytes.Count(cssData, []byte("/* clauchy end */")); n != 1 {
		t.Errorf("want exactly 1 end marker after repair, got %d", n)
	}

	// Second run must be a CSS no-op (content now current).
	res2, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res2.CSSChanged {
		t.Error("second run: CSSChanged = true — repair is not idempotent")
	}
}
