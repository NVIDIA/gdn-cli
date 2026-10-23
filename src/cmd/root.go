// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cmd wires the gdn command hierarchy.
package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	version = "dev"

	globalFlagEnv = map[string]string{
		"org": "NGC_ORG",
	}
)

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	args, fullHelp := rewriteHelpAll(os.Args[1:])
	oldFullHelp := fullHelpRequested
	fullHelpRequested = oldFullHelp || fullHelp
	defer func() { fullHelpRequested = oldFullHelp }()

	args, gf, err := extractGlobalFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	// Fold the selected org's stored config in below the shell environment but
	// above built-in defaults. The --org flag selects the org config. Explicit
	// global flags are applied afterwards so they always win.
	applyConfig(gf.values["org"])
	for key, value := range gf.values {
		if envName := globalFlagEnv[key]; envName != "" {
			_ = os.Setenv(envName, value)
		}
	}

	root := newRootCommand()
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		if err != errUsageShown {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		if isCobraUsageError(err) {
			return 2
		}
		return 1
	}
	return 0
}

func isCobraUsageError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "unknown command ") ||
		strings.HasPrefix(msg, "unknown flag: ") ||
		strings.HasPrefix(msg, "unknown shorthand flag: ")
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "gdn",
		Short:         "GDN CLI for publishing and managing GDN applications",
		Long:          "GDN CLI for publishing and managing GDN applications.\n\nUpload app folders to NGC, create or update GDN apps, manage app versions, and launch sessions.",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion, _ := cmd.Flags().GetBool("version"); showVersion {
				fmt.Fprintln(cmd.OutOrStdout(), version)
				return nil
			}
			return cmd.Help()
		},
	}

	root.Flags().BoolP("version", "v", false, "print version and exit")
	root.PersistentFlags().Bool(flagHelpAll, false, "show advanced help")
	_ = root.PersistentFlags().MarkHidden(flagHelpAll)
	root.PersistentFlags().String("org", "", "NGC org name; selects the stored org config (env NGC_ORG)")
	root.SetHelpFunc(cobraHelp)

	root.AddCommand(newAppCommand())
	root.AddCommand(newResourceCommand())
	root.AddCommand(newConfigCommand())
	root.AddCommand(newVersionCommand())
	root.AddCommand(newCompletionCommand())
	addLegacyCommands(root)
	return root
}

func newGroupCommand(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
}

func cobraHelp(cmd *cobra.Command, args []string) {
	full, _ := cmd.Flags().GetBool(flagHelpAll)
	printCobraHelp(cmd.OutOrStdout(), cmd, full || fullHelpRequested)
}

func printCobraHelp(w io.Writer, cmd *cobra.Command, full bool) {
	printCobraHelpHeader(w, cmd, full)

	if cmd.HasSubCommands() {
		fmt.Fprintf(w, "Usage: %s <command>\n\n", cmd.CommandPath())
		printAvailableCommands(w, cmd)
	} else {
		fmt.Fprintf(w, "Usage: %s [flags]\n", cmd.CommandPath())
	}

	if !full {
		printCompactHelpTail(w, cmd)
		return
	}
	printFullHelpFlags(w, cmd)
}

// printCobraHelpHeader prints the description and, at the root, the version.
func printCobraHelpHeader(w io.Writer, cmd *cobra.Command, full bool) {
	desc := strings.TrimSpace(cmd.Short)
	if full {
		desc = strings.TrimSpace(cmd.Long)
	}
	if desc == "" {
		desc = strings.TrimSpace(cmd.Long)
	}
	if desc != "" {
		fmt.Fprintln(w, desc)
		fmt.Fprintln(w)
	}
	if cmd.Root() == cmd && version != "" {
		fmt.Fprintf(w, "Version: %s\n\n", version)
	}
}

