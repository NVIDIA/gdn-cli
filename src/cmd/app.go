// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newAppCommand() *cobra.Command {
	app := newGroupCommand("app", "Manage GDN applications")
	app.AddCommand(leafCommand("create", "Upload an app folder and create a GDN application", runCreateApp))
	app.AddCommand(leafCommand("update", "Update a GDN application's instance config", runUpdateApp))
	app.AddCommand(leafCommand("clone", "Clone an existing GDN application", runCloneApp))
	app.AddCommand(leafCommand("delete", "Delete a GDN application and all of its versions", runDeleteApp))
	app.AddCommand(leafCommand("launch", "Obtain a GDN application launch URL", runLaunchApp))
	app.AddCommand(leafCommand("list", "List GDN applications", runListApps))

	setProduction := leafCommand("set-production", "Set the production (default) version for an application", runSetProduction)
	setProduction.Aliases = []string{"promote"}
	app.AddCommand(setProduction)

	versionGroup := newGroupCommand("version", "Manage GDN application versions")
	versionGroup.AddCommand(leafCommand("add", "Upload an app folder and add an app version", runAddVersion))
	versionGroup.AddCommand(leafCommand("list", "List an application's versions", runListVersions))
	app.AddCommand(versionGroup)
	return app
}

type appListItem struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	DefaultVersionID   string `json:"default_version_id,omitempty"`
	DefaultVersionName string `json:"default_version_name,omitempty"`
	VersionCount       int    `json:"version_count"`
}

