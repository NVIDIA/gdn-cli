// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// configCheckTimeout bounds the connectivity check so it fails fast instead of
// blocking on an unreachable gateway (the default client has no timeout, and the
// shared retry loop would otherwise re-attempt a wedged connection many times).
const configCheckTimeout = 15 * time.Second

func newConfigCommand() *cobra.Command {
	config := newGroupCommand("config", "Manage per-org CLI configuration")
	config.Long = "Manage the consolidated gdn config file, which holds one config per org.\n\n" +
		"Config lives in a TOML file (default ~/.config/gdn/config.toml, override with GDN_CONFIG_FILE) as\n" +
		"[org.<name>] sections. Scope any command to an org with --org (or NGC_ORG); the matching config is\n" +
		"loaded automatically. Endpoints default to production; override the NGC or GDN gateway base URL per org."
	config.AddCommand(leafCommand("list", "List configured orgs", runConfigList))
	config.AddCommand(leafCommand("show", "Show an org's resolved settings", runConfigShow))
	config.AddCommand(leafCommand("set", "Create or update an org's stored settings", runConfigSet))
	config.AddCommand(leafCommand("use", "Set the default org", runConfigUse))
	config.AddCommand(leafCommand("check", "Check GDN connectivity and list valid instance names", runConfigCheck))
	return config
}

func runConfigList(args []string) error {
	fs := flag.NewFlagSet("config list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "output the org list as JSON")
	fs.Usage = commandHelpWithExamples(fs, "config list", "List the orgs configured in the CLI config file.",
		[]string{"gdn config list", "gdn config list --json"},
	)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	cfg, err := loadCLIConfig("")
	if err != nil {
		return err
	}
	orgs := make([]string, 0, len(cfg.Orgs))
	for org := range cfg.Orgs {
		orgs = append(orgs, org)
	}
	sort.Strings(orgs)

	if *asJSON {
		items := make([]map[string]string, 0, len(orgs))
		for _, org := range orgs {
			items = append(items, map[string]string{
				"org":          org,
				"display_name": cfg.Orgs[org].DisplayName,
				"default":      boolStr(org == cfg.DefaultOrg),
			})
		}
		return printJSON(items)
	}

	if len(orgs) == 0 {
		fmt.Printf("No orgs configured in %s\n", firstNonEmpty(cfg.Path, "(no config file)"))
		return nil
	}
	fmt.Printf("%d org(s) in %s:\n", len(orgs), cfg.Path)
	for _, org := range orgs {
		marker := "  "
		if org == cfg.DefaultOrg {
			marker = "* "
		}
		fmt.Printf("%s%s\n", marker, orgFullName(org, cfg.Orgs[org].DisplayName))
	}
	fmt.Println("\n* = default org")
	return nil
}

func runConfigShow(args []string) error {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "output the resolved settings as JSON")
	fs.Usage = commandHelpWithExamples(fs, "config show", "Show an org's resolved settings (API key redacted). Defaults to the active org.",
		[]string{"gdn config show", "gdn config show my-org"},
	)
	// Flag parsing stops at the first non-flag argument, so the optional org has
	// to come off the front before parsing or it would swallow a trailing --json.
	positionalOrg, args := splitLeadingOperand(args)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	if fs.NArg() > 0 {
		return failUsage(fs, "unexpected argument %q", fs.Arg(0))
	}
	cfg, err := loadCLIConfig("")
	if err != nil {
		return err
	}
	org := positionalOrg
	if org == "" {
		org = resolveOrg("", cfg)
	}
	if org == "" {
		return fmt.Errorf("no org specified and no default_org set; pass an org name")
	}
	p, ok := cfg.Orgs[org]
	if !ok {
		return fmt.Errorf("no config for org %q in %s", org, firstNonEmpty(cfg.Path, "config"))
	}

	resolved := map[string]string{
		"org":             org,
		"display_name":    p.DisplayName,
		"api_key":         redactAPIKey(p.APIKey),
		"ngc_base_url":    firstNonEmpty(p.NGCBaseURL, ngcDefaultBaseURL),
		"gdn_base_url":    firstNonEmpty(p.GDNBaseURL, gdnGatewayBaseURL),
		"client_base_url": firstNonEmpty(p.ClientBaseURL, clientDefaultURL),
	}
	if *asJSON {
		return printJSON(resolved)
	}

	fmt.Printf("Org %s\n", orgFullName(org, p.DisplayName))
	for _, k := range []string{"api_key", "ngc_base_url", "gdn_base_url", "client_base_url"} {
		if resolved[k] != "" {
			fmt.Printf("  %-16s %s\n", k, resolved[k])
		}
	}
	return nil
}

