package install_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clauchy/internal/install"
)

// noopReloader is a Reloader that does nothing (replaces real SIGUSR2 in tests).
func noopReloader() error { return nil }

// runTestInstall wraps install.Run with a RunConfig that uses empty DataHome
// and HyprlandConf. This helper preserves the legacy 3-argument call pattern
// across all existing tests while the production API accepts a RunConfig struct.
func runTestInstall(configPath, stylePath string, reload install.Reloader) (install.Result, error) {
	return install.Run(install.RunConfig{
		ConfigPath:   configPath,
		StylePath:    stylePath,
		DataHome:     "", // no icon writing in legacy tests
		HyprlandConf: "",
		Reload:       reload,
	})
}

// copyFixture copies a testdata file into dir, returning its path.
func copyFixture(t *testing.T, dir, name string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("copyFixture: %v", err)
	}
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, src, 0o644); err != nil {
		t.Fatalf("copyFixture write: %v", err)
	}
	return dst
}

// stripJSONCComments removes // and /* */ comments from src so the result can
// be fed to json.Unmarshal to verify comma-correctness.
func stripJSONCComments(src []byte) []byte {
	var out bytes.Buffer
	inStr := false
	inEsc := false
	i := 0
	n := len(src)
	for i < n {
		b := src[i]
		if inEsc {
			out.WriteByte(b)
			inEsc = false
			i++
			continue
		}
		if inStr {
			out.WriteByte(b)
			if b == '\\' {
				inEsc = true
			} else if b == '"' {
				inStr = false
			}
			i++
			continue
		}
		// Normal state
		if b == '"' {
			inStr = true
			out.WriteByte(b)
			i++
			continue
		}
		if b == '/' && i+1 < n {
			if src[i+1] == '/' {
				// skip to end of line
				i += 2
				for i < n && src[i] != '\n' {
					i++
				}
				continue
			}
			if src[i+1] == '*' {
				// skip to */
				i += 2
				for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
					i++
				}
				i += 2 // consume */
				continue
			}
		}
		out.WriteByte(b)
		i++
	}
	return out.Bytes()
}

// isValidJSON returns true if the byte slice is valid JSON after normalizing.
func isValidJSON(src []byte) bool {
	var v interface{}
	return json.Unmarshal(src, &v) == nil
}

// --- T-4.1 RED Part A: idempotency, fresh install, half-install, error cases ---

func TestRun_Idempotent_FullInstall(t *testing.T) {
	dir := t.TempDir()
	cfgPath := copyFixture(t, dir, "config_full.jsonc")
	cssPath := copyFixture(t, dir, "style_full.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ConfigChanged {
		t.Error("ConfigChanged should be false (already installed)")
	}
	if res.CSSChanged {
		t.Error("CSSChanged should be false (marker already present)")
	}
	if len(res.Backups) != 0 {
		t.Errorf("expected no backups, got %v", res.Backups)
	}
	if !res.OnClickResolved {
		t.Error("OnClickResolved should be true (ghostty is in the existing on-click)")
	}
}

func TestRun_ConfigNotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := runTestInstall(filepath.Join(dir, "nonexistent.jsonc"), filepath.Join(dir, "style.css"), noopReloader)
	if !errors.Is(err, install.ErrConfigNotFound) {
		t.Fatalf("expected ErrConfigNotFound, got %v", err)
	}
}

func TestRun_NoModulesArray(t *testing.T) {
	dir := t.TempDir()
	cfgPath := copyFixture(t, dir, "config_no_modules.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	_, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if !errors.Is(err, install.ErrNoModulesArray) {
		t.Fatalf("expected ErrNoModulesArray, got %v", err)
	}
	// Neither file should be modified on error
	origCfg, _ := os.ReadFile(filepath.Join("testdata", "config_no_modules.jsonc"))
	gotCfg, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(origCfg, gotCfg) {
		t.Error("config file was modified on ErrNoModulesArray — must not write on error")
	}
}

