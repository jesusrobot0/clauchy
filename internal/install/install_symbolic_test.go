package install_test

// Change 17 — Theme-adaptive symbolic bar icon (RED phase tests, written first).
//
// Scope:
//   - Single claude-symbolic.svg with fill="#bebebe" instead of 4 color variants.
//   - CSS uses -gtk-recolor(url(...)), 12.5px/16px, severity via color: property.
//   - Old variant files (icon-{low,mid,high,critical}.svg) are best-effort removed.
//   - CSS idempotency: old 4-image block is detected as stale → repaired.
//   - IconSeverityColors() returns only the 3 severity hexes (no "low" key).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jesusrobot0/clauchy/internal/install"
)

// TestRun_SymbolicIcon_Written verifies that clauchy install writes exactly one
// claude-symbolic.svg file to DataHome/clauchy/ with the GTK symbolic base color
// (#bebebe fill) instead of four severity-colored variant files.
func TestRun_SymbolicIcon_Written(t *testing.T) {
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
	data, err := os.ReadFile(symbolicPath)
	if err != nil {
		t.Fatalf("claude-symbolic.svg not written: %v", err)
	}

	// Must carry the GTK symbolic convention base color #bebebe.
	if !bytes.Contains(data, []byte(`fill="#bebebe"`)) {
		t.Errorf("claude-symbolic.svg must have fill=#bebebe, got:\n%s", data)
	}
}

// TestRun_NoVariantFiles_Written verifies that the four old severity-colored
// variant files are NOT written (they are replaced by claude-symbolic.svg).
func TestRun_NoVariantFiles_Written(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")

	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	iconDir := filepath.Join(cfg.DataHome, "clauchy")
	for _, name := range []string{"icon-low.svg", "icon-mid.svg", "icon-high.svg", "icon-critical.svg"} {
		p := filepath.Join(iconDir, name)
		if _, err := os.Stat(p); err == nil {
			t.Errorf("variant file %s must NOT be written (replaced by claude-symbolic.svg)", name)
		}
	}
}

