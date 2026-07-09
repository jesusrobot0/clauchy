package main

import (
	"fmt"
	"testing"
)

// TestParseArgs verifies the parseArgs pure function correctly maps CLI
// argument slices to (mode, colorful) pairs.
// This is a RED test: parseArgs does not exist yet, so it will fail to compile.
func TestParseArgs(t *testing.T) {
	cases := []struct {
		name         string
		args         []string
		wantMode     string
		wantColorful bool
	}{
		{"no args → dashboard monochrome", []string{}, "dashboard", false},
		{"--colorful only → dashboard colorful", []string{"--colorful"}, "dashboard", true},
		{"waybar", []string{"waybar"}, "waybar", false},
		{"install", []string{"install"}, "install", false},
		{"version", []string{"--version"}, "version", false},
		{"--colorful + waybar", []string{"--colorful", "waybar"}, "waybar", true},
		{"waybar + --colorful", []string{"waybar", "--colorful"}, "waybar", true},
		{"install --colorful", []string{"install", "--colorful"}, "install", true},
		{"--colorful install", []string{"--colorful", "install"}, "install", true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("%s", tc.name), func(t *testing.T) {
			mode, colorful := parseArgs(tc.args)
			if mode != tc.wantMode {
				t.Errorf("parseArgs(%v): mode = %q, want %q", tc.args, mode, tc.wantMode)
			}
			if colorful != tc.wantColorful {
				t.Errorf("parseArgs(%v): colorful = %v, want %v", tc.args, colorful, tc.wantColorful)
			}
		})
	}
}
