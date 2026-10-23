// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// captureStdout runs fn with os.Stdout redirected and returns the captured
// output. The pipe is drained concurrently so a large write cannot deadlock by
// filling the OS pipe buffer before fn returns.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = pw

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, pr)
		done <- buf.String()
	}()

	fn()
	pw.Close()
	os.Stdout = old
	out := <-done
	_ = pr.Close()
	return out
}

func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout, oldStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	outDone := make(chan string, 1)
	errDone := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, outR)
		outDone <- buf.String()
	}()
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, errR)
		errDone <- buf.String()
	}()

	fn()
	outW.Close()
	errW.Close()
	os.Stdout, os.Stderr = oldStdout, oldStderr
	stdout, stderr = <-outDone, <-errDone
	_ = outR.Close()
	_ = errR.Close()
	return stdout, stderr
}

func TestIgnoredFlagsNote(t *testing.T) {
	newFS := func(args []string) *flag.FlagSet {
		fs := flag.NewFlagSet("upload", flag.ContinueOnError)
		fs.String("seat-type", "WINDOWS", "")
		fs.String("description", "", "")
		var instances stringList
		fs.Var(&instances, "instances", "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return fs
	}

	// Only flags the user explicitly set are reported.
	fs := newFS([]string{"--seat-type", "LINUX", "--description", "x"})
	got := ignoredFlagsNote(fs, "adding a version with --app-id", "instances", "seat-type", "description")
	want := "warning: --seat-type, --description ignored when adding a version with --app-id"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Nothing relevant set -> no note.
	if got := ignoredFlagsNote(newFS(nil), "mode", "instances", "seat-type"); got != "" {
		t.Errorf("expected empty note, got %q", got)
	}
}

func TestSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"My App", "my-app"},
		{"Boss Fight!! 2", "boss-fight-2"},
		{"  spaced  ", "spaced"},
		{"already-slug", "already-slug"},
	}
	for _, c := range cases {
		if got := slug(c.in); got != c.want {
			t.Errorf("slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStringList(t *testing.T) {
	var s stringList
	if err := s.Set("NVIDIA T5, NVIDIA A3 ,"); err != nil {
		t.Fatal(err)
	}
	if len(s) != 2 || s[0] != "NVIDIA T5" || s[1] != "NVIDIA A3" {
		t.Errorf("parsed = %#v", []string(s))
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "third"); got != "third" {
		t.Errorf("got %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestCommandOutputJSON(t *testing.T) {
	res := &result{
		Files:       []appFile{{Path: "a.txt"}, {Path: "b.txt"}},
		TotalBytes:  1024,
		ArtifactURI: "ngc://org/my-app-1:1.0.0",
		AppID:       "app-123",
		VersionID:   "ver-456",
	}
	raw, err := json.Marshal(commandOutput{
		ArtifactURI:      res.ArtifactURI,
		AppID:            res.AppID,
		VersionID:        res.VersionID,
		FileCount:        len(res.Files),
		TotalBytes:       res.TotalBytes,
		WallClockSeconds: (2*time.Second + 500*time.Millisecond).Seconds(),
	})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["artifact_uri"] != "ngc://org/my-app-1:1.0.0" {
		t.Errorf("artifact_uri = %v", got["artifact_uri"])
	}
	if got["app_id"] != "app-123" {
		t.Errorf("app_id = %v", got["app_id"])
	}
	if got["version_id"] != "ver-456" {
		t.Errorf("version_id = %v", got["version_id"])
	}
	if got["file_count"] != float64(2) {
		t.Errorf("file_count = %v", got["file_count"])
	}
	if got["wall_clock_seconds"] != 2.5 {
		t.Errorf("wall_clock_seconds = %v", got["wall_clock_seconds"])
	}
}

// A .env file in the working directory (or any parent) must not influence the
// CLI: the org config file and the environment are the only sources.
func TestDotEnvIsNotLoaded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("NGC_ORG=from-dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	t.Setenv("NGC_ORG", "")
	os.Unsetenv("NGC_ORG")
	t.Setenv("GDN_CONFIG_FILE", filepath.Join(dir, "none.toml"))

	applyConfig("")

	if got := os.Getenv("NGC_ORG"); got != "" {
		t.Errorf("NGC_ORG = %q, want empty (.env must be ignored)", got)
	}
}

// --- app create: instance defaulting ---

// TestCreateAppDryRunJSONFlag verifies that --json produces a JSON object on
// stdout containing the expected fields, not the human-readable summary.
func TestCreateAppDryRunJSONFlag(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut := captureOutput(t, func() {
		if err := runCreateApp([]string{
			"--dir", dir,
			"--app-name", "Test-App",
			"--version", "1.0.0",
			"--dry-run",
			"--json",
		}); err != nil {
			t.Errorf("runCreateApp --json: %v", err)
		}
	})

	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("--json stdout is not a single valid JSON object: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	for _, field := range []string{"artifact_uri", "wall_clock_seconds"} {
		if _, ok := got[field]; !ok {
			t.Errorf("JSON output missing field %q; got: %s", field, out)
		}
	}
	if strings.Contains(out, "Upload complete") {
		t.Errorf("--json output should not contain human-readable text; got: %s", out)
	}
	if !strings.Contains(errOut, "NGC target") {
		t.Errorf("--json progress logs should move to stderr; stderr: %s", errOut)
	}
}

func TestAddVersionJSONKeepsWarningsOnStderr(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errOut := captureOutput(t, func() {
		if err := runAddVersion([]string{
			"--app-id", "app-1",
			"--dir", dir,
			"--app-name", "Test-App",
			"--version", "1.0.0",
			"--instances", "NVIDIA A10",
			"--description", "ignored in add-version",
			"--display-mode", "4k@60",
			"--dry-run",
			"--json",
		}); err != nil {
			t.Errorf("runAddVersion --json: %v", err)
		}
	})

	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("--json stdout is not a single valid JSON object: %v\nstdout:\n%s\nstderr:\n%s", err, out, errOut)
	}
	if strings.Contains(out, "warning:") || strings.Contains(out, "ignored") {
		t.Fatalf("warning leaked into JSON stdout:\n%s", out)
	}
	for _, want := range []string{"warning:", "--instances", "--description", "--display-mode", "ignored when adding a version"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr missing warning text %q:\n%s", want, errOut)
		}
	}
}

// TestCreateAppDryRunNoInstancesOK is a regression guard: before the change,
// omitting --instances with --dry-run returned an error. It must now succeed.
func TestCreateAppDryRunNoInstancesOK(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runCreateApp([]string{
		"--dir", dir,
		"--app-name", "Test-App",
		"--version", "1.0.0",
		"--dry-run",
	})
	if err != nil {
		t.Errorf("runCreateApp with --dry-run and no --instances should not error: %v", err)
	}
}

// TestCreateAppSendsAllInstancesByDefault verifies that when --instances is
// omitted the portal receives every instance name create-config offers.
func TestCreateAppSendsAllInstancesByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("exe"), 0o644); err != nil {
		t.Fatal(err)
	}

	var capturedInstances []string
	var createRaw []byte
	var multipartInitiated bool
	var srv *httptest.Server
	srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/gdn/apps/create-config":
			writeJSON(w, `{"allowedInstanceNames":["NVIDIA L40","NVIDIA L40G"]}`)
		// GDN: duplicate-name check
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/gdn/apps"):
			writeJSON(w, `{"applications":[]}`)
		// GDN: create app — capture instanceNames
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/gdn/apps"):
			var body createAppReq
			createRaw, _ = io.ReadAll(r.Body)
			_ = json.Unmarshal(createRaw, &body)
			capturedInstances = body.InstanceNames
			writeJSON(w, `{"id":"app-new","versions":[{"id":"ver-1"}]}`)
		// GDN: set default version
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/gdn/apps"):
			writeJSON(w, `{"id":"app-new"}`)
		// NGC: create resource (409 = already exists, treated as success)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resources"):
			w.WriteHeader(http.StatusConflict)
		// NGC: create version (409 = already exists)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/resources/") && strings.HasSuffix(r.URL.Path, "/versions"):
			w.WriteHeader(http.StatusConflict)
		// NGC: initiate multipart upload
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/files/multipart") && !multipartInitiated:
			multipartInitiated = true
			writeJSON(w, `{"uploadID":"up-1","partSize":0}`)
		// NGC: get presigned part URLs
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/files/multipart"):
			writeJSON(w, `{"uploadID":"up-1","urls":["`+srv.URL+`/part"]}`)
		// S3: part PUT
		case r.Method == http.MethodPut && r.URL.Path == "/part":
			w.Header().Set("ETag", `"etag-1"`)
		// NGC: complete multipart
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/files/multipart"):
			writeJSON(w, `{}`)
		// NGC: commit
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/files/commit"):
			writeJSON(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	t.Setenv("NGC_API_KEY", "tok")
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_BASE_URL", srv.URL)

	if err := runCreateApp([]string{
		"--dir", dir,
		"--app-name", "My-App",
		"--version", "1.0.0",
		// --instances intentionally omitted
	}); err != nil {
		t.Fatalf("runCreateApp: %v", err)
	}

	if want := []string{"NVIDIA L40", "NVIDIA L40G"}; !slices.Equal(capturedInstances, want) {
		t.Errorf("instances = %v, want %v", capturedInstances, want)
	}
	// Without --app-features GDN applies its own defaults to every field.
	if strings.Contains(string(createRaw), "applicationFeatures") {
		t.Errorf("create request carries applicationFeatures without --app-features: %s", createRaw)
	}
}

// --- app update: validation ---

func TestRunUpdateAppRequiresAppID(t *testing.T) {
	err := runUpdateApp([]string{"--instances", "NVIDIA A10"})
	if err == nil {
		t.Fatal("expected error when --app-id is missing")
	}
}

func TestRunUpdateAppRequiresAtLeastOneField(t *testing.T) {
	// --dry-run bypasses credential checks so we can reach the "no fields" guard.
	err := runUpdateApp([]string{"--app-id", "app-1", "--dry-run"})
	if err == nil {
		t.Fatal("expected error when no update fields are provided")
	}
	if !strings.Contains(err.Error(), "usage already printed") && err.Error() == "" {
		// errUsageShown is returned; any non-nil error is the right outcome.
	}
}

// --- app update: dry-run output ---

func TestRunUpdateAppDryRun(t *testing.T) {
	var err error
	out := captureStdout(t, func() {
		err = runUpdateApp([]string{
			"--app-id", "app-42",
			"--instances", "NVIDIA A10,NVIDIA L20",
			"--touch-support=false",
			"--idle-timeout", "600",
			"--dry-run",
		})
	})

	if err != nil {
		t.Fatalf("dry-run error: %v", err)
	}
	for _, want := range []string{"app-42", "NVIDIA A10", "touch-support: false", "600"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q; got:\n%s", want, out)
		}
	}
}

