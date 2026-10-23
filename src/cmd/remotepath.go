// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Remote paths and identifiers come from callers (flags) or from the server
// (file listings). Either can carry sequences that would escape the prefix they
// are meant to stay under: ".." segments, absolute paths, drive letters, or
// separators that change meaning per platform. Everything that joins such a
// value into a URL or onto a local directory goes through the helpers here.

const maxRemotePathLen = 1024

var (
	// ngcIdentPattern bounds org and version values that are placed into URL
	// path segments. It cannot contain a separator, and it must start with an
	// alphanumeric so "." and ".." never match.
	ngcIdentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	driveLetter     = regexp.MustCompile(`^[A-Za-z]:`)
)

// validateRemotePath rejects a slash-separated relative path that could escape
// the prefix it is resolved under.
func validateRemotePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("remote path is empty")
	case len(p) > maxRemotePathLen:
		return fmt.Errorf("remote path exceeds %d bytes", maxRemotePathLen)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("remote path %q must be relative", p)
	case strings.Contains(p, `\`):
		return fmt.Errorf("remote path %q must use '/' separators, not '\\'", p)
	case driveLetter.MatchString(p):
		return fmt.Errorf("remote path %q must not start with a drive letter", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("remote path %q contains a control character", p)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return fmt.Errorf("remote path %q contains an empty segment", p)
		case ".", "..":
			return fmt.Errorf("remote path %q contains a %q segment", p, seg)
		}
	}
	return nil
}

func validateNGCOrg(org string) error {
	if org == "" || org == dryRunPlaceholder {
		return nil // a missing org is reported by the credential check
	}
	if !ngcIdentPattern.MatchString(org) {
		return fmt.Errorf("invalid org %q: must match %s", org, ngcIdentPattern)
	}
	return nil
}

func validateNGCVersion(version string) error {
	if !ngcIdentPattern.MatchString(version) {
		return fmt.Errorf("invalid version %q: must match %s", version, ngcIdentPattern)
	}
	return nil
}

// validateCoordinates checks every caller-supplied value that becomes part of an
// NGC URL path. An empty version is allowed for whole-resource operations.
func validateCoordinates(c coordinates) error {
	if err := validateNGCOrg(c.Org); err != nil {
		return err
	}
	if c.Version != "" {
		return validateNGCVersion(c.Version)
	}
	return nil
}

// safeJoin resolves a remote relative path under root and guarantees the result
// stays inside it. It also refuses to write through a symlink, which could
// redirect the write outside root even when the path itself is clean.
func safeJoin(root, rel string) (string, error) {
	if err := validateRemotePath(rel); err != nil {
		return "", err
	}
	local := filepath.FromSlash(rel)
	if !filepath.IsLocal(local) {
		return "", fmt.Errorf("remote path %q is not a local path", rel)
	}
	dest := filepath.Join(root, local)
	back, err := filepath.Rel(root, dest)
	if err != nil || !filepath.IsLocal(back) {
		return "", fmt.Errorf("remote path %q escapes the output directory", rel)
	}
	cur := root
	for _, seg := range strings.Split(local, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if err != nil {
			break // nothing exists from here down, so there is nothing to follow
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to write %q through symlink %q", rel, cur)
		}
	}
	return dest, nil
}

// pathSegment escapes one caller-supplied value for use as a URL path segment.
func pathSegment(s string) string { return url.PathEscape(s) }
