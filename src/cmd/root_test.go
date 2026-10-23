// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestRootHelpRecommendsGroupedCommands(t *testing.T) {
	oldVersion := version
	version = "1.0.0+test"
	t.Cleanup(func() { version = oldVersion })

	root := newRootCommand()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root help: %v", err)
	}

	got := out.String()
	for _, want := range []string{"GDN CLI", "Version: 1.0.0+test", "app", "resource"} {
		if !strings.Contains(got, want) {
			t.Fatalf("root help missing %q:\n%s", want, got)
		}
	}
	for _, legacy := range []string{"create-app", "add-version", "list-resources"} {
		if strings.Contains(got, legacy) {
			t.Fatalf("root help should not recommend legacy alias %q:\n%s", legacy, got)
		}
	}
}

func TestRootVersionFlags(t *testing.T) {
	oldVersion := version
	version = "1.0.0+test"
	t.Cleanup(func() { version = oldVersion })

	for _, flag := range []string{"-v", "--version"} {
		root := newRootCommand()
		root.SetErr(io.Discard)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs([]string{flag})

		if err := root.Execute(); err != nil {
			t.Fatalf("root %s: %v", flag, err)
		}
		if got := strings.TrimSpace(out.String()); got != "1.0.0+test" {
			t.Fatalf("root %s output = %q, want version", flag, got)
		}
	}
}

func TestLeafHelpAllPrintsFullFlagReference(t *testing.T) {
	_, stderr := captureOutput(t, func() {
		root := newRootCommand()
		root.SetArgs([]string{"app", "create", "--help-all"})
		if err := root.Execute(); err != nil {
			t.Fatalf("help-all execute: %v", err)
		}
	})

	for _, want := range []string{"All flags:", "-allow-duplicate", "come from the org config file"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("help-all missing %q:\n%s", want, stderr)
		}
	}
}

func TestLeafHelpHasActionableExamples(t *testing.T) {
	commands := [][]string{
		{"config", "check"},
		{"config", "list"},
		{"config", "show"},
		{"config", "set"},
		{"config", "use"},
		{"version"},
		{"app", "create"},
		{"app", "update"},
		{"app", "clone"},
		{"app", "delete"},
		{"app", "launch"},
		{"app", "list"},
		{"app", "version", "add"},
		{"resource", "upload"},
		{"resource", "update"},
		{"resource", "delete"},
		{"resource", "list"},
	}

	for _, command := range commands {
		name := strings.Join(command, " ")
		t.Run(name, func(t *testing.T) {
			args := append(append([]string(nil), command...), "--help")
			stdout, stderr := captureOutput(t, func() {
				root := newRootCommand()
				root.SetArgs(args)
				if err := root.Execute(); err != nil {
					t.Fatalf("%s --help: %v", name, err)
				}
			})
			got := stdout + stderr
			for _, want := range []string{"Usage:", "Examples:"} {
				if !strings.Contains(got, want) {
					t.Fatalf("%s compact help missing %q:\n%s", name, want, got)
				}
			}
			if strings.Contains(got, "All flags:") {
				t.Fatalf("%s compact help should not include full flag dump:\n%s", name, got)
			}
		})
	}
}

func TestGroupHelpIsCompactWithHelpAllEscapeHatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "root", args: []string{"--help"}},
		{name: "app", args: []string{"app", "--help"}},
		{name: "resource", args: []string{"resource", "--help"}},
		{name: "app version", args: []string{"app", "version", "--help"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newRootCommand()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(io.Discard)
			root.SetArgs(tc.args)

			if err := root.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			got := out.String()
			for _, want := range []string{"Usage:", "Commands:", "More:", "--help-all"} {
				if !strings.Contains(got, want) {
					t.Fatalf("compact group help missing %q:\n%s", want, got)
				}
			}
			for _, notWant := range []string{"Global Flags:", "--org"} {
				if strings.Contains(got, notWant) {
					t.Fatalf("compact group help should not include %q:\n%s", notWant, got)
				}
			}
		})
	}

	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"app", "--help-all"})
	if err := root.Execute(); err != nil {
		t.Fatalf("app --help-all: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Commands:", "Global flags:", "--org"} {
		if !strings.Contains(got, want) {
			t.Fatalf("full group help missing %q:\n%s", want, got)
		}
	}
}