// --- app update: partial PATCH body ---

// TestRunUpdateAppSendsOnlySetFields verifies that the fs.Visit-based detection
// sends only explicitly-provided flags: unset fields are absent from the PATCH
// body, and a bool flag set to false is still included (not silently dropped).
func TestRunUpdateAppSendsOnlySetFields(t *testing.T) {
	var gotMethod string
	var gotBody updateAppReq
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		writeJSON(w, `{"id":"app-7","name":"Renamed"}`)
	}))
	defer srv.Close()

	t.Setenv("NGC_API_KEY", "tok")
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_BASE_URL", srv.URL)

	if err := runUpdateApp([]string{
		"--app-id", "app-7",
		"--app-name", "Renamed",
		"--touch-support=false", // explicitly false — must appear in body, not be omitted
		// --instances, --description, --max-bitrate, etc. intentionally absent
	}); err != nil {
		t.Fatalf("runUpdateApp: %v", err)
	}

	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotBody.Name != "Renamed" {
		t.Errorf("Name = %q, want Renamed", gotBody.Name)
	}
	// Fields not passed must be absent / zero.
	if len(gotBody.InstanceNames) != 0 {
		t.Errorf("InstanceNames = %v, want empty (was not set)", gotBody.InstanceNames)
	}
	if gotBody.Description != "" {
		t.Errorf("Description = %q, want empty (was not set)", gotBody.Description)
	}
	// TouchSupport was explicitly set to false — must be present as a non-nil pointer.
	if gotBody.ApplicationFeatures == nil || gotBody.ApplicationFeatures.TouchSupport == nil {
		t.Fatal("ApplicationFeatures.TouchSupport should not be nil when --touch-support=false is passed")
	}
	if *gotBody.ApplicationFeatures.TouchSupport != false {
		t.Errorf("TouchSupport = %v, want false", *gotBody.ApplicationFeatures.TouchSupport)
	}
	// Unset feature fields must be nil (not sent as zero values).
	if gotBody.ApplicationFeatures.RuntimeDisplayModeChange != nil {
		t.Errorf("RuntimeDisplayModeChange should be nil when not set, got %v", *gotBody.ApplicationFeatures.RuntimeDisplayModeChange)
	}
	if gotBody.ApplicationFeatures.MaxBitDepth != nil {
		t.Errorf("MaxBitDepth should be nil when not set, got %v", *gotBody.ApplicationFeatures.MaxBitDepth)
	}
}

// --- app launch ---

// TestRunLaunchAppMissingFlags verifies that each required flag is enforced.
func TestRunLaunchAppMissingFlags(t *testing.T) {
	// The API key comes from the environment now; keep other launch env clear so that
	// removing a flag actually leaves the value empty.
	t.Setenv("NGC_API_KEY", "tok")
	for _, key := range []string{"NGC_ORG", "GDN_BASE_URL", "GDN_CLIENT_URL"} {
		t.Setenv(key, "")
	}
	base := []string{
		"--app-id", "app-1",
		"--org", "o",
	}
	remove := func(args []string, flag string) []string {
		out := make([]string, 0, len(args))
		skip := false
		for _, a := range args {
			if a == flag {
				skip = true
				continue
			}
			if skip {
				skip = false
				continue
			}
			out = append(out, a)
		}
		return out
	}
	// Only --app-id is user-required; the org must still resolve (from config or
	// env) for the nonce call. Gateway/client URLs default to production, so
	// removing them is no longer an error.
	required := []string{
		"--app-id",
		"--org",
	}
	for _, f := range required {
		if err := runLaunchApp(remove(base, f)); err == nil {
			t.Errorf("expected error when %s is missing", f)
		}
	}
}

func TestRunLaunchAppMissingAppIDShowsFixAfterUsage(t *testing.T) {
	_, stderr := captureOutput(t, func() {
		_ = runLaunchApp(nil)
	})

	errorAt := strings.Index(stderr, "error: --app-id is required")
	usageAt := strings.Index(stderr, "Usage: gdn app launch")
	fixAt := strings.Index(stderr, "Fix:")
	if errorAt < 0 || usageAt < 0 || fixAt < 0 {
		t.Fatalf("missing expected error/usage/fix output:\n%s", stderr)
	}
	if !(errorAt < usageAt && usageAt < fixAt) {
		t.Fatalf("expected error, then usage, then fix; got:\n%s", stderr)
	}
}

