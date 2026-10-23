// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// gdn is the GDN CLI for publishing and managing GDN applications.
// NGC_API_KEY, NGC_ORG (optional NGC_BASE_URL for NGC + app management, and
// GDN_BASE_URL for the launch gateway) come from env or .env.
package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// maxAppNameLen is the GDN backend limit on application display names.
const maxAppNameLen = 32

// appNamePattern is the GDN application name format: a letter or digit, then
// letters, digits, hyphens and underscores.
var appNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// validateAppName checks name against the GDN application name rules. The
// error suggests the nearest valid name when there is one.
func validateAppName(name string) error {
	n := utf8.RuneCountInString(name)
	var reason string
	switch {
	case !appNamePattern.MatchString(name):
		reason = "it must start with a letter or digit and contain only letters, digits, '-' and '_'"
	case n > maxAppNameLen:
		reason = fmt.Sprintf("it is %d characters, which exceeds the GDN limit of %d", n, maxAppNameLen)
	default:
		return nil
	}
	msg := fmt.Sprintf("invalid application name %q: %s", name, reason)
	if s := suggestAppName(name); s != "" {
		msg += fmt.Sprintf("; try %q", s)
	}
	return errors.New(msg)
}

// suggestAppName turns name into a valid application name: each run of other
// characters becomes one '-', characters that cannot start a name are dropped
// from the front, and the result is cut to the length limit.
func suggestAppName(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
			dash = r == '-'
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimLeft(b.String(), "-_")
	if len(s) > maxAppNameLen {
		s = s[:maxAppNameLen]
	}
	return strings.TrimRight(s, "-")
}

// cloneAppName derives a clone's name by appending "-N" to the source name.
func cloneAppName(name string, n int) string { return fmt.Sprintf("%s-%d", name, n) }

// maxCloneSuffix bounds the search for a free "<name>-N" so a short source name
// can't spin indefinitely looking for an unused suffix.
const maxCloneSuffix = 1000

// pickCloneName returns the first "<name>-N" (starting at N=1) that is not
// already taken (case-insensitive). It errors if that candidate would exceed
// the GDN name-length limit, or if no free suffix is found within the bound.
func pickCloneName(name string, taken map[string]bool) (string, error) {
	for n := 1; n <= maxCloneSuffix; n++ {
		cand := cloneAppName(name, n)
		if utf8.RuneCountInString(cand) > maxAppNameLen {
			return "", fmt.Errorf("cannot clone %q: clone name %q is %d characters, which exceeds the GDN limit of %d",
				name, cand, utf8.RuneCountInString(cand), maxAppNameLen)
		}
		if !taken[strings.ToLower(cand)] {
			return cand, nil
		}
	}
	return "", fmt.Errorf("cannot clone %q: %q through %q are all taken", name, cloneAppName(name, 1), cloneAppName(name, maxCloneSuffix))
}

// launchAppFlags are the flags of app launch. Everything but --app-id is
// optional; applyConfig folds the org's stored settings (or production
// defaults) into the environment at startup, so a launch only needs --app-id.
type launchAppFlags struct {
	appID             *string
	apiKey            *string
	org               *string
	versionID         *string
	mediaType         *string
	appFeaturesJSON   *string
	partnerSecureData *string
	streamLookupID    *string
	userID            *string
	sessionHost       *string
	preset            *string
	open              *bool
}

func registerLaunchAppFlags(fs *flag.FlagSet) *launchAppFlags {
	l := &launchAppFlags{}
	l.appID = fs.String(flagAppID, "", "GDN application id to launch (required)")
	l.apiKey = ngcAPIKeyFromEnv()
	l.org = fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	l.versionID = fs.String("version-id", envOr("GDN_VERSION_ID", ""), "GDN version id to launch (env GDN_VERSION_ID, optional — omit to use the app's default version)")
	l.mediaType = fs.String(flagUpstreamMediaType, envOr("GDN_MEDIA_TYPE", ""), "upstream media type, e.g. AUDIO (env GDN_MEDIA_TYPE) (optional)")
	l.appFeaturesJSON = fs.String(flagAppFeatures, "", "override applicationFeatures for this session as a JSON object (inline, @file, or - for stdin)")
	l.partnerSecureData = fs.String("partner-secure-data", "", "partnerSecureData string bound to the session nonce (optional)")
	l.streamLookupID = fs.String("stream-lookup-id", "", "streamLookupId bound to the session nonce (optional)")
	l.userID = fs.String("user-id", "", "userId bound to the session nonce (optional)")
	l.sessionHost = fs.String("session-host", envOr("GDN_SESSION_HOST", ""), "route the session to a specific host (env GDN_SESSION_HOST) (optional)")
	l.preset = fs.String("preset", "", "add a preset query param to the client URL (optional)")
	l.open = fs.Bool("open", true, "open the launch URL in the default browser; use --open=false to print it")
	hideFlagDefaults(fs,
		"org", "version-id", flagUpstreamMediaType,
		flagAppFeatures, "partner-secure-data", "stream-lookup-id", "user-id",
		"session-host", "preset",
	)
	return l
}

// nonceBody is the GIO nonce request payload. The optional fields are left out
// rather than sent empty.
func (l *launchAppFlags) nonceBody() (map[string]any, error) {
	body := map[string]any{"applicationId": *l.appID}
	if *l.versionID != "" {
		body["versionId"] = *l.versionID
	}
	if *l.partnerSecureData != "" {
		body["partnerSecureData"] = *l.partnerSecureData
	}
	if *l.streamLookupID != "" {
		body["streamLookupId"] = *l.streamLookupID
	}
	if *l.userID != "" {
		body["userId"] = *l.userID
	}
	if *l.appFeaturesJSON != "" {
		features, err := parseAppFeaturesOverride(*l.appFeaturesJSON)
		if err != nil {
			return nil, err
		}
		body["applicationFeatures"] = features
	}
	return body, nil
}

// clientLaunchURL builds the browser URL that starts the session.
func (l *launchAppFlags) clientLaunchURL(clientBaseURL, sessionServer, nonce string) string {
	q := url.Values{}
	if *l.mediaType != "" {
		q.Set("upstream-media-type", *l.mediaType)
	}
	if sessionServer != "" {
		q.Set("session-server-address", sessionServer)
	}
	q.Set("application-id", *l.appID)
	q.Set("org-name", *l.org)
	q.Set("nonce", nonce)
	if *l.preset != "" {
		q.Set("preset", *l.preset)
	}
	return strings.TrimRight(clientBaseURL, "/") + "/?" + q.Encode()
}

// sessionServerAddress is the host the session is routed to. It shares the
// gateway host but is reached under a leading label ("prod." by default);
// override replaces the entire FQDN to route the session elsewhere.
func sessionServerAddress(gatewayURL, override string) string {
	if h := strings.TrimSpace(override); h != "" {
		return h
	}
	if u, err := url.Parse(gatewayURL); err == nil && u.Host != "" {
		return "prod." + u.Host
	}
	return gatewayURL
}

// fetchLaunchNonce posts to the org-scoped GIO endpoint and returns the session
// nonce. The GDN client opened afterwards performs the matching
// /v1/orgs/{org}/authorize step during streaming, so the CLI only needs this.
func fetchLaunchNonce(ctx context.Context, endpoint, apiKey string, nonceBody map[string]any) (string, error) {
	nonceBodyJSON, _ := json.Marshal(nonceBody)
	body := strings.NewReader(string(nonceBodyJSON))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("NV-Client-Type", "GDNSDK")

	resp, err := secureClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("app launch: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	var nonceResp struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(raw, &nonceResp); err != nil || nonceResp.Nonce == "" {
		return "", fmt.Errorf("app launch: could not parse nonce from response: %s", strings.TrimSpace(string(raw)))
	}
	return nonceResp.Nonce, nil
}

func openOrPrintLaunchURL(launchURL string, open bool) {
	if !open {
		fmt.Println(launchURL)
		return
	}
	if err := openBrowser(launchURL); err != nil {
		warnf("could not open browser: %v", err)
		fmt.Println(launchURL)
		return
	}
	fmt.Fprintln(os.Stderr, "Opening in browser...")
}

func runLaunchApp(args []string) error {
	fs := flag.NewFlagSet("app launch", flag.ContinueOnError)
	l := registerLaunchAppFlags(fs)
	fs.Usage = launchHelp(fs)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	if *l.appID == "" {
		return failMissingAppID(fs)
	}
	if err := requireNGCCreds(false, *l.apiKey, *l.org); err != nil {
		return failUsageWithFix(fs, err.Error(), func() {
			printCredentialFix(os.Stderr)
		})
	}
	// The GDN API Gateway serves both the nonce (GIO) and the portal, and the
	// session server address is the gateway host. The browser launch client is
	// separate.
	gatewayURL := normalizeBaseURL(firstNonEmpty(os.Getenv("GDN_BASE_URL"), gdnGatewayBaseURL))
	clientURL := normalizeBaseURL(firstNonEmpty(os.Getenv("GDN_CLIENT_URL"), clientDefaultURL))
	sessionServer := sessionServerAddress(gatewayURL, *l.sessionHost)

	ctx, stop := signalCtx()
	defer stop()

	nonceBody, err := l.nonceBody()
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(gatewayURL, "/") + "/v1/orgs/" + url.PathEscape(*l.org) + "/nonce"
	nonce, err := fetchLaunchNonce(ctx, endpoint, *l.apiKey, nonceBody)
	if err != nil {
		return err
	}

	openOrPrintLaunchURL(l.clientLaunchURL(clientURL, sessionServer, nonce), *l.open)
	return nil
}

// parseAppFeaturesOverride reads an applicationFeatures override (inline JSON,
// @file, or - for stdin) and returns it verbatim for the nonce request. The
// value must be a JSON object; it is passed through unchanged so any
// GIO-supported feature shape works without CLI changes.
func parseAppFeaturesOverride(val string) (json.RawMessage, error) {
	raw, err := readJSONArg(val)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("--app-features must be a JSON object: %w", err)
	}
	return json.RawMessage(raw), nil
}

// openBrowser opens url in the user's default browser.
// On Linux it detects WSL and uses explorer.exe; otherwise xdg-open.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default: // linux + WSL
		if isWSL() {
			// explorer.exe is always available in WSL2; wslview requires wslu.
			if path, err := exec.LookPath("wslview"); err == nil {
				cmd = exec.Command(path, url)
			} else {
				cmd = exec.Command("explorer.exe", url)
			}
		} else {
			cmd = exec.Command("xdg-open", url)
		}
	}
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}

// isWSL reports whether the process is running inside Windows Subsystem for Linux.
func isWSL() bool {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(data))
	return strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl")
}

// uploadFlags shared by app create and app version add.
type uploadFlags struct {
	dir, executable                *string
	execArgs, instances            stringList
	apiKey, org                    *string
	artifact                       *string
	setLatest                      *bool
	appName, description, seatType *string
	version, versionName           *string
	hashWorkers                    *int
	uploadWorkers                  *int
	fileWorkers                    *int
	dryRun                         *bool
	allowDuplicate                 *bool
	jsonOutput                     *bool

	resources       resourceRefList
	execPath        *string
	startupResource *string
	launchConfig    *string
	envVars         envVarList
	appFeatures     *string
	displayModes    displayModeList
}