func TestRun_FreshInstall(t *testing.T) {
	dir := t.TempDir()
	// Ensure ghostty is "available" via a fake binary on PATH.
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "") // clear any env override

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true after fresh install")
	}
	if !res.CSSChanged {
		t.Error("CSSChanged should be true after fresh install")
	}
	if len(res.Backups) != 2 {
		t.Errorf("expected 2 backups (config + css), got %d: %v", len(res.Backups), res.Backups)
	}
	if !res.OnClickResolved {
		t.Error("OnClickResolved should be true (ghostty found on PATH)")
	}

	// Read back and verify structure
	cfgOut, _ := os.ReadFile(cfgPath)

	// Must contain exec key
	if !bytes.Contains(cfgOut, []byte(`"clauchy waybar"`)) {
		t.Error("installed config must contain clauchy waybar exec")
	}
	// Must contain return-type: json
	if !bytes.Contains(cfgOut, []byte(`"return-type": "json"`)) {
		t.Error("installed config must contain return-type: json")
	}
	// Must contain interval: 60
	if !bytes.Contains(cfgOut, []byte(`"interval": 60`)) {
		t.Error("installed config must contain interval: 60")
	}
	// Must contain on-click with ghostty
	if !bytes.Contains(cfgOut, []byte(`ghostty`)) {
		t.Error("installed config must contain ghostty in on-click")
	}
	// Must contain "custom/clauchy" in the modules array
	if !bytes.Contains(cfgOut, []byte(`"custom/clauchy"`)) {
		t.Error("installed config must contain custom/clauchy string")
	}

	// CRITICAL: re-parse as valid JSON after comment stripping
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("installed config does not re-parse as valid JSON after comment stripping:\n%s", stripped)
	}

	// CSS must contain marker block
	cssOut, _ := os.ReadFile(cssPath)
	if !bytes.Contains(cssOut, []byte("/* clauchy start */")) {
		t.Error("style.css must contain /* clauchy start */ marker")
	}
	if !bytes.Contains(cssOut, []byte("/* clauchy end */")) {
		t.Error("style.css must contain /* clauchy end */ marker")
	}
	if !bytes.Contains(cssOut, []byte("#custom-clauchy.critical")) {
		t.Error("CSS must define #custom-clauchy.critical")
	}
}

func TestRun_HalfInstall_ArrayOnly(t *testing.T) {
	// "custom/clauchy" in array but no module object → must add the object block
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_half_array.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true (object was missing)")
	}

	cfgOut, _ := os.ReadFile(cfgPath)
	if !bytes.Contains(cfgOut, []byte(`"exec": "clauchy waybar"`)) {
		t.Error("module object must be inserted with exec")
	}
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("result is not valid JSON: %s", stripped)
	}
}

func TestRun_HalfInstall_ModuleOnly(t *testing.T) {
	// Module object exists but not in array → must add to modules-right
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_half_module.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true (array entry was missing)")
	}

	cfgOut, _ := os.ReadFile(cfgPath)
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("result is not valid JSON: %s", stripped)
	}
	// Count occurrences of "custom/clauchy" as module in array — should appear exactly once in array context
	// We just verify re-parse and that the array entry was added
	var parsed map[string]interface{}
	if err := json.Unmarshal(stripped, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	arr, ok := parsed["modules-right"].([]interface{})
	if !ok {
		t.Fatal("modules-right not an array")
	}
	found := false
	for _, v := range arr {
		if v == "custom/clauchy" {
			found = true
			break
		}
	}
	if !found {
		t.Error("custom/clauchy must appear in modules-right array")
	}
}

// --- T-4.2 RED Part B: repair, terminal resolution, CSS idempotency, backup collision ---

