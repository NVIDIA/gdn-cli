// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

// The app list payload embeds only the current version, so version_count must
// come from the server's versionCount field — len(Versions) is at most 1.
func TestPrintAppListUsesServerVersionCount(t *testing.T) {
	apps := []gdnApp{{
		ID:               "app-1",
		Name:             "My App",
		DefaultVersionID: "ver-2",
		VersionCount:     2,
		Versions:         []gdnVersion{{ID: "ver-2", VersionName: "1.1.0"}},
	}}
	var err error
	out := captureStdout(t, func() { err = printAppList(apps, true, false) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"version_count": 2`, `"default_version_id": "ver-2"`, `"default_version_name": "1.1.0"`} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON output missing %s:\n%s", want, out)
		}
	}
}

func TestPrintAppListHumanShowsCurrentVersionName(t *testing.T) {
	apps := []gdnApp{
		{ID: "app-1", Name: "My App", DefaultVersionID: "ver-2", VersionCount: 2, Versions: []gdnVersion{{ID: "ver-2", VersionName: "1.1.0"}}},
		{ID: "app-2", Name: "No Default", VersionCount: 1},
	}
	var err error
	out := captureStdout(t, func() { err = printAppList(apps, false, false) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "- My App (app-1, default 1.1.0 (ver-2))") {
		t.Errorf("human output should name the embedded current version:\n%s", out)
	}
	if !strings.Contains(out, "- No Default (app-2)\n") {
		t.Errorf("app without a default version should print plainly:\n%s", out)
	}
}

// The JSON version list carries each version's metadata and marks exactly the
// default version as the production one.
func TestPrintVersionListJSON(t *testing.T) {
	versions := []gdnVersion{
		{ID: "ver-1", VersionName: "1.0.0"},
		{ID: "ver-2", VersionName: "1.1.0", Status: "READY"},
	}
	var err error
	out := captureStdout(t, func() { err = printVersionList("app-1", versions, "ver-2", true) })
	if err != nil {
		t.Fatal(err)
	}
	var items []versionListItem
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v, want 2 versions", items)
	}
	if items[0].ID != "ver-1" || items[0].VersionName != "1.0.0" || items[0].IsProduction {
		t.Errorf("ver-1 = %+v, want the non-production 1.0.0 entry", items[0])
	}
	if items[1].ID != "ver-2" || items[1].Status != "READY" || !items[1].IsProduction {
		t.Errorf("ver-2 = %+v, want the production READY entry", items[1])
	}
}