func registerUploadFlags(fs *flag.FlagSet) *uploadFlags {
	u := &uploadFlags{}
	u.dir = fs.String("dir", "", "path to the app folder to upload (required unless --resource is given)")
	u.executable = fs.String("executable", "", "executable path relative to --dir (guessed if omitted)")
	fs.Var(&u.execArgs, "exec-args", "comma-separated executable launch arguments")
	fs.Var(&u.instances, "instances", "comma-separated instance names (defaults to every name GDN offers when creating a new app)")

	fs.Var(&u.resources, "resource", "reference an already-uploaded resource as [<team>/]<name>:<version> (repeatable); use instead of or in addition to --dir")
	u.execPath = fs.String("exec-path", "", "executable path relative to the startup resource (use with --resource)")
	u.startupResource = fs.String("startup-resource", "", "name of the resource that holds the startup file (defaults to the uploaded resource, else the first --resource)")
	u.launchConfig = fs.String("launch-config", "", "per-version launchConfig as JSON (inline, @file, or - for stdin)")
	fs.Var(&u.envVars, "env-var", "launch environment variable as <key>:<value> (repeatable); merged into launchConfig.environment")
	u.appFeatures = fs.String(flagAppFeatures, "", "app-level applicationFeatures as JSON (inline, @file, or - for stdin)")
	fs.Var(&u.displayModes, flagDisplayMode, "allowed display mode as RES@FPS, e.g. 4k@60 (repeatable; GDN's default applies when omitted)")

	u.apiKey = ngcAPIKeyFromEnv()
	u.org = fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	u.artifact = fs.String("name", "", "NGC artifact name (defaults to --app-name slug)")
	u.setLatest = fs.Bool(flagSetLatest, true, "mark the NGC version as latest on commit")

	u.appName = fs.String(flagAppName, "", "application name: 1-32 letters, digits, '-' or '_', starting with a letter or digit (required)")
	u.description = fs.String("description", "", "application description")
	u.seatType = fs.String(flagSeatType, "WINDOWS", "seat type: WINDOWS | LINUX | LINUX_PROTON")
	u.version = fs.String("version", "", "artifact/app version, e.g. 1.0.0 (required)")
	u.versionName = fs.String("version-name", "", "app version name (defaults to --version)")
	u.hashWorkers = fs.Int(flagHashWorkers, defaultHashWorkers, usageHashWorkers)
	u.uploadWorkers = fs.Int(flagMultipartWorkers, defaultMultipartWorkers, usageMultipartWorkers)
	u.fileWorkers = fs.Int(flagFileWorkers, defaultFileWorkers, usageFileWorkers)
	u.dryRun = fs.Bool(flagDryRun, false, usageDryRunScan)
	u.jsonOutput = fs.Bool("json", false, usageJSONResult)
	hideFlagDefaults(fs, "org")
	return u
}

// run uploads and/or references resources and publishes. existingAppID empty = new app.
func (u *uploadFlags) run(existingAppID string) error {
	start := time.Now()
	ctx, stop := signalCtx()
	defer stop()
	log := logf
	if *u.jsonOutput {
		log = logStderr
	}

	coords := coordinates{
		Org:     firstNonEmpty(*u.org, dryRunPlaceholder),
		Type:    "RESOURCE",
		Name:    firstNonEmpty(*u.artifact, slug(*u.appName)),
		Version: *u.version,
	}
	ngc := newNGCForUpload(envOr("NGC_BASE_URL", ""), firstNonEmpty(*u.apiKey, dryRunPlaceholder), coords)

	var portal *portalClient
	if !*u.dryRun {
		portal = newPortal(envOr("NGC_BASE_URL", ""), *u.apiKey, *u.org, secureClient)
	}

	launch, err := u.parseLaunchConfig()
	if err != nil {
		return err
	}
	var features *appFeature
	instances := []string(u.instances)
	if existingAppID == "" {
		if features, instances, err = u.newAppSettings(ctx, portal); err != nil {
			return err
		}
	}

	res, err := publish(ctx, ngc, portal, options{
		AppDir:          *u.dir,
		ExecutablePath:  firstNonEmpty(*u.execPath, *u.executable),
		ExecutableArgs:  u.execArgs,
		AppName:         *u.appName,
		Description:     *u.description,
		SeatType:        strings.ToUpper(*u.seatType),
		InstanceNames:   instances,
		VersionName:     firstNonEmpty(*u.versionName, *u.version),
		ExistingAppID:   existingAppID,
		AllowDuplicate:  u.allowDuplicate != nil && *u.allowDuplicate,
		SetLatest:       *u.setLatest,
		DryRun:          *u.dryRun,
		HashWorkers:     *u.hashWorkers,
		UploadWorkers:   *u.uploadWorkers,
		FileWorkers:     *u.fileWorkers,
		Resources:       u.resources,
		StartupResource: *u.startupResource,
		LaunchConfig:    launch,
		AppFeatures:     features,
	}, log)
	if err != nil {
		return err
	}

	return printCommandOutput(res, time.Since(start), *u.jsonOutput, *u.appName, existingAppID == "")
}

// newAppSettings returns the applicationFeatures and instance names for a new
// app: --app-features with --display-mode folded in, and --instances resolved
// against what GDN offers. portal is nil on a dry run, which skips the checks
// that need it.
func (u *uploadFlags) newAppSettings(ctx context.Context, portal *portalClient) (*appFeature, []string, error) {
	instances := []string(u.instances)
	features, err := u.parseAppFeatures()
	if err != nil {
		return nil, nil, err
	}
	if len(u.displayModes) > 0 {
		if features == nil {
			features = &appFeature{}
		}
		if features.DisplayMode != nil {
			return nil, nil, fmt.Errorf("--%s and DISPLAY_MODE in --%s both set the display modes; use one", flagDisplayMode, flagAppFeatures)
		}
		features.DisplayMode = u.displayModes
	}
	if *u.dryRun {
		return features, instances, nil
	}
	if instances, err = portal.resolveInstanceNames(ctx, instances); err != nil {
		return nil, nil, err
	}
	if features != nil && len(features.DisplayMode) > 0 {
		if err := portal.checkDisplayModes(ctx, features.DisplayMode); err != nil {
			return nil, nil, err
		}
	}
	return features, instances, nil
}

// requireStartupWhenReferenceOnly ensures an executable is specified when the
// version is built purely from referenced resources (no --dir to guess from).
func (u *uploadFlags) requireStartupWhenReferenceOnly() error {
	if *u.dir != "" || len(u.resources) == 0 {
		return nil
	}
	if *u.execPath == "" && *u.executable == "" && (u.launchConfig == nil || *u.launchConfig == "") {
		return fmt.Errorf("--exec-path (or --launch-config) is required when building a version from existing resources without --dir")
	}
	return nil
}

// parseLaunchConfig builds the per-version launchConfig from the --launch-config
// JSON blob (if any) and merges any --env-var values into its environment. It
// returns nil when neither is supplied.
func (u *uploadFlags) parseLaunchConfig() (*launchConfig, error) {
	hasJSON := u.launchConfig != nil && *u.launchConfig != ""
	if !hasJSON && len(u.envVars) == 0 {
		return nil, nil
	}
	var lc launchConfig
	if hasJSON {
		if err := loadJSONInto(*u.launchConfig, "--launch-config", &lc); err != nil {
			return nil, err
		}
	}
	lc.Environment = append(lc.Environment, u.envVars...)
	return &lc, nil
}

// appFeaturesError attributes an applicationFeatures validation error to the
// --app-features flag.
func appFeaturesError(err error) error {
	return fmt.Errorf("--%s: %w", flagAppFeatures, err)
}

// parseAppFeatures decodes the --app-features JSON document, if provided.
func (u *uploadFlags) parseAppFeatures() (*appFeature, error) {
	if u.appFeatures == nil || *u.appFeatures == "" {
		return nil, nil
	}
	var feat appFeature
	if err := loadAppFeatures(*u.appFeatures, &feat); err != nil {
		return nil, err
	}
	if err := feat.validate(); err != nil {
		return nil, appFeaturesError(err)
	}
	if feat.isEmpty() {
		return nil, nil
	}
	return &feat, nil
}

func runCreateApp(args []string) error {
	const name = "app create"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	u := registerUploadFlags(fs)
	u.allowDuplicate = fs.Bool("allow-duplicate", false, "create the app even if one with the same name already exists")
	fs.Usage = uploadHelp(fs, name, "Upload an app folder to NGC and create a new GDN application, or reference existing resources.", createAppHelpSections, []string{
		"gdn app create --dir ./my-app --app-name my-app-1 --version 1.0.0",
		"gdn app create --app-name my-app-1 --version 1.0.0 --resource my-app-1:1.0.0 --exec-path app.exe",
		"gdn app create --dir ./my-app --app-name my-app-1 --version 1.0.0 --dry-run",
	})
	args = repairDirArgs(args)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	normalizeDirFlag(u.dir)

	required := map[string]string{flagAppName: *u.appName, "version": *u.version}
	if len(u.resources) == 0 {
		required["dir"] = *u.dir
	}
	if missing := missingFlags(required); len(missing) > 0 {
		return failMissingRequiredUploadFlags(fs, args, required)
	}
	if err := u.requireStartupWhenReferenceOnly(); err != nil {
		return failUsageWithFix(fs, err.Error(), func() {
			printCommandHints(os.Stderr, "Fix",
				commandHint{Text: "Name the executable inside the resource", Command: "gdn app create --app-name <name> --version <ver> --resource <name>:<ver> --exec-path <path>"},
			)
		})
	}
	if err := validateAppName(*u.appName); err != nil {
		return failUsage(fs, "%v", err)
	}
	if err := checkSeatType(strings.ToUpper(*u.seatType)); err != nil {
		return failUsage(fs, "%v", err)
	}
	if !*u.dryRun {
		if err := requireNGCCreds(false, *u.apiKey, *u.org); err != nil {
			if !*u.jsonOutput {
				return failUploadUsageWithFix(fs, name, createAppRequired, fmt.Sprintf("%v", err), func() {
					printCredentialFix(os.Stderr)
				})
			}
			return failUploadUsage(fs, name, createAppRequired, "%v", err)
		}
	}
	return u.run("")
}

var updateAppRequired = []string{
	"--app-id ID               existing GDN application id",
}

var updateAppCommon = []string{
	"--instances NAMES                 comma-separated instance names (see \"gdn config\")",
	"--app-name NAME                   new application name (letters, digits, - and _)",
	"--description TEXT                new description",
	"--seat-type TYPE                  WINDOWS | LINUX | LINUX_PROTON",
	"--dry-run                         print what would change, make no changes (display-mode edits still need credentials)",
}

var updateAppAdvanced = []string{
	"--app-features JSON               app-level applicationFeatures as JSON (inline, @file, or -)",
	"--display-mode RES@FPS            allowed display mode, e.g. 4k@60; replaces the current list (repeatable)",
	"--add-display-mode RES@FPS        add a display mode to the current list (repeatable)",
	"--remove-display-mode RES@FPS     remove a display mode from the current list (repeatable)",
	"--max-bit-depth N                 max color bit depth: 8 or 10",
	"--chroma-format FMT               chroma format: YUV420 or YUV444",
	"--qos-policy POLICY               QoS policy: QUALITY or LATENCY",
	"--max-bitrate N                   max bit rate in Mbps",
	"--touch-support[=true|false]      enable/disable touch input",
	"--runtime-display-mode-change[=true|false]  allow runtime display mode changes",
	"--idle-timeout N                  idle timeout in seconds",
	"--session-time-limit N            session time limit in seconds",
	"--instance-reuse-policy POLICY    e.g. BALANCED",
}

// updateAppFlags are the flags of app update. Every field but --app-id is
// optional: the command sends only the ones actually passed, so the flag values
// are always read through the "was it set" map rather than compared to zero.
type updateAppFlags struct {
	appID       *string
	instances   stringList
	appName     *string
	description *string
	seatType    *string

	displayModes        displayModeList
	addDisplayModes     displayModeList
	removeDisplayModes  displayModeList
	resolution          *string
	maxFPS              *int
	maxBitDepth         *int
	chromaFormat        *string
	qosPolicy           *string
	maxBitRate          *int
	touchSupport        *bool
	runtimeDMC          *bool
	idleTimeout         *int
	sessionTimeLimit    *int
	instanceReusePolicy *string
	appFeaturesJSON     *string
	rtx                 *bool

	apiKey *string
	org    *string
	dryRun *bool
}

