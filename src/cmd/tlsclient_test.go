// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientsEnforceTLSPolicy(t *testing.T) {
	clients := map[string]*http.Client{
		"secure":  newSecureClient(0),
		"shared":  secureClient,
		"api":     newAPIHTTPClient(),
		"part":    newPartHTTPClient(),
		"commit":  commitHTTPClient(newPartHTTPClient()),
		"timeout": newSecureClient(configCheckTimeout),
	}
	for name, hc := range clients {
		tr, ok := hc.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s: transport = %T, want *http.Transport", name, hc.Transport)
		}
		cfg := tr.TLSClientConfig
		if cfg == nil {
			t.Fatalf("%s: TLSClientConfig = nil", name)
		}
		if cfg.MinVersion < tls.VersionTLS12 {
			t.Errorf("%s: MinVersion = %#x, want >= TLS 1.2", name, cfg.MinVersion)
		}
		if cfg.InsecureSkipVerify {
			t.Errorf("%s: InsecureSkipVerify = true", name)
		}
	}
}

func TestClientsRefuseNonHTTPS(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()

	for name, hc := range map[string]*http.Client{
		"secure": newSecureClient(0),
		"api":    newAPIHTTPClient(),
		"part":   newPartHTTPClient(),
	} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		resp, err := hc.Do(req)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%s: plain-HTTP request succeeded", name)
		}
		if !strings.Contains(err.Error(), "non-HTTPS") {
			t.Errorf("%s: err = %v, want non-HTTPS refusal", name, err)
		}
	}
	if hit {
		t.Error("a plain-HTTP request reached the server")
	}
}

// A secure server may not bounce the client onto plain HTTP.
func TestClientsRefuseHTTPSDowngradeRedirect(t *testing.T) {
	var hit bool
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer tlsSrv.Close()

	hc := newSecureClient(0)
	hc.Transport.(*http.Transport).TLSClientConfig.RootCAs = tlsSrv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	resp, err := hc.Get(tlsSrv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("redirect to plain HTTP was followed")
	}
	if hit {
		t.Error("redirect target was contacted")
	}
}

// Certificate validation must not be skippable: an untrusted server fails.
func TestClientsRejectUntrustedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	// The other tests trust the shared httptest certificate through testRootCAs;
	// build this transport by hand so it uses only the system roots.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("request to a server with an untrusted certificate succeeded")
	}
}