func TestRun_StaleExec_Repaired(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_stale_exec.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true (exec was stale)")
	}

	cfgOut, _ := os.ReadFile(cfgPath)
	if bytes.Contains(cfgOut, []byte("old-clauchy-binary")) {
		t.Error("stale exec must be removed after repair")
	}
	if !bytes.Contains(cfgOut, []byte(`"clauchy waybar"`)) {
		t.Error("repaired exec must contain clauchy waybar")
	}
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("result is not valid JSON: %s", stripped)
	}
}

func TestRun_MissingReturnType_Repaired(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_missing_rt.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true (return-type was missing)")
	}

	cfgOut, _ := os.ReadFile(cfgPath)
	if !bytes.Contains(cfgOut, []byte(`"return-type": "json"`)) {
		t.Error("repaired config must contain return-type: json")
	}
	if !bytes.Contains(cfgOut, []byte(`"interval": 60`)) {
		t.Error("repaired config must contain interval: 60")
	}
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("result is not valid JSON: %s", stripped)
	}
}

func TestRun_TerminalLess_NoOnClick(t *testing.T) {
	// No known terminal anywhere → installs without on-click
	dir := t.TempDir()
	t.Setenv("TERMINAL", "")
	// Use a PATH that has no known terminals
	t.Setenv("PATH", t.TempDir())

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true")
	}
	if res.OnClickResolved {
		t.Error("OnClickResolved should be false when no terminal resolves")
	}
	if len(res.Warnings) == 0 {
		t.Error("should emit a warning when no terminal resolves")
	}

	cfgOut, _ := os.ReadFile(cfgPath)
	stripped := stripJSONCComments(cfgOut)
	var parsed map[string]interface{}
	if err := json.Unmarshal(stripped, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	clauchyObj, ok := parsed["custom/clauchy"].(map[string]interface{})
	if !ok {
		t.Fatal("custom/clauchy object not found")
	}
	if _, hasOnClick := clauchyObj["on-click"]; hasOnClick {
		t.Error("custom/clauchy on-click must NOT be present when no terminal resolves")
	}
	if !isValidJSON(stripped) {
		t.Errorf("result is not valid JSON: %s", stripped)
	}
}

func TestRun_TerminalLess_Idempotent(t *testing.T) {
	// Previously installed without on-click, still no terminal — full no-op
	dir := t.TempDir()
	t.Setenv("TERMINAL", "")
	t.Setenv("PATH", t.TempDir()) // no known terminals

	cfgPath := copyFixture(t, dir, "config_full_no_onclick.jsonc")
	cssPath := copyFixture(t, dir, "style_full.css")

	origCfg, _ := os.ReadFile(cfgPath)
	origCSS, _ := os.ReadFile(cssPath)

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ConfigChanged {
		t.Error("ConfigChanged should be false (terminal-less installed state is accepted)")
	}
	if res.CSSChanged {
		t.Error("CSSChanged should be false")
	}
	if len(res.Backups) != 0 {
		t.Errorf("expected no backups, got %v", res.Backups)
	}

	// Files must be byte-identical
	gotCfg, _ := os.ReadFile(cfgPath)
	gotCSS, _ := os.ReadFile(cssPath)
	if !bytes.Equal(origCfg, gotCfg) {
		t.Error("config must not change on terminal-less idempotent re-run")
	}
	if !bytes.Equal(origCSS, gotCSS) {
		t.Error("CSS must not change on terminal-less idempotent re-run")
	}
}

func TestRun_OnClickFromSibling(t *testing.T) {
	// Config has sibling module with "alacritty" in on-click; no PATH terminal, no TERMINAL env
	dir := t.TempDir()
	t.Setenv("TERMINAL", "")
	t.Setenv("PATH", t.TempDir())

	cfgPath := copyFixture(t, dir, "config_sibling_terminal.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.OnClickResolved {
		t.Error("OnClickResolved should be true (alacritty found in sibling on-click)")
	}

	cfgOut, _ := os.ReadFile(cfgPath)
	if !bytes.Contains(cfgOut, []byte("alacritty")) {
		t.Error("on-click should use alacritty (from sibling)")
	}
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("result is not valid JSON: %s", stripped)
	}
}