func runListApps(args []string) error {
	fs := flag.NewFlagSet("app list", flag.ContinueOnError)
	apiKey := ngcAPIKeyFromEnv()
	org := fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	asJSON := fs.Bool("json", false, "output the application list as JSON")
	namesOnly := fs.Bool("names-only", false, "print only application names, one per line")
	hideFlagDefaults(fs, "org")
	fs.Usage = commandHelpWithExamples(fs, "app list", "List GDN applications for the configured org.",
		[]string{
			"gdn app list",
			"gdn app list --names-only",
			"gdn app list --json",
		},
		helpSection{"Required", []string{
			helpLineOrg,
		}},
		helpSection{"Common", []string{
			"--json                  output full JSON",
			"--names-only            print one application name per line",
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
	apps, err := listApps(ctx, envOr("NGC_BASE_URL", ""), *apiKey, *org)
	if err != nil {
		return err
	}
	return printAppList(apps, *asJSON, *namesOnly)
}

func listApps(ctx context.Context, baseURL, apiKey, org string) ([]gdnApp, error) {
	return newPortal(baseURL, apiKey, org, secureClient).listApps(ctx)
}

type versionListItem struct {
	ID           string `json:"id"`
	VersionName  string `json:"version_name,omitempty"`
	Status       string `json:"status,omitempty"`
	IsProduction bool   `json:"is_production"`
}

func runListVersions(args []string) error {
	fs := flag.NewFlagSet("app version list", flag.ContinueOnError)
	appID := fs.String(flagAppID, "", "GDN application id (required)")
	apiKey := ngcAPIKeyFromEnv()
	org := fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	asJSON := fs.Bool("json", false, "output the version list as JSON")
	hideFlagDefaults(fs, "org")
	fs.Usage = commandHelpWithExamples(fs, "app version list", "List the versions of a GDN application, marking the production (default) version.",
		[]string{
			"gdn app version list --app-id <id>",
			"gdn app version list --app-id <id> --json",
		},
		helpSection{"Required", []string{
			"--app-id ID             GDN application id",
			helpLineOrg,
		}},
		helpSection{"Common", []string{
			"--json                  output full JSON",
		}},
	)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	if *appID == "" {
		return failMissingAppID(fs)
	}
	if err := requireNGCCreds(false, *apiKey, *org); err != nil {
		return failUsageWithFix(fs, err.Error(), func() {
			printCredentialFix(os.Stderr)
		})
	}

	ctx, stop := signalCtx()
	defer stop()
	versions, defaultID, err := newPortal(envOr("NGC_BASE_URL", ""), *apiKey, *org, secureClient).listVersions(ctx, *appID)
	if err != nil {
		return err
	}
	return printVersionList(*appID, versions, defaultID, *asJSON)
}

func printVersionList(appID string, versions []gdnVersion, defaultID string, asJSON bool) error {
	if asJSON {
		items := make([]versionListItem, len(versions))
		for i, v := range versions {
			items[i] = versionListItem{
				ID:           v.ID,
				VersionName:  v.VersionName,
				Status:       v.Status,
				IsProduction: v.ID == defaultID && defaultID != "",
			}
		}
		return printJSON(items)
	}
	fmt.Printf("%d version(s) for app %s:\n", len(versions), appID)
	for _, v := range versions {
		marker := "  "
		if v.ID == defaultID && defaultID != "" {
			marker = "* "
		}
		label := v.ID
		if v.VersionName != "" {
			label = fmt.Sprintf("%s (%s)", v.VersionName, v.ID)
		}
		if v.Status != "" {
			label = fmt.Sprintf("%s [%s]", label, v.Status)
		}
		fmt.Printf("%s%s\n", marker, label)
	}
	fmt.Println("\n* = production (default) version")
	return nil
}

func runSetProduction(args []string) error {
	fs := flag.NewFlagSet("app set-production", flag.ContinueOnError)
	appID := fs.String(flagAppID, "", "GDN application id (required)")
	versionID := fs.String("version-id", "", "version id to make the production/default version (required)")
	apiKey := ngcAPIKeyFromEnv()
	org := fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	asJSON := fs.Bool("json", false, "output the result as JSON")
	dryRun := fs.Bool(flagDryRun, false, "print what would change, make no API calls")
	hideFlagDefaults(fs, "org")
	fs.Usage = commandHelpWithExamples(fs, "app set-production", "Set the production (default) version for a GDN application. Aliased as \"promote\".",
		[]string{
			"gdn app set-production --app-id <id> --version-id <ver>",
			"gdn app promote --app-id <id> --version-id <ver>",
		},
		helpSection{"Required", []string{
			"--app-id ID             GDN application id",
			"--version-id ID         version to promote to production",
			helpLineOrg,
		}},
		helpSection{"Common", []string{
			"--dry-run               print what would change",
			"--json                  output the result as JSON",
		}},
	)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	if *appID == "" {
		return failMissingAppID(fs)
	}
	if *versionID == "" {
		return failUsageWithFix(fs, "--version-id is required", func() {
			printCommandHints(os.Stderr, "Fix",
				commandHint{Text: "List versions to find a version id", Command: fmt.Sprintf("gdn app version list --app-id %s", *appID)},
			)
		})
	}
	if err := requireNGCCreds(*dryRun, *apiKey, *org); err != nil {
		return failUsageWithFix(fs, err.Error(), func() {
			printCredentialFix(os.Stderr)
		})
	}

	if *dryRun {
		fmt.Printf("[dry-run] would set production (default) version of app %s to %s\n", *appID, *versionID)
		return nil
	}

	ctx, stop := signalCtx()
	defer stop()
	portal := newPortal(envOr("NGC_BASE_URL", ""), *apiKey, *org, secureClient)

	// Validate the version belongs to the app so a typo does not silently
	// point the app at a non-existent version.
	versions, _, err := portal.listVersions(ctx, *appID)
	if err != nil {
		return err
	}
	if !versionExists(versions, *versionID) {
		return fmt.Errorf("version %q not found on app %s; run \"gdn app version list --app-id %s\" to see valid version ids", *versionID, *appID, *appID)
	}

	if err := portal.setDefaultVersion(ctx, *appID, *versionID); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(map[string]string{"app_id": *appID, "default_version_id": *versionID})
	}
	fmt.Printf("Set production version of app %s to %s\n", *appID, *versionID)
	return nil
}

func versionExists(versions []gdnVersion, id string) bool {
	for _, v := range versions {
		if v.ID == id {
			return true
		}
	}
	return false
}

func printAppList(apps []gdnApp, asJSON, namesOnly bool) error {
	if namesOnly {
		for _, app := range apps {
			fmt.Println(app.Name)
		}
		return nil
	}
	if asJSON {
		items := make([]appListItem, len(apps))
		for i, app := range apps {
			items[i] = appListItem{
				ID:                 app.ID,
				Name:               app.Name,
				DefaultVersionID:   app.DefaultVersionID,
				DefaultVersionName: currentVersionName(&app),
				VersionCount:       app.VersionCount,
			}
		}
		raw, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal application list: %w", err)
		}
		fmt.Println(string(raw))
		return nil
	}
	fmt.Printf("%d application(s):\n", len(apps))
	for _, app := range apps {
		if app.DefaultVersionID == "" {
			fmt.Printf("  - %s (%s)\n", app.Name, app.ID)
			continue
		}
		label := app.DefaultVersionID
		if name := currentVersionName(&app); name != "" {
			label = fmt.Sprintf("%s (%s)", name, app.DefaultVersionID)
		}
		fmt.Printf("  - %s (%s, default %s)\n", app.Name, app.ID, label)
	}
	return nil
}

// currentVersionName returns the name of the app's current (default) version
// when the app payload embeds it, "" otherwise.
func currentVersionName(app *gdnApp) string {
	if v := app.defaultVersion(); v != nil && v.ID == app.DefaultVersionID {
		return v.VersionName
	}
	return ""
}