func TestValidationErrorsShowErrorUsageThenFix(t *testing.T) {
	for _, key := range []string{"NGC_API_KEY", "NGC_ORG", "GDN_BASE_URL", "NGC_BASE_URL"} {
		t.Setenv(key, "")
	}

	cases := []struct {
		name    string
		run     func() error
		usage   string
		wantFix bool
	}{
		{
			name: "app launch missing app id",
			run: func() error {
				return runLaunchApp(nil)
			},
			usage:   "Usage: gdn app launch",
			wantFix: true,
		},
		{
			name:    "app update missing app id",
			run:     func() error { return runUpdateApp(nil) },
			usage:   "Usage: gdn app update",
			wantFix: true,
		},
		{
			name:    "app clone missing app id",
			run:     func() error { return runCloneApp(nil) },
			usage:   "Usage: gdn app clone",
			wantFix: true,
		},
		{
			name:    "app delete missing app id",
			run:     func() error { return runDeleteApp(nil) },
			usage:   "Usage: gdn app delete",
			wantFix: true,
		},
		{
			name:    "resource delete missing artifact",
			run:     func() error { return runDeleteResource(nil) },
			usage:   "Usage: gdn resource delete",
			wantFix: true,
		},
		{
			name:    "resource delete missing version",
			run:     func() error { return runDeleteResource([]string{"--name", "my-app-1"}) },
			usage:   "Usage: gdn resource delete",
			wantFix: true,
		},
		{
			name: "app version add missing app id",
			run: func() error {
				return runAddVersion([]string{"--dir", ".", "--version", "1.0.0", "--app-name", "my-app-1"})
			},
			usage:   "Usage: gdn app version add",
			wantFix: true,
		},
		{
			name:    "app version add missing artifact name",
			run:     func() error { return runAddVersion([]string{"--dir", ".", "--version", "1.0.0", "--app-id", "app-1"}) },
			usage:   "Usage: gdn app version add",
			wantFix: true,
		},
		{
			name:    "app list missing credentials",
			run:     func() error { return runListApps(nil) },
			usage:   "Usage: gdn app list",
			wantFix: true,
		},
		{
			name:    "resource list missing credentials",
			run:     func() error { return runListResources(nil) },
			usage:   "Usage: gdn resource list",
			wantFix: true,
		},
		{
			name:    "config check missing credentials",
			run:     func() error { return runConfigCheck(nil) },
			usage:   "Usage: gdn config check",
			wantFix: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr := captureOutput(t, func() {
				_ = tc.run()
			})
			errorAt := strings.Index(stderr, "error:")
			usageAt := strings.Index(stderr, tc.usage)
			if errorAt < 0 || usageAt < 0 {
				t.Fatalf("missing error or usage output:\n%s", stderr)
			}
			if errorAt > usageAt {
				t.Fatalf("expected error before usage:\n%s", stderr)
			}
			fixAt := strings.Index(stderr, "Fix:")
			if tc.wantFix {
				if fixAt < 0 {
					t.Fatalf("missing fix output:\n%s", stderr)
				}
				if usageAt > fixAt {
					t.Fatalf("expected usage before fix:\n%s", stderr)
				}
			} else if fixAt >= 0 && fixAt < usageAt {
				t.Fatalf("fix should not appear before usage:\n%s", stderr)
			}
		})
	}
}

// TestRunLaunchAppSuccess verifies the happy path: nonce is fetched from the
// GIO endpoint and the printed launch URL contains all expected query params.
func TestRunLaunchAppSuccess(t *testing.T) {
	var gotPath, gotAuth, gotClientType string
	var gotBody struct {
		ApplicationID string `json:"applicationId"`
	}

	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotClientType = r.Header.Get("NV-Client-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		writeJSON(w, `{"nonce":"test-nonce-xyz"}`)
	}))
	defer srv.Close()

	t.Setenv("NGC_API_KEY", "tok-secret")
	t.Setenv("GDN_BASE_URL", srv.URL)
	t.Setenv("GDN_CLIENT_URL", "https://stg.client.gdn.nvidia.com")
	var err error
	launchURL := strings.TrimSpace(captureStdout(t, func() {
		err = runLaunchApp([]string{
			"--app-id", "app-42",
			"--org", "myorg",
			"--open=false",
		})
	}))

	if err != nil {
		t.Fatalf("runLaunchApp: %v", err)
	}

	// Nonce request: org-scoped path, auth header, and custom client-type header.
	if !strings.Contains(gotPath, "/v1/orgs/myorg/nonce") {
		t.Errorf("nonce path = %q, want /v1/orgs/myorg/nonce", gotPath)
	}
	if gotAuth != "Bearer tok-secret" {
		t.Errorf("Authorization = %q, want Bearer tok-secret", gotAuth)
	}
	if gotClientType != "GDNSDK" {
		t.Errorf("NV-Client-Type = %q, want GDNSDK", gotClientType)
	}
	if gotBody.ApplicationID != "app-42" {
		t.Errorf("request applicationId = %q, want app-42", gotBody.ApplicationID)
	}

	// Launch URL: correct base and all required query params present.
	if !strings.HasPrefix(launchURL, "https://stg.client.gdn.nvidia.com/") {
		t.Errorf("launch URL prefix wrong: %q", launchURL)
	}
	for _, want := range []string{
		"nonce=test-nonce-xyz",
		"application-id=app-42",
		"org-name=myorg",
	} {
		if !strings.Contains(launchURL, want) {
			t.Errorf("launch URL missing %q;\ngot: %s", want, launchURL)
		}
	}
	// session-server-address is derived from the GDN gateway host.
	if gwHost := srvHost(t, srv.URL); !strings.Contains(launchURL, "session-server-address="+url.QueryEscape(gwHost)) {
		t.Errorf("launch URL missing session-server-address=%s;\ngot: %s", gwHost, launchURL)
	}
}

// srvHost returns the session-server-address runLaunchApp derives from a GDN
// gateway base URL: the "prod." subdomain prepended to the host[:port].
func srvHost(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return "prod." + u.Host
}

func TestRunLaunchHelpDoesNotPrintEnvValues(t *testing.T) {
	t.Setenv("NGC_API_KEY", "tok-super-secret")
	t.Setenv("GDN_BASE_URL", "https://secret.gdn.example.com")
	t.Setenv("GDN_CLIENT_URL", "https://secret.client.example.com")
	_, stderr := captureOutput(t, func() {
		err := runLaunchApp([]string{"--help"})
		if err != nil {
			t.Fatalf("runLaunchApp --help error = %v, want nil", err)
		}
	})
	for _, secret := range []string{
		"tok-super-secret",
		"https://secret.gdn.example.com",
		"https://secret.client.example.com",
	} {
		if strings.Contains(stderr, secret) {
			t.Fatalf("help output leaked env value %q:\n%s", secret, stderr)
		}
	}
	for _, want := range []string{
		"Required:",
		"Common:",
		"More:",
		"--help-all",
		"--app-id",
		"--org",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("help output missing %q:\n%s", want, stderr)
		}
	}
	// gio/client are no longer launch flags; they are derived from org config.
	for _, notWant := range []string{"--gio-base-url", "--client-base-url"} {
		if strings.Contains(stderr, notWant) {
			t.Fatalf("compact help should not include removed flag %q:\n%s", notWant, stderr)
		}
	}
	if strings.Contains(stderr, "All flags:") {
		t.Fatalf("compact help should not include full flag dump:\n%s", stderr)
	}
	if strings.Contains(stderr, "Advanced:") {
		t.Fatalf("compact help should not include advanced section:\n%s", stderr)
	}
}

