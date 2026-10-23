// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestValidateNGCArtifactName(t *testing.T) {
	valid := []string{"my-app-1", "my-app-2", "sample_app", "v1.0.0"}
	for _, name := range valid {
		if err := validateNGCArtifactName(name); err != nil {
			t.Errorf("validateNGCArtifactName(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{"", "MyApp1", "1app", "App", "bad name"}
	for _, name := range invalid {
		if err := validateNGCArtifactName(name); err == nil {
			t.Errorf("validateNGCArtifactName(%q) = nil, want error", name)
		}
	}
}

func TestAppFileHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := appFile{LocalPath: path, Path: "f.txt"}
	if err := f.hash(context.Background()); err != nil {
		t.Fatal(err)
	}
	// base64(sha256("hello")) is a known, stable value.
	const want = "LPJNul+wow4m6DsqxbninhsWHlwfp0JecwQzYpOLmCQ="
	if f.SHA256 != want || f.Size != 5 {
		t.Errorf("hash recorded %q,%d; want %q,5", f.SHA256, f.Size, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.ModTime.Equal(info.ModTime()) {
		t.Errorf("hash recorded mtime %v, want %v", f.ModTime, info.ModTime())
	}
}

func TestCoordinatesURI(t *testing.T) {
	org := coordinates{Org: "o", Name: "my-app-1", Version: "1.0.0"}
	if got := org.uri(); got != "ngc://o/my-app-1:1.0.0" {
		t.Errorf("org uri = %q", got)
	}
}

func TestPartNumbers(t *testing.T) {
	cases := []struct {
		size, partSize int64
		want           []int32
	}{
		{0, 100, []int32{1}},
		{50, 100, []int32{1}},
		{100, 100, []int32{1}},
		{150, 100, []int32{1, 2}},
		{300, 100, []int32{1, 2, 3}},
	}
	for _, c := range cases {
		if got := partNumbers(c.size, c.partSize); !reflect.DeepEqual(got, c.want) {
			t.Errorf("partNumbers(%d,%d) = %v, want %v", c.size, c.partSize, got, c.want)
		}
	}
}

func TestDefaultPartSize(t *testing.T) {
	if got := defaultPartSize(10); got < minPartSize {
		t.Errorf("part size %d below minimum %d", got, minPartSize)
	}
	if got := defaultPartSize(32 * 1024 * 1024); got != defPartSize {
		t.Errorf("defaultPartSize(32MiB) = %d, want %d", got, defPartSize)
	}
	huge := defPartSize * maxParts * 4
	if n := huge/defaultPartSize(huge) + 1; n > maxParts {
		t.Errorf("part count %d exceeds max %d", n, maxParts)
	}
}

// A commit whose response is lost leaves NGC with the version already marked
// complete, so the retry sees 409. The work the caller asked for is done.
func TestCommitTreatsAlreadyCompletedAsSuccess(t *testing.T) {
	var hits int32
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"requestStatus":{"statusCode":"CONFLICT","statusDescription":"Upload marked as being completed"}}`)
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "n", Version: "v"}, srv.Client())
	if err := c.commit(context.Background(), []appFile{{Path: "a.txt", SHA256: "sum"}}, true); err != nil {
		t.Fatalf("commit = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("server hits = %d, want 1 (409 must not be retried)", got)
	}
}

func TestCommitPropagatesNonConflictErrors(t *testing.T) {
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"requestStatus":{"statusCode":"BAD_REQUEST"}}`)
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "n", Version: "v"}, srv.Client())
	err := c.commit(context.Background(), []appFile{{Path: "a.txt", SHA256: "sum"}}, true)
	if err == nil {
		t.Fatal("commit = nil, want error")
	}
	if !strings.Contains(err.Error(), "commit files") {
		t.Fatalf("commit err = %v, want it to mention commit files", err)
	}
}