func registerUpdateAppFlags(fs *flag.FlagSet) *updateAppFlags {
	u := &updateAppFlags{}
	u.appID = fs.String(flagAppID, "", "existing GDN application id (required)")

	fs.Var(&u.instances, "instances", "comma-separated instance names (\"gdn config check\" lists the names GDN offers)")
	u.appName = fs.String(flagAppName, "", "new application name: 1-32 letters, digits, '-' or '_', starting with a letter or digit")
	u.description = fs.String("description", "", "new application description")
	u.seatType = fs.String(flagSeatType, "", "seat type: WINDOWS | LINUX | LINUX_PROTON")

	fs.Var(&u.displayModes, flagDisplayMode, "allowed display mode as RES@FPS, e.g. 4k@60; replaces the current list (repeatable)")
	fs.Var(&u.addDisplayModes, "add-display-mode", "add a display mode (RES@FPS) to the app's current list (repeatable)")
	fs.Var(&u.removeDisplayModes, "remove-display-mode", "remove a display mode (RES@FPS) from the app's current list (repeatable)")
	u.resolution = fs.String(flagResolution, "", "deprecated: use --display-mode RES@FPS")
	u.maxFPS = fs.Int(flagMaxFPS, 0, "deprecated: use --display-mode RES@FPS")
	u.maxBitDepth = fs.Int("max-bit-depth", 0, "max color bit depth: 8 or 10")
	u.chromaFormat = fs.String("chroma-format", "", "supported chroma format: YUV420 or YUV444")
	u.qosPolicy = fs.String("qos-policy", "", "QoS policy: QUALITY or LATENCY")
	u.maxBitRate = fs.Int("max-bitrate", 0, "max bit rate in Mbps (5-100)")
	u.touchSupport = fs.Bool("touch-support", false, "enable touch input support (use --touch-support=false to disable)")
	u.runtimeDMC = fs.Bool("runtime-display-mode-change", false, "allow runtime display mode changes (use =false to disable)")
	u.idleTimeout = fs.Int("idle-timeout", 0, "idle timeout in seconds")
	u.sessionTimeLimit = fs.Int("session-time-limit", 0, "session time limit in seconds")
	u.instanceReusePolicy = fs.String("instance-reuse-policy", "", "instance reuse policy: PERFORMANCE_OPTIMIZED, BALANCED, or SECURITY_OPTIMIZED")
	u.appFeaturesJSON = fs.String(flagAppFeatures, "", "app-level applicationFeatures as JSON (inline, @file, or - for stdin); individual feature flags override matching fields")
	u.rtx = fs.Bool("rtx", false, "deprecated and ignored: every GDN instance runs with RTX enabled")

	u.apiKey = ngcAPIKeyFromEnv()
	u.org = fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	u.dryRun = fs.Bool(flagDryRun, false, "print what would change and make no changes; --add-/--remove-display-mode still read the app, so they need credentials")
	hideFlagDefaults(fs, "org")
	return u
}

// request builds the PATCH body from the flags in set, which fs.Visit fills
// with only the flags the user actually passed.
func (u *updateAppFlags) request(set map[string]bool) (updateAppReq, error) {
	req := updateAppReq{}
	if set["rtx"] {
		warnRTXIgnored("--rtx")
	}
	if set[flagAppName] {
		if err := validateAppName(*u.appName); err != nil {
			return req, err
		}
		req.Name = *u.appName
	}
	if set["description"] {
		req.Description = *u.description
	}
	if set[flagSeatType] {
		req.SeatType = strings.ToUpper(*u.seatType)
		if err := checkSeatType(req.SeatType); err != nil {
			return req, err
		}
	}
	if set["instances"] {
		req.InstanceNames = u.instances
	}
	feat, err := u.features(set)
	if err != nil {
		return req, err
	}
	req.ApplicationFeatures = feat
	return req, nil
}

// features builds the applicationFeatures half of the body, or nil if nothing
// touched it.
func (u *updateAppFlags) features(set map[string]bool) (*appFeature, error) {
	feat := appFeature{}
	touched := false

	// A --app-features JSON blob provides the base; individual feature flags
	// below override matching fields.
	if *u.appFeaturesJSON != "" {
		if err := loadAppFeatures(*u.appFeaturesJSON, &feat); err != nil {
			return nil, err
		}
		if err := feat.validate(); err != nil {
			return nil, appFeaturesError(err)
		}
		touched = true
	}
	modes, err := u.displayModesToSet(set)
	if err != nil {
		return nil, err
	}
	if modes != nil {
		if feat.DisplayMode != nil {
			return nil, fmt.Errorf("--display-mode and DISPLAY_MODE in --%s both set the display modes; use one", flagAppFeatures)
		}
		feat.DisplayMode = modes
		touched = true
	}
	if u.applyStreamingFeatures(set, &feat) {
		touched = true
	}
	if u.applySessionFeatures(set, &feat) {
		touched = true
	}
	if !touched || feat.isEmpty() {
		return nil, nil
	}
	if err := feat.validate(); err != nil {
		return nil, err
	}
	return &feat, nil
}

// displayModesToSet returns the list --display-mode replaces the current one
// with, or nil when it was not given. The deprecated --resolution and --max-fps
// still work, but only together, as one mode.
func (u *updateAppFlags) displayModesToSet(set map[string]bool) ([]displayMode, error) {
	legacy := set[flagResolution] || set[flagMaxFPS]
	switch {
	case legacy && len(u.displayModes) > 0:
		return nil, fmt.Errorf("--resolution and --max-fps cannot be combined with --display-mode")
	case legacy && !(set[flagResolution] && set[flagMaxFPS]):
		return nil, fmt.Errorf("--resolution and --max-fps must be given together; use --display-mode RES@FPS to set the list, or --add-display-mode / --remove-display-mode to change one mode")
	case legacy:
		m, err := parseDisplayMode(fmt.Sprintf("%s@%d", *u.resolution, *u.maxFPS))
		if err != nil {
			return nil, err
		}
		warnf("--resolution and --max-fps are deprecated; use --display-mode %s", m)
		return []displayMode{m}, nil
	case len(u.displayModes) > 0:
		return u.displayModes, nil
	}
	return nil, nil
}

// applyStreamingFeatures sets the picture-quality fields and reports whether it
// set any.
func (u *updateAppFlags) applyStreamingFeatures(set map[string]bool, feat *appFeature) bool {
	touched := false
	if set["max-bit-depth"] {
		v := uint64(*u.maxBitDepth)
		feat.MaxBitDepth = &v
		touched = true
	}
	if set["chroma-format"] {
		feat.SupportedChromaFormat = strings.ToUpper(*u.chromaFormat)
		touched = true
	}
	if set["qos-policy"] {
		feat.QosPolicy = strings.ToUpper(*u.qosPolicy)
		touched = true
	}
	if set["max-bitrate"] {
		v := uint64(*u.maxBitRate)
		feat.MaxBitRate = &v
		touched = true
	}
	return touched
}

// applySessionFeatures sets the session-behaviour fields and reports whether it
// set any.
func (u *updateAppFlags) applySessionFeatures(set map[string]bool, feat *appFeature) bool {
	touched := false
	if set["touch-support"] {
		v := *u.touchSupport
		feat.TouchSupport = &v
		touched = true
	}
	if set["runtime-display-mode-change"] {
		v := *u.runtimeDMC
		feat.RuntimeDisplayModeChange = &v
		touched = true
	}
	if set["idle-timeout"] {
		v := uint64(*u.idleTimeout)
		feat.IdleTimeout = &v
		touched = true
	}
	if set["session-time-limit"] {
		v := uint64(*u.sessionTimeLimit)
		feat.SessionTimeLimit = &v
		touched = true
	}
	if set["instance-reuse-policy"] {
		feat.InstanceReusePolicy = strings.ToUpper(*u.instanceReusePolicy)
		touched = true
	}
	return touched
}

// applyDisplayModeEdits reads the app's current display modes, applies
// --add-display-mode and --remove-display-mode, and sets the result in req when
// it differs. It returns the current list.
func (u *updateAppFlags) applyDisplayModeEdits(ctx context.Context, portal *portalClient, req *updateAppReq) ([]displayMode, error) {
	if len(u.addDisplayModes) > 0 {
		if err := portal.checkDisplayModes(ctx, u.addDisplayModes); err != nil {
			return nil, err
		}
	}
	app, err := portal.getApp(ctx, *u.appID)
	if err != nil {
		return nil, err
	}
	before := app.ApplicationFeatures.DisplayMode
	if before == nil {
		before = []displayMode{}
	}
	after, notes, err := editDisplayModes(before, u.addDisplayModes, u.removeDisplayModes)
	for _, n := range notes {
		warnf("%s", n)
	}
	if err != nil {
		return nil, err
	}
	if !slices.Equal(after, before) {
		if req.ApplicationFeatures == nil {
			req.ApplicationFeatures = &appFeature{}
		}
		req.ApplicationFeatures.DisplayMode = after
	}
	return before, nil
}

// printUpdateAppDryRun prints the update. displayModesBefore is the app's
// current list when the command read it, so the change shows as before/after.
func printUpdateAppDryRun(appID string, req updateAppReq, displayModesBefore []displayMode) {
	fmt.Printf("[dry-run] would update app %s:\n", appID)
	if req.Name != "" {
		fmt.Printf("  app-name: %s\n", req.Name)
	}
	if req.Description != "" {
		fmt.Printf("  description: %s\n", req.Description)
	}
	if req.SeatType != "" {
		fmt.Printf("  seat-type: %s\n", req.SeatType)
	}
	if len(req.InstanceNames) > 0 {
		fmt.Printf("  instances: %s\n", strings.Join(req.InstanceNames, ", "))
	}
	if req.ApplicationFeatures != nil {
		printUpdateAppFeatureDryRun(req.ApplicationFeatures, displayModesBefore)
	}
}

func printUpdateAppFeatureDryRun(f *appFeature, displayModesBefore []displayMode) {
	switch {
	case len(f.DisplayMode) > 0 && displayModesBefore != nil:
		fmt.Printf("  display modes:\n    before: %s\n    after:  %s\n", formatDisplayModes(displayModesBefore), formatDisplayModes(f.DisplayMode))
	case len(f.DisplayMode) > 0:
		fmt.Printf("  display modes: %s (replaces the current list)\n", formatDisplayModes(f.DisplayMode))
	}
	if f.MaxBitDepth != nil {
		fmt.Printf("  max-bit-depth: %d\n", *f.MaxBitDepth)
	}
	if f.SupportedChromaFormat != "" {
		fmt.Printf("  chroma-format: %s\n", f.SupportedChromaFormat)
	}
	if f.QosPolicy != "" {
		fmt.Printf("  qos-policy: %s\n", f.QosPolicy)
	}
	if f.MaxBitRate != nil {
		fmt.Printf("  max-bitrate: %d Mbps\n", *f.MaxBitRate)
	}
	if f.TouchSupport != nil {
		fmt.Printf("  touch-support: %t\n", *f.TouchSupport)
	}
	if f.RuntimeDisplayModeChange != nil {
		fmt.Printf("  runtime-display-mode-change: %t\n", *f.RuntimeDisplayModeChange)
	}
	if f.IdleTimeout != nil {
		fmt.Printf("  idle-timeout: %ds\n", *f.IdleTimeout)
	}
	if f.SessionTimeLimit != nil {
		fmt.Printf("  session-time-limit: %ds\n", *f.SessionTimeLimit)
	}
	if f.InstanceReusePolicy != "" {
		fmt.Printf("  instance-reuse-policy: %s\n", f.InstanceReusePolicy)
	}
	if tp := f.TransportPolicy; tp != nil {
		fmt.Printf("  transport-policy: %s, relay %s via %s\n", tp.Policy, tp.RelayProtocol, tp.RelayLocation)
	}
	if cp := f.CoverageAreaPolicy; cp != nil {
		fmt.Printf("  coverage-area-policy: %s\n", cp.describe())
	}
}