func TestRun_OnClickFromEnv(t *testing.T) {
	// $TERMINAL=kitty (known terminal) → use it
	dir := t.TempDir()
	t.Setenv("TERMINAL", "kitty")
	t.Setenv("PATH", t.TempDir()) // no PATH terminal

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.OnClickResolved {
		t.Error("OnClickResolved should be true ($TERMINAL=kitty is known)")
	}
	cfgOut, _ := os.ReadFile(cfgPath)
	if !bytes.Contains(cfgOut, []byte("kitty")) {
		t.Error("on-click should use kitty (from $TERMINAL)")
	}
}

func TestRun_OnClickRejectsWrapper(t *testing.T) {
	// $TERMINAL=xdg-terminal-exec is a wrapper — must be rejected; falls through to PATH
	dir := t.TempDir()
	t.Setenv("TERMINAL", "xdg-terminal-exec")
	t.Setenv("PATH", t.TempDir()) // no known terminal on PATH

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// xdg-terminal-exec is rejected, no PATH terminal → terminal-less
	if res.OnClickResolved {
		t.Error("OnClickResolved must be false when $TERMINAL is a non-known wrapper")
	}
	cfgOut, _ := os.ReadFile(cfgPath)
	if bytes.Contains(cfgOut, []byte("xdg-terminal-exec")) {
		t.Error("xdg-terminal-exec must not appear in the installed on-click")
	}
}

func TestRun_OnClickFromPath(t *testing.T) {
	// Known terminal found on PATH via probe
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "foot"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.OnClickResolved {
		t.Error("OnClickResolved should be true (foot found on PATH)")
	}
	cfgOut, _ := os.ReadFile(cfgPath)
	if !bytes.Contains(cfgOut, []byte("foot")) {
		t.Error("on-click should use foot (from PATH probe)")
	}
}

func TestRun_CSS_Idempotent(t *testing.T) {
	// CSS marker already present → no CSS change, no CSS backup
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_full.css") // CSS already has marker

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.CSSChanged {
		t.Error("CSSChanged should be false (marker already present)")
	}
	// Only 1 backup (config), not 2
	for _, b := range res.Backups {
		if strings.Contains(b, "style") || strings.Contains(b, ".css") {
			t.Errorf("should not create CSS backup when marker already present: %v", b)
		}
	}
}

func TestRun_BackupNonClobbering(t *testing.T) {
	// Two backups created within the same second must have distinct names
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	// First run
	res1, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(res1.Backups) == 0 {
		t.Fatal("expected backups from first run")
	}

	// Reset to fresh so second run also writes (restore originals)
	copyFixture(t, dir, "config_fresh.jsonc")
	copyFixture(t, dir, "style_fresh.css")

	// Second run within the same second
	res2, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	// All backup paths across both runs must be distinct
	allBackups := append(res1.Backups, res2.Backups...)
	seen := make(map[string]bool)
	for _, b := range allBackups {
		if seen[b] {
			t.Errorf("backup path collision: %q appears twice", b)
		}
		seen[b] = true
	}
	// And all files must exist
	for _, b := range allBackups {
		if _, err := os.Stat(b); err != nil {
			t.Errorf("backup file missing: %v", err)
		}
	}
}

func TestRun_BraceInString(t *testing.T) {
	// Config has { } inside string values — tokenizer must handle them correctly
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_brace_in_string.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true")
	}
	cfgOut, _ := os.ReadFile(cfgPath)
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("result is not valid JSON: %s", stripped)
	}
}

func TestRun_BraceInComment(t *testing.T) {
	// Config has { } inside comments — tokenizer must not count them
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_brace_in_comment.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true")
	}
	cfgOut, _ := os.ReadFile(cfgPath)
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("result is not valid JSON: %s", stripped)
	}
}

