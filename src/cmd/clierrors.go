// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var windowsDrivePath = regexp.MustCompile(`(?i)[A-Za-z]:\\`)

// missingFlags returns sorted --flag names whose values are empty.
func missingFlags(values map[string]string) []string {
	var missing []string
	for name, val := range values {
		if val == "" {
			missing = append(missing, "--"+name)
		}
	}
	sort.Strings(missing)
	return missing
}

func formatMissingFlagsError(missing []string) string {
	switch len(missing) {
	case 0:
		return "missing required flags"
	case 1:
		return "missing required flag: " + missing[0]
	default:
		return "missing required flags: " + strings.Join(missing, ", ")
	}
}

func formatRequiredFlagStatus(values map[string]string) string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("Required flags:")
	for _, name := range names {
		val := values[name]
		if val == "" {
			fmt.Fprintf(&b, "\n  --%-16s (missing)", name)
			continue
		}
		if name == "dir" && strings.HasPrefix(val, "-") {
			fmt.Fprintf(&b, "\n  --%-16s %s (does not look like a path)", name, val)
			continue
		}
		fmt.Fprintf(&b, "\n  --%-16s %s", name, val)
	}
	return b.String()
}

func emptyFlagSet(values map[string]string) map[string]bool {
	empty := make(map[string]bool, len(values))
	for name, val := range values {
		empty[name] = val == ""
	}
	return empty
}

// flagParseHints suggests fixes when required flags were not parsed from args.
func flagParseHints(args []string, empty map[string]bool) []string {
	var hints []string
	if empty["dir"] {
		if h := dirFlagSkippedHint(args); h != "" {
			hints = append(hints, h)
		}
		if h := trailingBackslashHint(args); h != "" {
			hints = append(hints, h)
		}
	} else if h := bogusDirValueHint(args, empty); h != "" {
		hints = append(hints, h)
	}
	return hints
}

func dirFlagSkippedHint(args []string) string {
	for i, arg := range args {
		if !isFlagName(arg, "dir") {
			continue
		}
		if i+1 >= len(args) {
			return "--dir was passed without a value."
		}
		next := args[i+1]
		if strings.HasPrefix(next, "-") {
			return fmt.Sprintf(
				"After --dir, the next argument is %q (another flag), not a folder path. "+
					"This often means the shell dropped or merged the path argument.",
				next,
			)
		}
	}
	return ""
}

func bogusDirValueHint(args []string, empty map[string]bool) string {
	for i, arg := range args {
		if !isFlagName(arg, "dir") || i+1 >= len(args) {
			continue
		}
		val := args[i+1]
		if !strings.HasPrefix(val, "-") {
			continue
		}
		if empty["version"] || empty["name"] {
			return fmt.Sprintf(
				"--dir consumed %q as its value, so later flags were not parsed. "+
					"This usually means the folder path was lost to shell quoting (common on Windows with a trailing backslash). "+
					"Try --dir \"C:\\Apps\" without a trailing slash.",
				val,
			)
		}
	}
	return ""
}

func trailingBackslashHint(args []string) string {
	for _, arg := range args {
		if !looksLikeWindowsPath(arg) {
			continue
		}
		if strings.HasSuffix(arg, `\`) || strings.HasSuffix(arg, `/`) {
			return fmt.Sprintf(
				"The path %q ends with a trailing slash. On Windows, a trailing backslash before a closing quote "+
					"escapes the quote (e.g. \"C:\\Apps\\\"), so the path may never reach gdn. "+
					"Drop the trailing slash, use \"C:\\Apps\", or end with a doubled backslash \"C:\\Apps\\\\\".",
				arg,
			)
		}
	}
	if sawDirFlag(args) && mentionsWindowsPath(args) {
		return "A Windows folder path may have been broken by shell quoting. " +
			"Try --dir without a trailing backslash, or use single quotes in PowerShell: --dir 'C:\\Apps'."
	}
	return ""
}

func isFlagName(arg, name string) bool {
	return arg == "--"+name || arg == "-"+name
}

func sawDirFlag(args []string) bool {
	for _, arg := range args {
		if isFlagName(arg, "dir") {
			return true
		}
	}
	return false
}

func looksLikeWindowsPath(s string) bool {
	return windowsDrivePath.MatchString(s)
}

func mentionsWindowsPath(args []string) bool {
	for _, arg := range args {
		if looksLikeWindowsPath(arg) || strings.Contains(arg, `\Program Files`) {
			return true
		}
	}
	return false
}