// printCompactHelpTail prints the default help tail: only the flags worth
// showing up front, then pointers to the fuller help.
func printCompactHelpTail(w io.Writer, cmd *cobra.Command) {
	fmt.Fprintln(w, "\nCommon:")
	fmt.Fprintln(w, "  -h, --help       show help")
	if cmd.Root() == cmd {
		fmt.Fprintln(w, "  -v, --version    print version")
	}
	if isCompletionCommand(cmd) {
		printCompletionQuickStart(w, cmd)
	}
	fmt.Fprintf(w, "\nMore:\n  %s --help-all    show every flag and configuration detail\n", cmd.CommandPath())
	if cmd.HasSubCommands() {
		fmt.Fprintf(w, "  %s <command> -h  show command help\n", cmd.CommandPath())
	}
}

// printFullHelpFlags prints every flag, which is what --help-all adds.
func printFullHelpFlags(w io.Writer, cmd *cobra.Command) {
	if cmd.Root() == cmd {
		printFlagSet(w, "Flags", cmd.Flags())
	} else {
		printFlagSet(w, "Flags", cmd.NonInheritedFlags())
		printFlagSet(w, "Global flags", cmd.InheritedFlags())
	}
	if cmd.HasSubCommands() {
		fmt.Fprintf(w, "\nUse \"%s <command> -h\" for command help.\n", cmd.CommandPath())
	}
}

func isCompletionCommand(cmd *cobra.Command) bool {
	path := cmd.CommandPath()
	return path == "gdn completion" || strings.HasPrefix(path, "gdn completion ")
}

func printCompletionQuickStart(w io.Writer, cmd *cobra.Command) {
	shell := ""
	if strings.HasPrefix(cmd.CommandPath(), "gdn completion ") {
		shell = cmd.Name()
	}
	if shell == "" {
		fmt.Fprintln(w, "\nInstall:")
		fmt.Fprintln(w, "  gdn completion <shell> -h    show setup for one shell")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Examples:")
		fmt.Fprintln(w, "  gdn completion zsh -h")
		fmt.Fprintln(w, "  gdn completion bash -h")
		return
	}
	for _, setup := range completionShellSetups {
		if setup.Shell != shell {
			continue
		}
		fmt.Fprintln(w, "\nCurrent session:")
		fmt.Fprintln(w, "  "+setup.Session)
		if len(setup.Persistent) > 0 {
			fmt.Fprintf(w, "\nPersistent %s:\n", setup.Shell)
			for _, line := range setup.Persistent {
				fmt.Fprintln(w, "  "+line)
			}
		}
		return
	}
}

// completionShellSetups holds the per-shell completion instructions. The four
// blocks this replaced were identical but for these three fields.
var completionShellSetups = []struct {
	Shell      string
	Session    string
	Persistent []string
}{
	{
		Shell:   "zsh",
		Session: "source <(gdn completion zsh --script)",
		Persistent: []string{
			"mkdir -p ~/.zsh/completions",
			"gdn completion zsh --script > ~/.zsh/completions/_gdn",
			"# ensure ~/.zsh/completions is in fpath and compinit runs from ~/.zshrc",
		},
	},
	{
		Shell:   "bash",
		Session: "source <(gdn completion bash --script)",
		Persistent: []string{
			"mkdir -p ~/.local/share/bash-completion/completions",
			"gdn completion bash --script > ~/.local/share/bash-completion/completions/gdn",
		},
	},
	{
		Shell:   "fish",
		Session: "gdn completion fish --script | source",
		Persistent: []string{
			"mkdir -p ~/.config/fish/completions",
			"gdn completion fish --script > ~/.config/fish/completions/gdn.fish",
		},
	},
	// PowerShell has no persistent form to offer here.
	{
		Shell:   "powershell",
		Session: "gdn completion powershell --script | Out-String | Invoke-Expression",
	},
}