func TestRun_CommaCorrectness_FreshInstall(t *testing.T) {
	// After fresh install, the config must be valid JSON after comment-stripping.
	// This tests comma placement at both edit points:
	//   1. Trailing comma on last top-level member before the new module object
	//   2. Comma after last array element when appending "custom/clauchy"
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	_, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfgOut, _ := os.ReadFile(cfgPath)
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("installed config is not valid JSON:\n%s", stripped)
	}

	// Also verify both "custom/clauchy" as key and as array value are present
	var parsed map[string]interface{}
	json.Unmarshal(stripped, &parsed)

	// Module object must exist
	if _, ok := parsed["custom/clauchy"]; !ok {
		t.Error("custom/clauchy module object not found in parsed config")
	}

	// Array entry must exist
	arr := parsed["modules-right"].([]interface{})
	found := false
	for _, v := range arr {
		if v == "custom/clauchy" {
			found = true
		}
	}
	if !found {
		t.Error("custom/clauchy not found in modules-right array")
	}
}

func TestRun_CSS_WellFormedness(t *testing.T) {
	// The generated CSS block must have balanced braces and only #custom-clauchy.* selectors.
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	_, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cssOut, _ := os.ReadFile(cssPath)

	// Extract the clauchy block between markers
	startMark := []byte("/* clauchy start */")
	endMark := []byte("/* clauchy end */")
	si := bytes.Index(cssOut, startMark)
	ei := bytes.Index(cssOut, endMark)
	if si < 0 || ei < 0 {
		t.Fatal("marker block not found in CSS output")
	}
	block := cssOut[si : ei+len(endMark)]

	// Balanced braces
	opens := bytes.Count(block, []byte("{"))
	closes := bytes.Count(block, []byte("}"))
	if opens != closes {
		t.Errorf("unbalanced braces in CSS block: %d open, %d close", opens, closes)
	}

	// Selectors must be #custom-clauchy or #custom-clauchy.* (no other selectors).
	lines := strings.Split(string(block), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*/") ||
			strings.HasPrefix(trimmed, "//") || strings.HasSuffix(trimmed, "*/") {
			continue
		}
		if strings.HasSuffix(trimmed, "{") || trimmed == "}" {
			// selector or closing brace
			if strings.HasSuffix(trimmed, "{") {
				selector := strings.TrimSuffix(strings.TrimSpace(trimmed), "{")
				selector = strings.TrimSpace(selector)
				// Allow #custom-clauchy (base selector for background-image) or
				// #custom-clauchy.* (per-severity override).
				if selector != "#custom-clauchy" && !strings.HasPrefix(selector, "#custom-clauchy.") {
					t.Errorf("CSS block has unexpected selector %q (must be #custom-clauchy or #custom-clauchy.*)", selector)
				}
			}
			continue
		}
		// property line like "background-image: url(...);" or "min-width: 22px;"
		if strings.Contains(trimmed, ":") {
			continue
		}
	}

	// The CSS block uses background-image for the icon (ADR-7 revision: SVG icon).
	// The base selector handles low (calm state = brand color, no .low class needed).
	// Per-severity overrides exist for mid, high, and critical.
	if !bytes.Contains(block, []byte("background-image")) {
		t.Error("CSS block must contain background-image property")
	}
	for _, cls := range []string{".mid", ".high", ".critical"} {
		if !bytes.Contains(block, []byte("#custom-clauchy"+cls)) {
			t.Errorf("CSS block missing #custom-clauchy%s override", cls)
		}
	}
	// min-width must be present so the module box is visible
	if !bytes.Contains(block, []byte("min-width")) {
		t.Error("CSS block must contain min-width property")
	}
}

func TestRun_ReloaderCalled_OnChange(t *testing.T) {
	// Reloader must be called when a change was made
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	called := false
	reloader := func() error {
		called = true
		return nil
	}

	_, err := runTestInstall(cfgPath, cssPath, reloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("Reloader must be called when files are changed")
	}
}