// TestRunLaunchAppMediaTypeOmittedByDefault verifies upstream-media-type is
// absent from the launch URL when not explicitly set.
func TestRunLaunchAppMediaTypeOmittedByDefault(t *testing.T) {
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_API_KEY", "tok")
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"nonce":"nonce-abc"}`)
	}))
	defer srv.Close()

	t.Setenv("GDN_BASE_URL", srv.URL)
	t.Setenv("GDN_CLIENT_URL", "https://client.example.com")
	launchURL := strings.TrimSpace(captureStdout(t, func() {
		_ = runLaunchApp([]string{
			"--app-id", "app-1",
			"--open=false",
		})
	}))

	if strings.Contains(launchURL, "upstream-media-type") {
		t.Errorf("upstream-media-type should be absent by default; got: %s", launchURL)
	}
}

// TestRunLaunchAppSessionServerFromGateway verifies that session-server-address
// is derived from the GDN gateway host and always included in the launch URL.
func TestRunLaunchAppSessionServerFromGateway(t *testing.T) {
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_API_KEY", "tok")
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"nonce":"nonce-abc"}`)
	}))
	defer srv.Close()

	t.Setenv("GDN_BASE_URL", srv.URL)
	t.Setenv("GDN_CLIENT_URL", "https://client.example.com")
	var err error
	launchURL := strings.TrimSpace(captureStdout(t, func() {
		err = runLaunchApp([]string{
			"--app-id", "app-1",
			"--open=false",
		})
	}))

	if err != nil {
		t.Fatalf("runLaunchApp: %v", err)
	}
	if want := "session-server-address=" + url.QueryEscape(srvHost(t, srv.URL)); !strings.Contains(launchURL, want) {
		t.Errorf("launch URL missing %q; got: %s", want, launchURL)
	}
	if !strings.Contains(launchURL, "nonce=nonce-abc") {
		t.Errorf("launch URL missing nonce; got: %s", launchURL)
	}
}

// TestRunLaunchAppNonceAPIError verifies that a non-2xx from the GIO endpoint
// surfaces as an error and no launch URL is printed.
func TestRunLaunchAppNonceAPIError(t *testing.T) {
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_API_KEY", "bad-tok")
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
	}))
	defer srv.Close()

	t.Setenv("GDN_BASE_URL", srv.URL)
	t.Setenv("GDN_CLIENT_URL", "https://client.example.com")
	err := runLaunchApp([]string{
		"--app-id", "app-1",
	})
	if err == nil {
		t.Fatal("expected error on 401 from nonce endpoint")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention 401, got: %v", err)
	}
}

