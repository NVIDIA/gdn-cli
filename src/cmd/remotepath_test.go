// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var escapingPaths = []string{
	"", "..", "../x", "a/../../x", "a/..", "./x", "a/./b", "/abs", "//abs", "a//b", "a/",
	`a\b`, `..\x`, `C:\x`, "C:x", "a\x00b", "a\nb", strings.Repeat("a", maxRemotePathLen+1),
}

func TestValidateRemotePathRejectsEscapes(t *testing.T) {
	for _, p := range escapingPaths {
		if err := validateRemotePath(p); err == nil {
			t.Errorf("validateRemotePath(%q) = nil, want error", p)
		}
	}
	for _, p := range []string{"a", "a/b/c.txt", "dir/file..name", ".hidden", "a b/c"} {
		if err := validateRemotePath(p); err != nil {
			t.Errorf("validateRemotePath(%q) = %v, want nil", p, err)
		}
	}
}

func TestParseFileSpecRejectsEscapes(t *testing.T) {
	for _, p := range escapingPaths {
		if p == "" || strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") {
			continue
		}
		// Windows treats a backslash as a separator, so parseFileSpec converts it to '/'.
		if runtime.GOOS == "windows" && p == `a\b` {
			continue
		}
		if _, _, err := parseFileSpec("local.bin=" + p); err == nil {
			t.Errorf("parseFileSpec remote %q accepted", p)
		}
	}
}

func TestValidateCoordinates(t *testing.T) {
	for _, bad := range []coordinates{
		{Org: "o/../x"}, {Org: "../x"}, {Org: "o", Version: "1.0/../../x"},
		{Org: "o", Version: ".."}, {Org: "o", Version: "1 0"}, {Org: "o", Version: "1?x=y"},
	} {
		if err := validateCoordinates(bad); err == nil {
			t.Errorf("validateCoordinates(%+v) = nil, want error", bad)
		}
	}
	for _, ok := range []coordinates{{Org: "0782973839888247", Version: "1.0.0-rc_1"}, {Org: "dry-run"}, {Org: "my-org"}} {
		if err := validateCoordinates(ok); err != nil {
			t.Errorf("validateCoordinates(%+v) = %v", ok, err)
		}
	}
}

func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	for _, p := range escapingPaths {
		if _, err := safeJoin(root, p); err == nil {
			t.Errorf("safeJoin(%q) = nil, want error", p)
		}
	}
	got, err := safeJoin(root, "a/b.txt")
	if err != nil || got != filepath.Join(root, "a", "b.txt") {
		t.Errorf("safeJoin = %q, %v", got, err)
	}
}

func TestSafeJoinRefusesSymlinkedParent(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := safeJoin(root, "link/x.txt"); err == nil {
		t.Error("safeJoin wrote through a symlink out of the root")
	}
}

func TestResourcePathEscapesSegments(t *testing.T) {
	c := &ngcClient{coords: coordinates{Org: "o/x", Name: "n", Version: "1/../2"}}
	if got := c.url(c.resourcePath("versions", c.coords.Version)); strings.Contains(got, "/../") || strings.Contains(got, "o/x") {
		t.Errorf("url = %q, separators in caller values were not escaped", got)
	}
}

// A hostile or compromised listing must not be able to write outside --out, and
// must be refused before any file is fetched.
func TestDownloadRejectsEscapingListedPath(t *testing.T) {
	var fetched bool
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/versions/") {
			writeJSON(w, `{"recipeFiles":[{"path":"../evil.txt","sizeInBytes":1}],"paginationInfo":{"totalPages":1}}`)
			return
		}
		fetched = true
	}))
	defer srv.Close()
	t.Setenv("NGC_API_KEY", "k")
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_BASE_URL", srv.URL)

	parent := t.TempDir()
	out := filepath.Join(parent, "out")
	err := runDownloadResource([]string{"--name", "n", "--version", "1.0.0", "--out", out})
	if err == nil || !strings.Contains(err.Error(), "refusing to download") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if fetched {
		t.Error("a file was requested despite its escaping path")
	}
	if _, statErr := os.Stat(filepath.Join(parent, "evil.txt")); statErr == nil {
		t.Error("a file was written outside the output directory")
	}
}