// The commit waits on one long server-side operation, so it must not inherit
// the part-upload response-header budget.
func TestCommitHTTPClientWidensResponseHeaderTimeout(t *testing.T) {
	upload := newPartHTTPClient()
	got := commitHTTPClient(upload)
	if got == upload {
		t.Fatal("commitHTTPClient returned the upload client unchanged")
	}
	tr, ok := got.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", got.Transport)
	}
	if tr.ResponseHeaderTimeout != commitResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, commitResponseHeaderTimeout)
	}

	// A client with a foreign transport has no *http.Transport to widen.
	custom := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil })}
	if commitHTTPClient(custom) != custom {
		t.Error("commitHTTPClient replaced a client with a non-*http.Transport")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewPartHTTPClient(t *testing.T) {
	hc := newPartHTTPClient()
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if tr.MaxIdleConns != uploadMaxIdleConns {
		t.Errorf("MaxIdleConns = %d, want %d", tr.MaxIdleConns, uploadMaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != uploadMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", tr.MaxIdleConnsPerHost, uploadMaxIdleConnsPerHost)
	}
	if tr.WriteBufferSize != uploadWriteBufferSize {
		t.Errorf("WriteBufferSize = %d, want %d", tr.WriteBufferSize, uploadWriteBufferSize)
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = true, want false")
	}
	// ALPN must be pinned to http/1.1. Clearing ForceAttemptHTTP2 on a clone of
	// http.DefaultTransport leaves "h2" in NextProtos, and a server that then
	// negotiates HTTP/2 answers with frames the HTTP/1.x reader cannot parse.
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig = nil, want ALPN pinned to http/1.1")
	}
	if got := tr.TLSClientConfig.NextProtos; !reflect.DeepEqual(got, []string{"http/1.1"}) {
		t.Errorf("NextProtos = %v, want [http/1.1]", got)
	}
}

// TestNewAPIHTTPClient guards the other half of the split: NGC metadata calls
// and small files must keep HTTP/2 so many concurrent latency-bound requests
// share one multiplexed connection.
func TestNewAPIHTTPClient(t *testing.T) {
	hc := newAPIHTTPClient()
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want true")
	}
	if tr.TLSClientConfig != nil && len(tr.TLSClientConfig.NextProtos) > 0 {
		if !slices.Contains(tr.TLSClientConfig.NextProtos, "h2") {
			t.Errorf("NextProtos = %v, want h2 advertised", tr.TLSClientConfig.NextProtos)
		}
	}
	if hc.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (this client also streams downloads)", hc.Timeout)
	}
}

// countingTransport records which requests a given client served, so a test can
// assert that a request went out on the intended transport.
type countingTransport struct {
	presignedPUTs atomic.Int32
	apiCalls      atomic.Int32
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut && !strings.Contains(req.URL.Path, "/files/multipart") {
		t.presignedPUTs.Add(1)
	} else {
		t.apiCalls.Add(1)
	}
	return secureClient.Transport.RoundTrip(req)
}

// TestPresignedPUTsNeverUseAPIClient pins the routing invariant: every presigned
// S3 PUT goes on the HTTP/1.1 part client, including single-part files. Sending
// payload over the shared HTTP/2 client would funnel bulk bytes onto one
// connection, where the shared flow-control and congestion windows cap it.
func TestPresignedPUTsNeverUseAPIClient(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  []byte
		partSize int
		wantPUTs int32
	}{
		{"single part", []byte("some bytes"), 0, 1},
		{"multipart", bytes.Repeat([]byte("x"), 12), 5, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.bin")
			if err := os.WriteFile(path, tc.payload, 0o644); err != nil {
				t.Fatal(err)
			}
			f := appFile{LocalPath: path, Path: "f.bin"}
			if err := f.hash(context.Background()); err != nil {
				t.Fatal(err)
			}

			var initiated bool
			var srv *httptest.Server
			srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && !initiated:
					initiated = true
					writeJSON(w, fmt.Sprintf(`{"uploadID":"up-1","partSize":%d}`, tc.partSize))
				case r.Method == http.MethodPost:
					step := tc.partSize
					if step <= 0 {
						step = len(tc.payload)
					}
					var urls []string
					for i := 0; i < len(tc.payload); i += step {
						urls = append(urls, `"`+srv.URL+`/part?n=`+fmt.Sprint(i)+`"`)
					}
					writeJSON(w, `{"uploadID":"up-1","urls":[`+strings.Join(urls, ",")+`]}`)
				case r.Method == http.MethodPut && r.URL.Path == "/part":
					w.Header().Set("ETag", `"etag-1"`)
				default:
					writeJSON(w, `{}`)
				}
			}))
			defer srv.Close()

			api, part := &countingTransport{}, &countingTransport{}
			c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "n", Version: "1.0.0"},
				&http.Client{Transport: api})
			c.partHTTP = &http.Client{Transport: part}

			if _, err := c.uploadFile(context.Background(), f, 2, nil); err != nil {
				t.Fatalf("uploadFile: %v", err)
			}
			if got := api.presignedPUTs.Load(); got != 0 {
				t.Errorf("API client served %d presigned PUT(s), want 0", got)
			}
			if got := part.presignedPUTs.Load(); got != tc.wantPUTs {
				t.Errorf("part client served %d presigned PUT(s), want %d", got, tc.wantPUTs)
			}
			if got := part.apiCalls.Load(); got != 0 {
				t.Errorf("part client served %d NGC API call(s), want 0", got)
			}
			if got := api.apiCalls.Load(); got == 0 {
				t.Error("API client served no NGC metadata calls")
			}
		})
	}
}

