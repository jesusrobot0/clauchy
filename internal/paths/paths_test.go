package paths_test

import (
	"os"
	"path/filepath"
	"testing"

	"clauchy/internal/paths"
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

	p, err := paths.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}

	want := filepath.Join(xdg, "claude", "projects")
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