// TestRunLaunchAppBadNonceResponse verifies that a 200 with no parseable
// nonce field surfaces as an error rather than producing a broken URL.
func TestRunLaunchAppBadNonceResponse(t *testing.T) {
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_API_KEY", "tok")
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"token":"something-unexpected"}`)
	}))
	defer srv.Close()

	t.Setenv("GDN_BASE_URL", srv.URL)
	t.Setenv("GDN_CLIENT_URL", "https://client.example.com")
	err := runLaunchApp([]string{
		"--app-id", "app-1",
	})
	if err == nil {
		t.Fatal("expected error when nonce field is absent from response")
	}
	if !strings.Contains(err.Error(), "nonce") {
		t.Errorf("error should mention nonce, got: %v", err)
	}
}

// TestRunLaunchAppVersionID verifies that --version-id is sent in the nonce
// request body when provided, and is absent when omitted.
func TestRunLaunchAppVersionID(t *testing.T) {
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_API_KEY", "tok")
	baseArgs := func(t *testing.T, srvURL string) []string {
		t.Setenv("GDN_BASE_URL", srvURL)
		t.Setenv("GDN_CLIENT_URL", "https://client.example.com")
		return []string{"--app-id", "app-1"}
	}

	t.Run("with version-id", func(t *testing.T) {
		var gotBody struct {
			ApplicationID string `json:"applicationId"`
			VersionID     string `json:"versionId"`
		}
		srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			writeJSON(w, `{"nonce":"n1"}`)
		}))
		defer srv.Close()

		var err error
		captureStdout(t, func() {
			err = runLaunchApp(append(baseArgs(t, srv.URL), "--version-id", "ver-99", "--open=false"))
		})
		if err != nil {
			t.Fatalf("runLaunchApp: %v", err)
		}
		if gotBody.VersionID != "ver-99" {
			t.Errorf("nonce body versionId = %q, want ver-99", gotBody.VersionID)
		}
		if gotBody.ApplicationID != "app-1" {
			t.Errorf("nonce body applicationId = %q, want app-1", gotBody.ApplicationID)
		}
	})

	t.Run("without version-id omits field", func(t *testing.T) {
		var rawBody []byte
		srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawBody, _ = io.ReadAll(r.Body)
			writeJSON(w, `{"nonce":"n2"}`)
		}))
		defer srv.Close()

		var err error
		captureStdout(t, func() {
			err = runLaunchApp(append(baseArgs(t, srv.URL), "--open=false"))
		})
		if err != nil {
			t.Fatalf("runLaunchApp: %v", err)
		}
		if strings.Contains(string(rawBody), "versionId") {
			t.Errorf("nonce body should not contain versionId when flag is omitted; got: %s", rawBody)
		}
	})
}

func TestRunLaunchAppFeaturesAndSecureData(t *testing.T) {
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_API_KEY", "tok")
	baseArgs := func(t *testing.T, srvURL string) []string {
		t.Setenv("GDN_BASE_URL", srvURL)
		t.Setenv("GDN_CLIENT_URL", "https://client.example.com")
		return []string{"--app-id", "app-1", "--open=false"}
	}

	t.Run("overrides forwarded to nonce", func(t *testing.T) {
		var gotBody struct {
			ApplicationID     string          `json:"applicationId"`
			PartnerSecureData string          `json:"partnerSecureData"`
			StreamLookupID    string          `json:"streamLookupId"`
			UserID            string          `json:"userId"`
			AppFeatures       json.RawMessage `json:"applicationFeatures"`
		}
		srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			writeJSON(w, `{"nonce":"n1"}`)
		}))
		defer srv.Close()

		var err error
		captureStdout(t, func() {
			err = runLaunchApp(append(baseArgs(t, srv.URL),
				"--app-features", `{"TOUCH_SUPPORT":true,"MAX_BIT_RATE":100}`,
				"--partner-secure-data", "secret:token",
				"--stream-lookup-id", "lookup-7",
				"--user-id", "user-9"))
		})
		if err != nil {
			t.Fatalf("runLaunchApp: %v", err)
		}
		if gotBody.PartnerSecureData != "secret:token" {
			t.Errorf("nonce partnerSecureData = %q, want secret:token", gotBody.PartnerSecureData)
		}
		if gotBody.StreamLookupID != "lookup-7" {
			t.Errorf("nonce streamLookupId = %q, want lookup-7", gotBody.StreamLookupID)
		}
		if gotBody.UserID != "user-9" {
			t.Errorf("nonce userId = %q, want user-9", gotBody.UserID)
		}
		var feats map[string]any
		if err := json.Unmarshal(gotBody.AppFeatures, &feats); err != nil {
			t.Fatalf("applicationFeatures not a JSON object: %v (raw %s)", err, gotBody.AppFeatures)
		}
		if feats["TOUCH_SUPPORT"] != true {
			t.Errorf("applicationFeatures TOUCH_SUPPORT = %v, want true", feats["TOUCH_SUPPORT"])
		}
	})

	t.Run("rejects non-object app-features", func(t *testing.T) {
		srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, `{"nonce":"n1"}`)
		}))
		defer srv.Close()

		err := runLaunchApp(append(baseArgs(t, srv.URL), "--app-features", `["not","an","object"]`))
		if err == nil {
			t.Fatal("expected error for non-object --app-features, got nil")
		}
		if !strings.Contains(err.Error(), "JSON object") {
			t.Errorf("error = %v, want it to mention 'JSON object'", err)
		}
	})

	t.Run("omitted by default", func(t *testing.T) {
		var rawBody []byte
		srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawBody, _ = io.ReadAll(r.Body)
			writeJSON(w, `{"nonce":"n2"}`)
		}))
		defer srv.Close()

		var err error
		captureStdout(t, func() {
			err = runLaunchApp(baseArgs(t, srv.URL))
		})
		if err != nil {
			t.Fatalf("runLaunchApp: %v", err)
		}
		for _, field := range []string{"applicationFeatures", "partnerSecureData", "streamLookupId", "userId"} {
			if strings.Contains(string(rawBody), field) {
				t.Errorf("nonce body should omit %s when the flag is unset; got: %s", field, rawBody)
			}
		}
	})
}

// TestConfigSetGDNBaseURL is a regression test for the bug where --gdn-base-url
// was silently dropped from the stored config.
func TestConfigSetGDNBaseURL(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("GDN_CONFIG_FILE", cfgFile)

	if err := runConfigSet([]string{"--org", "stg-gdn-dev", "--gdn-base-url", "https://api-stg.gdn.nvidia.com"}); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}

	cfg, err := loadCLIConfig(cfgFile)
	if err != nil {
		t.Fatalf("loadCLIConfig: %v", err)
	}
	p, ok := cfg.Orgs["stg-gdn-dev"]
	if !ok {
		t.Fatal("org stg-gdn-dev not written to config")
	}
	if p.GDNBaseURL != "https://api-stg.gdn.nvidia.com" {
		t.Errorf("gdn_base_url = %q, want https://api-stg.gdn.nvidia.com", p.GDNBaseURL)
	}
}

func TestCloneAppName(t *testing.T) {
	if got := cloneAppName("MyApp", 1); got != "MyApp-1" {
		t.Errorf("cloneAppName(MyApp, 1) = %q, want MyApp-1", got)
	}
	if got := cloneAppName("MyApp", 2); got != "MyApp-2" {
		t.Errorf("cloneAppName(MyApp, 2) = %q, want MyApp-2", got)
	}
}

func TestPickCloneName(t *testing.T) {
	// "-1" is free.
	if got, err := pickCloneName("App", map[string]bool{}); err != nil || got != "App-1" {
		t.Errorf("got %q, %v; want App-1", got, err)
	}
	// "-1" taken (case-insensitive) -> "-2".
	if got, err := pickCloneName("App", map[string]bool{"app-1": true}); err != nil || got != "App-2" {
		t.Errorf("got %q, %v; want App-2", got, err)
	}
	// "-1" and "-2" taken -> "-3".
	if got, err := pickCloneName("App", map[string]bool{"app-1": true, "app-2": true}); err != nil || got != "App-3" {
		t.Errorf("got %q, %v; want App-3", got, err)
	}
	// 31-char base -> "-1" is 33 chars, over the limit.
	long := strings.Repeat("a", 31)
	if _, err := pickCloneName(long, map[string]bool{}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("want length error, got %v", err)
	}
}

func TestValidateAppName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wantErr string // empty: the name is valid
		suggest string // empty: the error offers no suggestion
	}{
		{name: "my-app_1"},
		{name: "A"},
		{name: strings.Repeat("a", 32)},
		{name: "My App", wantErr: "letters, digits, '-' and '_'", suggest: "My-App"},
		{name: "-app", wantErr: "start with a letter or digit", suggest: "app"},
		{name: "_app", wantErr: "start with a letter or digit", suggest: "app"},
		{name: "app.v2 (beta)", wantErr: "invalid application name", suggest: "app-v2-beta"},
		{name: strings.Repeat("a", 33), wantErr: "exceeds the GDN limit of 32", suggest: strings.Repeat("a", 32)},
		{name: "", wantErr: "invalid application name"},
		{name: "!!!", wantErr: "invalid application name"},
	} {
		err := validateAppName(tc.name)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("validateAppName(%q) = %v, want nil", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("validateAppName(%q) = %v, want error containing %q", tc.name, err, tc.wantErr)
			continue
		}
		if tc.suggest == "" {
			if strings.Contains(err.Error(), "try ") {
				t.Errorf("validateAppName(%q) suggests a name, want none: %v", tc.name, err)
			}
		} else if !strings.Contains(err.Error(), fmt.Sprintf("try %q", tc.suggest)) {
			t.Errorf("validateAppName(%q) = %v, want suggestion %q", tc.name, err, tc.suggest)
		}
		if tc.suggest != "" && validateAppName(tc.suggest) != nil {
			t.Errorf("suggestion %q for %q is itself invalid", tc.suggest, tc.name)
		}
	}
}

// A bad name must fail before the folder is scanned or uploaded, dry run
// included, so a create never leaves an orphaned NGC resource behind.
func TestRunCreateAppRejectsInvalidNameBeforeUpload(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	stdout, stderr := captureOutput(t, func() {
		err = runCreateApp([]string{"--dir", dir, "--app-name", "My App", "--version", "1.0.0", "--dry-run"})
	})
	if err == nil {
		t.Fatal("expected an error for an invalid application name")
	}
	if !strings.Contains(stderr, `try "My-App"`) {
		t.Errorf("stderr lacks the suggested name:\n%s", stderr)
	}
	if strings.Contains(stdout+stderr, "scanning") {
		t.Errorf("the folder was scanned before the name was checked:\n%s%s", stdout, stderr)
	}
}

func TestRunUpdateAppRejectsUnknownInstanceName(t *testing.T) {
	patched := false
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/gdn/apps/create-config":
			writeJSON(w, `{"allowedInstanceNames":["NVIDIA L40"]}`)
		case r.Method == http.MethodPatch:
			patched = true
			writeJSON(w, `{"id":"app-1"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("NGC_API_KEY", "k")
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_BASE_URL", srv.URL)

	err := runUpdateApp([]string{"--app-id", "app-1", "--instances", "NVIDIA L10"})
	if err == nil || !strings.Contains(err.Error(), `unknown instance name(s) "NVIDIA L10"`) || !strings.Contains(err.Error(), "NVIDIA L40") {
		t.Errorf("err = %v, want an unknown-instance error that lists NVIDIA L40", err)
	}
	if patched {
		t.Error("app update must not PATCH with an unknown instance name")
	}
}

func TestRunUpdateAppRejectsInvalidName(t *testing.T) {
	err := runUpdateApp([]string{"--app-id", "app-1", "--app-name", "My App", "--dry-run"})
	if err == nil || !strings.Contains(err.Error(), `try "My-App"`) {
		t.Errorf("err = %v, want an invalid-name error suggesting My-App", err)
	}
}

func TestCloneResolveNameValidates(t *testing.T) {
	explicit := "bad name"
	c := &cloneAppFlags{cloneName: &explicit}
	if _, err := c.resolveName("src", map[string]bool{}); err == nil || !strings.Contains(err.Error(), `try "bad-name"`) {
		t.Errorf("explicit --name: err = %v, want an invalid-name error suggesting bad-name", err)
	}

	// A source app created before the name rule existed yields an invalid
	// generated name, so the user has to pick one.
	generated := ""
	c = &cloneAppFlags{cloneName: &generated}
	if _, err := c.resolveName("Old App", map[string]bool{}); err == nil || !strings.Contains(err.Error(), "pass --name") {
		t.Errorf("generated name: err = %v, want a hint to pass --name", err)
	}
}