// TestNewNGCForUploadSplitsClients pins the wiring: distinct clients, with the
// part client used for presigned PUTs.
func TestNewNGCForUploadSplitsClients(t *testing.T) {
	c := newNGCForUpload("https://example.invalid", "k", coordinates{Org: "o", Type: "RESOURCE"})
	if c.http == nil || c.partHTTP == nil {
		t.Fatal("both clients must be set")
	}
	if c.http == c.partHTTP {
		t.Error("API and part clients must be distinct")
	}
	if c.partClient() != c.partHTTP {
		t.Error("partClient() should return partHTTP when set")
	}
	// Falling back keeps older single-client callers working.
	plain := newNGC("https://example.invalid", "k", coordinates{}, secureClient)
	if plain.partClient() != secureClient {
		t.Error("partClient() should fall back to the API client when partHTTP is nil")
	}
}

func TestIsConflict(t *testing.T) {
	for _, s := range []string{"CONFLICT: already exists", "409 something", "File with the same checksum already exists"} {
		if !isConflict(errString(s)) {
			t.Errorf("isConflict(%q) = false, want true", s)
		}
	}
	if isConflict(errString("404 not found")) {
		t.Error("404 should not be a conflict")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestUploadFile exercises the full multipart flow against a fake NGC + S3.
func TestUploadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "my-app-1.bin")
	if err := os.WriteFile(path, []byte("some bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := hashedFile(t, path, "my-app-1.bin")

	var steps struct{ initiated, urls, put, completed bool }
	var srv *httptest.Server
	srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart" && !steps.initiated:
			steps.initiated = true
			writeJSON(w, `{"uploadID":"up-1","partSize":0}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart":
			steps.urls = true
			writeJSON(w, `{"uploadID":"up-1","urls":["`+srv.URL+`/part?n=1"]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/part":
			steps.put = true
			w.Header().Set("ETag", `"etag-1"`)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/org/o/files/multipart":
			steps.completed = true
			writeJSON(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}, secureClient)
	if _, err := c.uploadFile(context.Background(), f, 1, nil); err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if !steps.initiated || !steps.urls || !steps.put || !steps.completed {
		t.Errorf("missing step: %+v", steps)
	}
}

// NGC answers the initiate call with a presigned URL for every part, so the
// upload must spend one POST per file rather than two. On a tree of tens of
// thousands of small files that round-trip is the dominant cost, so this is
// pinned rather than left to chance.
func TestUploadFileReusesInitiateURLs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.bin")
	payload := bytes.Repeat([]byte("x"), 12)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	f := hashedFile(t, path, "large.bin")

	var posts, puts atomic.Int32
	var srv *httptest.Server
	srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart":
			posts.Add(1)
			// partSize 5 over 12 bytes is 3 parts, and every one comes back here.
			writeJSON(w, `{"uploadID":"up-1","partSize":5,"urls":["`+
				srv.URL+`/part?n=1","`+srv.URL+`/part?n=2","`+srv.URL+`/part?n=3"]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/part":
			puts.Add(1)
			w.Header().Set("ETag", `"etag-1"`)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/org/o/files/multipart":
			writeJSON(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", testCoords(), secureClient)
	if _, err := c.uploadFile(context.Background(), f, 2, nil); err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("POSTs to files/multipart = %d, want 1 (initiate already carried the URLs)", got)
	}
	if got := puts.Load(); got != 3 {
		t.Errorf("part PUTs = %d, want 3", got)
	}
}

// A response that does not cover every part still has to work: partNumberList
// is how a subset gets requested, so the second call remains the fallback.
func TestUploadFileFallsBackWhenInitiateURLsIncomplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.bin")
	payload := bytes.Repeat([]byte("x"), 12)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	f := hashedFile(t, path, "large.bin")

	var posts atomic.Int32
	var srv *httptest.Server
	srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart":
			// One URL for a three-part file: too few to use, so a second call
			// with an explicit partNumberList has to follow.
			if posts.Add(1) == 1 {
				writeJSON(w, `{"uploadID":"up-1","partSize":5,"urls":["`+srv.URL+`/part?n=1"]}`)
				return
			}
			var got multipartBody
			_ = json.NewDecoder(r.Body).Decode(&got)
			if len(got.PartNumberList) != 3 {
				t.Errorf("partNumberList = %v, want 3 entries", got.PartNumberList)
			}
			writeJSON(w, `{"uploadID":"up-1","urls":["`+
				srv.URL+`/part?n=1","`+srv.URL+`/part?n=2","`+srv.URL+`/part?n=3"]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/part":
			w.Header().Set("ETag", `"etag-1"`)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/org/o/files/multipart":
			writeJSON(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", testCoords(), secureClient)
	if _, err := c.uploadFile(context.Background(), f, 2, nil); err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if got := posts.Load(); got != 2 {
		t.Errorf("POSTs to files/multipart = %d, want 2 (fallback must engage)", got)
	}
}

func TestUploadFileMultiPartParallel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.bin")
	payload := bytes.Repeat([]byte("x"), 12)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	f := hashedFile(t, path, "large.bin")

	var concurrent int32
	var peak int32
	var steps struct{ initiated, urls, completed bool }

	var srv *httptest.Server
	srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart" && !steps.initiated:
			steps.initiated = true
			writeJSON(w, `{"uploadID":"up-1","partSize":5}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart":
			steps.urls = true
			writeJSON(w, `{"uploadID":"up-1","urls":["`+srv.URL+`/part?n=1","`+srv.URL+`/part?n=2","`+srv.URL+`/part?n=3"]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/part":
			cur := atomic.AddInt32(&concurrent, 1)
			for {
				old := atomic.LoadInt32(&peak)
				if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&concurrent, -1)
			w.Header().Set("ETag", `"etag"`)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/org/o/files/multipart":
			steps.completed = true
			writeJSON(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}, secureClient)
	if _, err := c.uploadFile(context.Background(), f, 3, nil); err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if !steps.initiated || !steps.urls || !steps.completed {
		t.Errorf("missing step: %+v", steps)
	}
	if peak < 2 {
		t.Fatalf("expected parallel part uploads, peak concurrency = %d", peak)
	}
}

func TestUploadFileAlreadyPresent(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"checksum dedup on initiate": {http.StatusBadRequest, `{"x":"File with the same checksum already exists"}`},
		"upload in progress":         {http.StatusConflict, `{"detail":"Upload marked as being completed"}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}, secureClient)
			if _, err := c.uploadFile(context.Background(), appFile{Path: "my-app-1.bin", Size: 1, SHA256: "x"}, 1, nil); err != nil {
				t.Errorf("should be a no-op, got: %v", err)
			}
		})
	}
}

func TestUploadFileAlreadyPresentOnGetURLs(t *testing.T) {
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["partNumberList"]; !ok {
				writeJSON(w, `{"uploadID":"up-1","partSize":0}`)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"File with the same checksum already exists"}`))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}, secureClient)
	if _, err := c.uploadFile(context.Background(), appFile{Path: "my-app-1.bin", Size: 1, SHA256: "x"}, 1, nil); err != nil {
		t.Errorf("should be a no-op, got: %v", err)
	}
}

// TestEnsureArtifact creates the resource then the version, tolerating 409s.
func TestEnsureArtifact(t *testing.T) {
	var hits []string
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"statusCode":"CONFLICT"}`))
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}, secureClient)
	if err := c.ensureArtifact(context.Background(), "My App", "desc"); err != nil {
		t.Fatalf("ensureArtifact: %v", err)
	}
	want := []string{"/v2/org/o/resources", "/v2/org/o/resources/my-app-1/versions"}
	if !reflect.DeepEqual(hits, want) {
		t.Errorf("paths = %v, want %v", hits, want)
	}
}