// configSetFlags are the org settings config set can write.
type configSetFlags struct {
	org        *string
	apiKey     *string
	ngcBase    *string
	gdnBase    *string
	clientBase *string
}

func registerConfigSetFlags(fs *flag.FlagSet) *configSetFlags {
	c := &configSetFlags{}
	c.org = fs.String("org", envOr("NGC_ORG", ""), "org to create or update (env NGC_ORG) (required)")
	c.apiKey = fs.String("api-key", "", "NGC API key to store for the org")
	c.ngcBase = fs.String("ngc-base-url", "", "NGC API base URL override")
	c.gdnBase = fs.String("gdn-base-url", "", "GDN gateway base URL override")
	c.clientBase = fs.String("client-base-url", "", "GDN client base URL override")
	hideFlagDefault(fs, "org")
	return c
}

// apply folds the flags the user actually set, per set, into the org's stored
// config, leaving the rest of it alone.
func (c *configSetFlags) apply(p orgConfig, set map[string]bool) orgConfig {
	if set["api-key"] {
		p.APIKey = *c.apiKey
	}
	if set["ngc-base-url"] {
		p.NGCBaseURL = *c.ngcBase
	}
	if set["gdn-base-url"] {
		p.GDNBaseURL = *c.gdnBase
	}
	if set["client-base-url"] {
		p.ClientBaseURL = *c.clientBase
	}
	if p.APIKey != "" {
		// Cache the name so reads never make this request. It may change on the
		// backend, so refresh it — but only on success, or a failed lookup would
		// blank a name that is merely unverified.
		ctx, stop := signalCtx()
		if name := lookupOrgDisplayName(ctx, firstNonEmpty(*c.ngcBase, p.NGCBaseURL), p.APIKey, *c.org); name != "" {
			p.DisplayName = name
		}
		stop()
	}
	return p
}

func runConfigSet(args []string) error {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	c := registerConfigSetFlags(fs)
	fs.Usage = commandHelpWithExamples(fs, "config set", "Create or update an org's stored settings in the config file. Only the flags you pass are changed.",
		[]string{
			"gdn config set --org my-org --api-key nvapi-...",
			"gdn config set --org my-org --gdn-base-url https://api.stg.gdn.nvidia.com",
		},
		helpSection{"Required", []string{
			"--org ORG               org to create or update (env NGC_ORG)",
		}},
		helpSection{"Common", []string{
			"--api-key KEY           NGC API key to store",
		}},
		helpSection{"Advanced", []string{
			"--ngc-base-url URL      NGC API base URL override",
			"--gdn-base-url URL      GDN gateway base URL override",
			"--client-base-url URL   GDN client base URL override",
		}},
	)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	// Unlike `config use`, the org is a flag here. Rejecting a positional is what
	// makes that discoverable: flag parsing stops at the first non-flag argument,
	// so `config set <org> --api-key K` would also discard --api-key and fall back
	// to NGC_ORG, reporting success while updating a different org.
	if fs.NArg() > 0 {
		return failUsage(fs, "unexpected argument %q; pass the org as a flag: gdn config set --org %s ...", fs.Arg(0), fs.Arg(0))
	}
	if *c.org == "" {
		return failUsage(fs, "--org is required")
	}

	// Only apply the flags the user actually set, so updates are non-destructive.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg, err := loadCLIConfig("")
	if err != nil {
		return err
	}
	if cfg.Path == "" {
		return fmt.Errorf("could not determine config file path; set GDN_CONFIG_FILE")
	}
	if cfg.Orgs == nil {
		cfg.Orgs = map[string]orgConfig{}
	}
	_, existed := cfg.Orgs[*c.org]
	p := c.apply(cfg.Orgs[*c.org], set)
	cfg.Orgs[*c.org] = p
	// Make the first configured org the default for convenience.
	if cfg.DefaultOrg == "" {
		cfg.DefaultOrg = *c.org
	}
	if err := writeCLIConfig(cfg); err != nil {
		return err
	}
	verb := "Updated"
	if !existed {
		verb = "Created"
	}
	fmt.Printf("%s config for org %s in %s\n", verb, orgFullName(*c.org, p.DisplayName), cfg.Path)
	return nil
}