func runUpdateApp(args []string) error {
	fs := flag.NewFlagSet("app update", flag.ContinueOnError)
	u := registerUpdateAppFlags(fs)

	fs.Usage = commandHelpWithExamples(fs, "app update", "Update an existing GDN application. All fields are optional; only the flags you pass are sent to the API.",
		[]string{
			"gdn app update --app-id <id> --instances NVIDIA_L40S",
			"gdn app update --app-id <id> --description \"Stable test deployment\"",
			"gdn app update --app-id <id> --dry-run --seat-type WINDOWS",
			"gdn app update --app-id <id> --add-display-mode 1440p@90 --dry-run",
		},
		helpSection{"Required", updateAppRequired},
		helpSection{"Common", updateAppCommon},
		helpSection{"Advanced", updateAppAdvanced},
	)

	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	if *u.appID == "" {
		return failMissingAppID(fs)
	}
	editing := len(u.addDisplayModes) > 0 || len(u.removeDisplayModes) > 0
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	req, err := u.request(set)
	if err != nil {
		return err
	}
	if done, err := u.checkRequest(fs, req, set, editing); done || err != nil {
		return err
	}

	if *u.dryRun && !editing {
		printUpdateAppDryRun(*u.appID, req, nil)
		return nil
	}
	return u.apply(req, editing)
}

// checkRequest rejects an app update request that cannot be sent. done means
// there is nothing more to do: either err is set, or there is nothing to send
// and a message saying so has been printed.
func (u *updateAppFlags) checkRequest(fs *flag.FlagSet, req updateAppReq, set map[string]bool, editing bool) (done bool, err error) {
	if editing && req.ApplicationFeatures != nil && req.ApplicationFeatures.DisplayMode != nil {
		return true, failUsage(fs, "--add-display-mode and --remove-display-mode change the current list; they cannot be combined with --display-mode, --resolution/--max-fps or DISPLAY_MODE in --app-features, which replace it")
	}
	// Editing the display mode list reads the app, so it needs credentials
	// even for a dry run.
	if err := requireNGCCreds(*u.dryRun && !editing, *u.apiKey, *u.org); err != nil {
		return true, failUsageWithFix(fs, err.Error(), func() {
			printCredentialFix(os.Stderr)
		})
	}
	if !req.isEmpty() || editing {
		return false, nil
	}
	if set["rtx"] || set[flagAppFeatures] {
		fmt.Println("Nothing to update: the given features leave no field to send")
		return true, nil
	}
	return true, failUsage(fs, "no fields to update; pass at least one optional flag (run \"gdn app update -h\" for the full list)")
}

// apply sends a checked app update request, first applying any display mode
// edits to the app's current list. A dry run stops before the update.
func (u *updateAppFlags) apply(req updateAppReq, editing bool) error {
	ctx, stop := signalCtx()
	defer stop()

	portal := newPortal(envOr("NGC_BASE_URL", ""), *u.apiKey, *u.org, secureClient)
	displayModesBefore, err := u.resolveDisplayModes(ctx, portal, &req, editing)
	if err != nil {
		return err
	}
	if editing && req.isEmpty() {
		fmt.Println("Nothing to update: the display modes already match")
		return nil
	}
	if *u.dryRun {
		printUpdateAppDryRun(*u.appID, req, displayModesBefore)
		return nil
	}
	if len(req.InstanceNames) > 0 {
		if req.InstanceNames, err = portal.resolveInstanceNames(ctx, req.InstanceNames); err != nil {
			return err
		}
	}
	app, err := portal.updateApp(ctx, *u.appID, req)
	if err != nil {
		return err
	}
	fmt.Printf("Updated application %s (%s)\n", app.Name, app.ID)
	if f := req.ApplicationFeatures; displayModesBefore != nil && f != nil && len(f.DisplayMode) > 0 {
		fmt.Printf("  display modes: %s\n", formatDisplayModes(f.DisplayMode))
	}
	return nil
}

// resolveDisplayModes settles the display modes req sends. When editing, it
// applies --add-display-mode and --remove-display-mode to the app's current
// list and returns that list as it was; otherwise it checks any list being set
// against the modes GDN offers.
func (u *updateAppFlags) resolveDisplayModes(ctx context.Context, portal *portalClient, req *updateAppReq, editing bool) ([]displayMode, error) {
	if editing {
		return u.applyDisplayModeEdits(ctx, portal, req)
	}
	if f := req.ApplicationFeatures; f != nil && len(f.DisplayMode) > 0 {
		return nil, portal.checkDisplayModes(ctx, f.DisplayMode)
	}
	return nil, nil
}

// cloneAppFlags are the flags of app clone. Everything but --app-id overrides
// a setting the clone would otherwise inherit from the source app.
type cloneAppFlags struct {
	appID            *string
	cloneName        *string
	description      *string
	seatType         *string
	instances        stringList
	resources        resourceRefList
	executable       *string
	execPath         *string
	startupResource  *string
	execArgs         stringList
	envVars          envVarList
	launchConfigJSON *string
	appFeaturesJSON  *string
	apiKey           *string
	org              *string
	dryRun           *bool
}

func registerCloneAppFlags(fs *flag.FlagSet) *cloneAppFlags {
	c := &cloneAppFlags{}
	c.appID = fs.String(flagAppID, "", "existing GDN application id to clone (required)")
	c.cloneName = fs.String("name", "", "name for the clone (defaults to \"<source>-N\")")
	c.description = fs.String("description", "", "override the clone's description")
	c.seatType = fs.String(flagSeatType, "", "override the clone's seat type: WINDOWS | LINUX | LINUX_PROTON")
	fs.Var(&c.instances, "instances", "override the clone's instance names (comma-separated)")
	fs.Var(&c.resources, "resource", "retarget the clone to reference these resources as [<team>/]<name>:<version> (repeatable)")
	c.executable = fs.String("executable", "", "override the executable path (relative to the startup resource)")
	c.execPath = fs.String("exec-path", "", "alias for --executable when retargeting resources")
	c.startupResource = fs.String("startup-resource", "", "resource that holds the startup file (defaults to the first artifact)")
	fs.Var(&c.execArgs, "exec-args", "override the executable launch arguments (comma-separated)")
	fs.Var(&c.envVars, "env-var", "override launch environment variables as <key>:<value> (repeatable)")
	c.launchConfigJSON = fs.String("launch-config", "", "override the per-version launchConfig as JSON (inline, @file, or -)")
	c.appFeaturesJSON = fs.String(flagAppFeatures, "", "override app-level applicationFeatures as JSON (inline, @file, or -)")
	c.apiKey = ngcAPIKeyFromEnv()
	c.org = fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	c.dryRun = fs.Bool(flagDryRun, false, "fetch the source app and print the clone plan, make no write API calls")
	hideFlagDefaults(fs, "org")
	return c
}

// takenAppNames is the lowercased set of names already in use in the org,
// which is what a generated "<source>-N" clone name has to avoid.
func takenAppNames(ctx context.Context, portal *portalClient) (map[string]bool, error) {
	apps, err := portal.listApps(ctx)
	if err != nil {
		return nil, err
	}
	taken := make(map[string]bool, len(apps))
	for _, a := range apps {
		taken[strings.ToLower(a.Name)] = true
	}
	return taken, nil
}

// resolveName returns the clone's name: --name when it is usable, otherwise the
// next free "<source>-N".
func (c *cloneAppFlags) resolveName(srcName string, taken map[string]bool) (string, error) {
	if *c.cloneName == "" {
		name, err := pickCloneName(srcName, taken)
		if err != nil {
			return "", err
		}
		if err := validateAppName(name); err != nil {
			return "", fmt.Errorf("cannot clone %q: %w; pass --name", srcName, err)
		}
		return name, nil
	}
	if err := validateAppName(*c.cloneName); err != nil {
		return "", err
	}
	if taken[strings.ToLower(*c.cloneName)] {
		return "", fmt.Errorf("an application named %q already exists; choose a different --name", *c.cloneName)
	}
	return *c.cloneName, nil
}

// cloneSourceVersion returns the version the clone copies. The app payload
// embeds the current version in full (launchConfig and artifacts included), so
// the extra fetch is only a fallback for a slim embedded version; the clone
// must reference the same artifact either way.
func cloneSourceVersion(ctx context.Context, portal *portalClient, src *gdnApp) (*gdnVersion, error) {
	srcVer := src.defaultVersion()
	if srcVer == nil {
		return nil, fmt.Errorf("cannot clone %q (%s): application has no versions to copy", src.Name, src.ID)
	}
	if len(srcVer.Artifacts) == 0 || srcVer.LaunchConfig.ExecutablePath == "" {
		full, err := portal.getVersion(ctx, src.ID, srcVer.ID)
		if err != nil {
			return nil, err
		}
		srcVer = full
	}
	if len(srcVer.Artifacts) == 0 {
		return nil, fmt.Errorf("cannot clone %q (%s): source version %s has no artifacts to reference", src.Name, src.ID, srcVer.ID)
	}
	return srcVer, nil
}

// cloneArtifacts returns the artifact refs for the clone and the type they
// share. --resource replaces them outright; otherwise they are rebuilt from the
// source version.
//
// The version GET returns artifacts with a "uri" but no "orgName"; the create
// endpoint rejects that ("either orgName or uri must be set"), so each ref is
// rebuilt with orgName like app create does. The resource itself is untouched --
// the clone just references the same name/version.
func (c *cloneAppFlags) cloneArtifacts(srcVer *gdnVersion) ([]artifactRef, string) {
	srcType := firstNonEmpty(srcVer.Artifacts[0].Type, "RESOURCE")
	if len(c.resources) > 0 {
		artifacts := make([]artifactRef, 0, len(c.resources))
		for _, ref := range c.resources {
			artifacts = append(artifacts, artifactRef{
				Name:      ref.Name,
				OrgName:   *c.org,
				TeamName:  ref.Team,
				Type:      srcType,
				VersionID: ref.Version,
			})
		}
		return artifacts, srcType
	}
	artifacts := make([]artifactRef, len(srcVer.Artifacts))
	for i, a := range srcVer.Artifacts {
		artifacts[i] = artifactRef{
			Name:      a.Name,
			OrgName:   firstNonEmpty(a.OrgName, *c.org),
			TeamName:  a.TeamName,
			Type:      a.Type,
			VersionID: a.VersionID,
		}
	}
	return artifacts, srcType
}

// cloneLaunchConfig starts from the source version's launch config and applies
// the --launch-config, --executable, --exec-args, and --env-var overrides.
func (c *cloneAppFlags) cloneLaunchConfig(srcVer *gdnVersion, artifacts []artifactRef, srcType string) (launchConfig, error) {
	launch := srcVer.LaunchConfig
	if *c.launchConfigJSON != "" {
		var lc launchConfig
		if err := loadJSONInto(*c.launchConfigJSON, "--launch-config", &lc); err != nil {
			return launch, err
		}
		launch = lc
	}
	if raw := firstNonEmpty(*c.execPath, *c.executable); raw != "" {
		startupName := firstNonEmpty(*c.startupResource, artifacts[0].Name)
		launch.ExecutablePath = gdnLaunchPath(coordinates{Type: srcType, Name: startupName}, raw)
	}
	if len(c.execArgs) > 0 {
		launch.ExecutableArgs = c.execArgs
	}
	if len(c.envVars) > 0 {
		launch.Environment = c.envVars
	}
	return launch, nil
}