func TestGeneratedCommandHelpIsCompact(t *testing.T) {
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"completion", "bash", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("completion bash help: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Usage:",
		"Common:",
		"Current session:",
		"source <(gdn completion bash --script)",
		"Persistent bash:",
		"~/.local/share/bash-completion/completions/gdn",
		"--help-all",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("completion compact help missing %q:\n%s", want, got)
		}
	}
	for _, notWant := range []string{"/etc/bash_completion.d/gdn", "Global flags:"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("completion compact help should not include long docs %q:\n%s", notWant, got)
		}
	}
}

func TestCompletionParentHelpPointsToShellHelp(t *testing.T) {
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"completion", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("completion help: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Commands:",
		"gdn completion <shell> -h",
		"gdn completion zsh -h",
		"gdn completion bash -h",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("completion parent help missing %q:\n%s", want, got)
		}
	}
	for _, notWant := range []string{
		"gdn completion fish --script | source",
		"gdn completion powershell --script | Out-String | Invoke-Expression",
		"~/.config/fish/completions",
	} {
		if strings.Contains(got, notWant) {
			t.Fatalf("completion parent help should not include shell setup detail %q:\n%s", notWant, got)
		}
	}
}

func TestCompletionCommandShowsHelpByDefault(t *testing.T) {
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"completion", "bash"})

	if err := root.Execute(); err != nil {
		t.Fatalf("completion bash: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Current session:", "--script"} {
		if !strings.Contains(got, want) {
			t.Fatalf("completion bash help missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "__start_gdn") {
		t.Fatalf("completion bash should show help by default, not script:\n%s", got)
	}
}

func TestCompletionCommandGeneratesScriptWithScriptFlag(t *testing.T) {
	var err error
	got := captureStdout(t, func() {
		root := newRootCommand()
		root.SetErr(io.Discard)
		root.SetArgs([]string{"completion", "bash", "--script"})
		err = root.Execute()
	})
	if err != nil {
		t.Fatalf("completion bash --script: %v", err)
	}
	for _, want := range []string{"__start_gdn", "complete -o default", "__complete"} {
		if !strings.Contains(got, want) {
			t.Fatalf("completion script missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Current session:") {
		t.Fatalf("completion command generated help instead of script:\n%s", got)
	}
}

func TestExecuteHelpAllForGeneratedCommandDoesNotRunCommand(t *testing.T) {
	oldArgs := os.Args
	os.Args = []string{"gdn", "completion", "bash", "--help-all"}
	t.Cleanup(func() { os.Args = oldArgs })

	var code int
	stdout, stderr := captureOutput(t, func() {
		code = Execute()
	})
	if code != 0 {
		t.Fatalf("Execute exit code = %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Generate bash completion setup or script") || !strings.Contains(stdout, "Flags:") {
		t.Fatalf("help-all did not print generated command help:\n%s", stdout)
	}
	if strings.Contains(stdout, "__start_gdn") {
		t.Fatalf("help-all generated completion script instead of help:\n%s", stdout)
	}
}

func TestLegacyAliasWorksAndWarns(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/app.exe", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldStderr := os.Stderr
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = pw
	t.Cleanup(func() { os.Stderr = oldStderr })

	root := newRootCommand()
	root.SetArgs([]string{
		"create-app",
		"--dir", dir,
		"--app-name", "Alias-App",
		"--version", "1.0.0",
		"--dry-run",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("legacy alias execute: %v", err)
	}

	_ = pw.Close()
	os.Stderr = oldStderr
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, pr)
	if !strings.Contains(buf.String(), "deprecated") || !strings.Contains(buf.String(), "gdn app create") {
		t.Fatalf("legacy alias warning missing replacement:\n%s", buf.String())
	}
}
