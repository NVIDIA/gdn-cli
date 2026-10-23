// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"path"
	"strings"
)

// normalizeDir fixes common Windows shell quoting damage on folder paths:
// trailing \, orphan " from \" escaping, and paired surrounding quotes.
func normalizeDir(p string) string {
	if p == "" {
		return p
	}
	p = trimShellQuotes(strings.TrimSpace(p))
	if p == "" {
		return p
	}
	if strings.Contains(p, `\`) {
		p = strings.TrimRight(p, `\`)
		if len(p) == 2 && p[1] == ':' {
			return p + `\`
		}
		return p
	}
	// Forward-slash paths: clean with the OS-independent path package so
	// separators are preserved (filepath.Clean would rewrite them to \ on Windows).
	return path.Clean(p)
}

func trimShellQuotes(path string) string {
	for {
		path = strings.TrimSpace(path)
		if path == "" {
			return path
		}
		// Paired surrounding quotes, either kind.
		if len(path) >= 2 && (path[0] == '"' || path[0] == '\'') && path[len(path)-1] == path[0] {
			path = path[1 : len(path)-1]
			continue
		}
		// Broken Windows quoting: "C:\Apps\" leaves a literal trailing " in argv.
		if strings.HasSuffix(path, `"`) && !strings.HasPrefix(path, `"`) {
			path = strings.TrimSuffix(path, `"`)
			continue
		}
		if strings.HasPrefix(path, `"`) && !strings.HasSuffix(path, `"`) {
			path = strings.TrimPrefix(path, `"`)
			continue
		}
		return path
	}
}

func normalizeDirFlag(dir *string) {
	*dir = normalizeDir(*dir)
}

func repairDirArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	out := make([]string, 0, len(args)+4)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "--dir=") {
			if path, rest, ok := splitDirValueWithFlags(arg[len("--dir="):]); ok {
				out = append(out, "--dir", path)
				out = append(out, rest...)
				continue
			}
		}
		if isFlagName(arg, "dir") && i+1 < len(args) {
			if path, rest, ok := splitDirValueWithFlags(args[i+1]); ok {
				out = append(out, "--dir", path)
				out = append(out, rest...)
				i++
				continue
			}
		}
		out = append(out, arg)
	}
	return out
}

// splitDirValueWithFlags recovers when a trailing \ broke shell quoting and
// glued the folder path to later flags, e.g.
// C:\Apps\ --version 1.0.0 --name my-app-1
func splitDirValueWithFlags(val string) (path string, rest []string, ok bool) {
	if val == "" || !looksLikePathPrefix(val) {
		return "", nil, false
	}
	idx := strings.Index(val, " --")
	if idx <= 0 {
		return "", nil, false
	}
	path = normalizeDir(val[:idx])
	if path == "" || !looksLikePathPrefix(path) {
		return "", nil, false
	}
	tail := strings.TrimSpace(val[idx:])
	if tail == "" {
		return "", nil, false
	}
	return path, strings.Fields(tail), true
}

func looksLikePathPrefix(s string) bool {
	return looksLikeWindowsPath(s) || strings.HasPrefix(s, `/`) || strings.HasPrefix(s, `\\`)
}