func runConfigUse(args []string) error {
	fs := flag.NewFlagSet("config use", flag.ContinueOnError)
	fs.Usage = commandHelpWithExamples(fs, "config use", "Set the default org in the config file. Creates the org's config if it does not exist.",
		[]string{"gdn config use my-org"},
	)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	org := fs.Arg(0)
	if org == "" {
		return failUsage(fs, "org name is required")
	}
	if fs.NArg() > 1 {
		return failUsage(fs, "unexpected argument %q; `config use` takes one org name", fs.Arg(1))
	}
	cfg, err := loadCLIConfig("")
	if err != nil {
		return err
	}
	if cfg.Path == "" {
		return fmt.Errorf("could not determine config file path; set GDN_CONFIG_FILE")
	}
	if cfg.Orgs == nil {
		cfg.Orgs = map[string]orgConfig{}
	}
	created := false
	if _, ok := cfg.Orgs[org]; !ok {
		cfg.Orgs[org] = orgConfig{}
		created = true
	}
	cfg.DefaultOrg = org
	if err := writeCLIConfig(cfg); err != nil {
		return err
	}
	if created {
		fmt.Printf("Created and selected config for org %s in %s\n", orgFullName(org, cfg.Orgs[org].DisplayName), cfg.Path)
		return nil
	}
	fmt.Printf("Default org set to %s in %s\n", orgFullName(org, cfg.Orgs[org].DisplayName), cfg.Path)
	return nil
}

func runConfigCheck(args []string) error {
	fs := flag.NewFlagSet("config check", flag.ContinueOnError)
	apiKey := ngcAPIKeyFromEnv()
	org := fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	hideFlagDefaults(fs, "org")
	fs.Usage = commandHelpWithExamples(fs, "config check", "Check GDN connectivity and print valid instance names + defaults.",
		[]string{
			"gdn config check",
			"gdn config check --org my-org",
		},
		helpSection{"Required", []string{
			helpLineOrg,
		}},
	)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	if err := requireNGCCreds(false, *apiKey, *org); err != nil {
		return failUsageWithFix(fs, err.Error(), func() {
			printCredentialFix(os.Stderr)
		})
	}

	ctx, stop := signalCtx()
	defer stop()
	// A connectivity check must fail fast rather than block on an unreachable or
	// blackholed gateway. Bound the whole check with a deadline so the retry loop
	// stops promptly, and give the client its own timeout as a backstop.
	ctx, cancel := context.WithTimeout(ctx, configCheckTimeout)
	defer cancel()
	client := newSecureClient(configCheckTimeout)

	ngcBase := envOr("NGC_BASE_URL", "")
	portal := newPortal(ngcBase, *apiKey, *org, client)
	apps, err := portal.listApps(ctx)
	if err != nil {
		return fmt.Errorf("GDN not reachable for org %s at %s: %w", *org, firstNonEmpty(ngcBase, ngcDefaultBaseURL), err)
	}
	displayName := lookupOrgDisplayName(ctx, ngcBase, *apiKey, *org)
	cacheOrgDisplayName(*org, displayName)

	fmt.Printf("GDN reachable for org %s: OK (%d existing application(s))\n", orgFullName(*org, displayName), len(apps))
	for _, a := range apps {
		fmt.Printf("  - %s (%s)\n", a.Name, a.ID)
	}
	fmt.Println("\nInstance names GDN offers (use with --instances):")
	cfg, err := portal.getCreateConfig(ctx)
	switch {
	case err != nil:
		fmt.Printf("  (could not fetch: %v)\n", err)
		return nil
	case len(cfg.AllowedInstanceNames) == 0:
		fmt.Println("  (none right now: GDN's instance catalogue is unavailable)")
	default:
		for _, n := range cfg.AllowedInstanceNames {
			fmt.Printf("  - %s\n", n)
		}
	}
	if s := cfg.DefaultApplicationFeatures.summary(); s != "" {
		fmt.Printf("\nDefaults GDN applies to new apps: %s\n", s)
	}
	return nil
}

// splitLeadingOperand pulls a leading non-flag argument off the front so the
// remaining flags still reach flag.Parse, which otherwise treats everything
// after the first operand as positional.
func splitLeadingOperand(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

func redactAPIKey(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	if len(apiKey) <= 6 {
		return "***"
	}
	return apiKey[:3] + "***" + apiKey[len(apiKey)-2:]
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