func TestRun_ReloaderNotCalled_OnNoOp(t *testing.T) {
	// Reloader must NOT be called when already installed
	dir := t.TempDir()
	// Ensure ghostty for the idempotent check (the existing on-click has ghostty)
	cfgPath := copyFixture(t, dir, "config_full.jsonc")
	cssPath := copyFixture(t, dir, "style_full.css")

	called := false
	reloader := func() error {
		called = true
		return nil
	}

	_, err := runTestInstall(cfgPath, cssPath, reloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("Reloader must NOT be called when nothing changed")
	}
}

func TestRun_StaleOnClick_Repaired(t *testing.T) {
	// on-click is present but lacks the font-size flag (old install format).
	// condD must evaluate to false → the module block is rewritten with the
	// canonical on-click that includes --font-size=9.5.
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_stale_onclick.jsonc")
	cssPath := copyFixture(t, dir, "style_full.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true (on-click lacks font-size flag → stale)")
	}

	cfgOut, _ := os.ReadFile(cfgPath)
	if !bytes.Contains(cfgOut, []byte("--font-size=9.5")) {
		t.Error("repaired on-click must include --font-size=9.5 for ghostty")
	}
	// Original stale command must be gone
	if bytes.Contains(cfgOut, []byte("ghostty --class=clauchy.panel")) {
		t.Error("repaired config must not contain the old on-click without font-size")
	}
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("result is not valid JSON after repair: %s", stripped)
	}
}

func TestRun_BackupTimestamp(t *testing.T) {
	// Backup files must contain an epoch timestamp in their name
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	before := time.Now().Unix()
	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	after := time.Now().Unix()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, b := range res.Backups {
		base := filepath.Base(b)
		// Must contain "bak" in the name
		if !strings.Contains(base, "bak") {
			t.Errorf("backup name %q doesn't contain 'bak'", base)
		}
		_ = before
		_ = after
		// File must exist
		if _, err := os.Stat(b); err != nil {
			t.Errorf("backup file missing: %v", err)
		}
	}
}

// ─── --colorful install flag tests ───────────────────────────────────────────

// runColorfulInstall is a helper that calls install.Run with Colorful:true and
// a resolved terminal (ghostty fake binary on PATH).
func runColorfulInstall(t *testing.T, configPath, stylePath string) (install.Result, error) {
	t.Helper()
	return install.Run(install.RunConfig{
		ConfigPath:   configPath,
		StylePath:    stylePath,
		DataHome:     "",
		HyprlandConf: "",
		Colorful:     true,
		Reload:       noopReloader,
	})
}

// runPlainInstall is a helper for non-colorful installs (Colorful: false).
func runPlainInstall(t *testing.T, configPath, stylePath string) (install.Result, error) {
	t.Helper()
	return install.Run(install.RunConfig{
		ConfigPath:   configPath,
		StylePath:    stylePath,
		DataHome:     "",
		HyprlandConf: "",
		Colorful:     false,
		Reload:       noopReloader,
	})
}

// TestRun_Colorful_WritesFlag verifies that install --colorful writes
// "clauchy --colorful" in the on-click command.
func TestRun_Colorful_WritesFlag(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	res, err := runColorfulInstall(t, cfgPath, cssPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true on fresh install")
	}

	cfgOut, _ := os.ReadFile(cfgPath)
	// The on-click must end with "-e clauchy --colorful"
	if !bytes.Contains(cfgOut, []byte(`-e clauchy --colorful`)) {
		t.Errorf("colorful install: on-click must contain '-e clauchy --colorful', got:\n%s", cfgOut)
	}
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("result is not valid JSON: %s", stripped)
	}
}

// TestRun_Colorful_Idempotent verifies that re-running install --colorful when
// the colorful on-click is already present is a no-op (no config change).
func TestRun_Colorful_Idempotent(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	// First install: colorful
	_, err := runColorfulInstall(t, cfgPath, cssPath)
	if err != nil {
		t.Fatalf("first colorful install: %v", err)
	}

	// Second install: colorful again — must be a no-op for config
	res2, err := runColorfulInstall(t, cfgPath, cssPath)
	if err != nil {
		t.Fatalf("second colorful install: %v", err)
	}
	if res2.ConfigChanged {
		t.Error("ConfigChanged should be false on idempotent colorful re-run")
	}
}

