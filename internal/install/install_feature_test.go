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

// TestRun_IconsWritten_OnFreshInstall verifies that the single claude-symbolic.svg
// file is written to DataHome/clauchy/ on a fresh install (Change 17 — symbolic icon).
func TestRun_IconsWritten_OnFreshInstall(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")

	res, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.IconsWritten {
		t.Error("IconsWritten should be true after writing claude-symbolic.svg")
	}

	iconDir := filepath.Join(cfg.DataHome, "clauchy")
	symbolicPath := filepath.Join(iconDir, "claude-symbolic.svg")
	if _, err := os.Stat(symbolicPath); err != nil {
		t.Errorf("claude-symbolic.svg missing: %v", err)
	}
}

// TestRun_SymbolicIcon_BaseFill verifies claude-symbolic.svg uses the GTK symbolic
// base color (#bebebe) so -gtk-recolor can recolor it at runtime.
func TestRun_SymbolicIcon_BaseFill(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cfg.DataHome, "clauchy", "claude-symbolic.svg"))
	if err != nil {
		t.Fatalf("read claude-symbolic.svg: %v", err)
	}
	if !bytes.Contains(data, []byte(`fill="#bebebe"`)) {
		t.Errorf("claude-symbolic.svg must have GTK symbolic base color #bebebe, got:\n%s", data)
	}
}

// TestRun_CSS_SeverityColorOverrides verifies that severity states override color:
// using the Catppuccin Mocha hex values — now via CSS color: (not background-image:).
func TestRun_CSS_SeverityColorOverrides(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cssData, err := os.ReadFile(cfg.StylePath)
	if err != nil {
		t.Fatalf("read css: %v", err)
	}
	s := string(cssData)

	cases := []struct{ class, hex string }{
		{".mid", "#f9e2af"},
		{".high", "#fab387"},
		{".critical", "#f38ba8"},
	}
	for _, c := range cases {
		if !strings.Contains(s, "color: "+c.hex) {
			t.Errorf("CSS must contain color: %s for %s override", c.hex, c.class)
		}
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

// TestRun_Icons_ContainSVGStructure verifies claude-symbolic.svg is a valid SVG.
func TestRun_Icons_ContainSVGStructure(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	iconDir := filepath.Join(cfg.DataHome, "clauchy")
	data, err := os.ReadFile(filepath.Join(iconDir, "claude-symbolic.svg"))
	if err != nil {
		t.Fatalf("read claude-symbolic.svg: %v", err)
	}
	if !bytes.Contains(data, []byte(`<svg`)) {
		t.Error("claude-symbolic.svg: missing <svg element")
	}
	if !bytes.Contains(data, []byte(`</svg>`)) {
		t.Error("claude-symbolic.svg: missing </svg> closing tag")
	}
}

// ─── A: CSS background-image rules ───────────────────────────────────────────

// TestRun_CSS_HasBackgroundImage verifies the generated CSS block includes a
// -gtk-recolor background-image rule referencing the symbolic icon (Change 17).
func TestRun_CSS_HasBackgroundImage(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cssData, err := os.ReadFile(cfg.StylePath)
	if err != nil {
		t.Fatalf("read css: %v", err)
	}

	// Must use -gtk-recolor wrapping the symbolic icon path.
	if !bytes.Contains(cssData, []byte("background-image")) {
		t.Error("CSS block must contain background-image property")
	}
	if !bytes.Contains(cssData, []byte("claude-symbolic.svg")) {
		t.Error("CSS base selector must reference claude-symbolic.svg")
	}
	if !bytes.Contains(cssData, []byte("-gtk-recolor")) {
		t.Error("CSS base selector must use -gtk-recolor for theme-adaptive coloring")
	}
}

// TestRun_CSS_BaseSelector_MinWidth verifies the base #custom-clauchy selector
// uses the Change 17 sizing: 12.5px × 12.5px (~10% smaller) and min-width: 16px.
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

	if !strings.Contains(s, "min-width: 16px") {
		t.Error("CSS must contain min-width: 16px in #custom-clauchy base selector")
	}
	if !strings.Contains(s, "background-size: 12.5px 12.5px") {
		t.Error("CSS must contain background-size: 12.5px 12.5px")
	}
	if !strings.Contains(s, "background-repeat: no-repeat") {
		t.Error("CSS must contain background-repeat: no-repeat")
	}
	if !strings.Contains(s, "background-position: center") {
		t.Error("CSS must contain background-position: center")
	}
}

// TestRun_CSS_PerClassOverrides verifies .mid/.high/.critical classes set the
// color: property to the Catppuccin Mocha severity hexes (Change 17 — color: via
// -gtk-recolor instead of per-class background-image:).
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

	cases := []struct{ class, hex string }{
		{".mid", "#f9e2af"},
		{".high", "#fab387"},
		{".critical", "#f38ba8"},
	}
	for _, c := range cases {
		if !strings.Contains(s, "#custom-clauchy"+c.class) {
			t.Errorf("CSS must contain #custom-clauchy%s selector", c.class)
		}
		if !strings.Contains(s, "color: "+c.hex) {
			t.Errorf("CSS must set color: %s for %s override", c.hex, c.class)
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

// TestCrossConsistency_IconColors verifies that the CSS color: hex values exposed
// by install.IconSeverityColors() match the Catppuccin Mocha severity palette.
// Change 17: "low" is no longer in the map — low inherits the theme foreground
// via -gtk-recolor (no CSS color: override). Only mid/high/critical have explicit
// hex overrides.
func TestCrossConsistency_IconColors(t *testing.T) {
	colors := install.IconSeverityColors()

	// mid = #f9e2af (Catppuccin Mocha Yellow)
	if colors["mid"] != "#f9e2af" {
		t.Errorf("mid color = %q, want #f9e2af", colors["mid"])
	}
	// high = #fab387 (Catppuccin Mocha Peach)
	if colors["high"] != "#fab387" {
		t.Errorf("high color = %q, want #fab387", colors["high"])
	}
	// critical = #f38ba8 (Catppuccin Mocha Red)
	if colors["critical"] != "#f38ba8" {
		t.Errorf("critical color = %q, want #f38ba8", colors["critical"])
	}
	// "low" must NOT be present (inherits theme foreground via -gtk-recolor).
	if v, ok := colors["low"]; ok {
		t.Errorf("IconSeverityColors() must not contain 'low' key (got %q); low inherits theme foreground", v)
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
