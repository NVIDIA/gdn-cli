// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// String renders the mode the way it is typed on the command line: RES@FPS.
func (m displayMode) String() string { return fmt.Sprintf("%s@%d", m.MaxResolution, m.MaxFPS) }

// parseDisplayMode parses "RES@FPS", e.g. "1080p@60". The resolution is
// lower-cased to the form GDN uses, so "4K@60" becomes "4k@60".
func parseDisplayMode(s string) (displayMode, error) {
	res, fps, ok := strings.Cut(strings.TrimSpace(s), "@")
	res = strings.ToLower(strings.TrimSpace(res))
	n, err := strconv.ParseInt(strings.TrimSpace(fps), 10, 64)
	if !ok || res == "" || err != nil || n <= 0 {
		return displayMode{}, fmt.Errorf("invalid display mode %q: expected RES@FPS, e.g. 1080p@60", s)
	}
	return displayMode{MaxResolution: res, MaxFPS: n}, nil
}

// displayModeList is a repeatable display-mode flag. A value may hold several
// comma-separated modes, and a mode given twice is kept once.
type displayModeList []displayMode

func (l *displayModeList) String() string { return formatDisplayModes(*l) }

func (l *displayModeList) Set(v string) error {
	for part := range strings.SplitSeq(v, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		m, err := parseDisplayMode(part)
		if err != nil {
			return err
		}
		if !slices.Contains(*l, m) {
			*l = append(*l, m)
		}
	}
	return nil
}

func formatDisplayModes(modes []displayMode) string {
	if len(modes) == 0 {
		return "(none)"
	}
	parts := make([]string, len(modes))
	for i, m := range modes {
		parts[i] = m.String()
	}
	return strings.Join(parts, ", ")
}

// formatOfferedModes groups modes by resolution, e.g. "720p@30/60, 1080p@60".
func formatOfferedModes(modes []displayMode) string {
	var order []string
	fps := map[string][]string{}
	for _, m := range modes {
		if _, seen := fps[m.MaxResolution]; !seen {
			order = append(order, m.MaxResolution)
		}
		fps[m.MaxResolution] = append(fps[m.MaxResolution], strconv.FormatInt(m.MaxFPS, 10))
	}
	parts := make([]string, len(order))
	for i, res := range order {
		parts[i] = res + "@" + strings.Join(fps[res], "/")
	}
	return strings.Join(parts, ", ")
}

// checkDisplayModes reports modes GDN does not offer. When create-config
// cannot be read, or lists no modes, the modes are sent unchecked and GDN
// validates them.
func (c *portalClient) checkDisplayModes(ctx context.Context, modes []displayMode) error {
	cfg, err := c.getCreateConfig(ctx)
	if err != nil {
		warnf("could not check display modes against GDN (%v); sending them unchecked", err)
		return nil
	}
	if len(cfg.AllowedDisplayModes) == 0 {
		warnf("GDN listed no display modes; sending them unchecked")
		return nil
	}
	var unknown []string
	for _, m := range modes {
		if !slices.Contains(cfg.AllowedDisplayModes, m) {
			unknown = append(unknown, m.String())
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("display mode(s) %s not offered by GDN; it offers %s",
			strings.Join(unknown, ", "), formatOfferedModes(cfg.AllowedDisplayModes))
	}
	return nil
}

// editDisplayModes removes, then adds, modes in the current list. Adding a
// mode already listed, or removing one that is not, changes nothing and is
// reported in notes. Emptying a non-empty list is an error.
func editDisplayModes(current, add, remove []displayMode) ([]displayMode, []string, error) {
	out := slices.Clone(current)
	var notes []string
	for _, m := range remove {
		i := slices.Index(out, m)
		if i < 0 {
			notes = append(notes, fmt.Sprintf("%s is not in the list; nothing to remove", m))
			continue
		}
		out = slices.Delete(out, i, i+1)
	}
	for _, m := range add {
		if slices.Contains(out, m) {
			notes = append(notes, fmt.Sprintf("%s is already in the list; nothing to add", m))
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 && len(current) > 0 {
		return nil, notes, fmt.Errorf("removing %s would leave no display modes; use --display-mode to replace the list instead", formatDisplayModes(remove))
	}
	return out, notes, nil
}