func newCompletionCommand() *cobra.Command {
	completion := newGroupCommand("completion", "Generate shell completion setup or scripts")
	completion.Long = "Generate shell completion setup help or the completion script for one shell."
	completion.AddCommand(
		completionShellCommand("bash", "Generate bash completion setup or script", func(root *cobra.Command, w io.Writer, noDesc bool) error {
			return root.GenBashCompletionV2(w, !noDesc)
		}),
		completionShellCommand("zsh", "Generate zsh completion setup or script", func(root *cobra.Command, w io.Writer, noDesc bool) error {
			if noDesc {
				return root.GenZshCompletionNoDesc(w)
			}
			return root.GenZshCompletion(w)
		}),
		completionShellCommand("fish", "Generate fish completion setup or script", func(root *cobra.Command, w io.Writer, noDesc bool) error {
			return root.GenFishCompletion(w, !noDesc)
		}),
		completionShellCommand("powershell", "Generate PowerShell completion setup or script", func(root *cobra.Command, w io.Writer, noDesc bool) error {
			if noDesc {
				return root.GenPowerShellCompletion(w)
			}
			return root.GenPowerShellCompletionWithDesc(w)
		}),
	)
	return completion
}

func completionShellCommand(use, short string, generate func(root *cobra.Command, w io.Writer, noDesc bool) error) *cobra.Command {
	var script bool
	var noDesc bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long:  short + ". Use --script to print the generated completion script to stdout.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !script {
				return cmd.Help()
			}
			return generate(cmd.Root(), cmd.OutOrStdout(), noDesc)
		},
	}
	cmd.Flags().BoolVar(&script, "script", false, "print the completion script to stdout")
	cmd.Flags().BoolVar(&noDesc, "no-descriptions", false, "disable completion descriptions in the generated script")
	return cmd
}

func printAvailableCommands(w io.Writer, cmd *cobra.Command) {
	commands := cmd.Commands()
	width := 0
	for _, child := range commands {
		if child.Hidden {
			continue
		}
		if len(child.Name()) > width {
			width = len(child.Name())
		}
	}
	if width == 0 {
		return
	}
	fmt.Fprintln(w, "Commands:")
	for _, child := range commands {
		if child.Hidden {
			continue
		}
		fmt.Fprintf(w, "  %-*s  %s\n", width, child.Name(), child.Short)
	}
}

func printFlagSet(w io.Writer, title string, flags *pflag.FlagSet) {
	if flags == nil || !flags.HasAvailableFlags() {
		return
	}
	fmt.Fprintf(w, "\n%s:\n", title)
	flags.SetOutput(w)
	flags.PrintDefaults()
}

func leafCommand(use, short string, run func([]string) error) *cobra.Command {
	return &cobra.Command{
		Use:                use + " [flags]",
		Short:              short,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			runArgs, fullHelp := rewriteHelpAll(args)
			oldFullHelp := fullHelpRequested
			fullHelpRequested = oldFullHelp || fullHelp
			defer func() { fullHelpRequested = oldFullHelp }()
			return run(runArgs)
		},
	}
}

func rewriteHelpAll(args []string) ([]string, bool) {
	for i, arg := range args {
		if arg != "--help-all" {
			continue
		}
		out := append([]string(nil), args...)
		out[i] = "--help"
		return out, true
	}
	return args, false
}

// globalFlags holds the values extracted from global flags before Cobra runs.
type globalFlags struct {
	values map[string]string // keys from globalFlagEnv
}

// isGlobalFlagName reports whether name is a global flag handled before Cobra.
func isGlobalFlagName(name string) bool {
	return globalFlagEnv[name] != ""
}

func extractGlobalFlags(args []string) (remaining []string, gf globalFlags, err error) {
	gf.values = map[string]string{}
	remaining = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			remaining = append(remaining, args[i:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") {
			remaining = append(remaining, arg)
			continue
		}

		nameValue := strings.TrimPrefix(arg, "--")
		name, value, hasValue := strings.Cut(nameValue, "=")
		if !isGlobalFlagName(name) {
			remaining = append(remaining, arg)
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return nil, globalFlags{}, fmt.Errorf("--%s requires a value", name)
			}
			i++
			value = args[i]
		}
		gf.values[name] = value
	}
	return remaining, gf, nil
}
