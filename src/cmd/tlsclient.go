// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Every backend request goes through a client built here. The policy is fixed
// and has no override: HTTPS only, certificates always verified, TLS 1.2 or
// newer. Nothing in this package sets InsecureSkipVerify or lowers MinVersion,
// and no flag, environment variable or config key can.

// testRootCAs, when non-nil, is added as the trusted roots of every hardened
// transport. Only tests set it, to trust the certificate of an httptest TLS
// server; nothing in a release build can reach it, and it never disables
// verification.
var testRootCAs *x509.CertPool

// secureClient is the shared client for callers that need no custom tuning. It
// has no Timeout because some of its callers stream.
var secureClient = newSecureClient(0)

// newSecureClient returns a client over a hardened clone of the default
// transport.
func newSecureClient(timeout time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	hardenTransport(t)
	return &http.Client{Transport: t, Timeout: timeout}
}

// hardenTransport enforces the TLS policy on t. It must be applied to every
// transport the CLI builds. It leaves ALPN settings alone.
func hardenTransport(t *http.Transport) {
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.MinVersion = tls.VersionTLS12
	t.TLSClientConfig.InsecureSkipVerify = false
	if testRootCAs != nil {
		t.TLSClientConfig.RootCAs = testRootCAs
	}

	// The transport consults Proxy for every request, redirect hops included, and
	// aborts the request when it errors. That makes it the one choke point that
	// sees every URL, including presigned storage URLs and Location headers
	// returned by the server, without wrapping the transport.
	proxy := t.Proxy
	t.Proxy = func(r *http.Request) (*url.URL, error) {
		if err := requireHTTPS(r.URL); err != nil {
			return nil, err
		}
		if proxy != nil {
			return proxy(r)
		}
		return nil, nil
	}
}

// requireHTTPS rejects any URL that is not an https URL with a host.
func requireHTTPS(u *url.URL) error {
	if u == nil || u.Scheme != "https" || u.Host == "" {
		shown := "<nil>"
		if u != nil {
			shown = u.Redacted()
		}
		return fmt.Errorf("refusing non-HTTPS URL %q: all GDN traffic must use HTTPS (TLS 1.2 or newer)", shown)
	}
	return nil
}