// sourceApp mirrors a slim GDN GET-app payload: the embedded version carries
// no launchConfig/artifacts, so clone falls back to the version GET. The
// current API embeds the full current version instead (see
// TestRunCloneAppEmbeddedFullVersion).
const sourceApp = `{
	"id":"app-1","name":"My-App","description":"d","seatType":"LINUX",
	"instanceNames":["NVIDIA A10","NVIDIA L20"],
	"applicationFeatures":{"DISPLAY_MODE":[{"MAX_RESOLUTION":"4K","MAX_FPS":120}],"RTX_ENABLED":true,"MAX_BIT_RATE":200,
		"TRANSPORT_POLICY":{"POLICY":"RELAY","RELAY_PROTOCOL":"TCP","RELAY_LOCATION":"ZONE"},
		"COVERAGE_AREA_POLICY":{"POLICY":"CUSTOM","COVERAGE_AREAS":["us-north-east"]}},
	"defaultVersionId":"ver-2",
	"versions":[{"id":"ver-2","versionName":"1.1.0"}]
}`

// sourceVersion mirrors the real version GET: artifacts have a "uri" but no
// "orgName", which is exactly what tripped up the create call.
const sourceVersion = `{
	"id":"ver-2","versionName":"1.1.0",
	"launchConfig":{"executablePath":"%GDN_ROOT%\\RESOURCE\\my-app-1\\app.exe"},
	"artifacts":[{"id":"art-1","name":"my-app-1","teamName":"dev","versionId":"1","type":"RESOURCE","uri":"https://api/v2/org/o/team/dev/resources/my-app-1/1/files","size":123}]
}`

