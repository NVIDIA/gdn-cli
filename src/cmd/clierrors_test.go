// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"strings"
	"testing"
)

func TestMissingFlags(t *testing.T) {
	got := missingFlags(map[string]string{
		"version": "1.0.0",
		"name":    "my-app-1",
		"dir":     "",
	})
	if len(got) != 1 || got[0] != "--dir" {
		t.Fatalf("missingFlags() = %v, want [--dir]", got)
	}
}

func TestFormatMissingFlagsError(t *testing.T) {
	if got := formatMissingFlagsError([]string{"--dir"}); got != "missing required flag: --dir" {
		t.Errorf("single = %q", got)
	}
	if got := formatMissingFlagsError([]string{"--dir", "--version"}); got != "missing required flags: --dir, --version" {
		t.Errorf("multiple = %q", got)
	}
}

func TestFormatRequiredFlagStatus(t *testing.T) {
	got := formatRequiredFlagStatus(map[string]string{
		"dir":     "",
		"version": "1.0.0",
		"name":    "my-app-1",
	})
	if !strings.Contains(got, "--dir") || !strings.Contains(got, "(missing)") {
		t.Errorf("expected missing dir line, got:\n%s", got)
	}
	if !strings.Contains(got, "1.0.0") || !strings.Contains(got, "my-app-1") {
		t.Errorf("expected parsed values, got:\n%s", got)
	}
}

func TestDirFlagSkippedHint(t *testing.T) {
	args := []string{"--dir", "--version", "1.0.0", "--name", "my-app-1"}
	got := dirFlagSkippedHint(args)
	if got == "" {
		t.Fatal("expected hint when --dir is followed by another flag")
	}
	if !strings.Contains(got, "--version") {
		t.Errorf("hint = %q", got)
	}
}

func TestTrailingBackslashHint(t *testing.T) {
	args := []string{"--dir", `C:\Program Files\My App\`, "--version", "1.0.0"}
	got := trailingBackslashHint(args)
	if got == "" {
		t.Fatal("expected trailing backslash hint")
	}
	if !strings.Contains(got, "trailing slash") {
		t.Errorf("hint = %q", got)
	}
}

func TestFlagParseHintsWindowsQuoting(t *testing.T) {
	args := []string{"--dir", "--version", "1.0.0", "--name", "my-app-1"}
	hints := flagParseHints(args, map[string]bool{"dir": true})
	if len(hints) == 0 {
		t.Fatal("expected at least one hint")
	}
	combined := strings.Join(hints, " ")
	if !strings.Contains(combined, "After --dir") {
		t.Errorf("hints = %v", hints)
	}
}

func TestFlagParseHintsDirConsumedVersion(t *testing.T) {
	args := []string{"--dir", "--version", "1.0.0", "--name", "my-app-1"}
	hints := flagParseHints(args, map[string]bool{"dir": false, "version": true, "name": true})
	if len(hints) == 0 {
		t.Fatal("expected hint when --dir consumed --version")
	}
	if !strings.Contains(hints[0], "consumed") {
		t.Errorf("hints = %v", hints)
	}
}

func TestFormatRequiredFlagStatusSuspiciousDir(t *testing.T) {
	got := formatRequiredFlagStatus(map[string]string{"dir": "--version", "version": ""})
	if !strings.Contains(got, "does not look like a path") {
		t.Errorf("got:\n%s", got)
	}
}