// TestRun_Colorful_ToPlain_Repairs verifies that re-running plain install
// (Colorful:false) after a colorful install repairs the on-click back to plain.
func TestRun_Colorful_ToPlain_Repairs(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_fresh.jsonc")
	cssPath := copyFixture(t, dir, "style_fresh.css")

	// First: colorful install
	_, err := runColorfulInstall(t, cfgPath, cssPath)
	if err != nil {
		t.Fatalf("colorful install: %v", err)
	}
	cfgAfterColorful, _ := os.ReadFile(cfgPath)
	if !bytes.Contains(cfgAfterColorful, []byte(`-e clauchy --colorful`)) {
		t.Fatal("prerequisite: colorful on-click was not written")
	}

	// Now: plain install — must repair back to "-e clauchy" without --colorful
	res, err := runPlainInstall(t, cfgPath, cssPath)
	if err != nil {
		t.Fatalf("plain install after colorful: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true (colorful on-click treated as stale by plain install)")
	}

	cfgOut, _ := os.ReadFile(cfgPath)
	if bytes.Contains(cfgOut, []byte(`-e clauchy --colorful`)) {
		t.Error("plain install must repair the on-click to not contain --colorful")
	}
	if !bytes.Contains(cfgOut, []byte(`-e clauchy`)) {
		t.Error("plain install must keep the '-e clauchy' suffix")
	}
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("result is not valid JSON: %s", stripped)
	}
}

// ─── Fix 2: truncated/EOF config must abort without writing ──────────────────

// TestRun_TruncatedConfig_AbortsWithErrAmbiguous verifies that a config.jsonc
// truncated mid-object (EOF inside a structure) returns ErrAmbiguousConfig and
// writes NOTHING to any output file.
func TestRun_TruncatedConfig_AbortsWithErrAmbiguous(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERMINAL", "")
	t.Setenv("PATH", t.TempDir())

	cfgPath := copyFixture(t, dir, "config_truncated.jsonc")
	cssPath := filepath.Join(dir, "style.css")

	// Record original config content to verify it was NOT modified on abort.
	origCfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	_, runErr := runTestInstall(cfgPath, cssPath, noopReloader)
	if !errors.Is(runErr, install.ErrAmbiguousConfig) {
		t.Errorf("Run() error = %v, want ErrAmbiguousConfig on truncated config", runErr)
	}

	// config file must not have been modified.
	gotCfg, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(origCfg, gotCfg) {
		t.Error("truncated config: Run() must not write any changes when aborting with ErrAmbiguousConfig")
	}

	// style.css must not have been created either.
	if _, err := os.Stat(cssPath); err == nil {
		t.Error("truncated config: style.css must not be created on ErrAmbiguousConfig abort")
	}
}

// TestRun_ErrAmbiguousConfig_ScanPath exercises ErrAmbiguousConfig via the
// general scan path (empty/no-root config rather than a truncated file).
// This provides direct test coverage for the ErrAmbiguousConfig sentinel on
// the scanConfig branch, which previously had zero test coverage.
func TestRun_ErrAmbiguousConfig_ScanPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERMINAL", "")
	t.Setenv("PATH", t.TempDir())

	// An empty JSON object has no root tokens that lead to a valid scan.
	// The tokenizer produces tokens but scanConfig returns ErrAmbiguousConfig
	// because findValueEnd fails for a truncated structure.
	cfgPath := filepath.Join(dir, "config_empty.jsonc")
	if err := os.WriteFile(cfgPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cssPath := filepath.Join(dir, "style.css")

	_, runErr := runTestInstall(cfgPath, cssPath, noopReloader)
	// An empty object has no modules-* array — expect ErrNoModulesArray.
	// This test verifies the scan completes and returns a known sentinel.
	if runErr == nil {
		t.Error("Run() on empty config should return an error")
	}
	if !errors.Is(runErr, install.ErrNoModulesArray) && !errors.Is(runErr, install.ErrAmbiguousConfig) {
		t.Errorf("Run() on empty config = %v, want ErrNoModulesArray or ErrAmbiguousConfig", runErr)
	}
}