func TestRunCloneApp(t *testing.T) {
	var createBody createAppReq
	var createRaw []byte
	var patchedVersion string
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/orgs/o/gdn/apps/app-1":
			writeJSON(w, sourceApp)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/orgs/o/gdn/apps/app-1/versions/ver-2":
			writeJSON(w, sourceVersion)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/gdn/apps/create-config":
			writeJSON(w, `{"allowedInstanceNames":["NVIDIA A10","NVIDIA L20","NVIDIA L40"]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/orgs/o/gdn/apps":
			// "My-App-1" already exists, so the clone must become "My-App-2".
			writeJSON(w, `{"applications":[{"id":"app-1","name":"My-App"},{"id":"app-2","name":"My-App-1"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/orgs/o/gdn/apps":
			createRaw, _ = io.ReadAll(r.Body)
			_ = json.Unmarshal(createRaw, &createBody)
			writeJSON(w, `{"id":"app-3","name":"My-App-2","versions":[{"id":"new-ver","artifacts":[{"id":"art-new","name":"my-app-1","type":"RESOURCE","versionId":"1"}]}]}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/v2/orgs/o/gdn/apps/app-3":
			var b map[string]string
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &b)
			patchedVersion = b["defaultVersionId"]
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	t.Setenv("NGC_API_KEY", "k")
	t.Setenv("NGC_BASE_URL", srv.URL)
	pr, pw, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = pw
	err := runCloneApp([]string{"--app-id", "app-1", "--org", "o"})
	pw.Close()
	os.Stdout = old
	io.Copy(io.Discard, pr)

	if err != nil {
		t.Fatalf("runCloneApp: %v", err)
	}
	if createBody.Name != "My-App-2" {
		t.Errorf("clone name = %q, want My-App-2 (My-App-1 already taken)", createBody.Name)
	}
	if createBody.SeatType != "LINUX" || len(createBody.InstanceNames) != 2 {
		t.Errorf("settings not copied: %+v", createBody)
	}
	if f := createBody.ApplicationFeatures; f == nil || f.MaxBitRate == nil || *f.MaxBitRate != 200 || len(f.DisplayMode) != 1 {
		t.Errorf("features not copied: %+v", f)
	}
	if tp := createBody.ApplicationFeatures.TransportPolicy; tp == nil || *tp != (transportPolicy{Policy: "RELAY", RelayProtocol: "TCP", RelayLocation: "ZONE"}) {
		t.Errorf("TRANSPORT_POLICY not copied: %+v", tp)
	}
	if cp := createBody.ApplicationFeatures.CoverageAreaPolicy; cp == nil || cp.Policy != "CUSTOM" || !slices.Equal(cp.CoverageAreas, []string{"us-north-east"}) {
		t.Errorf("COVERAGE_AREA_POLICY not copied: %+v", cp)
	}
	// The source app still carries the RTX_ENABLED key the backend stores;
	// the clone must not send it back.
	if strings.Contains(string(createRaw), "RTX_ENABLED") {
		t.Errorf("clone request carries RTX_ENABLED: %s", createRaw)
	}
	if len(createBody.Version.Artifacts) != 1 {
		t.Fatalf("artifact (resource) not preserved: %+v", createBody.Version.Artifacts)
	}
	art := createBody.Version.Artifacts[0]
	if art.Name != "my-app-1" || art.VersionID != "1" || art.Type != "RESOURCE" {
		t.Errorf("artifact ref not preserved: %+v", art)
	}
	// Regression: the create API rejects an artifact ref with neither orgName
	// nor uri, so the org must be filled in from the request when missing.
	if art.OrgName != "o" {
		t.Errorf("artifact orgName = %q, want o (must be set for create to succeed)", art.OrgName)
	}
	// Dropping the team would point the clone at a different (org-scoped) resource.
	if art.TeamName != "dev" {
		t.Errorf("artifact teamName = %q, want dev", art.TeamName)
	}
	if createBody.Version.LaunchConfig.ExecutablePath == "" {
		t.Error("launch config not copied")
	}
	if patchedVersion != "new-ver" {
		t.Errorf("default version PATCH = %q, want new-ver", patchedVersion)
	}
}

// The current GDN API embeds the current version in the app payload with full
// launchConfig and artifacts, so clone must not pay for the extra version GET.
func TestRunCloneAppDryRunSkipsInstanceCheck(t *testing.T) {
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/orgs/o/gdn/apps/app-1":
			writeJSON(w, sourceApp)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/orgs/o/gdn/apps/app-1/versions/ver-2":
			writeJSON(w, sourceVersion)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/orgs/o/gdn/apps":
			writeJSON(w, `{"applications":[{"id":"app-1","name":"My-App"}]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("NGC_API_KEY", "k")
	t.Setenv("NGC_BASE_URL", srv.URL)

	var err error
	out := captureStdout(t, func() {
		err = runCloneApp([]string{"--app-id", "app-1", "--org", "o", "--dry-run"})
	})
	if err != nil {
		t.Fatalf("runCloneApp --dry-run: %v", err)
	}
	if !strings.Contains(out, "instances: NVIDIA A10, NVIDIA L20") {
		t.Errorf("dry-run output lacks the source's instance names:\n%s", out)
	}
}

func TestRunCloneAppEmbeddedFullVersion(t *testing.T) {
	var createBody createAppReq
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/orgs/o/gdn/apps/app-1":
			writeJSON(w, `{
				"id":"app-1","name":"My-App","description":"d","seatType":"LINUX",
				"instanceNames":["NVIDIA A10"],
				"applicationFeatures":{"DISPLAY_MODE":[{"MAX_RESOLUTION":"4K","MAX_FPS":120}],"RTX_ENABLED":true,"MAX_BIT_RATE":200},
				"defaultVersionId":"ver-2","versionCount":2,
				"versions":[{"id":"ver-2","versionName":"1.1.0",
					"launchConfig":{"executablePath":"%GDN_ROOT%\\RESOURCE\\my-app-1\\app.exe"},
					"artifacts":[{"id":"art-1","name":"my-app-1","versionId":"1","type":"RESOURCE","uri":"https://api/v2/org/o/resources/my-app-1/1/files","size":123}]}]
			}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/gdn/apps/create-config":
			writeJSON(w, `{"allowedInstanceNames":["NVIDIA A10","NVIDIA L20","NVIDIA L40"]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/orgs/o/gdn/apps":
			writeJSON(w, `{"applications":[{"id":"app-1","name":"My-App"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/orgs/o/gdn/apps":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &createBody)
			writeJSON(w, `{"id":"app-3","name":"My-App-1","versions":[{"id":"new-ver","artifacts":[{"id":"art-new","name":"my-app-1","type":"RESOURCE","versionId":"1"}]}]}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/v2/orgs/o/gdn/apps/app-3":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s (the embedded current version should make the version GET unnecessary)", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	t.Setenv("NGC_API_KEY", "k")
	t.Setenv("NGC_BASE_URL", srv.URL)
	var err error
	captureStdout(t, func() { err = runCloneApp([]string{"--app-id", "app-1", "--org", "o"}) })

	if err != nil {
		t.Fatalf("runCloneApp: %v", err)
	}
	if createBody.Name != "My-App-1" {
		t.Errorf("clone name = %q, want My-App-1", createBody.Name)
	}
	if len(createBody.Version.Artifacts) != 1 || createBody.Version.Artifacts[0].Name != "my-app-1" {
		t.Errorf("artifacts not preserved from the embedded version: %+v", createBody.Version.Artifacts)
	}
	if createBody.Version.LaunchConfig.ExecutablePath == "" {
		t.Error("launch config not copied from the embedded version")
	}
}

func TestRunCloneAppNameTooLong(t *testing.T) {
	// 31-char source name -> 33-char clone, exceeding the 32-char limit.
	longName := strings.Repeat("a", 31)
	body := `{"id":"app-1","name":"` + longName + `","seatType":"WINDOWS","defaultVersionId":"v1",` +
		`"versions":[{"id":"v1","artifacts":[{"name":"my-app-1","versionId":"1.0.0"}],"launchConfig":{"executablePath":"x"}}]}`
	posted := false
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
		}
		writeJSON(w, body)
	}))
	defer srv.Close()

	t.Setenv("NGC_API_KEY", "k")
	t.Setenv("NGC_BASE_URL", srv.URL)
	err := runCloneApp([]string{"--app-id", "app-1", "--org", "o"})
	if err == nil {
		t.Fatal("expected an error for an over-long clone name")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want it to mention the length limit", err)
	}
	if posted {
		t.Error("app clone must not create an app when the name is too long")
	}
}

func TestCloneFeaturesMergesPolicyOverrides(t *testing.T) {
	src := &gdnApp{}
	if err := json.Unmarshal([]byte(`{"applicationFeatures":{
		"TRANSPORT_POLICY":{"POLICY":"RELAY","RELAY_PROTOCOL":"TCP","RELAY_LOCATION":"ZONE"},
		"COVERAGE_AREA_POLICY":{"POLICY":"CUSTOM","COVERAGE_AREAS":["us-north-east"]}}}`), src); err != nil {
		t.Fatal(err)
	}

	override := `{"TRANSPORT_POLICY":{"POLICY":"DIRECT"}}`
	c := &cloneAppFlags{appFeaturesJSON: &override}
	got, err := c.cloneFeatures(src)
	if err != nil {
		t.Fatalf("cloneFeatures: %v", err)
	}
	if want := (transportPolicy{Policy: "DIRECT", RelayProtocol: "TCP", RelayLocation: "ZONE"}); got.TransportPolicy == nil || *got.TransportPolicy != want {
		t.Errorf("TRANSPORT_POLICY = %+v, want %+v (the override merges into the source)", got.TransportPolicy, want)
	}

	clearAreas := `{"COVERAGE_AREA_POLICY":{"POLICY":"CUSTOM","COVERAGE_AREAS":[]}}`
	c = &cloneAppFlags{appFeaturesJSON: &clearAreas}
	if _, err := c.cloneFeatures(src); err == nil || !strings.Contains(err.Error(), "needs COVERAGE_AREAS") {
		t.Errorf("err = %v, want the CUSTOM-without-areas error", err)
	}
}

func TestCloneFeaturesReplacesDisplayModes(t *testing.T) {
	src := &gdnApp{}
	if err := json.Unmarshal([]byte(`{"applicationFeatures":{"DISPLAY_MODE":[
		{"MAX_RESOLUTION":"4k","MAX_FPS":60},{"MAX_RESOLUTION":"1080p","MAX_FPS":120}]}}`), src); err != nil {
		t.Fatal(err)
	}

	override := `{"DISPLAY_MODE":[{"MAX_RESOLUTION":"720p","MAX_FPS":30}]}`
	c := &cloneAppFlags{appFeaturesJSON: &override}
	got, err := c.cloneFeatures(src)
	if err != nil {
		t.Fatalf("cloneFeatures: %v", err)
	}
	if want := []displayMode{{MaxResolution: "720p", MaxFPS: 30}}; !slices.Equal(got.DisplayMode, want) {
		t.Errorf("DISPLAY_MODE = %+v, want %+v (the override replaces the list)", got.DisplayMode, want)
	}

	partial := `{"DISPLAY_MODE":[{"MAX_RESOLUTION":"720p"}]}`
	c = &cloneAppFlags{appFeaturesJSON: &partial}
	if _, err := c.cloneFeatures(src); err == nil || !strings.Contains(err.Error(), "needs MAX_RESOLUTION and MAX_FPS") {
		t.Errorf("err = %v, want the incomplete-entry error, not an FPS taken from the source", err)
	}

	other := `{"TOUCH_SUPPORT":true}`
	c = &cloneAppFlags{appFeaturesJSON: &other}
	if got, err = c.cloneFeatures(src); err != nil || len(got.DisplayMode) != 2 {
		t.Errorf("DISPLAY_MODE = %+v, err = %v; want the source's two modes kept", got.DisplayMode, err)
	}
}

func TestUpdateFeaturesRejectPartialTransportPolicy(t *testing.T) {
	fs := flag.NewFlagSet("app update", flag.ContinueOnError)
	u := registerUpdateAppFlags(fs)
	if err := fs.Parse([]string{"--app-id", "a", "--app-features", `{"TRANSPORT_POLICY":{"POLICY":"RELAY"}}`}); err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if _, err := u.features(set); err == nil || !strings.Contains(err.Error(), "--app-features: TRANSPORT_POLICY needs") {
		t.Errorf("err = %v, want the partial TRANSPORT_POLICY error", err)
	}
}

func TestCreateAppFeaturesAcceptPolicies(t *testing.T) {
	doc := `{"COVERAGE_AREA_POLICY":{"POLICY":"CUSTOM","COVERAGE_AREAS":["us-north-east"]},"TRANSPORT_POLICY":{"POLICY":"ALL","RELAY_PROTOCOL":"ALL","RELAY_LOCATION":"CSP"}}`
	u := &uploadFlags{appFeatures: &doc}
	f, err := u.parseAppFeatures()
	if err != nil {
		t.Fatalf("parseAppFeatures: %v", err)
	}
	if f.CoverageAreaPolicy == nil || f.TransportPolicy == nil {
		t.Errorf("policies not decoded: %+v", f)
	}
}

func TestParseResourceRef(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    resourceRef
		wantErr bool
	}{
		{in: "my-app:1.0.0", want: resourceRef{Name: "my-app", Version: "1.0.0"}},
		{in: " dev/my-app : 1.0.0 ", want: resourceRef{Team: "dev", Name: "my-app", Version: "1.0.0"}},
		{in: "my-app:1.0:rc", want: resourceRef{Name: "my-app", Version: "1.0:rc"}},
		{in: "my-app", wantErr: true},
		{in: "my-app:", wantErr: true},
		{in: ":1.0.0", wantErr: true},
		{in: "/my-app:1.0.0", wantErr: true},
		{in: "dev/:1.0.0", wantErr: true},
		{in: "a/b/c:1.0.0", wantErr: true},
	} {
		got, err := parseResourceRef(tc.in)
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "[<team>/]<resource-name>:<version>") {
				t.Errorf("parseResourceRef(%q) = %+v, %v; want a format error", tc.in, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseResourceRef(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
}

func TestResourceRefString(t *testing.T) {
	if got := (resourceRef{Name: "a", Version: "1"}).String(); got != "a:1" {
		t.Errorf("org-scoped String() = %q, want a:1", got)
	}
	if got := (resourceRef{Team: "dev", Name: "a", Version: "1"}).String(); got != "dev/a:1" {
		t.Errorf("team-scoped String() = %q, want dev/a:1", got)
	}
}

func TestPublishArtifactsSendsTeam(t *testing.T) {
	ngc := &ngcClient{coords: coordinates{Org: "o", Type: "RESOURCE"}}
	var logged []string
	log := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	artifacts, res, err := publishArtifacts(context.Background(), ngc, options{
		Resources: []resourceRef{{Team: "dev", Name: "bundle", Version: "2.3.1"}, {Name: "shared", Version: "1"}},
	}, log)
	if err != nil {
		t.Fatalf("publishArtifacts: %v", err)
	}
	want := []artifactRef{
		{Name: "bundle", OrgName: "o", TeamName: "dev", Type: "RESOURCE", VersionID: "2.3.1"},
		{Name: "shared", OrgName: "o", Type: "RESOURCE", VersionID: "1"},
	}
	if !slices.Equal(artifacts, want) {
		t.Errorf("artifacts = %+v\nwant        %+v", artifacts, want)
	}
	if res.ArtifactURI != "ngc://o/dev/bundle:2.3.1" {
		t.Errorf("ArtifactURI = %q, want ngc://o/dev/bundle:2.3.1", res.ArtifactURI)
	}
	if len(logged) == 0 || !strings.Contains(logged[0], "ngc://o/dev/bundle:2.3.1") {
		t.Errorf("log = %v, want the team-scoped reference", logged)
	}
}

func TestRunUpdateAppRejectsInvalidFeatureValues(t *testing.T) {
	for _, tc := range []struct {
		flag, wantErr string
	}{
		{"--max-bit-depth=7", "MAX_BIT_DEPTH must be 8 or 10, got 7"},
		{"--chroma-format=BANANA", `SUPPORTED_CHROMA_FORMAT must be one of YUV444, YUV420, got "BANANA"`},
		{"--qos-policy=NOPE", `QOS_POLICY must be one of QUALITY, LATENCY, got "NOPE"`},
		{"--instance-reuse-policy=WHATEVER", `INSTANCE_REUSE_POLICY must be one of`},
		{"--seat-type=MACOS", `seat type must be one of WINDOWS, LINUX, LINUX_PROTON, got "MACOS"`},
	} {
		err := runUpdateApp([]string{"--app-id", "app-1", tc.flag, "--dry-run"})
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want %q", tc.flag, err, tc.wantErr)
		}
	}
}

func TestRunUpdateAppNormalizesEnumCase(t *testing.T) {
	var err error
	out := captureStdout(t, func() {
		err = runUpdateApp([]string{"--app-id", "app-1", "--chroma-format=yuv444", "--qos-policy=quality", "--dry-run"})
	})
	if err != nil {
		t.Fatalf("runUpdateApp: %v", err)
	}
	for _, want := range []string{"chroma-format: YUV444", "qos-policy: QUALITY"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
}

func TestRunCreateAppRejectsUnknownSeatType(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	_, stderr := captureOutput(t, func() {
		err = runCreateApp([]string{"--dir", dir, "--app-name", "my-app", "--version", "1.0.0", "--seat-type", "macos", "--dry-run"})
	})
	if err == nil || !strings.Contains(stderr, `seat type must be one of WINDOWS, LINUX, LINUX_PROTON, got "MACOS"`) {
		t.Errorf("err = %v, stderr:\n%s", err, stderr)
	}
}

func TestLoadAppFeaturesDropsRTXEnabled(t *testing.T) {
	var f appFeature
	var err error
	_, stderr := captureOutput(t, func() {
		err = loadAppFeatures(`{"RTX_ENABLED":false,"MAX_BIT_RATE":50}`, &f)
	})
	if err != nil {
		t.Fatalf("loadAppFeatures: %v", err)
	}
	if f.MaxBitRate == nil || *f.MaxBitRate != 50 {
		t.Errorf("MAX_BIT_RATE = %v, want 50", f.MaxBitRate)
	}
	if !strings.Contains(stderr, "RTX_ENABLED is no longer supported and was not sent") {
		t.Errorf("stderr lacks the RTX warning:\n%s", stderr)
	}

	// A misspelt field is still rejected.
	if err := loadAppFeatures(`{"MAX_BITRATE":50}`, &appFeature{}); err == nil || !strings.Contains(err.Error(), `unknown field "MAX_BITRATE"`) {
		t.Errorf("typo: err = %v, want an unknown-field error", err)
	}
}

func TestRunUpdateAppIgnoresRTX(t *testing.T) {
	var patched []byte
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		patched, _ = io.ReadAll(r.Body)
		writeJSON(w, `{"id":"app-1","name":"a"}`)
	}))
	defer srv.Close()
	t.Setenv("NGC_API_KEY", "k")
	t.Setenv("NGC_ORG", "o")
	t.Setenv("NGC_BASE_URL", srv.URL)

	var err error
	_, stderr := captureOutput(t, func() {
		err = runUpdateApp([]string{"--app-id", "app-1", "--rtx=false", "--app-features", `{"RTX_ENABLED":false,"TOUCH_SUPPORT":true}`})
	})
	if err != nil {
		t.Fatalf("runUpdateApp: %v", err)
	}
	if string(patched) != `{"applicationFeatures":{"TOUCH_SUPPORT":true}}` {
		t.Errorf("PATCH body = %s, want only TOUCH_SUPPORT", patched)
	}
	for _, want := range []string{"--rtx is no longer supported", "RTX_ENABLED is no longer supported"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}

	// RTX alone leaves nothing to send, and the command still succeeds.
	for _, args := range [][]string{{"--rtx"}, {"--app-features", `{"RTX_ENABLED":true}`}} {
		patched = nil
		stdout, _ := captureOutput(t, func() {
			err = runUpdateApp(append([]string{"--app-id", "app-1"}, args...))
		})
		if err != nil || patched != nil || !strings.Contains(stdout, "Nothing to update") {
			t.Errorf("%v: err = %v, patched = %s, stdout = %q; want success, no PATCH and a nothing-to-update note", args, err, patched, stdout)
		}
	}
}

func TestCreateAppFeaturesOnlyRTXSendsNone(t *testing.T) {
	doc := `{"RTX_ENABLED":true}`
	u := &uploadFlags{appFeatures: &doc}
	var f *appFeature
	var err error
	captureOutput(t, func() { f, err = u.parseAppFeatures() })
	if err != nil || f != nil {
		t.Errorf("parseAppFeatures = %+v, %v; want nil, nil so no applicationFeatures is sent", f, err)
	}
}
