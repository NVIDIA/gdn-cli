// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file that shrinks after putPartAt's fstat has already accepted it can only
// be caught while the part body is on the wire. This is the end-to-end version
// of TestExactReaderReportsTruncationMidBody: instead of handing exactReader a
// hand-built section that overstates the file, it truncates a real file during
// a real presigned PUT, which is the only way to prove the two detection layers
// are wired the way the upload path needs them - the snapshot check passes, and
// exactReader turns the short body into errFileChanged instead of letting
// net/http's opaque "ContentLength=N with Body length M" reach the user.
func TestUploadFileDetectsTruncationWhileBodyStreams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	// Big enough that the body takes many socket writes, so the truncation
	// lands with most of the part still unread.
	if err := os.WriteFile(path, bytes.Repeat([]byte("z"), 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	f := hashedFile(t, path, "app.log")

	srv, _ := newFakeNGCServer(t, nil)
	defer srv.Close()

	// Truncate from a hook on the client's own socket writes rather than from
	// the handler: the hook runs on the goroutine that is copying the file into
	// the connection, so every remaining read of the file provably happens
	// after the truncation. Truncating from the handler would leave it to
	// kernel buffer sizes whether the client had already drained the file.
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &writeHookConn{Conn: conn, after: 64 << 10, hook: func() {
			if err := os.Truncate(path, 0); err != nil {
				t.Errorf("truncate %q: %v", path, err)
			}
		}}, nil
	}, TLSClientConfig: &tls.Config{RootCAs: testRootCAs, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()

	c := newNGC(srv.URL, "k", testCoords(), &http.Client{Transport: transport})
	_, err := c.uploadFile(context.Background(), f, 1, nil)

	if !errors.Is(err, errFileChanged) {
		t.Fatalf("err = %v, want errFileChanged", err)
	}
	msg := err.Error()
	// "ended after" is exactReader's wording; the fstat snapshot check reports
	// mtimes instead. Asserting on it pins which layer caught the truncation,
	// so the test cannot silently degrade into a duplicate of
	// TestUploadFileDetectsTruncationAfterHashing.
	for _, want := range []string{"app.log", "changed on disk", "ended after"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "ContentLength") {
		t.Errorf("message still exposes the raw net/http error: %q", msg)
	}
}

// writeHookConn runs hook once the connection has carried more than "after"
// bytes, on the caller's goroutine and before that write reaches the socket.
type writeHookConn struct {
	net.Conn
	after   int64
	written int64
	hook    func()
}

func (c *writeHookConn) Write(p []byte) (int, error) {
	c.written += int64(len(p))
	if c.hook != nil && c.written > c.after {
		c.hook()
		c.hook = nil
	}
	return c.Conn.Write(p)
}
