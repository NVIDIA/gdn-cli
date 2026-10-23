// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrgDisplayName(t *testing.T) {
	var gotPath, gotAuth string
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		writeJSON(w, `{"organizations":{"id":42,"name":"1234567890","displayName":"GDN Staging"}}`)
	}))
	defer srv.Close()

	name, err := newOrgClient(srv.URL, "secret", "1234567890", secureClient).
		displayName(context.Background())
	if err != nil {
		t.Fatalf("displayName: %v", err)
	}
	if gotPath != "/v2/orgs/1234567890" {
		t.Errorf("path = %q, want /v2/orgs/1234567890", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth = %q", gotAuth)
	}
	if name != "GDN Staging" {
		t.Errorf("displayName = %q, want GDN Staging", name)
	}
}

// "organizations" is a single object despite the plural key; a response with
// no usable name yields "" rather than an error.
func TestOrgDisplayNameResponseShapes(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"object", `{"organizations":{"displayName":"My Org"}}`, "My Org"},
		{"missing key", `{"requestStatus":{"statusCode":"SUCCESS"}}`, ""},
		{"no display name", `{"organizations":{"name":"1234567890"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, tc.body)
			}))
			defer srv.Close()

			got, err := newOrgClient(srv.URL, "k", "1234567890", secureClient).
				displayName(context.Background())
			if err != nil {
				t.Fatalf("displayName: %v", err)
			}
			if got != tc.want {
				t.Errorf("displayName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOrgFullName(t *testing.T) {
	if got := orgFullName("1234567890", "GDN Staging"); got != "1234567890 (GDN Staging)" {
		t.Errorf("got %q, want 1234567890 (GDN Staging)", got)
	}
	// Nothing to add when the name is unknown or identical to the org.
	if got := orgFullName("1234567890", ""); got != "1234567890" {
		t.Errorf("got %q, want 1234567890", got)
	}
	if got := orgFullName("my-org", "my-org"); got != "my-org" {
		t.Errorf("got %q, want my-org", got)
	}
}

// A failed lookup must not fail the command that asked for it.
func TestLookupOrgDisplayNameIsBestEffort(t *testing.T) {
	var requests int
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if got := lookupOrgDisplayName(context.Background(), srv.URL, "k", "1234567890"); got != "" {
		t.Errorf("got %q, want empty on error", got)
	}
	// No key means nothing to authenticate with, so no request is attempted.
	if got := lookupOrgDisplayName(context.Background(), srv.URL, "", "1234567890"); got != "" {
		t.Errorf("got %q, want empty without an API key", got)
	}
	if requests != 1 {
		t.Errorf("server saw %d requests, want 1 (the keyless lookup must not call out)", requests)
	}
}

// Once cached, the name renders from the config file alone: no key, no request.
func TestConfigListRendersCachedNameOffline(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("GDN_CONFIG_FILE", cfgFile)
	if err := os.WriteFile(cfgFile, []byte(
		"default_org = \"1234567890\"\n\n[org.1234567890]\ndisplay_name = \"my-org\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runConfigList(nil); err != nil {
			t.Fatalf("runConfigList: %v", err)
		}
	})
	if !strings.Contains(out, "1234567890 (my-org)") {
		t.Errorf("config list = %q, want the org labeled with its display name", out)
	}
}

// `config set` refreshes on every run that has a key, not only when the key
// itself changes.
func TestConfigSetRefreshesDisplayName(t *testing.T) {
	current := "my-org"
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"organizations":{"displayName":"`+current+`"}}`)
	}))
	defer srv.Close()

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("GDN_CONFIG_FILE", cfgFile)

	captureStdout(t, func() {
		if err := runConfigSet([]string{"--org", "1234567890", "--api-key", "nvapi-one", "--ngc-base-url", srv.URL}); err != nil {
			t.Fatalf("runConfigSet: %v", err)
		}
	})

	// Renamed on the backend, and the next `set` touches an unrelated field.
	current = "my-renamed-org"
	out := captureStdout(t, func() {
		if err := runConfigSet([]string{"--org", "1234567890", "--gdn-base-url", "https://api.stg.gdn.nvidia.com"}); err != nil {
			t.Fatalf("runConfigSet: %v", err)
		}
	})
	if !strings.Contains(out, "1234567890 (my-renamed-org)") {
		t.Errorf("config set = %q, want the refreshed name", out)
	}

	cfg, err := loadCLIConfig(cfgFile)
	if err != nil {
		t.Fatalf("loadCLIConfig: %v", err)
	}
	if got := cfg.Orgs["1234567890"].DisplayName; got != "my-renamed-org" {
		t.Errorf("display_name = %q, want my-renamed-org", got)
	}
}