// cloneFeatures is the source app's applicationFeatures with --app-features
// applied over the top. A policy object in --app-features merges into the
// source's, so it may name only the members that change.
func (c *cloneAppFlags) cloneFeatures(src *gdnApp) (appFeature, error) {
	features := src.ApplicationFeatures
	if *c.appFeaturesJSON == "" {
		return features, nil
	}
	if err := loadAppFeatures(*c.appFeaturesJSON, &features); err != nil {
		return features, err
	}
	if err := features.validate(); err != nil {
		return features, appFeaturesError(err)
	}
	return features, nil
}

func printCloneAppDryRun(src *gdnApp, req createAppReq) {
	fmt.Printf("[dry-run] would clone app %s (%q) into a new app:\n", src.ID, src.Name)
	fmt.Printf("  app-name: %s\n", req.Name)
	if req.SeatType != "" {
		fmt.Printf("  seat-type: %s\n", req.SeatType)
	}
	if len(req.InstanceNames) > 0 {
		fmt.Printf("  instances: %s\n", strings.Join(req.InstanceNames, ", "))
	}
	for _, a := range req.Version.Artifacts {
		fmt.Printf("  artifact: %s\n", a.ngcURI())
	}
	if req.Version.LaunchConfig.ExecutablePath != "" {
		fmt.Printf("  launch: %s\n", req.Version.LaunchConfig.ExecutablePath)
	}
}

// promoteInitialVersion makes a freshly created app's first version its
// production (default) version.
func promoteInitialVersion(ctx context.Context, portal *portalClient, app *gdnApp) error {
	versionID := app.initialVersionID()
	if versionID == "" {
		return nil
	}
	return portal.setDefaultVersion(ctx, app.ID, versionID)
}

func runCloneApp(args []string) error {
	fs := flag.NewFlagSet("app clone", flag.ContinueOnError)
	c := registerCloneAppFlags(fs)
	fs.Usage = commandHelpWithExamples(fs, "app clone", "Clone an existing GDN application. By default the clone keeps all the same "+
		"settings and reuses the same NGC artifact/version; pass overrides to change the clone in one step.",
		[]string{
			"gdn app clone --app-id <id>",
			"gdn app clone --app-id <id> --name my-app-1-qa --resource my-app-1:1.1.0",
			"gdn app clone --app-id <id> --dry-run",
		},
		helpSection{"Required", []string{
			"--app-id ID             existing GDN application id to clone",
			helpLineOrg,
		}},
		helpSection{"Common", []string{
			"--name NAME             name for the clone (defaults to \"<source>-N\")",
			"--resource NAME:VER     retarget the clone to these resource versions, as [team/]name:version (repeatable)",
			"--dry-run               fetch the source app and print the clone plan",
		}},
		helpSection{"Advanced", []string{
			"--description TEXT      override the clone's description",
			"--seat-type TYPE        override the seat type",
			"--instances NAMES       override the instance names",
			"--executable PATH       override the executable path (relative to the startup resource)",
			"--startup-resource NAME resource that holds the startup file",
			"--exec-args ARGS        override the executable launch arguments",
			"--env-var KEY:VALUE     override launch environment variables (repeatable)",
			"--launch-config JSON    override the launchConfig as JSON (inline, @file, or -)",
			"--app-features JSON     override applicationFeatures as JSON (inline, @file, or -)",
		}},
	)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	if *c.appID == "" {
		return failMissingAppID(fs)
	}
	if err := requireNGCCreds(false, *c.apiKey, *c.org); err != nil {
		return failUsageWithFix(fs, err.Error(), func() {
			printCredentialFix(os.Stderr)
		})
	}

	ctx, stop := signalCtx()
	defer stop()

	portal := newPortal(envOr("NGC_BASE_URL", ""), *c.apiKey, *c.org, secureClient)
	src, err := portal.getApp(ctx, *c.appID)
	if err != nil {
		return err
	}
	taken, err := takenAppNames(ctx, portal)
	if err != nil {
		return err
	}
	newName, err := c.resolveName(src.Name, taken)
	if err != nil {
		return err
	}
	srcVer, err := cloneSourceVersion(ctx, portal, src)
	if err != nil {
		return err
	}

	artifacts, srcType := c.cloneArtifacts(srcVer)
	launch, err := c.cloneLaunchConfig(srcVer, artifacts, srcType)
	if err != nil {
		return err
	}
	features, err := c.cloneFeatures(src)
	if err != nil {
		return err
	}
	if err := checkSeatType(strings.ToUpper(*c.seatType)); err != nil {
		return err
	}

	req := createAppReq{
		Name:                newName,
		Description:         firstNonEmpty(*c.description, src.Description),
		SeatType:            firstNonEmpty(strings.ToUpper(*c.seatType), src.SeatType),
		InstanceNames:       src.InstanceNames,
		ApplicationFeatures: &features,
		Version: createVersionReq{
			VersionName:  srcVer.VersionName,
			LaunchConfig: launch,
			Artifacts:    artifacts,
		},
	}
	if len(c.instances) > 0 {
		req.InstanceNames = c.instances
	}
	if *c.dryRun {
		printCloneAppDryRun(src, req)
		return nil
	}
	if req.InstanceNames, err = portal.resolveInstanceNames(ctx, req.InstanceNames); err != nil {
		return err
	}

	clone, err := portal.createApp(ctx, req)
	if err != nil {
		return err
	}
	if err := promoteInitialVersion(ctx, portal, clone); err != nil {
		return err
	}
	fmt.Printf("Cloned application %s (%s) -> %s (%s)\n", src.Name, src.ID, clone.Name, clone.ID)
	return nil
}

func runAddVersion(args []string) error {
	const name = "app version add"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	u := registerUploadFlags(fs)
	setFlagUsage(fs, flagAppName, "application display name; used to derive NGC artifact name when --name is omitted")
	setFlagUsage(fs, "description", usageIgnoredOnAdd)
	setFlagUsage(fs, "instances", usageIgnoredOnAdd)
	setFlagUsage(fs, flagSeatType, usageIgnoredOnAdd)
	setFlagUsage(fs, flagDisplayMode, usageIgnoredOnAdd)
	hideFlagDefault(fs, flagSeatType)
	appID := fs.String(flagAppID, "", "existing GDN application id to add the version to (required)")
	fs.Usage = uploadHelp(fs, name, "Upload an app folder to NGC and add a version to an existing GDN application, or reference existing resources.", addVersionHelpSections, []string{
		"gdn app version add --app-id <id> --dir ./my-app --version 1.1.0 --app-name my-app-1",
		"gdn app version add --app-id <id> --version 1.1.0 --resource my-app-1:1.1.0 --exec-path app.exe",
		"gdn app version add --app-id <id> --dir ./my-app --version 1.1.0 --name my-app-1",
	})
	args = repairDirArgs(args)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	normalizeDirFlag(u.dir)

	uploadMode := *u.dir != ""
	required := map[string]string{"version": *u.version}
	if len(u.resources) == 0 {
		required["dir"] = *u.dir
	}
	if missing := missingFlags(required); len(missing) > 0 {
		return failMissingRequiredUploadFlags(fs, args, required)
	}
	if *appID == "" {
		return failUploadUsageWithFix(fs, name, addVersionRequired, "--app-id is required", func() {
			printCommandHints(os.Stderr, "Fix",
				commandHint{Text: "List apps to find an app ID", Command: "gdn app list"},
				commandHint{Text: "Create the app first if it does not exist", Command: "gdn app create --dir <path> --app-name <name> --version <ver>"},
			)
		})
	}
	if uploadMode && *u.appName == "" && *u.artifact == "" {
		return failUploadUsageWithFix(fs, name, addVersionRequired, "--app-name or --name is required", func() {
			printCommandHints(os.Stderr, "Fix",
				commandHint{Text: "Pass the application name or artifact name", Command: "gdn app version add --app-id <id> --dir <path> --version <ver> --app-name <name>"},
			)
		})
	}
	if err := u.requireStartupWhenReferenceOnly(); err != nil {
		return failUsageWithFix(fs, err.Error(), func() {
			printCommandHints(os.Stderr, "Fix",
				commandHint{Text: "Name the executable inside the resource", Command: "gdn app version add --app-id <id> --version <ver> --resource <name>:<ver> --exec-path <path>"},
			)
		})
	}
	if err := requireNGCCreds(*u.dryRun, *u.apiKey, *u.org); err != nil {
		if !*u.jsonOutput {
			return failUploadUsageWithFix(fs, name, addVersionRequired, fmt.Sprintf("%v", err), func() {
				printCredentialFix(os.Stderr)
			})
		}
		return failUploadUsage(fs, name, addVersionRequired, "%v", err)
	}
	if w, ok := ignoredFlagsWarning(fs, "adding a version", "instances", flagSeatType, "description", flagAppFeatures, flagDisplayMode); ok {
		emitWarning(w)
	}
	return u.run(*appID)
}

func runUpload(args []string) error {
	const name = "resource upload"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	dir := fs.String("dir", "", "path to the folder to upload (required)")
	version := fs.String("version", "", "artifact version, e.g. 1.0.0 (required)")
	artifact := fs.String("name", "", "NGC artifact name (required)")
	displayName := fs.String("display-name", "", "NGC resource display name (defaults to --name)")
	description := fs.String("description", "", "NGC resource/version description")
	apiKey := ngcAPIKeyFromEnv()
	org := fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	setLatest := fs.Bool(flagSetLatest, true, "mark the NGC version as latest on commit")
	hashWorkers := fs.Int(flagHashWorkers, defaultHashWorkers, usageHashWorkers)
	uploadWorkers := fs.Int(flagMultipartWorkers, defaultMultipartWorkers, usageMultipartWorkers)
	fileWorkers := fs.Int(flagFileWorkers, defaultFileWorkers, usageFileWorkers)
	dryRun := fs.Bool(flagDryRun, false, usageDryRunScan)
	jsonOutput := fs.Bool("json", false, usageJSONResult)
	hideFlagDefaults(fs, "org")
	fs.Usage = uploadHelp(fs, name, "Upload a folder to NGC as an artifact version. Does not create a GDN application.", uploadHelpSections, []string{
		"gdn resource upload --dir ./my-app --name my-app-1 --version 1.0.0",
		"gdn resource upload --dir ./my-app --name my-app-1 --version 1.0.0 --dry-run",
	})
	args = repairDirArgs(args)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	normalizeDirFlag(dir)

	if missing := missingFlags(map[string]string{
		"dir": *dir, "version": *version, "name": *artifact,
	}); len(missing) > 0 {
		return failMissingRequiredUploadFlags(fs, args, map[string]string{
			"dir": *dir, "version": *version, "name": *artifact,
		})
	}
	if err := requireNGCCreds(*dryRun, *apiKey, *org); err != nil {
		if !*jsonOutput {
			return failUploadUsageWithFix(fs, name, uploadRequired, fmt.Sprintf("%v", err), func() {
				printCredentialFix(os.Stderr)
			})
		}
		return failUploadUsage(fs, name, uploadRequired, "%v", err)
	}

	start := time.Now()
	ctx, stop := signalCtx()
	defer stop()
	log := logf
	if *jsonOutput {
		log = logStderr
	}
	coords := coordinates{
		Org:     firstNonEmpty(*org, dryRunPlaceholder),
		Type:    "RESOURCE",
		Name:    *artifact,
		Version: *version,
	}
	ngc := newNGCForUpload(envOr("NGC_BASE_URL", ""), firstNonEmpty(*apiKey, dryRunPlaceholder), coords)

	res, err := uploadToNGC(ctx, ngc, options{
		AppDir:        *dir,
		DisplayName:   firstNonEmpty(*displayName, *artifact),
		Description:   *description,
		SetLatest:     *setLatest,
		DryRun:        *dryRun,
		HashWorkers:   *hashWorkers,
		UploadWorkers: *uploadWorkers,
		FileWorkers:   *fileWorkers,
	}, log)
	if err != nil {
		return err
	}

	return printCommandOutput(res, time.Since(start), *jsonOutput, "", false)
}