// TestRun_OldVariantFiles_Removed verifies that existing old variant files
// (from a previous install) are best-effort removed on re-run.
func TestRun_OldVariantFiles_Removed(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")

	iconDir := filepath.Join(cfg.DataHome, "clauchy")
	if err := os.MkdirAll(iconDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Plant old variant files simulating a previous install.
	for _, name := range []string{"icon-low.svg", "icon-mid.svg", "icon-high.svg", "icon-critical.svg"} {
		if err := os.WriteFile(filepath.Join(iconDir, name), []byte("<svg/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Old files should be gone (best-effort; errors are ignored so test must see them removed).
	for _, name := range []string{"icon-low.svg", "icon-mid.svg", "icon-high.svg", "icon-critical.svg"} {
		p := filepath.Join(iconDir, name)
		if _, err := os.Stat(p); err == nil {
			t.Errorf("old variant file %s was not removed on re-install", name)
		}
	}
}

// TestRun_CSS_GtkRecolor verifies the generated CSS block uses -gtk-recolor() and
// the updated sizing (12.5px / 16px) with severity via color: property.
func TestRun_CSS_GtkRecolor(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")

	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cssData, err := os.ReadFile(cfg.StylePath)
	if err != nil {
		t.Fatalf("read css: %v", err)
	}
	s := string(cssData)

	// Must use -gtk-recolor wrapping the symbolic icon.
	if !strings.Contains(s, "-gtk-recolor(url(") {
		t.Error("CSS must use -gtk-recolor(url(...))")
	}
	if !strings.Contains(s, "claude-symbolic.svg") {
		t.Error("CSS must reference claude-symbolic.svg")
	}

	// Sizing: 12.5px 12.5px and min-width: 16px.
	if !strings.Contains(s, "background-size: 12.5px 12.5px") {
		t.Error("CSS must contain background-size: 12.5px 12.5px")
	}
	if !strings.Contains(s, "min-width: 16px") {
		t.Error("CSS must contain min-width: 16px")
	}

	// Severity overrides use color: not background-image:.
	if !strings.Contains(s, "#custom-clauchy.mid") {
		t.Error("CSS must contain #custom-clauchy.mid override")
	}
	if !strings.Contains(s, "color: #f9e2af") {
		t.Error("CSS .mid override must use color: #f9e2af")
	}
	if !strings.Contains(s, "color: #fab387") {
		t.Error("CSS .high override must use color: #fab387")
	}
	if !strings.Contains(s, "color: #f38ba8") {
		t.Error("CSS .critical override must use color: #f38ba8")
	}

	// There must be NO .low color override (low inherits theme foreground).
	if strings.Contains(s, "#custom-clauchy.low") {
		t.Error("CSS must NOT have a .low override (low inherits theme foreground)")
	}

	// Old background-image severity overrides must not be present.
	if strings.Contains(s, "icon-low.svg") || strings.Contains(s, "icon-mid.svg") {
		t.Error("CSS must not reference old severity variant filenames")
	}
}

// TestRun_CSS_StaleVariantBlock_Repaired verifies that an existing CSS block
// that uses the old 4-image background-image approach is detected as stale and
// repaired to the new -gtk-recolor block. The idempotency fixture style_full.css
// currently has the old content; after repair style_full.css content triggers
// CSSChanged=true and the new block replaces the old one.
func TestRun_CSS_StaleVariantBlock_Repaired(t *testing.T) {
	// style_full.css contains the old background-image block (14px/18px variant-based).
	cfg, _ := makeRunConfig(t, "config_full.jsonc", "style_full.css")

	res, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.CSSChanged {
		t.Fatal("CSSChanged = false — old variant-based CSS block was not repaired to -gtk-recolor")
	}

	cssData, err := os.ReadFile(cfg.StylePath)
	if err != nil {
		t.Fatalf("read css: %v", err)
	}
	s := string(cssData)

	if !strings.Contains(s, "-gtk-recolor(url(") {
		t.Error("repaired CSS must contain -gtk-recolor")
	}
	if strings.Contains(s, "icon-low.svg") {
		t.Error("repaired CSS must not contain old icon-low.svg reference")
	}
	// Exactly one marker pair.
	if n := strings.Count(s, "/* clauchy start */"); n != 1 {
		t.Errorf("want exactly 1 start marker after repair, got %d", n)
	}

	// Second run must be a no-op.
	res2, err := install.Run(cfg)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res2.CSSChanged {
		t.Error("second run: CSSChanged = true — repair is not idempotent")
	}
}

// TestIconSeverityColors_OnlyThreeSeverities verifies that IconSeverityColors()
// returns exactly the three severity hexes (mid/high/critical) for the CSS color:
// overrides. The "low" key must not be present because low has no color override —
// it inherits the theme foreground via -gtk-recolor.
func TestIconSeverityColors_OnlyThreeSeverities(t *testing.T) {
	colors := install.IconSeverityColors()

	// Must have the three severity entries.
	if colors["mid"] != "#f9e2af" {
		t.Errorf("mid = %q, want #f9e2af", colors["mid"])
	}
	if colors["high"] != "#fab387" {
		t.Errorf("high = %q, want #fab387", colors["high"])
	}
	if colors["critical"] != "#f38ba8" {
		t.Errorf("critical = %q, want #f38ba8", colors["critical"])
	}

	// "low" must NOT be present (low inherits theme foreground; no CSS color override).
	if _, ok := colors["low"]; ok {
		t.Error("IconSeverityColors() must not contain 'low' key (no CSS color override for low state)")
	}
}

// TestRun_SymbolicIcon_ContainsSVGStructure verifies claude-symbolic.svg is a
// valid SVG document (has <svg and </svg>).
func TestRun_SymbolicIcon_ContainsSVGStructure(t *testing.T) {
	cfg, _ := makeRunConfig(t, "config_fresh.jsonc", "style_fresh.css")
	if _, err := install.Run(cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cfg.DataHome, "clauchy", "claude-symbolic.svg"))
	if err != nil {
		t.Fatalf("read claude-symbolic.svg: %v", err)
	}
	if !bytes.Contains(data, []byte("<svg")) {
		t.Error("claude-symbolic.svg: missing <svg element")
	}
	if !bytes.Contains(data, []byte("</svg>")) {
		t.Error("claude-symbolic.svg: missing </svg> closing tag")
	}
}
