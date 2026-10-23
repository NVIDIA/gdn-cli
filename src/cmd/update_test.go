// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseFileSpec(t *testing.T) {
	local, remote, err := parseFileSpec("./install.cmd")
	if err != nil || local != "./install.cmd" || remote != "install.cmd" {
		t.Fatalf("basename: got %q %q %v", local, remote, err)
	}
	local, remote, err = parseFileSpec("/tmp/a.exe=bin/a.exe")
	if err != nil || local != "/tmp/a.exe" || remote != "bin/a.exe" {
		t.Fatalf("remap: got %q %q %v", local, remote, err)
	}
	if _, _, err := parseFileSpec("=/x"); err == nil {
		t.Fatal("expected error for empty local")
	}
	if _, _, err := parseFileSpec("a=../b"); err == nil {
		t.Fatal("expected error for .. in remote")
	}
}

func TestValidateBaseVersion(t *testing.T) {
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/versions") {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, `{"recipeVersions":[{"versionId":"1.0.0","status":"UPLOAD_COMPLETE","totalFileCount":3},{"versionId":"pending","status":"UPLOAD_PENDING","totalFileCount":1}],"paginationInfo":{"totalPages":1}}`)
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "n", Version: "2.0.0"}, srv.Client())
	if err := c.validateBaseVersion(context.Background(), "1.0.0"); err != nil {
		t.Fatalf("valid base: %v", err)
	}
	if err := c.validateBaseVersion(context.Background(), "pending"); err == nil || !strings.Contains(err.Error(), "UPLOAD_COMPLETE") {
		t.Fatalf("pending base err = %v", err)
	}
	if err := c.validateBaseVersion(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing base err = %v", err)
	}
	if err := c.validateBaseVersion(context.Background(), "org/n:1.0.0"); err == nil {
		t.Fatal("expected error for qualified base version")
	}
}

func TestCommitFilesSendsBaseVersionAndUploadPending(t *testing.T) {
	var gotBody map[string]any
	var gotQuery string
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("body: %v", err)
		}
		writeJSON(w, `{}`)
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "n", Version: "2.0.0"}, srv.Client())
	if err := c.commitFiles(context.Background(), commitOpts{
		BaseVersion:   "1.0.0",
		UploadPending: true,
	}); err != nil {
		t.Fatalf("commitFiles: %v", err)
	}
	if gotQuery != "upload-pending=true" {
		t.Errorf("query = %q, want upload-pending=true", gotQuery)
	}
	if gotBody["baseVersion"] != "1.0.0" {
		t.Errorf("baseVersion = %v", gotBody["baseVersion"])
	}
	files, _ := gotBody["files"].([]any)
	if len(files) != 0 {
		t.Errorf("files = %v, want empty", files)
	}
}

func TestMarkUploadComplete(t *testing.T) {
	var method, path, query string
	var body map[string]any
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query = r.Method, r.URL.Path, r.URL.RawQuery
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeJSON(w, `{}`)
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "n", Version: "2.0.0"}, srv.Client())
	if err := c.markUploadComplete(context.Background(), true); err != nil {
		t.Fatalf("markUploadComplete: %v", err)
	}
	if method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", method)
	}
	if !strings.HasSuffix(path, "/resources/n/versions/2.0.0") {
		t.Errorf("path = %s", path)
	}
	if query != "set-latest=true" {
		t.Errorf("query = %q", query)
	}
	if body["status"] != "UPLOAD_COMPLETE" {
		t.Errorf("body = %v", body)
	}
}

func TestUpdateToNGCInheritsThenCommitsThenFinalizes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "install.cmd")
	if err := os.WriteFile(path, []byte("@echo off\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		listedVersions int32
		commits        []map[string]any
		commitQueries  []string
		patched        bool
		steps          struct{ initiated, urls bool }
	)
	var srv *httptest.Server
	srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/versions") && !strings.Contains(r.URL.Path, "/files"):
			atomic.AddInt32(&listedVersions, 1)
			writeJSON(w, `{"recipeVersions":[{"versionId":"1.0.0","status":"UPLOAD_COMPLETE","totalFileCount":10}],"paginationInfo":{"totalPages":1}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resources"):
			w.WriteHeader(http.StatusConflict)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/versions"):
			w.WriteHeader(http.StatusCreated)
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
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			commits = append(commits, body)
			commitQueries = append(commitQueries, r.URL.RawQuery)
			writeJSON(w, `{}`)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/versions/1.0.1"):
			patched = true
			if r.URL.Query().Get("set-latest") != "true" {
				t.Errorf("PATCH query = %s", r.URL.RawQuery)
			}
			writeJSON(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	ngc := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.1"}, secureClient)
	res, err := updateToNGC(context.Background(), ngc, updateOptions{
		BaseVersion: "1.0.0",
		Files:       []string{path},
		SetLatest:   true,
	}, nil)
	if err != nil {
		t.Fatalf("updateToNGC: %v", err)
	}
	if atomic.LoadInt32(&listedVersions) < 1 {
		t.Fatal("expected base version list")
	}
	if len(commits) != 2 {
		t.Fatalf("commits = %d, want 2 (inherit then additions)", len(commits))
	}
	if commits[0]["baseVersion"] != "1.0.0" {
		t.Errorf("first commit baseVersion = %v", commits[0]["baseVersion"])
	}
	if files, _ := commits[0]["files"].([]any); len(files) != 0 {
		t.Errorf("first commit files = %v, want empty", files)
	}
	if commitQueries[0] != "upload-pending=true" || commitQueries[1] != "upload-pending=true" {
		t.Errorf("commit queries = %v", commitQueries)
	}
	addFiles, _ := commits[1]["files"].([]any)
	if len(addFiles) != 1 {
		t.Fatalf("second commit files = %v", addFiles)
	}
	if !patched {
		t.Fatal("expected PATCH UPLOAD_COMPLETE")
	}
	if len(res.Files) != 1 || res.Files[0].Path != "install.cmd" {
		t.Errorf("result files = %+v", res.Files)
	}
}

func TestCollectUpdateFilesRejectsPathConflict(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "install.cmd"), []byte("from-dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "install.cmd")
	if err := os.WriteFile(other, []byte("from-file"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := collectUpdateFiles(context.Background(), updateOptions{
		Dir:   dir,
		Files: []string{other},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate remote path") {
		t.Fatalf("err = %v, want duplicate remote path", err)
	}
}

func TestRunUpdateResourceDryRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.cmd")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runUpdateResource([]string{
		"--name", "my-app-1",
		"--base-version", "1.0.0",
		"--version", "1.0.1",
		"--file", path,
		"--org", "dry-org",
		"--dry-run",
	})
	if err != nil {
		t.Fatalf("runUpdateResource: %v", err)
	}
}