func runUpdateResource(args []string) error {
	const name = "resource update"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	var files fileSpecList
	fs.Var(&files, "file", "local file to add (PATH or LOCAL=REMOTE); repeatable")
	dir := fs.String("dir", "", "folder of files to add (paths relative to the folder)")
	version := fs.String("version", "", "new artifact version to create, e.g. 1.1.0 (required)")
	baseVersion := fs.String("base-version", "", "existing UPLOAD_COMPLETE version to inherit (required)")
	artifact := fs.String("name", "", "NGC artifact name (required)")
	displayName := fs.String("display-name", "", "NGC resource display name (defaults to --name)")
	description := fs.String("description", "", "NGC resource/version description")
	apiKey := ngcAPIKeyFromEnv()
	org := fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	setLatest := fs.Bool(flagSetLatest, true, "mark the NGC version as latest on complete")
	hashWorkers := fs.Int(flagHashWorkers, defaultHashWorkers, usageHashWorkers)
	uploadWorkers := fs.Int(flagMultipartWorkers, defaultMultipartWorkers, usageMultipartWorkers)
	fileWorkers := fs.Int(flagFileWorkers, defaultFileWorkers, usageFileWorkers)
	dryRun := fs.Bool(flagDryRun, false, usageDryRunScan)
	jsonOutput := fs.Bool("json", false, usageJSONResult)
	hideFlagDefaults(fs, "org")
	fs.Usage = uploadHelp(fs, name,
		"Create a new NGC resource version that inherits all files from --base-version, then uploads only the files given via --file and/or --dir. Does not create a GDN application.",
		updateHelpSections, []string{
			"gdn resource update --name my-app-1 --base-version 1.0.0 --version 1.0.1 --file ./install.cmd",
			"gdn resource update --name my-app-1 --base-version 1.0.0 --version 1.0.1 --dir ./patch",
			"gdn resource update --name my-app-1 --base-version 1.0.0 --version 1.0.1 --file ./a.exe=bin/a.exe",
		})
	args = repairDirArgs(args)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}
	normalizeDirFlag(dir)

	if missing := missingFlags(map[string]string{
		"version": *version, "base-version": *baseVersion, "name": *artifact,
	}); len(missing) > 0 {
		return failUploadUsage(fs, name, updateRequired, "missing required flags: %s", strings.Join(missing, ", "))
	}
	if *dir == "" && len(files) == 0 {
		return failUploadUsage(fs, name, updateRequired, "pass --file and/or --dir with the files to add")
	}
	if err := requireNGCCreds(*dryRun, *apiKey, *org); err != nil {
		if !*jsonOutput {
			return failUploadUsageWithFix(fs, name, updateRequired, fmt.Sprintf("%v", err), func() {
				printCredentialFix(os.Stderr)
			})
		}
		return failUploadUsage(fs, name, updateRequired, "%v", err)
	}

	start := time.Now()
	ctx, stop := signalCtx()
	defer stop()
	log := logf
	if *jsonOutput {
		log = logStderr
	}
	coords := coordinates{
		Org:     firstNonEmpty(*org, dryRunPlaceholder),
		Type:    "RESOURCE",
		Name:    *artifact,
		Version: *version,
	}
	ngc := newNGCForUpload(envOr("NGC_BASE_URL", ""), firstNonEmpty(*apiKey, dryRunPlaceholder), coords)

	res, err := updateToNGC(ctx, ngc, updateOptions{
		BaseVersion:   *baseVersion,
		Dir:           *dir,
		Files:         []string(files),
		DisplayName:   firstNonEmpty(*displayName, *artifact),
		Description:   *description,
		SetLatest:     *setLatest,
		DryRun:        *dryRun,
		HashWorkers:   *hashWorkers,
		UploadWorkers: *uploadWorkers,
		FileWorkers:   *fileWorkers,
	}, log)
	if err != nil {
		return err
	}

	return printCommandOutput(res, time.Since(start), *jsonOutput, "", false)
}

// downloadResult is the JSON summary for "resource download --json".
type downloadResult struct {
	ArtifactURI string             `json:"artifact_uri"`
	OutDir      string             `json:"out_dir"`
	Files       []downloadFileInfo `json:"files"`
	TotalBytes  int64              `json:"total_bytes"`
	// Files left alone because the local copy already matched, and the bytes
	// that therefore never crossed the wire. Both are omitted when nothing was
	// skipped, so the plain download summary is unchanged.
	SkippedFiles int   `json:"skipped_files,omitempty"`
	SkippedBytes int64 `json:"skipped_bytes,omitempty"`
}

type downloadFileInfo struct {
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	Skipped bool   `json:"skipped,omitempty"`
}

func runDownloadResource(args []string) error {
	const name = "resource download"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	artifact := fs.String("name", "", "NGC artifact (resource) name (defaults to --app-name slug)")
	appName := fs.String(flagAppName, "", "application display name, used to derive the artifact name")
	version := fs.String("version", "", "resource version to download (required)")
	out := fs.String("out", ".", "output directory for downloaded files")
	var paths stringList
	fs.Var(&paths, "path", "download only the file at this path, or files under this directory (repeatable; default: all files)")
	apiKey := ngcAPIKeyFromEnv()
	org := fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	asJSON := fs.Bool("json", false, "output the download summary as JSON")
	dryRun := fs.Bool(flagDryRun, false, "list the files that would be downloaded, make no downloads")
	skipExisting := fs.Bool(flagSkipExisting, false, usageSkipExisting)
	fileWorkers := fs.Int(flagFileWorkers, defaultDownloadWorkers, usageDownloadWorkers)
	chunkWorkers := fs.Int(flagChunkWorkers, defaultDownloadChunkWorkers, usageChunkWorkers)
	hideFlagDefaults(fs, "org", "out")
	fs.Usage = commandHelpWithExamples(fs, name, "Download the files of an NGC resource version to a local directory.",
		[]string{
			"gdn resource download --name my-app-1 --version 1.0.0 --out ./my-app-1",
			"gdn resource download --name my-app-1 --version 1.0.0 --path config.json",
			"gdn resource download --name my-app-1 --version 1.0.0 --dry-run",
		},
		helpSection{"Required", []string{
			"--name NAME             NGC resource name, or --app-name to derive it",
			"--version VERSION       resource version to download",
			helpLineOrg,
		}},
		helpSection{"Common", []string{
			"--out DIR               output directory (default current directory)",
			"--path PATH             download only this file or directory (repeatable)",
			"--skip-existing         leave files whose local copy already matches",
			helpLineJSONMachine,
			"--dry-run               list files without downloading",
		}},
		helpSection{"Advanced", []string{
			"--file-workers int      parallel file downloads (default 20; auto-throttles on NGC 429s)",
			"--chunk-workers int     parallel range downloads per large file (default 8; 0 = one per CPU)",
		}},
	)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}

	resource := firstNonEmpty(*artifact, slug(*appName))
	if resource == "" {
		return failUsageWithFix(fs, "--name or --app-name is required", func() {
			printCommandHints(os.Stderr, "Fix",
				commandHint{Text: "List resources to find an artifact name", Command: cmdResourceList},
			)
		})
	}
	if err := validateNGCArtifactName(resource); err != nil {
		return err
	}
	if err := validateCoordinates(coordinates{Org: *org, Version: *version}); err != nil {
		return err
	}
	if *version == "" {
		return failUsageWithFix(fs, "--version is required", func() {
			printCommandHints(os.Stderr, "Fix",
				commandHint{Text: "List resources to find a version", Command: cmdResourceList},
			)
		})
	}
	if err := requireNGCCreds(false, *apiKey, *org); err != nil {
		return failUsageWithFix(fs, err.Error(), func() {
			printCredentialFix(os.Stderr)
		})
	}

	ctx, stop := signalCtx()
	defer stop()

	coords := coordinates{Org: *org, Type: "RESOURCE", Name: resource, Version: *version}
	ngc := newNGCForDownload(envOr("NGC_BASE_URL", ""), *apiKey, coords)

	files, err := ngc.listVersionFiles(ctx)
	if err != nil {
		return err
	}
	selected := filterDownloadFiles(files, paths)
	if len(selected) == 0 {
		if len(paths) > 0 {
			return fmt.Errorf("no files in %s match the requested --path filter(s)", coords.uri())
		}
		return fmt.Errorf("resource version %s has no files to download", coords.uri())
	}

	if *dryRun {
		return printDownloadPlan(coords, *out, selected, *asJSON)
	}

	res := downloadResult{ArtifactURI: coords.uri(), OutDir: *out}
	opts := downloadOptions{
		OutDir:       *out,
		FileWorkers:  *fileWorkers,
		ChunkWorkers: *chunkWorkers,
		SkipExisting: *skipExisting,
	}
	if err := downloadSelected(ctx, ngc, selected, opts, *asJSON, &res); err != nil {
		return err
	}
	return printDownloadResult(res, *asJSON)
}

// downloadSelected fetches each file under out and records it in res. A path
// that would land outside out is refused before its request is made.
func downloadSelected(ctx context.Context, ngc *ngcClient, selected []ngcFileEntry, opts downloadOptions, asJSON bool, res *downloadResult) error {
	log := logf
	if asJSON {
		log = logStderr
	}
	for _, f := range selected {
		if _, err := safeJoin(opts.OutDir, f.filePath()); err != nil {
			return fmt.Errorf("refusing to download %q: %w", f.filePath(), err)
		}
	}

	// A digest is the only thing a local copy can be verified against, so say
	// so plainly rather than letting the flag look like it did something.
	if opts.SkipExisting && !listingHasChecksums(selected) {
		emitWarning(cliWarning{
			Message:    "--skip-existing was requested but this resource version reports no per-file checksums",
			Suggestion: "every file will be downloaded, since a local copy cannot be verified without one",
		})
	}

	sum, err := downloadFilesParallel(ctx, ngc, selected, opts, log)
	if err != nil {
		return err
	}
	res.Files, res.TotalBytes = sum.Files, sum.TotalBytes
	res.SkippedFiles, res.SkippedBytes = sum.SkippedFiles, sum.SkippedBytes
	return nil
}

