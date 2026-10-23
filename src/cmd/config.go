// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// clientDefaultURL is the production GDN browser launch client. It is kept
// separate from the GDN API Gateway (GDN_BASE_URL) because it serves the
// streaming client, not the API.
const clientDefaultURL = "https://client.gdn.nvidia.com"

// orgConfig holds the settings for one org. The org name is the config's
// identity (the map key in cliConfig.Orgs), so it is not repeated here. All
// fields are optional and default to the production endpoints when unset.
type orgConfig struct {
	APIKey        string
	DisplayName   string
	NGCBaseURL    string
	GDNBaseURL    string
	ClientBaseURL string
}

// cliConfig is the parsed config file. Orgs are keyed by org name; there is
// exactly one config per org.
type cliConfig struct {
	Path       string
	DefaultOrg string
	Orgs       map[string]orgConfig
}

// orgFieldByKey maps TOML keys to orgConfig setters, keeping parse and
// serialize in sync. There is no "org" key: the section name is the org.
var orgFieldByKey = map[string]func(*orgConfig, string){
	"api_key":         func(p *orgConfig, v string) { p.APIKey = v },
	"display_name":    func(p *orgConfig, v string) { p.DisplayName = v },
	"ngc_base_url":    func(p *orgConfig, v string) { p.NGCBaseURL = v },
	"gdn_base_url":    func(p *orgConfig, v string) { p.GDNBaseURL = v },
	"client_base_url": func(p *orgConfig, v string) { p.ClientBaseURL = v },
}

// defaultConfigPath returns the config file path. GDN_CONFIG_FILE overrides
// it (useful for tests and non-standard setups); otherwise it follows
// XDG_CONFIG_HOME or ~/.config/gdn/config.toml.
func defaultConfigPath() string {
	if p := os.Getenv("GDN_CONFIG_FILE"); p != "" {
		return p
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "gdn", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "gdn", "config.toml")
}

// loadCLIConfig parses the config file. A missing file yields an empty config
// (not an error) so the CLI works without any orgs configured. Pass an
// explicit path or "" to use defaultConfigPath.
func loadCLIConfig(path string) (cliConfig, error) {
	if path == "" {
		path = defaultConfigPath()
	}
	cfg := cliConfig{Path: path, Orgs: map[string]orgConfig{}}
	if path == "" {
		return cfg, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	defer f.Close()
	return parseCLIConfig(f, cfg)
}

// parseCLIConfig reads the config body into cfg, which already carries the
// path the lines came from for error messages.
func parseCLIConfig(r io.Reader, cfg cliConfig) (cliConfig, error) {
	var (
		section string // "" = top level, else org name
		cur     orgConfig
	)
	flush := func() {
		if section != "" {
			cfg.Orgs[section] = cur
		}
	}

	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if header, ok := configSectionHeader(line); ok {
			flush()
			section = configSectionOrg(header)
			cur = orgConfig{}
			continue
		}
		if err := cfg.applyConfigLine(line, section, &cur); err != nil {
			return cfg, fmt.Errorf("%s:%d: %w", cfg.Path, lineNo, err)
		}
	}
	flush()
	return cfg, scanner.Err()
}

// configSectionHeader returns the text inside a "[...]" line.
func configSectionHeader(line string) (string, bool) {
	if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
		return "", false
	}
	return strings.TrimSpace(line[1 : len(line)-1]), true
}

// configSectionOrg returns the org a section header names. Accepts
// "[org.<name>]", "[profile.<name>]" (legacy), or a bare "[<name>]".
func configSectionOrg(header string) string {
	for _, prefix := range []string{"org.", "profile."} {
		if strings.HasPrefix(header, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(header, prefix))
		}
	}
	return header
}

// applyConfigLine applies one "key = value" line: to the top level when
// section is empty, otherwise to the org config being accumulated in cur.
func (c *cliConfig) applyConfigLine(line, section string, cur *orgConfig) error {
	key, val, ok := strings.Cut(line, "=")
	if !ok {
		return fmt.Errorf("expected key = value")
	}
	key = strings.ToLower(strings.TrimSpace(key))
	val = unquoteConfigValue(strings.TrimSpace(val))

	if section == "" {
		switch key {
		case "default_org", "default-org", "default_profile", "default-profile":
			c.DefaultOrg = val
		}
		return nil
	}
	if set := orgFieldByKey[key]; set != nil {
		set(cur, val)
	}
	return nil
}

func unquoteConfigValue(val string) string {
	if len(val) >= 2 &&
		((val[0] == '"' && val[len(val)-1] == '"') ||
			(val[0] == '\'' && val[len(val)-1] == '\'')) {
		return val[1 : len(val)-1]
	}
	if idx := strings.Index(val, " #"); idx >= 0 {
		val = strings.TrimSpace(val[:idx])
	}
	return val
}

