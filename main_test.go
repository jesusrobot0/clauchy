package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
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
		wantErr      bool
	}{
		{"no args → dashboard monochrome", []string{}, "dashboard", false, false},
		{"--colorful only → dashboard colorful", []string{"--colorful"}, "dashboard", true, false},
		{"waybar", []string{"waybar"}, "waybar", false, false},
		{"install", []string{"install"}, "install", false, false},
		{"version", []string{"--version"}, "version", false, false},
		{"help", []string{"--help"}, "help", false, false},
		{"--colorful + waybar", []string{"--colorful", "waybar"}, "waybar", true, false},
		{"waybar + --colorful", []string{"waybar", "--colorful"}, "waybar", true, false},
		{"install --colorful", []string{"install", "--colorful"}, "install", true, false},
		{"--colorful install", []string{"--colorful", "install"}, "install", true, false},
		{"unknown argument", []string{"typo"}, "", false, true},
		{"multiple modes", []string{"waybar", "install"}, "", false, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("%s", tc.name), func(t *testing.T) {
			mode, colorful, err := parseArgs(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseArgs(%v): error = %v, wantErr %v", tc.args, err, tc.wantErr)
			}
			if mode != tc.wantMode {
				t.Errorf("parseArgs(%v): mode = %q, want %q", tc.args, mode, tc.wantMode)
			}
			if colorful != tc.wantColorful {
				t.Errorf("parseArgs(%v): colorful = %v, want %v", tc.args, colorful, tc.wantColorful)
			}
		})
	}
}

func TestResolvedVersion(t *testing.T) {
	cases := []struct {
		name   string
		linked string
		info   *debug.BuildInfo
		ok     bool
		want   string
	}{
		{name: "ldflags wins", linked: "v1.2.3", info: &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}}, ok: true, want: "v1.2.3"},
		{name: "go install module version", linked: "dev", info: &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, ok: true, want: "v1.2.3"},
		{name: "local build", linked: "dev", info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, ok: true, want: "dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvedVersion(tc.linked, func() (*debug.BuildInfo, bool) { return tc.info, tc.ok })
			if got != tc.want {
				t.Fatalf("resolvedVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCLIContracts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping CLI build in short mode")
	}
	binary := filepath.Join(t.TempDir(), "clauchy")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}

	help := exec.Command(binary, "--help")
	helpOutput, err := help.CombinedOutput()
	if err != nil {
		t.Fatalf("clauchy --help: %v\n%s", err, helpOutput)
	}
	if !strings.Contains(string(helpOutput), "Usage:") {
		t.Fatalf("help output missing Usage: %q", helpOutput)
	}

	versionCmd := exec.Command(binary, "--version")
	versionOutput, err := versionCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("clauchy --version: %v\n%s", err, versionOutput)
	}
	if !strings.HasPrefix(string(versionOutput), "clauchy ") {
		t.Fatalf("version output = %q, want clauchy prefix", versionOutput)
	}

	invalid := exec.Command(binary, "typo")
	invalidOutput, err := invalid.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("clauchy typo exit = %v, want code 2; output: %s", err, invalidOutput)
	}
	if !strings.Contains(string(invalidOutput), `unknown argument "typo"`) {
		t.Fatalf("invalid-argument output missing explanation: %q", invalidOutput)
	}
}