// filterDownloadFiles keeps files matching any of paths (exact file match or
// directory prefix). With no paths, all files are kept.
func filterDownloadFiles(files []ngcFileEntry, paths []string) []ngcFileEntry {
	if len(paths) == 0 {
		return files
	}
	norm := make([]string, 0, len(paths))
	for _, p := range paths {
		norm = append(norm, strings.Trim(filepath.ToSlash(p), "/"))
	}
	var out []ngcFileEntry
	for _, f := range files {
		rel := strings.Trim(filepath.ToSlash(f.filePath()), "/")
		for _, p := range norm {
			if p == "" || rel == p || strings.HasPrefix(rel, p+"/") {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

func printDownloadPlan(coords coordinates, out string, files []ngcFileEntry, asJSON bool) error {
	if asJSON {
		res := downloadResult{ArtifactURI: coords.uri(), OutDir: out}
		for _, f := range files {
			res.Files = append(res.Files, downloadFileInfo{Path: f.filePath(), Bytes: f.fileSize()})
			res.TotalBytes += f.fileSize()
		}
		return printJSON(res)
	}
	var total int64
	for _, f := range files {
		total += f.fileSize()
	}
	fmt.Printf("[dry-run] would download %d file(s) (%s) from %s to %s:\n", len(files), humanBytes(total), coords.uri(), out)
	for _, f := range files {
		fmt.Printf("  - %s (%s)\n", f.filePath(), humanBytes(f.fileSize()))
	}
	return nil
}

func printDownloadResult(res downloadResult, asJSON bool) error {
	if asJSON {
		return printJSON(res)
	}
	fmt.Printf("Downloaded %d file(s) (%s) from %s\n",
		len(res.Files)-res.SkippedFiles, humanBytes(res.TotalBytes), res.ArtifactURI)
	if res.SkippedFiles > 0 {
		fmt.Printf("  Already up to date: %d (%s)\n", res.SkippedFiles, humanBytes(res.SkippedBytes))
	}
	fmt.Printf("  Output: %s\n", res.OutDir)
	return nil
}

func runDeleteApp(args []string) error {
	fs := flag.NewFlagSet("app delete", flag.ContinueOnError)
	appID := fs.String(flagAppID, "", "GDN application id to delete (required)")
	apiKey := ngcAPIKeyFromEnv()
	org := fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	fs.BoolVar(yes, "y", false, "skip the confirmation prompt (shorthand for --yes)")
	dryRun := fs.Bool(flagDryRun, false, "print what would be deleted, make no API calls")
	hideFlagDefaults(fs, "org")
	fs.Usage = commandHelpWithExamples(fs, "app delete", "Delete a GDN application and all of its versions.",
		[]string{
			"gdn app delete --app-id <id> --dry-run",
			"gdn app delete --app-id <id> --yes",
		},
		helpSection{"Required", []string{
			"--app-id ID             GDN application id to delete",
			helpLineOrg,
		}},
		helpSection{"Common", []string{
			"--dry-run               print what would be deleted",
			"--yes, -y               skip the confirmation prompt",
		}},
	)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}

	if *appID == "" {
		return failMissingAppID(fs)
	}
	if err := requireNGCCreds(*dryRun, *apiKey, *org); err != nil {
		return failUsageWithFix(fs, err.Error(), func() {
			printCredentialFix(os.Stderr)
		})
	}

	target := fmt.Sprintf("GDN application %s (org %s)", *appID, firstNonEmpty(*org, dryRunPlaceholder))
	if *dryRun {
		fmt.Printf("[dry-run] would delete %s\n", target)
		return nil
	}
	if ok, err := confirmDelete(*yes, target); err != nil || !ok {
		return err
	}

	ctx, stop := signalCtx()
	defer stop()
	if err := newPortal(envOr("NGC_BASE_URL", ""), *apiKey, *org, secureClient).deleteApp(ctx, *appID); err != nil {
		return err
	}
	fmt.Printf("Deleted %s\n", target)
	return nil
}

func runDeleteResource(args []string) error {
	fs := flag.NewFlagSet("resource delete", flag.ContinueOnError)
	artifact := fs.String("name", "", "NGC artifact (resource) name (defaults to --app-name slug)")
	appName := fs.String(flagAppName, "", "application display name, used to derive the artifact name")
	version := fs.String("version", "", "artifact version to delete (required unless --all-versions)")
	allVersions := fs.Bool("all-versions", false, "delete the entire resource and all of its versions")
	apiKey := ngcAPIKeyFromEnv()
	org := fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	fs.BoolVar(yes, "y", false, "skip the confirmation prompt (shorthand for --yes)")
	dryRun := fs.Bool(flagDryRun, false, "print what would be deleted, make no API calls")
	hideFlagDefaults(fs, "org")
	fs.Usage = commandHelpWithExamples(fs, "resource delete", "Delete an NGC resource version, or the whole resource with --all-versions.",
		[]string{
			"gdn resource delete --name my-app-1 --version 1.0.0 --dry-run",
			"gdn resource delete --name my-app-1 --version 1.0.0 --yes",
			"gdn resource delete --name my-app-1 --all-versions --yes",
		},
		helpSection{"Required", []string{
			"--name NAME             NGC resource name, or --app-name to derive it",
			"--version VERSION       resource version, unless --all-versions is set",
			helpLineOrg,
		}},
		helpSection{"Common", []string{
			"--all-versions          delete the whole resource",
			"--dry-run               print what would be deleted",
			"--yes, -y               skip the confirmation prompt",
		}},
	)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}

	name := firstNonEmpty(*artifact, slug(*appName))
	if name == "" {
		return failUsageWithFix(fs, "--name or --app-name is required", func() {
			printCommandHints(os.Stderr, "Fix",
				commandHint{Text: "List resources to find an artifact name", Command: cmdResourceList},
			)
		})
	}
	if err := validateNGCArtifactName(name); err != nil {
		return err
	}
	if err := validateCoordinates(coordinates{Org: *org, Version: *version}); err != nil {
		return err
	}
	if !*allVersions && *version == "" {
		return failUsageWithFix(fs, "--version is required (or pass --all-versions to delete the whole resource)", func() {
			printCommandHints(os.Stderr, "Fix",
				commandHint{Text: "List resources to confirm the version", Command: cmdResourceList},
				commandHint{Text: "Delete every version of this resource", Command: fmt.Sprintf("gdn resource delete --name %s --all-versions", name)},
			)
		})
	}
	if err := requireNGCCreds(*dryRun, *apiKey, *org); err != nil {
		return failUsageWithFix(fs, err.Error(), func() {
			printCredentialFix(os.Stderr)
		})
	}

	coords := coordinates{
		Org:     firstNonEmpty(*org, dryRunPlaceholder),
		Type:    "RESOURCE",
		Name:    name,
		Version: *version,
	}
	target := fmt.Sprintf("NGC resource version %s", coords.uri())
	if *allVersions {
		target = fmt.Sprintf("NGC resource %q and ALL of its versions (org %s)", name, coords.Org)
	}

	if *dryRun {
		fmt.Printf("[dry-run] would delete %s\n", target)
		return nil
	}
	if ok, err := confirmDelete(*yes, target); err != nil || !ok {
		return err
	}

	ctx, stop := signalCtx()
	defer stop()
	ngc := newNGC(envOr("NGC_BASE_URL", ""), *apiKey, coords, secureClient)
	if *allVersions {
		if err := ngc.deleteResource(ctx); err != nil {
			return err
		}
	} else if err := ngc.deleteVersion(ctx); err != nil {
		return err
	}
	fmt.Printf("Deleted %s\n", target)
	return nil
}

func runListResources(args []string) error {
	fs := flag.NewFlagSet("resource list", flag.ContinueOnError)
	apiKey := ngcAPIKeyFromEnv()
	org := fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	asJSON := fs.Bool("json", false, "output the resource list as JSON")
	namesOnly := fs.Bool("names-only", false, "print only resource names, one per line (script-friendly)")
	hideFlagDefaults(fs, "org")
	fs.Usage = commandHelpWithExamples(fs, "resource list", "List all NGC resources in the org.",
		[]string{
			"gdn resource list",
			"gdn resource list --names-only",
			"gdn resource list --json",
		},
		helpSection{"Required", []string{
			helpLineOrg,
		}},
		helpSection{"Common", []string{
			"--json                  output full JSON",
			"--names-only            print one resource name per line",
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
	coords := coordinates{Org: *org, Type: "RESOURCE"}
	resources, err := newNGC(envOr("NGC_BASE_URL", ""), *apiKey, coords, secureClient).listResources(ctx)
	if err != nil {
		return err
	}
	return printResourceList(resources, *asJSON, *namesOnly)
}

// resourceVersionItem is the JSON representation of one resource version.
type resourceVersionItem struct {
	Version string `json:"version"`
	Status  string `json:"status,omitempty"`
	Files   int64  `json:"files,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Created string `json:"created,omitempty"`
	Updated string `json:"updated,omitempty"`
}

func runListResourceVersions(args []string) error {
	const cmdName = "resource versions"
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	name := fs.String("name", "", "NGC resource name (required)")
	apiKey := ngcAPIKeyFromEnv()
	org := fs.String("org", envOr("NGC_ORG", ""), usageOrg)
	asJSON := fs.Bool("json", false, "output the version list as JSON")
	namesOnly := fs.Bool("names-only", false, "print only version identifiers, one per line (script-friendly)")
	hideFlagDefaults(fs, "org")
	fs.Usage = commandHelpWithExamples(fs, cmdName, "List the versions of a specific NGC resource.",
		[]string{
			"gdn resource versions --name my-app-1",
			"gdn resource versions --name my-app-1 --names-only",
			"gdn resource versions --name my-app-1 --json",
		},
		helpSection{"Required", []string{
			"--name NAME             NGC resource name",
			helpLineOrg,
		}},
		helpSection{"Common", []string{
			"--json                  output full JSON",
			"--names-only            print one version per line",
		}},
	)
	if err := fs.Parse(args); err != nil {
		return ignoreHelp(err)
	}

	resource := *name
	if resource == "" {
		return failUsageWithFix(fs, "--name is required", func() {
			printCommandHints(os.Stderr, "Fix",
				commandHint{Text: "List resources to find a name", Command: cmdResourceList},
			)
		})
	}
	if err := validateNGCArtifactName(resource); err != nil {
		return err
	}
	if err := validateCoordinates(coordinates{Org: *org}); err != nil {
		return err
	}
	if err := requireNGCCreds(false, *apiKey, *org); err != nil {
		return failUsageWithFix(fs, err.Error(), func() {
			printCredentialFix(os.Stderr)
		})
	}

	ctx, stop := signalCtx()
	defer stop()
	coords := coordinates{Org: *org, Type: "RESOURCE", Name: resource}
	versions, err := newNGC(envOr("NGC_BASE_URL", ""), *apiKey, coords, secureClient).listVersions(ctx)
	if err != nil {
		return err
	}
	return printResourceVersionList(resource, versions, *asJSON, *namesOnly)
}

func printResourceVersionList(resource string, versions []ngcResourceVersion, asJSON, namesOnly bool) error {
	if namesOnly {
		for _, v := range versions {
			fmt.Println(v.version())
		}
		return nil
	}
	if asJSON {
		items := make([]resourceVersionItem, len(versions))
		for i, v := range versions {
			items[i] = resourceVersionItem{
				Version: v.version(),
				Status:  v.Status,
				Files:   v.fileCount(),
				Bytes:   v.TotalSizeInBytes,
				Created: v.CreatedDate,
				Updated: v.UpdatedDate,
			}
		}
		return printJSON(items)
	}
	fmt.Printf("%d version(s) for resource %s:\n", len(versions), resource)
	for _, v := range versions {
		label := v.version()
		if v.Status != "" {
			label = fmt.Sprintf("%s [%s]", label, v.Status)
		}
		if v.TotalSizeInBytes > 0 {
			label = fmt.Sprintf("%s (%s)", label, humanBytes(v.TotalSizeInBytes))
		}
		fmt.Printf("  - %s\n", label)
	}
	return nil
}

var errUsageShown = errors.New("usage already printed")

var fullHelpRequested bool

func failUsage(fs *flag.FlagSet, format string, args ...any) error {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n\n", args...)
	fs.Usage()
	return errUsageShown
}

func failUsageWithFix(fs *flag.FlagSet, message string, fix func()) error {
	fmt.Fprintf(os.Stderr, "error: %s\n\n", message)
	fs.Usage()
	if fix != nil {
		fix()
	}
	return errUsageShown
}

// failMissingAppID reports an absent --app-id. Every command that takes an app
// id needs the same "list apps" nudge, so they all come through here; extra
// hints are appended after it.
func failMissingAppID(fs *flag.FlagSet, extra ...commandHint) error {
	return failUsageWithFix(fs, "--app-id is required", func() {
		hints := append([]commandHint{
			{Text: "List apps to find an app ID", Command: "gdn app list"},
		}, extra...)
		printCommandHints(os.Stderr, "Fix", hints...)
	})
}

type helpSection struct {
	Title string
	Lines []string
}

func printHelpSections(w io.Writer, sections []helpSection) {
	for i, section := range sections {
		if len(section.Lines) == 0 {
			continue
		}
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s:\n", section.Title)
		for _, line := range section.Lines {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
}

func compactHelpSections(sections []helpSection) []helpSection {
	out := make([]helpSection, 0, len(sections))
	for _, section := range sections {
		if section.Title == "Advanced" {
			continue
		}
		out = append(out, section)
	}
	return out
}

func commandHelpWithExamples(fs *flag.FlagSet, name, desc string, examples []string, sections ...helpSection) func() {
	return func() {
		printCommandHelp(os.Stderr, fs, name, desc, sections, examples, true)
	}
}

func printCommandHelp(w io.Writer, fs *flag.FlagSet, name, desc string, sections []helpSection, examples []string, showConfigNote bool) {
	fmt.Fprintf(w, "Usage: gdn %s [flags]\n\n%s\n\n", name, desc)
	if fullHelpRequested {
		printHelpSections(w, sections)
	} else {
		printHelpSections(w, compactHelpSections(sections))
	}
	if len(examples) > 0 {
		fmt.Fprintln(w, "\nExamples:")
		for _, example := range examples {
			fmt.Fprintf(w, "  %s\n", example)
		}
	}

	flagCount := 0
	fs.VisitAll(func(*flag.Flag) { flagCount++ })
	if !fullHelpRequested {
		if flagCount > 0 || showConfigNote {
			fmt.Fprintf(w, "\nMore:\n  gdn %s --help-all    show advanced options and every flag\n", name)
		}
		return
	}

	fmt.Fprintln(w, "\nAll flags:")
	if flagCount == 0 {
		fmt.Fprintln(w, "  none")
	} else {
		fs.PrintDefaults()
	}
	if showConfigNote {
		fmt.Fprintf(w, "\nCredentials and endpoints come from the org config file\n"+
			"(~/.config/gdn/config.toml, override with GDN_CONFIG_FILE) or from\n"+
			"environment variables, which take precedence. Select an org with --org:\n\n"+
			"  gdn config set --org <org> --api-key <key>\n"+
			"  gdn --org <org> %s [flags]\n", name)
	}
}

func failUploadUsage(fs *flag.FlagSet, _ string, _ []string, format string, args ...any) error {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n\n", args...)
	fs.Usage()
	return errUsageShown
}

func failUploadUsageWithFix(fs *flag.FlagSet, _ string, _ []string, message string, fix func()) error {
	fmt.Fprintf(os.Stderr, "error: %s\n\n", message)
	fs.Usage()
	if fix != nil {
		fix()
	}
	return errUsageShown
}

func failMissingRequiredUploadFlags(fs *flag.FlagSet, args []string, values map[string]string) error {
	fmt.Fprintf(os.Stderr, "error: %s\n", formatMissingFlagsError(missingFlags(values)))
	fmt.Fprintf(os.Stderr, "%s\n", formatRequiredFlagStatus(values))
	for _, hint := range flagParseHints(args, emptyFlagSet(values)) {
		fmt.Fprintf(os.Stderr, "hint: %s\n", hint)
	}
	fmt.Fprintln(os.Stderr)
	fs.Usage()
	return errUsageShown
}

func signalCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func logf(format string, args ...any) { fmt.Printf(format+"\n", args...) }

func logStderr(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }

// confirmDelete prompts unless skip; only y/yes proceeds.
func confirmDelete(skip bool, target string) (bool, error) {
	if skip {
		return true, nil
	}
	fmt.Fprintf(os.Stderr, "Delete %s? This cannot be undone. [y/N]: ", target)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		fmt.Println("aborted.")
		return false, nil
	}
}

func launchHelp(fs *flag.FlagSet) func() {
	return func() {
		printCommandHelp(os.Stderr, fs, "app launch", "Obtain a nonce and open a GDN session URL.",
			[]helpSection{
				{"Required", []string{
					"--app-id ID             GDN application id",
				}},
				{"Common", []string{
					"--open=false            print the launch URL instead of opening a browser",
					"--version-id ID         launch a specific app version; omit to use the default version",
					"--org ORG               NGC org name; usually taken from config (env NGC_ORG)",
				}},
				{"Advanced", []string{
					"--app-features JSON             override applicationFeatures for this session as JSON (inline, @file, or -)",
					"--partner-secure-data STR       partnerSecureData string bound to the session nonce",
					"--stream-lookup-id ID           streamLookupId bound to the session nonce",
					"--user-id ID                    userId bound to the session nonce",
					"--upstream-media-type TYPE      optional upstream media type, e.g. AUDIO",
					"--session-host HOST             route the session to a specific host (env GDN_SESSION_HOST)",
					"--preset NAME                  add a preset query param to the client URL",
				}},
			},
			[]string{
				"gdn app launch --app-id <id>",
				"gdn app launch --app-id <id> --open=false",
				"gdn app launch --app-id <id> --app-features '{\"MAX_BIT_RATE\":50}' --partner-secure-data <secure-data>",
			},
			true,
		)
	}
}

var (
	uploadRequired = []string{
		"--dir PATH                folder to upload",
		"--version VERSION         artifact version, e.g. 1.0.0",
		"--name NAME               NGC artifact name",
	}
	updateRequired = []string{
		"--name NAME               NGC artifact name",
		"--base-version VERSION    existing version to inherit",
		"--version VERSION         new version to create",
		"--file PATH and/or --dir  files to add or overwrite",
	}
	createAppRequired = []string{
		"--dir PATH                app folder to upload",
		"--app-name NAME           application name (letters, digits, - and _)",
		"--version VERSION         artifact/app version, e.g. 1.0.0",
	}
	addVersionRequired = []string{
		"--dir PATH                app folder to upload",
		"--version VERSION         artifact/app version, e.g. 1.0.0",
		"--app-id ID               existing GDN application id",
		"--app-name or --name      NGC artifact name",
	}
	uploadPerformanceAdvanced = []string{
		"--hash-workers int        parallel file hashing during scan (default 32; 0 = one per CPU)",
		"--file-workers int        parallel file upload sessions (default 20; auto-throttles on NGC 429s)",
		"--multipart-workers int   parallel S3 part uploads per file (default 8; 0 = one per CPU)",
	}
	createAppHelpSections = []helpSection{
		{"Required", createAppRequired},
		{"Common", append([]string{
			"--resource NAME:VER     reference an already-uploaded resource as [team/]name:version (repeatable; use instead of/with --dir)",
			"--exec-path PATH        executable path inside the startup resource (with --resource)",
			"--instances NAMES       comma-separated instance names; defaults to every name GDN offers",
			"--executable PATH       executable path relative to --dir",
			"--exec-args ARGS        comma-separated launch arguments",
			"--env-var KEY:VALUE     launch environment variable (repeatable)",
			helpLineDryRunScan,
			helpLineJSONMachine,
		}, uploadPerformanceAdvanced...)},
		{"Advanced", []string{
			"--display-mode RES@FPS  allowed display mode, e.g. 4k@60 (repeatable; GDN's default when omitted)",
			"--app-features JSON     app-level applicationFeatures as JSON (inline, @file, or -)",
			"--launch-config JSON    per-version launchConfig as JSON (inline, @file, or -)",
			"--startup-resource NAME resource that holds the startup file (defaults to uploaded, else first --resource)",
			"--seat-type TYPE        WINDOWS | LINUX | LINUX_PROTON",
			"--description TEXT      application description",
			"--version-name NAME     app version name; defaults to --version",
			"--name NAME             NGC artifact name; defaults to app-name slug",
			helpLineNoSetLatest,
			"--allow-duplicate       create even if an app with the same name exists",
		}},
	}
	addVersionHelpSections = []helpSection{
		{"Required", addVersionRequired},
		{"Common", append([]string{
			"--resource NAME:VER     reference an already-uploaded resource as [team/]name:version (repeatable; use instead of/with --dir)",
			"--exec-path PATH        executable path inside the startup resource (with --resource)",
			"--executable PATH       executable path relative to --dir",
			"--exec-args ARGS        comma-separated launch arguments",
			"--env-var KEY:VALUE     launch environment variable (repeatable)",
			helpLineDryRunScan,
			helpLineJSONMachine,
		}, uploadPerformanceAdvanced...)},
		{"Advanced", []string{
			"--launch-config JSON    per-version launchConfig as JSON (inline, @file, or -)",
			"--startup-resource NAME resource that holds the startup file (defaults to uploaded, else first --resource)",
			"--version-name NAME     app version name; defaults to --version",
			helpLineNoSetLatest,
		}},
	}
	uploadHelpSections = []helpSection{
		{"Required", uploadRequired},
		{"Common", append([]string{
			"--display-name NAME     NGC resource display name",
			"--description TEXT      NGC resource/version description",
			helpLineDryRunScan,
			helpLineJSONMachine,
		}, uploadPerformanceAdvanced...)},
		{"Advanced", []string{
			helpLineNoSetLatest,
		}},
	}
	updateHelpSections = []helpSection{
		{"Required", updateRequired},
		{"Common", append([]string{
			"--file PATH             file to add (PATH or LOCAL=REMOTE); repeatable",
			"--dir PATH              folder of files to add",
			"--display-name NAME     NGC resource display name",
			"--description TEXT      NGC resource/version description",
			helpLineDryRunScan,
			helpLineJSONMachine,
		}, uploadPerformanceAdvanced...)},
		{"Advanced", []string{
			helpLineNoSetLatest,
		}},
	}
)

func uploadHelp(fs *flag.FlagSet, name, desc string, sections []helpSection, examples []string) func() {
	return func() {
		printCommandHelp(os.Stderr, fs, name, desc, sections, examples, true)
	}
}

func ignoreHelp(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

// ignoredFlagsNote warns about explicitly-set flags with no effect in mode.
func ignoredFlagsWarning(fs *flag.FlagSet, mode string, names ...string) (cliWarning, bool) {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var ignored []string
	for _, n := range names {
		if set[n] {
			ignored = append(ignored, "--"+n)
		}
	}
	if len(ignored) == 0 {
		return cliWarning{}, false
	}
	return cliWarning{Message: fmt.Sprintf("%s ignored when %s", strings.Join(ignored, ", "), mode)}, true
}

func ignoredFlagsNote(fs *flag.FlagSet, mode string, names ...string) string {
	w, ok := ignoredFlagsWarning(fs, mode, names...)
	if !ok {
		return ""
	}
	return formatWarning(w)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ngcAPIKeyFromEnv returns the NGC API key from the environment. There is no
// --api-key command flag; the key comes from NGC_API_KEY, which the config
// loader may have populated from the selected org's stored config. Returns a
// *string so callers can share the same code paths as the other credentials.
func ngcAPIKeyFromEnv() *string {
	t := envOr("NGC_API_KEY", "")
	return &t
}

func hideFlagDefault(fs *flag.FlagSet, name string) {
	if f := fs.Lookup(name); f != nil {
		f.DefValue = ""
	}
}

func hideFlagDefaults(fs *flag.FlagSet, names ...string) {
	for _, name := range names {
		hideFlagDefault(fs, name)
	}
}

func setFlagUsage(fs *flag.FlagSet, name, usage string) {
	if f := fs.Lookup(name); f != nil {
		f.Usage = usage
	}
}

func requireNGCCreds(dryRun bool, apiKey, org string) error {
	if dryRun {
		return nil
	}
	if org == "" {
		return fmt.Errorf("no org specified; pass --org <org> or set a default with `gdn config use <org>`")
	}
	if apiKey == "" {
		return fmt.Errorf("credentials not present for org %q; store an API key with `gdn config set --org %s --api-key <key>` or export NGC_API_KEY", org, org)
	}
	return nil
}

type stringList []string // comma-separated repeatable flag

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

// fileSpecList is a repeatable --file flag that does not split on commas
// (file paths may contain commas).
type fileSpecList []string

func (s *fileSpecList) String() string { return strings.Join(*s, ", ") }
func (s *fileSpecList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
