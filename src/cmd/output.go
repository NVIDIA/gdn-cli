// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type commandOutput struct { // JSON summary after upload/publish
	ArtifactURI string `json:"artifact_uri"`
	AppID       string `json:"app_id,omitempty"`
	VersionID   string `json:"version_id,omitempty"`
	FileCount   int    `json:"file_count,omitempty"`
	TotalBytes  int64  `json:"total_bytes,omitempty"`
	// Deduped* count every file NGC declined because it already had the
	// checksum; Preexisting* are the subset it held before this upload began,
	// as opposed to copies of a file this same upload had just sent. All four
	// are emitted even as zero, so a caller timing an upload can tell "nothing
	// was skipped" apart from "this build does not report it".
	DedupedFileCount     int     `json:"deduped_file_count"`
	DedupedBytes         int64   `json:"deduped_bytes"`
	PreexistingFileCount int     `json:"preexisting_file_count"`
	PreexistingBytes     int64   `json:"preexisting_bytes"`
	WallClockSeconds     float64 `json:"wall_clock_seconds"`
}

type resourceListItem struct { // JSON shape for "resource list --json"
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Org         string `json:"org,omitempty"`
	Description string `json:"description,omitempty"`
	Updated     string `json:"updated,omitempty"`
}

type commandHint struct {
	Text    string
	Command string
}

// printJSON writes v to stdout as indented JSON.
func printJSON(v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	fmt.Println(string(raw))
	return nil
}

func printCommandHints(w io.Writer, title string, hints ...commandHint) {
	if len(hints) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s:\n", title)
	for _, hint := range hints {
		if hint.Text != "" {
			fmt.Fprintf(w, "  %s:\n", hint.Text)
		}
		fmt.Fprintf(w, "    %s\n", hint.Command)
	}
}

// printCredentialFix prints the same two credential hints for every command.
// It takes no command name because the hints do not mention one.
func printCredentialFix(w io.Writer) {
	printCommandHints(w, "Fix",
		commandHint{Text: "Store credentials for an org", Command: "gdn config set --org <org> --api-key <key>"},
		commandHint{Text: "Or configure credentials in your shell", Command: "export NGC_API_KEY=<key> NGC_ORG=<org>"},
	)
}

// printCommandOutput writes a human-readable summary (default) or JSON (--json).
// appName is used in the human header; isNewApp distinguishes app create from app version add.
func printCommandOutput(res *result, elapsed time.Duration, asJSON bool, appName string, isNewApp bool) error {
	if asJSON {
		return writeJSONCommandOutput(os.Stdout, res, elapsed)
	}
	writeHumanCommandOutput(os.Stdout, res, elapsed, appName, isNewApp)
	return nil
}

