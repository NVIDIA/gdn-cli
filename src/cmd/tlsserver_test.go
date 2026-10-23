// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
)

// The CLI refuses plain HTTP, so tests that drive a real client need an HTTPS
// server. Every httptest TLS server presents the same built-in certificate, so
// trust it once here, before any test starts, instead of per server: mutating
// these package globals from a test races with goroutines left over from the
// previous one.
func init() {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	testRootCAs = x509.NewCertPool()
	testRootCAs.AddCert(srv.Certificate())
	secureClient = newSecureClient(0)
}

// newTLSTestServer starts an HTTPS test server that the CLI's hardened clients
// trust.
func newTLSTestServer(h http.Handler) *httptest.Server {
	return httptest.NewTLSServer(h)
}