// TestDeleteResource covers both the single-version and whole-resource deletes.
func TestDeleteResource(t *testing.T) {
	var gotMethod, gotPath string
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}, secureClient)

	if err := c.deleteVersion(context.Background()); err != nil {
		t.Fatalf("deleteVersion: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v2/org/o/resources/my-app-1/versions/1.0.0" {
		t.Errorf("deleteVersion: got %s %s", gotMethod, gotPath)
	}

	if err := c.deleteResource(context.Background()); err != nil {
		t.Fatalf("deleteResource: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v2/org/o/resources/my-app-1" {
		t.Errorf("deleteResource: got %s %s", gotMethod, gotPath)
	}
}

// TestListResources walks paginated recipe pages until totalPages is reached.
func TestListResources(t *testing.T) {
	var pages []string
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/org/o/resources" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		pages = append(pages, r.URL.Query().Get("page-number"))
		switch r.URL.Query().Get("page-number") {
		case "0":
			writeJSON(w, `{"recipes":[{"name":"app-a","displayName":"App A"},{"name":"app-b"}],"paginationInfo":{"totalPages":2}}`)
		case "1":
			writeJSON(w, `{"recipes":[{"name":"app-c"}],"paginationInfo":{"totalPages":2}}`)
		default:
			t.Errorf("requested extra page %q", r.URL.Query().Get("page-number"))
			writeJSON(w, `{"recipes":[]}`)
		}
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE"}, secureClient)
	got, err := c.listResources(context.Background())
	if err != nil {
		t.Fatalf("listResources: %v", err)
	}
	names := []string{}
	for _, r := range got {
		names = append(names, r.Name)
	}
	want := []string{"app-a", "app-b", "app-c"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if !reflect.DeepEqual(pages, []string{"0", "1"}) {
		t.Errorf("requested pages = %v, want [0 1]", pages)
	}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestIsRetryableTransferErr(t *testing.T) {
	retryable := []error{
		fmt.Errorf(`Put "https://s3.example/file": %w`, net.ErrClosed),
		fmt.Errorf(`write tcp: %w`, syscall.ECONNRESET),
		fmt.Errorf(`write tcp: %w`, syscall.EPIPE),
		fmt.Errorf(`dial tcp: %w`, syscall.ECONNREFUSED),
		&transferStatusError{Method: "PUT", Code: 503, Status: "503 Service Unavailable", Body: "slow down"},
		&transferStatusError{Method: "GET", Code: 429, Status: "429 Too Many Requests", Body: "rate limit"},
		fmt.Errorf(`Put "https://s3.example/file": %w`, io.EOF),
		fmt.Errorf(`Put "https://s3.example/file": %w`, io.ErrUnexpectedEOF),
		&net.OpError{Op: "read", Err: timeoutError{}},
	}
	for _, err := range retryable {
		if !isRetryableTransferErr(err) {
			t.Errorf("isRetryableTransferErr(%v) = false, want true", err)
		}
	}
	notRetryable := []error{
		nil,
		&transferStatusError{Method: "GET", Code: 403, Status: "403 Forbidden", Body: "access denied"},
		context.Canceled,
		errors.New("open file: permission denied"),
	}
	for _, err := range notRetryable {
		if isRetryableTransferErr(err) {
			t.Errorf("isRetryableTransferErr(%v) = true, want false", err)
		}
	}
}

// timeoutError implements net.Error reporting a timeout, mirroring what the
// net stack returns for i/o and handshake timeouts.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

type flakyUploadTransport struct {
	attempts int32
	fails    int
	inner    http.RoundTripper
}

func (t *flakyUploadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&t.attempts, 1)
	if t.fails > 0 {
		t.fails--
		return nil, &net.OpError{
			Op:   "write",
			Net:  "tcp",
			Addr: &net.TCPAddr{IP: net.IPv4(16, 12, 96, 186), Port: 443},
			Err:  syscall.ECONNRESET,
		}
	}
	if t.inner == nil {
		t.inner = secureClient.Transport
	}
	return t.inner.RoundTrip(req)
}

func TestPutPartRetriesTransientNetworkError(t *testing.T) {
	t.Parallel()
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"etag-1"`)
	}))
	defer srv.Close()

	transport := &flakyUploadTransport{fails: 2}
	c := newNGC("", "k", coordinates{}, &http.Client{Transport: transport})
	etag, err := c.putPart(context.Background(), c.partClient(), srv.URL, bytes.NewReader([]byte("data")), 4)
	if err != nil {
		t.Fatalf("putPart: %v", err)
	}
	if etag != "etag-1" {
		t.Fatalf("etag = %q, want etag-1", etag)
	}
	if got := atomic.LoadInt32(&transport.attempts); got != 3 {
		t.Fatalf("transport attempts = %d, want 3 (2 failures + 1 success)", got)
	}
}

// TestPutPartProgressUnwindsFailedAttempt covers the retry accounting: progress
// is reported as the body reaches the wire, so a failed attempt that already
// streamed bytes must subtract them back out or the total would overshoot.
func TestPutPartProgressUnwindsFailedAttempt(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if atomic.AddInt32(&hits, 1) == 1 {
			// Body accepted, then the connection dies before any response.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.Header().Set("ETag", `"etag-1"`)
	}))
	defer srv.Close()

	payload := bytes.Repeat([]byte("x"), 4096)
	var total atomic.Int64
	c := newNGC(srv.URL, "k", coordinates{}, secureClient)
	c.progress = func(n int64) { total.Add(n) }

	etag, err := c.putPart(context.Background(), c.partClient(), srv.URL,
		bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("putPart: %v", err)
	}
	if etag != "etag-1" {
		t.Fatalf("etag = %q, want etag-1", etag)
	}
	if got := atomic.LoadInt32(&hits); got < 2 {
		t.Fatalf("server hits = %d, want at least 2 (a retry must have happened)", got)
	}
	if got, want := total.Load(), int64(len(payload)); got != want {
		t.Errorf("progress total = %d, want %d (the failed attempt must be unwound)", got, want)
	}
}

// TestUploadFileProgressCountsSkippedFile checks that a file NGC already has
// still advances progress; otherwise the percentage would stall on re-runs.
func TestUploadFileProgressCountsSkippedFile(t *testing.T) {
	t.Parallel()
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"statusCode":"CONFLICT"}`))
	}))
	defer srv.Close()

	var total atomic.Int64
	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}, secureClient)
	c.progress = func(n int64) { total.Add(n) }

	f := appFile{Path: "my-app-1.bin", Size: 1234, SHA256: "x"}
	if _, err := c.uploadFile(context.Background(), f, 1, nil); err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if got := total.Load(); got != f.Size {
		t.Errorf("progress total = %d, want %d", got, f.Size)
	}
}