// `config set` fills the cache from NGC when the user did not supply a name.
func TestConfigSetLooksUpDisplayName(t *testing.T) {
	var gotPath string
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, `{"organizations":{"name":"1234567890","displayName":"GDN Staging"}}`)
	}))
	defer srv.Close()

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("GDN_CONFIG_FILE", cfgFile)

	captureStdout(t, func() {
		err := runConfigSet([]string{"--org", "1234567890", "--api-key", "nvapi-test", "--ngc-base-url", srv.URL})
		if err != nil {
			t.Fatalf("runConfigSet: %v", err)
		}
	})
	if gotPath != "/v2/orgs/1234567890" {
		t.Errorf("lookup path = %q, want /v2/orgs/1234567890", gotPath)
	}

	cfg, err := loadCLIConfig(cfgFile)
	if err != nil {
		t.Fatalf("loadCLIConfig: %v", err)
	}
	if got := cfg.Orgs["1234567890"].DisplayName; got != "GDN Staging" {
		t.Errorf("display_name = %q, want GDN Staging", got)
	}
}

// Refreshing must not be destructive: a failed lookup keeps the cached name.
func TestConfigSetKeepsCachedNameWhenLookupFails(t *testing.T) {
	reachable := true
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !reachable {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		writeJSON(w, `{"organizations":{"displayName":"my-org"}}`)
	}))
	defer srv.Close()

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("GDN_CONFIG_FILE", cfgFile)

	captureStdout(t, func() {
		if err := runConfigSet([]string{"--org", "1234567890", "--api-key", "nvapi-test", "--ngc-base-url", srv.URL}); err != nil {
			t.Fatalf("runConfigSet: %v", err)
		}
	})

	reachable = false
	captureStdout(t, func() {
		if err := runConfigSet([]string{"--org", "1234567890", "--gdn-base-url", "https://api.stg.gdn.nvidia.com"}); err != nil {
			t.Fatalf("runConfigSet: %v", err)
		}
	})

	cfg, err := loadCLIConfig(cfgFile)
	if err != nil {
		t.Fatalf("loadCLIConfig: %v", err)
	}
	p := cfg.Orgs["1234567890"]
	if p.DisplayName != "my-org" {
		t.Errorf("display_name = %q, want my-org (a failed lookup must not blank it)", p.DisplayName)
	}
	if p.GDNBaseURL != "https://api.stg.gdn.nvidia.com" {
		t.Errorf("gdn_base_url = %q", p.GDNBaseURL)
	}
}

// `config check` refreshes the cached name, so a rename in NGC is picked up.
func TestConfigCheckRefreshesDisplayName(t *testing.T) {
	current := "GDN Staging"
	srv := newTLSTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/gdn/apps/create-config" {
			writeJSON(w, `{"allowedInstanceNames":["NVIDIA L40"]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/gdn/apps") {
			writeJSON(w, `{"applications":[]}`)
			return
		}
		writeJSON(w, `{"organizations":{"displayName":"`+current+`"}}`)
	}))
	defer srv.Close()

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("GDN_CONFIG_FILE", cfgFile)
	t.Setenv("NGC_API_KEY", "nvapi-test")
	t.Setenv("NGC_BASE_URL", srv.URL)

	if err := runConfigSet([]string{"--org", "1234567890"}); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}

	// First check backfills an org that was configured without a name.
	out := captureStdout(t, func() {
		if err := runConfigCheck([]string{"--org", "1234567890"}); err != nil {
			t.Fatalf("runConfigCheck: %v", err)
		}
	})
	if !strings.Contains(out, "1234567890 (GDN Staging)") {
		t.Errorf("config check = %q, want the org labeled", out)
	}
	if !strings.Contains(out, "Instance names GDN offers (use with --instances):\n  - NVIDIA L40\n") {
		t.Errorf("config check = %q, want the instance names from create-config", out)
	}

	// A later check picks up a rename instead of trusting the cache.
	current = "GDN Staging Renamed"
	out = captureStdout(t, func() {
		if err := runConfigCheck([]string{"--org", "1234567890"}); err != nil {
			t.Fatalf("runConfigCheck: %v", err)
		}
	})
	if !strings.Contains(out, "1234567890 (GDN Staging Renamed)") {
		t.Errorf("config check = %q, want the refreshed name", out)
	}
	cfg, err := loadCLIConfig(cfgFile)
	if err != nil {
		t.Fatalf("loadCLIConfig: %v", err)
	}
	if got := cfg.Orgs["1234567890"].DisplayName; got != "GDN Staging Renamed" {
		t.Errorf("cached display_name = %q, want GDN Staging Renamed", got)
	}
}

// cacheOrgDisplayName backfills an existing org but must not invent config.
func TestCacheOrgDisplayName(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("GDN_CONFIG_FILE", cfgFile)

	if err := runConfigSet([]string{"--org", "1234567890"}); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}
	cacheOrgDisplayName("1234567890", "GDN Staging")
	cacheOrgDisplayName("9999999999", "Never Configured")

	cfg, err := loadCLIConfig(cfgFile)
	if err != nil {
		t.Fatalf("loadCLIConfig: %v", err)
	}
	if got := cfg.Orgs["1234567890"].DisplayName; got != "GDN Staging" {
		t.Errorf("display_name = %q, want GDN Staging", got)
	}
	if _, ok := cfg.Orgs["9999999999"]; ok {
		t.Error("caching a name should not create config for an unconfigured org")
	}
}
