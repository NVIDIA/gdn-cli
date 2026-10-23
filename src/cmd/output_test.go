// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFmtDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{4 * time.Second, "4s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{90 * time.Second, "1m 30s"},
		{2*time.Minute + 22*time.Second, "2m 22s"},
		{10 * time.Minute, "10m"},
		{400 * time.Millisecond, "0s"},               // sub-second rounds down to zero.
		{2*time.Second + 500*time.Millisecond, "3s"}, // 2.5s rounds up to 3s.
	}
	for _, c := range cases {
		if got := fmtDuration(c.d); got != c.want {
			t.Errorf("fmtDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestWriteJSONCommandOutput(t *testing.T) {
	res := &result{
		Files:       []appFile{{Path: "a.txt"}, {Path: "b.txt"}},
		TotalBytes:  1024,
		ArtifactURI: "ngc://org/my-app-1:1.0.0",
		AppID:       "app-123",
		VersionID:   "ver-456",
	}
	var buf bytes.Buffer
	if err := writeJSONCommandOutput(&buf, res, 2*time.Second+500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	checks := map[string]any{
		"artifact_uri":       "ngc://org/my-app-1:1.0.0",
		"app_id":             "app-123",
		"version_id":         "ver-456",
		"file_count":         float64(2),
		"wall_clock_seconds": 2.5,
	}
	for key, want := range checks {
		if got[key] != want {
			t.Errorf("JSON[%q] = %v, want %v", key, got[key], want)
		}
	}
}

func TestWriteHumanCommandOutputNewApp(t *testing.T) {
	res := &result{
		Files:       make([]appFile, 47),
		TotalBytes:  1_234_567_890,
		ArtifactURI: "ngc://org/my-app-1:1.0.0",
		AppID:       "app-abc",
		VersionID:   "ver-xyz",
	}
	var buf bytes.Buffer
	writeHumanCommandOutput(&buf, res, 2*time.Minute+22*time.Second, "MyApp", true)
	out := buf.String()

	for _, want := range []string{
		"2m 22s",
		"47",
		"ngc://org/my-app-1:1.0.0",
		"App created: MyApp",
		"App ID:  app-abc",
		"Version: ver-xyz",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q;\ngot:\n%s", want, out)
		}
	}
}

func TestWriteHumanCommandOutputAddVersion(t *testing.T) {
	res := &result{
		Files:       make([]appFile, 3),
		TotalBytes:  512,
		ArtifactURI: "ngc://org/my-app-1:1.0.0",
		AppID:       "app-existing",
		VersionID:   "ver-new",
	}
	var buf bytes.Buffer
	writeHumanCommandOutput(&buf, res, 5*time.Second, "MyApp", false)
	out := buf.String()

	for _, want := range []string{
		"Version added to existing app",
		"app-existing",
		"ver-new",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human output (app version add) missing %q;\ngot:\n%s", want, out)
		}
	}
}

// A version NGC assembled entirely from content it already held transfers no
// bytes, so the elapsed time is not a throughput measurement. Saying so is the
// only thing that distinguishes it from a genuinely fast upload.
func TestWriteHumanCommandOutputAllFilesDeduped(t *testing.T) {
	res := &result{
		Files:       make([]appFile, 4),
		TotalBytes:  1024,
		ArtifactURI: "ngc://org/asset:0.1.0",
		dedupSummary: dedupSummary{
			DedupedFiles: 4, DedupedBytes: 1024,
			PreexistingFiles: 4, PreexistingBytes: 1024,
		},
	}
	var buf bytes.Buffer
	writeHumanCommandOutput(&buf, res, 3*time.Second, "", false)
	out := buf.String()

	for _, want := range []string{
		"In NGC:   4",
		"no data was transferred",
		// The obvious remedy is deleting the version, which does not work:
		// point at the one that does.
		"deleting versions does not",
		"gdn resource delete --name asset --all-versions",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fully-deduped output missing %q;\ngot:\n%s", want, out)
		}
	}
}