func TestPutPartRetriesOnStall(t *testing.T) {
	t.Parallel()
	var hits int32
	release := make(chan struct{})
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			<-release // first attempt: wedge until released so the watchdog fires
			return
		}
		w.Header().Set("ETag", `"etag-1"`)
	}))
	defer srv.Close()
	defer close(release)

	c := newNGC(srv.URL, "k", coordinates{}, secureClient)
	c.stallTimeout = 100 * time.Millisecond
	etag, err := c.putPart(context.Background(), c.partClient(), srv.URL, bytes.NewReader([]byte("payload")), 7)
	if err != nil {
		t.Fatalf("putPart: %v", err)
	}
	if etag != "etag-1" {
		t.Fatalf("etag = %q, want etag-1", etag)
	}
	if got := atomic.LoadInt32(&hits); got < 2 {
		t.Fatalf("server hits = %d, want >= 2 (stall + retry)", got)
	}
}

func TestPutPartRespectsCallerCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the response open until the test tears down
	}))
	defer srv.Close()
	defer close(release)

	c := newNGC(srv.URL, "k", coordinates{}, secureClient)
	c.stallTimeout = time.Hour // ensure the watchdog is not what stops us
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := c.putPart(ctx, c.partClient(), srv.URL, bytes.NewReader([]byte("payload")), 7)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("putPart err = %v, want context.DeadlineExceeded", err)
	}
}

