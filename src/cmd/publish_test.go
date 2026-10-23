// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestScanFilesCancelledBeforeStart checks that an already-cancelled context
// stops the scan instead of hashing the whole tree first.
func TestScanFilesCancelledBeforeStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i := range 50 {
		path := filepath.Join(dir, fmt.Sprintf("f%02d.bin", i))
		if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := scanFiles(ctx, dir, 4, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("scanFiles error = %v, want context.Canceled", err)
	}
}

// TestScanFilesCancelledMidHash covers Ctrl+C arriving while hashing is in
// flight: the scan must return promptly rather than running to completion.
func TestScanFilesCancelledMidHash(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Enough data that a full scan clearly outlasts the cancellation.
	payload := make([]byte, 4<<20)
	for i := range 40 {
		path := filepath.Join(dir, fmt.Sprintf("f%02d.bin", i))
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	start := time.Now()
	_, _, err := scanFiles(ctx, dir, 2, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scanFiles error = %v, want context.Canceled", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("scanFiles took %v after cancel; expected a prompt return", elapsed)
	}
}

// TestHashFileCancelledMidFile pins that cancellation lands inside a single
// large file, not only between files.
func TestHashFileCancelledMidFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, make([]byte, 32<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := appFile{LocalPath: path, Path: "big.bin"}
	if err := f.hash(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("hash error = %v, want context.Canceled", err)
	}
}

// newPublishServer builds a minimal httptest server that handles all requests
// needed by publish() for new-app and app version add paths. It records whether
// the setDefaultVersion PATCH was called and what versionID was sent.
func newPublishServer(t *testing.T) (srv *httptest.Server, defaultVersionSet *string) {
	t.Helper()
	var dvID string
	defaultVersionSet = &dvID

	srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// duplicate-name check (list apps) — no existing apps
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/gdn/apps") &&
			!strings.Contains(r.URL.Path, "/versions"):
			writeJSON(w, `{"applications":[]}`)

		// create app (POST /gdn/apps)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/gdn/apps"):
			writeJSON(w, `{"id":"app-new","versions":[{"id":"ver-1","artifacts":[{"id":"art-1","name":"my-app-1","type":"RESOURCE","versionId":"1.0"}]}]}`)

		// create version (POST /gdn/apps/{id}/versions)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/gdn/apps/") &&
			strings.HasSuffix(r.URL.Path, "/versions"):
			writeJSON(w, `{"id":"ver-2","artifacts":[{"id":"art-2","name":"my-app-1","type":"RESOURCE","versionId":"1.1"}]}`)

		// set default version (PATCH /gdn/apps/{id})
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/gdn/apps/"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			*defaultVersionSet = body["defaultVersionId"]
			writeJSON(w, `{"id":"app-new"}`)

		// NGC: create resource, create version (409 = already exists)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resources"):
			w.WriteHeader(http.StatusConflict)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/resources/") &&
			strings.HasSuffix(r.URL.Path, "/versions"):
			w.WriteHeader(http.StatusConflict)

		// NGC multipart upload
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/files/multipart"):
			writeJSON(w, `{"uploadID":"u1","partSize":0,"urls":["`+srv.URL+`/part"]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/part":
			w.Header().Set("ETag", `"e1"`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/files/multipart"):
			writeJSON(w, `{}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/files/commit"):
			writeJSON(w, `{}`)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	return srv, defaultVersionSet
}

// publish must fail fast (before any upload) when creating an app whose name
// already exists, unless --allow-duplicate is set. The server only serves the
// app listing; if publish got past the guard it would try to hit other
// endpoints and the test would notice via a different failure.
func TestPublishRejectsDuplicateAppName(t *testing.T) {
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"applications":[{"id":"app-existing","name":"My App"}]}`)
	}))
	defer srv.Close()

	portal := newPortal(srv.URL, "k", "o", secureClient)
	ngc := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}, secureClient)

	_, err := publish(context.Background(), ngc, portal, options{AppName: "My App"}, nil)
	if err == nil {
		t.Fatal("expected an error creating a duplicate-named app")
	}
	if !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "app-existing") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPublishSetsDefaultVersionOnNewApp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, defaultVersionSet := newPublishServer(t)
	defer srv.Close()

	portal := newPortal(srv.URL, "k", "o", secureClient)
	ngc := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0"}, secureClient)

	res, err := publish(context.Background(), ngc, portal, options{
		AppDir:         dir,
		AppName:        "New App",
		SeatType:       "WINDOWS",
		InstanceNames:  []string{"NVIDIA T5"},
		ExecutablePath: "app.exe",
		AllowDuplicate: true,
	}, nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.AppID != "app-new" {
		t.Errorf("AppID = %q, want app-new", res.AppID)
	}
	if *defaultVersionSet != "ver-1" {
		t.Errorf("defaultVersionId = %q, want ver-1", *defaultVersionSet)
	}
}

func TestPublishSetsDefaultVersionOnAddVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, defaultVersionSet := newPublishServer(t)
	defer srv.Close()

	portal := newPortal(srv.URL, "k", "o", secureClient)
	ngc := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.1"}, secureClient)

	res, err := publish(context.Background(), ngc, portal, options{
		AppDir:         dir,
		ExistingAppID:  "app-existing",
		ExecutablePath: "app.exe",
	}, nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if res.AppID != "app-existing" || res.VersionID != "ver-2" {
		t.Errorf("res = %+v", res)
	}
	if *defaultVersionSet != "ver-2" {
		t.Errorf("defaultVersionId = %q, want ver-2", *defaultVersionSet)
	}
}

func TestUploadToNGCRejectsInvalidArtifactNameBeforeScan(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("exe"), 0o644); err != nil {
		t.Fatal(err)
	}

	ngc := newNGC("", "k", coordinates{Org: "o", Type: "RESOURCE", Name: "MyApp1", Version: "1.0.0"}, secureClient)
	_, err := uploadToNGC(context.Background(), ngc, options{AppDir: dir}, nil)
	if err == nil {
		t.Fatal("expected error for invalid artifact name")
	}
	if !strings.Contains(err.Error(), "MyApp1") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUploadToNGCDryRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("exe"), 0o644); err != nil {
		t.Fatal(err)
	}

	ngc := newNGC("", "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}, secureClient)
	res, err := uploadToNGC(context.Background(), ngc, options{AppDir: dir, DryRun: true}, nil)
	if err != nil {
		t.Fatalf("uploadToNGC: %v", err)
	}
	if len(res.Files) != 1 || res.ArtifactURI != "ngc://o/my-app-1:1.0.0" {
		t.Errorf("result = %+v", res)
	}
}

func TestUploadToNGCCommitsWithoutPortal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(path, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	var committed bool
	var steps struct{ initiated, urls bool }
	var srv *httptest.Server
	srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resources"):
			w.WriteHeader(http.StatusConflict)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/versions"):
			w.WriteHeader(http.StatusConflict)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart" && !steps.initiated:
			steps.initiated = true
			writeJSON(w, `{"uploadID":"up-1","partSize":0}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart":
			steps.urls = true
			writeJSON(w, `{"uploadID":"up-1","urls":["`+srv.URL+`/part"]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/part":
			w.Header().Set("ETag", `"etag-1"`)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/org/o/files/multipart":
			writeJSON(w, `{}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/files/commit"):
			committed = true
			writeJSON(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	ngc := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}, secureClient)
	res, err := uploadToNGC(context.Background(), ngc, options{AppDir: dir, DisplayName: "App"}, nil)
	if err != nil {
		t.Fatalf("uploadToNGC: %v", err)
	}
	if !committed {
		t.Fatal("expected commit call")
	}
	if len(res.Files) != 1 || res.ArtifactURI != "ngc://o/my-app-1:1.0.0" {
		t.Errorf("result = %+v", res)
	}
}

// A file that changed once (an app exiting and truncating its log) should be
// re-hashed and uploaded under its new digest, not abort the run.
func TestUploadFileRehashingRetriesChangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := hashedFile(t, path, "server.log")

	if err := os.WriteFile(path, []byte("restarted"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := hashedFile(t, path, "server.log")

	srv, initiations := newFakeNGCServer(t, nil)
	defer srv.Close()

	c := newNGC(srv.URL, "k", testCoords(), secureClient)
	got, outcome, err := uploadFileRehashing(context.Background(), c, stale, 1, nil)
	if err != nil {
		t.Fatalf("uploadFileRehashing: %v", err)
	}
	if outcome.Abandoned {
		t.Fatal("file settled after one change; it should not have been skipped")
	}
	if got.Size != want.Size || got.SHA256 != want.SHA256 {
		t.Errorf("uploaded metadata = %d/%s, want %d/%s", got.Size, got.SHA256, want.Size, want.SHA256)
	}
	if n := atomic.LoadInt32(initiations); n != 2 {
		t.Errorf("multipart initiations = %d, want 2 (a fresh session for the new digest)", n)
	}
}

// A file being written to continuously cannot be captured consistently, so it
// is skipped after the retry budget instead of failing the whole upload.
func TestUploadFileRehashingSkipsPerpetuallyChangingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.log")
	if err := os.WriteFile(path, []byte("start"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := hashedFile(t, path, "live.log")

	// Append on every initiate so each attempt sees a file that moved again.
	srv, initiations := newFakeNGCServer(t, func() {
		fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Error(err)
			return
		}
		defer fh.Close()
		_, _ = fh.WriteString("more log output\n")
	})
	defer srv.Close()

	var logged strings.Builder
	c := newNGC(srv.URL, "k", testCoords(), secureClient)
	_, outcome, err := uploadFileRehashing(context.Background(), c, f, 1,
		func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) })
	if err != nil {
		t.Fatalf("a perpetually changing file should be skipped, not fail the run: %v", err)
	}
	if !outcome.Abandoned {
		t.Fatal("outcome.Abandoned = false, want true")
	}
	if n := atomic.LoadInt32(initiations); n != int32(maxFileChangeAttempts) {
		t.Errorf("initiations = %d, want %d", n, maxFileChangeAttempts)
	}
	out := logged.String()
	for _, want := range []string{"warning", "skipping live.log"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

// The commit manifest must never reference a file that was skipped, or NGC
// would be told about a blob it never received.
func TestUploadToNGCExcludesSkippedFileFromCommit(t *testing.T) {
	dir := t.TempDir()
	stable := filepath.Join(dir, "stable.bin")
	volatile := filepath.Join(dir, "volatile.log")
	if err := os.WriteFile(stable, []byte("stable contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(volatile, []byte("log line 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var commitBody struct {
		Files []struct {
			FilePath string `json:"filePath"`
			SHA256   string `json:"sha256"`
		} `json:"files"`
	}
	var srv *httptest.Server
	srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resources"),
			r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/versions"):
			w.WriteHeader(http.StatusConflict)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["partNumberList"]; !ok {
				// Keep appending to the volatile file so it never settles.
				if body["filePath"] == "volatile.log" {
					mu.Lock()
					fh, err := os.OpenFile(volatile, os.O_APPEND|os.O_WRONLY, 0o644)
					if err == nil {
						_, _ = fh.WriteString("another line\n")
						fh.Close()
					}
					mu.Unlock()
				}
				writeJSON(w, `{"uploadID":"up-1","partSize":0}`)
				return
			}
			writeJSON(w, `{"uploadID":"up-1","urls":["`+srv.URL+`/part"]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/part":
			w.Header().Set("ETag", `"etag-1"`)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/org/o/files/multipart":
			writeJSON(w, `{}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/files/commit"):
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&commitBody)
			mu.Unlock()
			writeJSON(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	ngc := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}, secureClient)
	res, err := uploadToNGC(context.Background(), ngc, options{AppDir: dir}, nil)
	if err != nil {
		t.Fatalf("uploadToNGC should complete despite one volatile file: %v", err)
	}

	var committed []string
	for _, f := range commitBody.Files {
		committed = append(committed, f.FilePath)
	}
	if !reflect.DeepEqual(committed, []string{"stable.bin"}) {
		t.Errorf("committed files = %v, want [stable.bin]", committed)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "stable.bin" {
		t.Errorf("result files = %+v, want only stable.bin", res.Files)
	}
	if res.TotalBytes != int64(len("stable contents")) {
		t.Errorf("TotalBytes = %d, want %d", res.TotalBytes, len("stable contents"))
	}
}

// A 409 on initiate means NGC already holds the blob. The file still belongs in
// the version, so it must be committed, but its bytes never moved and the
// result has to say so — otherwise a caller timing the upload reads the elapsed
// time as throughput. NGC keys that store by resource name and checksum, so
// this happens on a version that has never seen the file before.
func TestUploadToNGCCountsFilesNGCAlreadyHeld(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "held.bin"), []byte("already there"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fresh.bin"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var commitBody struct {
		Files []struct {
			FilePath string `json:"filePath"`
		} `json:"files"`
	}
	var srv *httptest.Server
	srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resources"),
			r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/versions"):
			w.WriteHeader(http.StatusConflict)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["filePath"] == "held.bin" {
				w.WriteHeader(http.StatusConflict)
				return
			}
			if _, ok := body["partNumberList"]; !ok {
				writeJSON(w, `{"uploadID":"up-1","partSize":0}`)
				return
			}
			writeJSON(w, `{"uploadID":"up-1","urls":["`+srv.URL+`/part"]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/part":
			w.Header().Set("ETag", `"etag-1"`)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/org/o/files/multipart":
			writeJSON(w, `{}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/files/commit"):
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&commitBody)
			mu.Unlock()
			writeJSON(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	ngc := newNGC(srv.URL, "k", testCoords(), secureClient)
	res, err := uploadToNGC(context.Background(), ngc, options{AppDir: dir, FileWorkers: 1}, nil)
	if err != nil {
		t.Fatalf("uploadToNGC: %v", err)
	}

	if res.DedupedFiles != 1 {
		t.Errorf("DedupedFiles = %d, want 1", res.DedupedFiles)
	}
	if want := int64(len("already there")); res.DedupedBytes != want {
		t.Errorf("DedupedBytes = %d, want %d", res.DedupedBytes, want)
	}
	// No other file in the tree carries that checksum, so the conflict can only
	// mean NGC had it beforehand.
	if res.PreexistingFiles != 1 {
		t.Errorf("PreexistingFiles = %d, want 1", res.PreexistingFiles)
	}
	// Reuse is not exclusion: both files are part of the version.
	if len(res.Files) != 2 {
		t.Errorf("result files = %+v, want both", res.Files)
	}
	var committed []string
	for _, f := range commitBody.Files {
		committed = append(committed, f.FilePath)
	}
	sort.Strings(committed)
	if !reflect.DeepEqual(committed, []string{"fresh.bin", "held.bin"}) {
		t.Errorf("committed files = %v, want both", committed)
	}
}

// NGC keys its store by checksum, not by path, so a tree that repeats content
// collides with itself on every copy after the first. Telling that apart from
// content NGC held before the upload is the whole point of the summary: the
// first is unavoidable, the second means nothing was transferred.
func TestSummarizeDedup(t *testing.T) {
	// dup.bin appears three times; NGC accepts the first and rejects the other
	// two. old.bin is a single file NGC already had. new.bin uploads normally.
	files := []appFile{
		{Path: "dup-a.bin", SHA256: "dup", Size: 10},
		{Path: "dup-b.bin", SHA256: "dup", Size: 10},
		{Path: "dup-c.bin", SHA256: "dup", Size: 10},
		{Path: "old.bin", SHA256: "old", Size: 100},
		{Path: "new.bin", SHA256: "new", Size: 7},
		{Path: "gone.log", SHA256: "gone", Size: 5},
	}
	abandoned := []bool{false, false, false, false, false, true}
	deduped := []bool{false, true, true, true, false, false}

	got := summarizeDedup(files, abandoned, deduped)

	if got.DedupedFiles != 3 || got.DedupedBytes != 120 {
		t.Errorf("deduped = %d files/%d bytes, want 3/120", got.DedupedFiles, got.DedupedBytes)
	}
	if got.PreexistingFiles != 1 || got.PreexistingBytes != 100 {
		t.Errorf("preexisting = %d files/%d bytes, want 1/100 (only old.bin)",
			got.PreexistingFiles, got.PreexistingBytes)
	}
	if got.DuplicateFiles() != 2 || got.DuplicateBytes() != 20 {
		t.Errorf("duplicates = %d files/%d bytes, want 2/20",
			got.DuplicateFiles(), got.DuplicateBytes())
	}
}

// Every copy of a repeated file rejected means NGC held it beforehand: a cold
// resource would have accepted exactly one of them.
func TestSummarizeDedupCountsEveryCopyRejectedAsPreexisting(t *testing.T) {
	files := []appFile{
		{Path: "a.bin", SHA256: "same", Size: 10},
		{Path: "b.bin", SHA256: "same", Size: 10},
	}
	got := summarizeDedup(files, []bool{false, false}, []bool{true, true})

	if got.PreexistingFiles != 2 || got.PreexistingBytes != 20 {
		t.Errorf("preexisting = %d files/%d bytes, want 2/20",
			got.PreexistingFiles, got.PreexistingBytes)
	}
	if got.DuplicateFiles() != 0 {
		t.Errorf("duplicates = %d, want 0", got.DuplicateFiles())
	}
}

func TestDropSkipped(t *testing.T) {
	files := []appFile{
		{Path: "a.bin", Size: 10},
		{Path: "b.log", Size: 5},
		{Path: "c.bin", Size: 7},
	}
	var logged strings.Builder
	kept, total := dropSkipped(files, []bool{false, true, false},
		func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) })

	if len(kept) != 2 || kept[0].Path != "a.bin" || kept[1].Path != "c.bin" {
		t.Errorf("kept = %+v, want a.bin and c.bin", kept)
	}
	if total != 17 {
		t.Errorf("total = %d, want 17", total)
	}
	if !strings.Contains(logged.String(), "b.log") {
		t.Errorf("warning should name the skipped file: %q", logged.String())
	}
}