// writeJSONCommandOutput writes the machine-readable summary to w.
func writeJSONCommandOutput(w io.Writer, res *result, elapsed time.Duration) error {
	out := commandOutput{
		ArtifactURI:          res.ArtifactURI,
		AppID:                res.AppID,
		VersionID:            res.VersionID,
		FileCount:            len(res.Files),
		TotalBytes:           res.TotalBytes,
		DedupedFileCount:     res.DedupedFiles,
		DedupedBytes:         res.DedupedBytes,
		PreexistingFileCount: res.PreexistingFiles,
		PreexistingBytes:     res.PreexistingBytes,
		WallClockSeconds:     elapsed.Seconds(),
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	fmt.Fprintln(w, string(raw))
	return nil
}

// writeHumanCommandOutput writes a multi-line human-readable summary to w.
func writeHumanCommandOutput(w io.Writer, res *result, elapsed time.Duration, appName string, isNewApp bool) {
	if len(res.Files) > 0 {
		fmt.Fprintf(w, "Upload complete in %s\n", fmtDuration(elapsed))
		fmt.Fprintf(w, "  Files:    %d (%s)\n", len(res.Files), humanBytes(res.TotalBytes))
		if n := res.DuplicateFiles(); n > 0 {
			fmt.Fprintf(w, "  Reused:   %d (%s) duplicate content, sent once\n",
				n, humanBytes(res.DuplicateBytes()))
		}
		if res.PreexistingFiles > 0 {
			fmt.Fprintf(w, "  In NGC:   %d (%s) already present before this upload\n",
				res.PreexistingFiles, humanBytes(res.PreexistingBytes))
		}
		fmt.Fprintf(w, "  Artifact: %s\n", res.ArtifactURI)
		printDedupNote(w, res)
	} else {
		fmt.Fprintf(w, "Done in %s\n", fmtDuration(elapsed))
		if res.ArtifactURI != "" {
			fmt.Fprintf(w, "  Resource: %s\n", res.ArtifactURI)
		}
	}

	if res.AppID == "" {
		return
	}

	fmt.Fprintln(w)
	if isNewApp {
		label := appName
		if label == "" {
			label = res.AppID
		}
		fmt.Fprintf(w, "App created: %s\n", label)
	} else {
		fmt.Fprintln(w, "Version added to existing app")
	}
	fmt.Fprintf(w, "  App ID:  %s\n", res.AppID)
	fmt.Fprintf(w, "  Version: %s\n", res.VersionID)
}

// printDedupNote explains a version assembled entirely from content NGC held
// before the upload started. It is worth spelling out because the elapsed time
// above then measures metadata calls rather than a transfer, and because the
// obvious remedy does not work: NGC keys this store by (org, resource name,
// checksum) and it outlives the versions that reference it, so deleting
// versions — even all of them — leaves it intact. Only deleting the resource
// clears it.
//
// Copies of a file this same upload had just sent are a different thing and
// stay quiet: they are normal in any tree that repeats content, and no amount
// of deleting would change them.
func printDedupNote(w io.Writer, res *result) {
	if res.PreexistingFiles == 0 || res.PreexistingFiles < len(res.Files) {
		return
	}
	fmt.Fprint(w, "\nNote: NGC already held every file, so no data was transferred and the time\n"+
		"above does not measure upload throughput. NGC stores this content against\n"+
		"the resource name rather than the version, so deleting versions does not\n"+
		"clear it.\n")
	name := resourceNameFromURI(res.ArtifactURI)
	if name == "" {
		name = "<name>"
	}
	printCommandHints(w, "To force a real upload", commandHint{
		Text:    "Delete the resource, which also drops the content it holds",
		Command: fmt.Sprintf("gdn resource delete --name %s --all-versions", name),
	})
}

// resourceNameFromURI pulls the artifact name out of an ngc://org/name:version
// URI, returning "" for anything not in that shape.
func resourceNameFromURI(uri string) string {
	rest, ok := strings.CutPrefix(uri, "ngc://")
	if !ok {
		return ""
	}
	_, nameVersion, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	name, _, ok := strings.Cut(nameVersion, ":")
	if !ok {
		return ""
	}
	return name
}

// fmtDuration formats a duration as a compact human string, e.g. "4s", "1m 30s", "2m".
func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	switch {
	case m == 0:
		return fmt.Sprintf("%ds", s)
	case s == 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%dm %ds", m, s)
	}
}

// printResourceList renders resources as plain names, JSON, or a human summary.
func printResourceList(resources []ngcResource, asJSON, namesOnly bool) error {
	if namesOnly {
		for _, r := range resources {
			fmt.Println(r.Name)
		}
		return nil
	}
	if asJSON {
		items := make([]resourceListItem, len(resources))
		for i, r := range resources {
			items[i] = resourceListItem{r.Name, r.DisplayName, r.OrgName, r.Description, r.UpdatedDate}
		}
		raw, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal resource list: %w", err)
		}
		fmt.Println(string(raw))
		return nil
	}
	fmt.Printf("%d resource(s):\n", len(resources))
	for _, r := range resources {
		if r.DisplayName != "" && r.DisplayName != r.Name {
			fmt.Printf("  - %s (%s)\n", r.Name, r.DisplayName)
		} else {
			fmt.Printf("  - %s\n", r.Name)
		}
	}
	return nil
}