// Presigned part URLs sign over content-length, so an empty file must be sent
// with an explicit "Content-Length: 0" instead of chunked encoding, or S3
// answers 403 SignatureDoesNotMatch.
func TestUploadFileEmptyFileSendsContentLengthZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.lock")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f := hashedFile(t, path, "empty.lock")
	if f.Size != 0 {
		t.Fatalf("fixture size = %d, want 0", f.Size)
	}

	var gotContentLength string
	var gotTransferEncoding []string
	var srv *httptest.Server
	srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["partNumberList"]; !ok {
				writeJSON(w, `{"uploadID":"up-1","partSize":0}`)
				return
			}
			writeJSON(w, `{"uploadID":"up-1","urls":["`+srv.URL+`/part?n=1"]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/part":
			gotContentLength = r.Header.Get("Content-Length")
			gotTransferEncoding = r.TransferEncoding
			w.Header().Set("ETag", `"etag-1"`)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/org/o/files/multipart":
			writeJSON(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := newNGC(srv.URL, "k", coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}, secureClient)
	if _, err := c.uploadFile(context.Background(), f, 1, nil); err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	if gotContentLength != "0" {
		t.Errorf("Content-Length header = %q, want \"0\"", gotContentLength)
	}
	if len(gotTransferEncoding) != 0 {
		t.Errorf("Transfer-Encoding = %v, want none", gotTransferEncoding)
	}
}

// newFakeNGCServer serves the multipart initiate/urls/put/complete sequence and
// counts how many multipart sessions were initiated, so tests can tell a
// re-hashed retry (new session) from a part-level retry.
func newFakeNGCServer(t *testing.T, onInitiate func()) (*httptest.Server, *int32) {
	t.Helper()
	var initiations int32
	var srv *httptest.Server
	srv = newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/org/o/files/multipart":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["partNumberList"]; !ok {
				atomic.AddInt32(&initiations, 1)
				if onInitiate != nil {
					onInitiate()
				}
				writeJSON(w, `{"uploadID":"up-1","partSize":0}`)
				return
			}
			writeJSON(w, `{"uploadID":"up-1","urls":["`+srv.URL+`/part?n=1"]}`)
		case r.Method == http.MethodPut && r.URL.Path == "/part":
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("ETag", `"etag-1"`)
		case r.Method == http.MethodPut && r.URL.Path == "/v2/org/o/files/multipart":
			writeJSON(w, `{}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	return srv, &initiations
}