// A tree holding the same bytes at several paths sees a conflict for every copy
// after the first no matter how clean the resource is. That is ordinary, so it
// gets a count and nothing more: warning about it would train people to ignore
// the warning that matters.
func TestWriteHumanCommandOutputDuplicateContentIsNotAWarning(t *testing.T) {
	res := &result{
		Files:       make([]appFile, 4),
		TotalBytes:  1024,
		ArtifactURI: "ngc://org/asset:0.1.0",
		dedupSummary: dedupSummary{
			DedupedFiles: 3, DedupedBytes: 768,
		},
	}
	var buf bytes.Buffer
	writeHumanCommandOutput(&buf, res, 3*time.Second, "", false)
	out := buf.String()

	if !strings.Contains(out, "Reused:   3") {
		t.Errorf("output missing the duplicate-content count;\ngot:\n%s", out)
	}
	for _, unwanted := range []string{"In NGC:", "no data was transferred"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("duplicate content should not be reported as %q;\ngot:\n%s", unwanted, out)
		}
	}
}

// Both causes at once: the counts are disjoint, and only the pre-existing one
// is worth explaining.
func TestWriteHumanCommandOutputSeparatesDedupCauses(t *testing.T) {
	res := &result{
		Files:       make([]appFile, 10),
		TotalBytes:  1000,
		ArtifactURI: "ngc://org/asset:0.1.0",
		dedupSummary: dedupSummary{
			DedupedFiles: 5, DedupedBytes: 500,
			PreexistingFiles: 2, PreexistingBytes: 200,
		},
	}
	var buf bytes.Buffer
	writeHumanCommandOutput(&buf, res, 3*time.Second, "", false)
	out := buf.String()

	if !strings.Contains(out, "Reused:   3") {
		t.Errorf("duplicate count should exclude the pre-existing files;\ngot:\n%s", out)
	}
	if !strings.Contains(out, "In NGC:   2") {
		t.Errorf("output missing the pre-existing count;\ngot:\n%s", out)
	}
	// Only some files were pre-existing, so bytes did move.
	if strings.Contains(out, "no data was transferred") {
		t.Errorf("a partial reuse should not claim nothing was transferred;\ngot:\n%s", out)
	}
}

func TestWriteHumanCommandOutputNoDedupLine(t *testing.T) {
	res := &result{
		Files:       make([]appFile, 4),
		TotalBytes:  1024,
		ArtifactURI: "ngc://org/asset:0.1.0",
	}
	var buf bytes.Buffer
	writeHumanCommandOutput(&buf, res, 3*time.Second, "", false)
	out := buf.String()
	for _, unwanted := range []string{"Reused", "In NGC:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("an upload that reused nothing should not mention %q;\ngot:\n%s", unwanted, out)
		}
	}
}

func TestResourceNameFromURI(t *testing.T) {
	cases := map[string]string{
		"ngc://org/asset:0.1.0":       "asset",
		"ngc://0782973839888247/a:b":  "a",
		"ngc://org/asset":             "", // no version
		"https://example.com/a:b":     "", // wrong scheme
		"ngc://org-only":              "", // no name
		"ngc://org/my.app_1-x:v1.2.3": "my.app_1-x",
	}
	for uri, want := range cases {
		if got := resourceNameFromURI(uri); got != want {
			t.Errorf("resourceNameFromURI(%q) = %q, want %q", uri, got, want)
		}
	}
}

func TestWriteHumanCommandOutputUploadOnly(t *testing.T) {
	res := &result{
		Files:       make([]appFile, 10),
		TotalBytes:  2048,
		ArtifactURI: "ngc://org/asset:0.1.0",
	}
	var buf bytes.Buffer
	writeHumanCommandOutput(&buf, res, 30*time.Second, "", false)
	out := buf.String()

	if strings.Contains(out, "App") {
		t.Errorf("upload-only output should not mention 'App';\ngot:\n%s", out)
	}
	if strings.Contains(out, "app launch") {
		t.Errorf("upload-only output should not mention 'app launch';\ngot:\n%s", out)
	}
	if !strings.Contains(out, "ngc://org/asset:0.1.0") {
		t.Errorf("upload-only output should include artifact URI;\ngot:\n%s", out)
	}
}