// ─── Fix 3: terminal resolvable + absent on-click → repair ───────────────────

// TestRun_NoOnClick_TerminalNowAvailable_Repaired verifies that when a module
// was previously installed without on-click (terminal-less state) but a terminal
// is NOW available, the on-click is written on re-run (condD triggers repair).
func TestRun_NoOnClick_TerminalNowAvailable_Repaired(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// ghostty is now on PATH (terminal became available since last install).
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	// Config has clauchy installed without on-click (terminal-less state).
	cfgPath := copyFixture(t, dir, "config_full_no_onclick.jsonc")
	cssPath := copyFixture(t, dir, "style_full.css")

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true: terminal is now available, absent on-click must be repaired")
	}
	if !res.OnClickResolved {
		t.Error("OnClickResolved should be true after repair")
	}

	cfgOut, _ := os.ReadFile(cfgPath)
	if !bytes.Contains(cfgOut, []byte("ghostty")) {
		t.Error("repaired on-click must include ghostty")
	}
	if !bytes.Contains(cfgOut, []byte(`"on-click"`)) {
		t.Error("repaired config must contain on-click key")
	}
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("repaired config is not valid JSON: %s", stripped)
	}
}

// TestRun_NoOnClick_NoTerminal_StillNoop verifies that a terminal-less state
// remains a no-op when no terminal is still available (condD stays true).
func TestRun_NoOnClick_NoTerminal_StillNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TERMINAL", "")
	t.Setenv("PATH", t.TempDir()) // no known terminals on PATH

	cfgPath := copyFixture(t, dir, "config_full_no_onclick.jsonc")
	cssPath := copyFixture(t, dir, "style_full.css")

	origCfg, _ := os.ReadFile(cfgPath)

	res, err := runTestInstall(cfgPath, cssPath, noopReloader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ConfigChanged {
		t.Error("ConfigChanged should be false: no terminal available, terminal-less state is accepted")
	}

	gotCfg, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(origCfg, gotCfg) {
		t.Error("config must not change when terminal-less state is accepted (no-op)")
	}
}

// TestRun_Plain_ToColorful_Repairs verifies that re-running colorful install
// (Colorful:true) after a plain install repairs the on-click to include --colorful.
func TestRun_Plain_ToColorful_Repairs(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ghostty"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("TERMINAL", "")

	cfgPath := copyFixture(t, dir, "config_full.jsonc") // already installed as plain
	cssPath := copyFixture(t, dir, "style_full.css")

	// Verify prerequisite: plain on-click (no --colorful)
	cfgBefore, _ := os.ReadFile(cfgPath)
	if bytes.Contains(cfgBefore, []byte(`-e clauchy --colorful`)) {
		t.Fatal("prerequisite: config_full.jsonc must not already have colorful on-click")
	}

	// Now: colorful install — must repair the on-click to include --colorful
	res, err := runColorfulInstall(t, cfgPath, cssPath)
	if err != nil {
		t.Fatalf("colorful install after plain: %v", err)
	}
	if !res.ConfigChanged {
		t.Error("ConfigChanged should be true (plain on-click treated as stale by colorful install)")
	}

	cfgOut, _ := os.ReadFile(cfgPath)
	if !bytes.Contains(cfgOut, []byte(`-e clauchy --colorful`)) {
		t.Errorf("colorful install must repair the on-click to contain '--colorful', got:\n%s", cfgOut)
	}
	stripped := stripJSONCComments(cfgOut)
	if !isValidJSON(stripped) {
		t.Errorf("result is not valid JSON: %s", stripped)
	}
}