func testCoords() coordinates {
	return coordinates{Org: "o", Type: "RESOURCE", Name: "my-app-1", Version: "1.0.0"}
}

// hashedFile describes path as it exists right now, the way a scan would.
// Tests that then mutate the file on disk leave the returned snapshot stale,
// which is exactly the condition the upload is meant to detect.
func hashedFile(t *testing.T, path, name string) appFile {
	t.Helper()
	f := appFile{LocalPath: path, Path: name}
	if err := f.hash(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

// A file truncated after hashing must fail with a clear errFileChanged rather
// than net/http's opaque "ContentLength=N with Body length M".
func TestUploadFileDetectsTruncationAfterHashing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	f := hashedFile(t, path, "server.log")

	// The app exits and truncates its log between the hash pass and the upload.
	if err := os.WriteFile(path, []byte("restarted"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, _ := newFakeNGCServer(t, nil)
	defer srv.Close()

	c := newNGC(srv.URL, "k", testCoords(), secureClient)
	_, err := c.uploadFile(context.Background(), f, 1, nil)

	if !errors.Is(err, errFileChanged) {
		t.Fatalf("err = %v, want errFileChanged", err)
	}
	msg := err.Error()
	for _, want := range []string{"server.log", "changed on disk", "4.00 KiB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "ContentLength") {
		t.Errorf("message still exposes the raw net/http error: %q", msg)
	}
}

// An in-place rewrite that preserves the size must still be caught, since the
// committed checksum would otherwise not describe the uploaded bytes.
func TestUploadFileDetectsSameSizeRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(path, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := hashedFile(t, path, "config.ini")

	if err := os.WriteFile(path, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Guarantee a distinct mtime even on coarse-granularity filesystems.
	newMod := f.ModTime.Add(2 * time.Second)
	if err := os.Chtimes(path, newMod, newMod); err != nil {
		t.Fatal(err)
	}

	srv, _ := newFakeNGCServer(t, nil)
	defer srv.Close()

	c := newNGC(srv.URL, "k", testCoords(), secureClient)
	_, err := c.uploadFile(context.Background(), f, 1, nil)

	if !errors.Is(err, errFileChanged) {
		t.Fatalf("err = %v, want errFileChanged", err)
	}
	// The sizes match, so only the differing mtimes can explain the mismatch.
	if !strings.Contains(err.Error(), newMod.Format(time.RFC3339)) {
		t.Errorf("message should report the new mtime: %q", err.Error())
	}
}

// A file that shrinks after the pre-upload check (so the truncation is only
// visible once the body is being read) must also report errFileChanged.
func TestExactReaderReportsTruncationMidBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing.log")
	if err := os.WriteFile(path, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	// Claim 100 bytes when only 5 exist, mimicking a stat that raced the write.
	body := &exactReader{r: io.NewSectionReader(file, 0, 100), path: "growing.log", size: 100, left: 100}
	if _, err = io.ReadAll(body); !errors.Is(err, errFileChanged) {
		t.Fatalf("err = %v, want errFileChanged", err)
	}
}

// exactReader must stay seekable so ordinary transient part retries still work.
func TestExactReaderRemainsSeekableForRetries(t *testing.T) {
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("ETag", `"etag-1"`)
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "data.bin")
	payload := bytes.Repeat([]byte("y"), 64)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	f := hashedFile(t, path, "data.bin")

	transport := &flakyUploadTransport{fails: 2, inner: secureClient.Transport}
	c := newNGC("", "k", testCoords(), &http.Client{Transport: transport})
	etag, err := c.putPartAt(context.Background(), c.partClient(), f, srv.URL, 0, f.Size)
	if err != nil {
		t.Fatalf("putPartAt: %v", err)
	}
	if etag != "etag-1" {
		t.Fatalf("etag = %q, want etag-1", etag)
	}
	if got := atomic.LoadInt32(&transport.attempts); got != 3 {
		t.Fatalf("attempts = %d, want 3 (2 transient failures then success)", got)
	}
}

func TestPutPartDoesNotRetryClientError(t *testing.T) {
	transport := &flakyUploadTransport{}
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "access denied", http.StatusForbidden)
	}))
	defer srv.Close()
	transport.inner = secureClient.Transport

	c := newNGC("", "k", coordinates{}, &http.Client{Transport: transport})
	_, err := c.putPart(context.Background(), c.partClient(), srv.URL, bytes.NewReader([]byte("data")), 4)
	if err == nil {
		t.Fatal("putPart: expected error")
	}
	if got := atomic.LoadInt32(&transport.attempts); got != 1 {
		t.Fatalf("transport attempts = %d, want 1 (no retry on 403)", got)
	}
}