// resolveOrg returns the org whose config should be used. The org can be
// selected several ways; the first non-empty wins: the --org flag, NGC_ORG from
// the shell/.env, and finally the config's default_org.
func resolveOrg(orgFlag string, cfg cliConfig) string {
	return firstNonEmpty(
		orgFlag,
		os.Getenv("NGC_ORG"),
		cfg.DefaultOrg,
	)
}

// applyConfig resolves the org whose config to use and folds that org's stored
// settings into the process environment so downstream commands (which read env
// vars) pick them up. Precedence is preserved by only setting env vars that are
// not already present (shell env and .env have already been loaded; explicit
// flags are applied by the caller afterwards). Production endpoints are applied
// as the final fallback so the tool works with no config at all.
func applyConfig(orgFlag string) {
	cfg, err := loadCLIConfig("")
	if err != nil {
		warnf("could not load config %s: %v", cfg.Path, err)
	}

	org := resolveOrg(orgFlag, cfg)
	p := cfg.Orgs[org] // zero value when there is no stored config for org

	// The org is the config's identity, so set NGC_ORG from the resolved name.
	setEnvIfUnset("NGC_ORG", org)
	setEnvIfUnset("NGC_API_KEY", p.APIKey)
	setEnvIfUnset("NGC_BASE_URL", normalizeBaseURL(p.NGCBaseURL))
	setEnvIfUnset("GDN_BASE_URL", normalizeBaseURL(p.GDNBaseURL))
	setEnvIfUnset("GDN_CLIENT_URL", normalizeBaseURL(p.ClientBaseURL))

	// Production defaults for the two API bases and the launch client.
	setEnvIfUnset("NGC_BASE_URL", ngcDefaultBaseURL)
	setEnvIfUnset("GDN_BASE_URL", gdnGatewayBaseURL)
	setEnvIfUnset("GDN_CLIENT_URL", clientDefaultURL)
}

// normalizeBaseURL ensures a configured base URL carries a scheme. Users often
// store a bare host (e.g. "api.gdn.nvidia.com"); default to https so the HTTP
// client does not fail with `unsupported protocol scheme ""`.
func normalizeBaseURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if !strings.Contains(s, "://") {
		return "https://" + s
	}
	return s
}

func setEnvIfUnset(key, val string) {
	if val == "" {
		return
	}
	if _, ok := os.LookupEnv(key); ok {
		return
	}
	_ = os.Setenv(key, val)
}

// writeCLIConfig serializes the config back to its file as a TOML subset,
// creating parent directories as needed. Comments are not preserved.
func writeCLIConfig(cfg cliConfig) error {
	if cfg.Path == "" {
		return fmt.Errorf("no config file path")
	}
	if dir := filepath.Dir(cfg.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create config dir: %w", err)
		}
	}

	var b strings.Builder
	b.WriteString("# gdn CLI configuration. Managed by `gdn config`.\n")
	b.WriteString("# Each [org.<name>] section holds one org's settings.\n")
	if cfg.DefaultOrg != "" {
		fmt.Fprintf(&b, "default_org = %q\n", cfg.DefaultOrg)
	}

	orgs := make([]string, 0, len(cfg.Orgs))
	for org := range cfg.Orgs {
		orgs = append(orgs, org)
	}
	sort.Strings(orgs)
	for _, org := range orgs {
		p := cfg.Orgs[org]
		fmt.Fprintf(&b, "\n[org.%s]\n", org)
		writeConfigLine(&b, "api_key", p.APIKey)
		writeConfigLine(&b, "display_name", p.DisplayName)
		writeConfigLine(&b, "ngc_base_url", p.NGCBaseURL)
		writeConfigLine(&b, "gdn_base_url", p.GDNBaseURL)
		writeConfigLine(&b, "client_base_url", p.ClientBaseURL)
	}

	if err := os.WriteFile(cfg.Path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// cacheOrgDisplayName stores an org's display name so later commands render it
// without a network call. It only updates an org that already has a config
// section, since `config check` accepts any --org, and warns rather than fails.
func cacheOrgDisplayName(org, displayName string) {
	if org == "" || displayName == "" {
		return
	}
	cfg, err := loadCLIConfig("")
	if err != nil || cfg.Path == "" {
		return
	}
	p, ok := cfg.Orgs[org]
	if !ok || p.DisplayName == displayName {
		return
	}
	p.DisplayName = displayName
	cfg.Orgs[org] = p
	if err := writeCLIConfig(cfg); err != nil {
		warnf("could not cache the display name for org %s: %v", org, err)
	}
}

func writeConfigLine(b *strings.Builder, key, val string) {
	if val == "" {
		return
	}
	fmt.Fprintf(b, "%s = %q\n", key, val)
}
