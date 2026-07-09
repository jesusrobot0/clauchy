// Package paths resolves all filesystem locations used by clauchy from
// environment variables and the user's home directory.
// All resolution is driven by os.Getenv so tests can inject any layout
// via t.Setenv without touching the real home directory.
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// Paths holds every filesystem location clauchy needs.
type Paths struct {
	// CredentialsFile is the resolved path to the Claude OAuth credentials JSON.
	// The first tier (CLAUDE_CONFIG_DIR entry, XDG, or ~/.claude) whose
	// .credentials.json actually exists wins; default: ~/.claude/.credentials.json.
	CredentialsFile string

	// CacheDir is the directory where clauchy stores its runtime cache files
	// (usage.json, .fetch.lock). Defaults to $XDG_CACHE_HOME/clauchy or
	// ~/.cache/clauchy.
	CacheDir string

	// PricingOverride is the optional per-model pricing JSON that overrides the
	// embedded rate table. Defaults to $XDG_CONFIG_HOME/clauchy/pricing.json or
	// ~/.config/clauchy/pricing.json.
	PricingOverride string

	// WaybarConfig is the path to the Waybar config.jsonc file.
	WaybarConfig string

	// WaybarStyle is the sibling style.css of WaybarConfig.
	WaybarStyle string

	// DataHome is $XDG_DATA_HOME (or ~/.local/share). The install package writes
	// SVG icon variants to DataHome/clauchy/.
	DataHome string

	// HyprlandConf is the path to ~/.config/hypr/hyprland.conf.
	// It is set only when that file exists on disk; empty otherwise (non-Hyprland
	// setup). Install checks the path and skips gracefully when empty.
	HyprlandConf string

	// TranscriptRoots is the ordered list of directories in which clauchy
	// searches for Claude session JSONL files.
	TranscriptRoots []string
}

// Resolve builds a Paths value from the current environment.
// Resolution order for TranscriptRoots:
//  1. Comma-separated entries in CLAUDE_CONFIG_DIR (each appended with /projects).
//  2. If CLAUDE_CONFIG_DIR is unset/empty: $XDG_CONFIG_HOME/claude/projects.
//  3. If XDG_CONFIG_HOME is also unset/empty: $HOME/.claude/projects.
//
// Credentials file uses existence-gating across ALL tiers in priority order.
//
// HyprlandConf is set only when ~/.config/hypr/hyprland.conf exists on disk.
// DataHome is $XDG_DATA_HOME or ~/.local/share.
func Resolve() (Paths, error) {
	home := os.Getenv("HOME")
	configHome := xdgConfigHome(home)
	cacheHome := xdgCacheHome(home)
	dataHome := xdgDataHome(home)

	p := Paths{
		CacheDir:        filepath.Join(cacheHome, "clauchy"),
		PricingOverride: filepath.Join(configHome, "clauchy", "pricing.json"),
		WaybarConfig:    filepath.Join(configHome, "waybar", "config.jsonc"),
		WaybarStyle:     filepath.Join(configHome, "waybar", "style.css"),
		DataHome:        dataHome,
	}

	// HyprlandConf: only set when the file exists (non-Hyprland setups have no
	// hyprland.conf; install skips gracefully when this field is empty).
	hyprConf := filepath.Join(configHome, "hypr", "hyprland.conf")
	if _, err := os.Stat(hyprConf); err == nil {
		p.HyprlandConf = hyprConf
	}

	claudeConfigDir := os.Getenv("CLAUDE_CONFIG_DIR")

	if claudeConfigDir != "" {
		// CLAUDE_CONFIG_DIR: comma-split; each entry contributes a transcript root.
		// Whitespace-only entries are ignored; only non-blank entries are used.
		for _, entry := range strings.Split(claudeConfigDir, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			p.TranscriptRoots = append(p.TranscriptRoots, filepath.Join(entry, "projects"))
		}
	}

	if len(p.TranscriptRoots) == 0 {
		// CLAUDE_CONFIG_DIR was empty/unset OR contained only whitespace entries
		// that all got filtered out. Fall through to XDG/home tiers.
		//
		// Existence-gate the XDG tier: only select it when
		// $XDG_CONFIG_HOME/claude/projects actually EXISTS. This prevents
		// returning ~/.config/claude/projects when ~/.claude/projects has files.
		xdgAdded := false
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			candidate := filepath.Join(xdg, "claude", "projects")
			if _, err := os.Stat(candidate); err == nil {
				p.TranscriptRoots = append(p.TranscriptRoots, candidate)
				xdgAdded = true
			}
		}
		if !xdgAdded {
			// Home fallback: always returned (whether or not it exists yet),
			// consistent with credential default behavior.
			p.TranscriptRoots = append(p.TranscriptRoots, filepath.Join(home, ".claude", "projects"))
		}
	}

	// Credentials file: existence-gate all tiers in priority order.
	p.CredentialsFile = resolveCredentials(home, configHome, claudeConfigDir)

	return p, nil
}

// resolveCredentials returns the path to the first existing .credentials.json
// found by scanning tiers in priority order. If none exists the default
// ~/.claude/.credentials.json is returned.
func resolveCredentials(home, configHome, claudeConfigDir string) string {
	var tiers []string

	// Tier 1: each CLAUDE_CONFIG_DIR entry.
	if claudeConfigDir != "" {
		for _, entry := range strings.Split(claudeConfigDir, ",") {
			entry = strings.TrimSpace(entry)
			if entry != "" {
				tiers = append(tiers, entry)
			}
		}
	}

	// Tier 2: XDG config home / claude.
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		tiers = append(tiers, filepath.Join(xdg, "claude"))
	}

	// Tier 3: ~/.claude (always appended; used as default when nothing exists).
	tiers = append(tiers, filepath.Join(home, ".claude"))

	for _, dir := range tiers {
		candidate := filepath.Join(dir, ".credentials.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Default: ~/.claude/.credentials.json (whether or not it exists yet).
	return filepath.Join(home, ".claude", ".credentials.json")
}

// xdgConfigHome returns $XDG_CONFIG_HOME when set, otherwise $HOME/.config.
func xdgConfigHome(home string) string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	return filepath.Join(home, ".config")
}

// xdgCacheHome returns $XDG_CACHE_HOME when set, otherwise $HOME/.cache.
func xdgCacheHome(home string) string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return v
	}
	return filepath.Join(home, ".cache")
}

// xdgDataHome returns $XDG_DATA_HOME when set, otherwise $HOME/.local/share.
func xdgDataHome(home string) string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v
	}
	return filepath.Join(home, ".local", "share")
}
