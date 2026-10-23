// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// gdnGatewayBaseURL is the GDN API Gateway, used only for launch (the GIO
// nonce and session server). App management (this portal client) lives on the
// NGC base URL instead.
const gdnGatewayBaseURL = "https://api.gdn.nvidia.com"

// portalClient is the GDN apps API (/v2/orgs/{org}/gdn/apps). It is served by
// the NGC gateway and uses the same NGC API key, so it defaults to the NGC base
// URL rather than the GDN launch gateway.
type portalClient struct {
	baseURL string
	apiKey  string
	org     string
	http    *http.Client
	// createConfig holds the first successful create-config answer, so one
	// command reads it once however many checks use it.
	createConfig *appCreateConfig
}

func newPortal(baseURL, apiKey, org string, hc *http.Client) *portalClient {
	return &portalClient{baseURL: strings.TrimRight(firstNonEmpty(baseURL, ngcDefaultBaseURL), "/"), apiKey: apiKey, org: org, http: hc}
}

type displayMode struct {
	MaxResolution string `json:"MAX_RESOLUTION"`
	MaxFPS        int64  `json:"MAX_FPS"`
}

// appFeature is the applicationFeatures object. Every field is optional so a
// request carries only what the caller set: GDN fills an omitted field with
// its default on create and keeps the stored value on update.
type appFeature struct {
	DisplayMode              []displayMode       `json:"DISPLAY_MODE,omitempty"`
	MaxBitDepth              *uint64             `json:"MAX_BIT_DEPTH,omitempty"`
	SupportedChromaFormat    string              `json:"SUPPORTED_CHROMA_FORMAT,omitempty"`
	QosPolicy                string              `json:"QOS_POLICY,omitempty"`
	MaxBitRate               *uint64             `json:"MAX_BIT_RATE,omitempty"`
	TouchSupport             *bool               `json:"TOUCH_SUPPORT,omitempty"`
	RuntimeDisplayModeChange *bool               `json:"RUNTIME_DISPLAY_MODE_CHANGE,omitempty"`
	IdleTimeout              *uint64             `json:"IDLE_TIMEOUT,omitempty"`
	SessionTimeLimit         *uint64             `json:"SESSION_TIME_LIMIT,omitempty"`
	InstanceReusePolicy      string              `json:"INSTANCE_REUSE_POLICY,omitempty"`
	TransportPolicy          *transportPolicy    `json:"TRANSPORT_POLICY,omitempty"`
	CoverageAreaPolicy       *coverageAreaPolicy `json:"COVERAGE_AREA_POLICY,omitempty"`
}

// transportPolicy is applicationFeatures.TRANSPORT_POLICY. All three members
// are required, so it is always sent whole.
type transportPolicy struct {
	Policy        string `json:"POLICY"`
	RelayProtocol string `json:"RELAY_PROTOCOL"`
	RelayLocation string `json:"RELAY_LOCATION"`
}

// coverageAreaPolicy is applicationFeatures.COVERAGE_AREA_POLICY. POLICY is
// required, and COVERAGE_AREAS applies only when POLICY is CUSTOM.
type coverageAreaPolicy struct {
	Policy        string   `json:"POLICY"`
	CoverageAreas []string `json:"COVERAGE_AREAS,omitempty"`
}

// describe renders the policy, with its areas when it is CUSTOM.
func (p coverageAreaPolicy) describe() string {
	if p.Policy == "CUSTOM" {
		return fmt.Sprintf("CUSTOM (%s)", strings.Join(p.CoverageAreas, ", "))
	}
	return p.Policy
}

// Enum values GDN accepts.
var (
	seatTypes             = []string{"WINDOWS", "LINUX", "LINUX_PROTON"}
	chromaFormats         = []string{"YUV444", "YUV420"}
	qosPolicies           = []string{"QUALITY", "LATENCY"}
	instanceReusePolicies = []string{"PERFORMANCE_OPTIMIZED", "BALANCED", "SECURITY_OPTIMIZED"}
	transportPolicies     = []string{"ALL", "DIRECT", "RELAY"}
	relayProtocols        = []string{"ALL", "UDP", "TCP"}
	// relayLocations is what the GDN backend accepts, a subset of the
	// protocols enum.
	relayLocations   = []string{"CSP", "ZONE"}
	coveragePolicies = []string{"ALL", "CUSTOM"}
)

// enumCheck is one field value and the values GDN accepts for it. An empty
// value leaves the field unset and passes.
type enumCheck struct {
	field, value string
	allowed      []string
}

func (c enumCheck) err() error {
	if c.value == "" || slices.Contains(c.allowed, c.value) {
		return nil
	}
	return fmt.Errorf("%s must be one of %s, got %q", c.field, strings.Join(c.allowed, ", "), c.value)
}

func checkSeatType(seatType string) error {
	return enumCheck{"seat type", seatType, seatTypes}.err()
}

// validate reports applicationFeatures values GDN would reject: an unknown
// enum value, or a policy object with members missing.
func (f *appFeature) validate() error {
	for _, m := range f.DisplayMode {
		if m.MaxResolution == "" || m.MaxFPS <= 0 {
			return fmt.Errorf("each DISPLAY_MODE entry needs MAX_RESOLUTION and MAX_FPS, got %+v", m)
		}
	}
	if f.MaxBitDepth != nil && *f.MaxBitDepth != 8 && *f.MaxBitDepth != 10 {
		return fmt.Errorf("MAX_BIT_DEPTH must be 8 or 10, got %d", *f.MaxBitDepth)
	}
	checks := []enumCheck{
		{"SUPPORTED_CHROMA_FORMAT", f.SupportedChromaFormat, chromaFormats},
		{"QOS_POLICY", f.QosPolicy, qosPolicies},
		{"INSTANCE_REUSE_POLICY", f.InstanceReusePolicy, instanceReusePolicies},
	}
	policyChecks, err := f.policyChecks()
	if err != nil {
		return err
	}
	for _, c := range append(checks, policyChecks...) {
		if err := c.err(); err != nil {
			return err
		}
	}
	return nil
}

// policyChecks reports a policy object with members missing, and otherwise
// returns the enum checks for the policy members that are set.
func (f *appFeature) policyChecks() ([]enumCheck, error) {
	var checks []enumCheck
	if tp := f.TransportPolicy; tp != nil {
		if tp.Policy == "" || tp.RelayProtocol == "" || tp.RelayLocation == "" {
			return nil, fmt.Errorf("TRANSPORT_POLICY needs POLICY, RELAY_PROTOCOL and RELAY_LOCATION together")
		}
		checks = append(checks,
			enumCheck{"TRANSPORT_POLICY.POLICY", tp.Policy, transportPolicies},
			enumCheck{"TRANSPORT_POLICY.RELAY_PROTOCOL", tp.RelayProtocol, relayProtocols},
			enumCheck{"TRANSPORT_POLICY.RELAY_LOCATION", tp.RelayLocation, relayLocations},
		)
	}
	if cp := f.CoverageAreaPolicy; cp != nil {
		if cp.Policy == "" {
			return nil, fmt.Errorf("COVERAGE_AREA_POLICY needs POLICY (ALL or CUSTOM)")
		}
		if cp.Policy == "CUSTOM" && len(cp.CoverageAreas) == 0 {
			return nil, fmt.Errorf("COVERAGE_AREA_POLICY with POLICY CUSTOM needs COVERAGE_AREAS")
		}
		checks = append(checks, enumCheck{"COVERAGE_AREA_POLICY.POLICY", cp.Policy, coveragePolicies})
	}
	return checks, nil
}

// isEmpty reports whether no field is set.
func (f appFeature) isEmpty() bool {
	b, err := json.Marshal(f)
	return err == nil && string(b) == "{}"
}

// summary lists the fields that are set, for display.
func (f appFeature) summary() string {
	var parts []string
	for _, m := range f.DisplayMode {
		parts = append(parts, m.String())
	}
	if f.MaxBitDepth != nil {
		parts = append(parts, fmt.Sprintf("%d-bit", *f.MaxBitDepth))
	}
	if f.SupportedChromaFormat != "" {
		parts = append(parts, f.SupportedChromaFormat)
	}
	if f.QosPolicy != "" {
		parts = append(parts, "QoS "+f.QosPolicy)
	}
	if f.MaxBitRate != nil {
		parts = append(parts, fmt.Sprintf("%d Mbps", *f.MaxBitRate))
	}
	if f.TouchSupport != nil {
		parts = append(parts, fmt.Sprintf("touch %t", *f.TouchSupport))
	}
	if f.RuntimeDisplayModeChange != nil {
		parts = append(parts, fmt.Sprintf("runtime display mode change %t", *f.RuntimeDisplayModeChange))
	}
	if f.IdleTimeout != nil {
		parts = append(parts, fmt.Sprintf("idle %ds", *f.IdleTimeout))
	}
	if f.SessionTimeLimit != nil {
		parts = append(parts, fmt.Sprintf("session %ds", *f.SessionTimeLimit))
	}
	if f.InstanceReusePolicy != "" {
		parts = append(parts, "reuse "+f.InstanceReusePolicy)
	}
	if tp := f.TransportPolicy; tp != nil {
		parts = append(parts, fmt.Sprintf("transport %s/%s/%s", tp.Policy, tp.RelayProtocol, tp.RelayLocation))
	}
	if cp := f.CoverageAreaPolicy; cp != nil {
		parts = append(parts, "coverage "+cp.describe())
	}
	return strings.Join(parts, ", ")
}

// launchEnvVar is a single environment variable injected when the app
// executable is launched on the seat (GDN launchConfig.environment item).
type launchEnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// launchConfig mirrors the GDN Portal API launchConfig schema. Fields beyond
// executablePath/executableArgs (environment, log/crashdump paths) are set via
// the --launch-config JSON document; they pass through publish untouched.
type launchConfig struct {
	ExecutablePath    string         `json:"executablePath"`
	ExecutableArgs    []string       `json:"executableArgs,omitempty"`
	Environment       []launchEnvVar `json:"environment,omitempty"`
	AppLogPaths       []string       `json:"appLogPaths,omitempty"`
	AppCrashdumpPaths []string       `json:"appCrashdumpPaths,omitempty"`
	AppReadyEnabled   bool           `json:"appReadyEnabled,omitempty"`
}

type artifactRef struct {
	// ID is the backend artifact id, returned on reads and create responses.
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	OrgName string `json:"orgName,omitempty"`
	// TeamName is the NGC team that owns a team-scoped artifact; empty for an
	// org-scoped one.
	TeamName  string `json:"teamName,omitempty"`
	Type      string `json:"type"`
	VersionID string `json:"versionId"`
	// URI is returned by the GDN version GET (in place of orgName) and is
	// accepted by the create endpoint; preserved so a clone references the
	// exact same NGC resource version.
	URI string `json:"uri,omitempty"`
}

// ngcURI renders the reference as ngc://<org>/[<team>/]<name>:<version>.
func (a artifactRef) ngcURI() string {
	if a.TeamName == "" {
		return fmt.Sprintf("ngc://%s/%s:%s", a.OrgName, a.Name, a.VersionID)
	}
	return fmt.Sprintf("ngc://%s/%s/%s:%s", a.OrgName, a.TeamName, a.Name, a.VersionID)
}

type createVersionReq struct {
	VersionName  string        `json:"versionName,omitempty"`
	LaunchConfig launchConfig  `json:"launchConfig"`
	Artifacts    []artifactRef `json:"artifacts"`
}

type createAppReq struct {
	Name                string           `json:"name"`
	Description         string           `json:"description,omitempty"`
	SeatType            string           `json:"seatType"`
	InstanceNames       []string         `json:"instanceNames"`
	ApplicationFeatures *appFeature      `json:"applicationFeatures,omitempty"`
	Version             createVersionReq `json:"version"`
}

type gdnVersion struct {
	ID           string        `json:"id"`
	VersionName  string        `json:"versionName"`
	Status       string        `json:"status"`
	LaunchConfig launchConfig  `json:"launchConfig"`
	Artifacts    []artifactRef `json:"artifacts"`
}

type listApplicationVersionsResponse struct {
	Versions []gdnVersion `json:"versions"`
}

func (r *listApplicationVersionsResponse) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return json.Unmarshal(data, &r.Versions)
	}

	type response listApplicationVersionsResponse
	var decoded response
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if decoded.Versions == nil {
		return fmt.Errorf("application version list response is missing versions")
	}
	*r = listApplicationVersionsResponse(decoded)
	return nil
}

type gdnApp struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	Description         string     `json:"description"`
	SeatType            string     `json:"seatType"`
	InstanceNames       []string   `json:"instanceNames"`
	ApplicationFeatures appFeature `json:"applicationFeatures"`
	DefaultVersionID    string     `json:"defaultVersionId"`
	// VersionCount is the server's total version count. Versions, by
	// contrast, embeds only the current (default) version — never the full
	// list — so len(Versions) is at most 1 and must not be used as a count.
	VersionCount int          `json:"versionCount"`
	Versions     []gdnVersion `json:"versions"`
}

func (a *gdnApp) initialVersionID() string {
	if len(a.Versions) > 0 && a.Versions[0].ID != "" {
		return a.Versions[0].ID
	}
	return a.DefaultVersionID
}

// defaultVersion returns the app's current (default) version from the
// embedded Versions list (or the first one as a fallback), nil if the app
// payload embeds no version.
func (a *gdnApp) defaultVersion() *gdnVersion {
	for i := range a.Versions {
		if a.Versions[i].ID == a.DefaultVersionID && a.DefaultVersionID != "" {
			return &a.Versions[i]
		}
	}
	if len(a.Versions) > 0 {
		return &a.Versions[0]
	}
	return nil
}

// appCreateConfig is the create-config payload: the values GDN currently
// accepts when an application is created or updated.
type appCreateConfig struct {
	// AllowedInstanceNames comes from GDN's live instance catalogue. It is
	// empty, with a 200, while that catalogue is unavailable.
	AllowedInstanceNames []string `json:"allowedInstanceNames"`
	// AllowedDisplayModes lists every resolution and FPS pair GDN accepts in
	// DISPLAY_MODE.
	AllowedDisplayModes []displayMode `json:"allowedDisplayModes"`
	// DefaultApplicationFeatures is what GDN fills in for any applicationFeatures
	// field a create request leaves out.
	DefaultApplicationFeatures appFeature `json:"defaultApplicationFeatures"`
}

// getCreateConfig fetches create-config. Its path has no org segment: the
// catalogue is the same for every org.
func (c *portalClient) getCreateConfig(ctx context.Context) (*appCreateConfig, error) {
	if c.createConfig != nil {
		return c.createConfig, nil
	}
	var out appCreateConfig
	if err := c.get(ctx, c.baseURL+"/v2/gdn/apps/create-config", &out); err != nil {
		return nil, fmt.Errorf("get application create config: %w", err)
	}
	c.createConfig = &out
	return &out, nil
}

// resolveInstanceNames returns the instance names to send. With none
// requested it returns every name GDN offers, which needs create-config to
// answer with a non-empty list. Requested names are checked against that list;
// when the list cannot be read they are sent unchecked and GDN validates them.
func (c *portalClient) resolveInstanceNames(ctx context.Context, requested []string) ([]string, error) {
	cfg, err := c.getCreateConfig(ctx)
	if len(requested) == 0 {
		if err != nil {
			return nil, fmt.Errorf("%w; pass --instances to choose instance names", err)
		}
		if len(cfg.AllowedInstanceNames) == 0 {
			return nil, fmt.Errorf("GDN offers no instance names right now (its instance catalogue is unavailable); pass --instances")
		}
		return cfg.AllowedInstanceNames, nil
	}
	if err != nil {
		warnf("could not check instance names against GDN (%v); sending them unchecked", err)
		return requested, nil
	}
	offered := cfg.AllowedInstanceNames
	if len(offered) == 0 {
		warnf("GDN's instance catalogue is unavailable; sending instance names unchecked")
		return requested, nil
	}
	var unknown []string
	for _, name := range requested {
		if !slices.Contains(offered, name) {
			unknown = append(unknown, fmt.Sprintf("%q", name))
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown instance name(s) %s; GDN currently offers: %s",
			strings.Join(unknown, ", "), strings.Join(offered, ", "))
	}
	return requested, nil
}

func (c *portalClient) listApps(ctx context.Context) ([]gdnApp, error) {
	var out struct {
		Applications []gdnApp `json:"applications"`
	}
	if err := c.get(ctx, c.appsURL(""), &out); err != nil {
		return nil, fmt.Errorf("list applications: %w", err)
	}
	return out.Applications, nil
}

func (c *portalClient) findAppByName(ctx context.Context, name string) (*gdnApp, error) {
	apps, err := c.listApps(ctx)
	if err != nil {
		return nil, err
	}
	for i := range apps {
		if strings.EqualFold(apps[i].Name, name) {
			return &apps[i], nil
		}
	}
	return nil, nil
}

func (c *portalClient) getApp(ctx context.Context, appID string) (*gdnApp, error) {
	var out gdnApp
	if err := c.get(ctx, c.appsURL(appID), &out); err != nil {
		return nil, fmt.Errorf("get application: %w", err)
	}
	return &out, nil
}

func (c *portalClient) getVersion(ctx context.Context, appID, versionID string) (*gdnVersion, error) {
	var out gdnVersion
	if err := c.get(ctx, c.appsURL(appID+"/versions/"+versionID), &out); err != nil {
		return nil, fmt.Errorf("get application version: %w", err)
	}
	return &out, nil
}

// getAppVersions returns every version of an application. The app payload
// itself embeds only the current (default) version, so the full list comes
// from the dedicated versions endpoint.
func (c *portalClient) getAppVersions(ctx context.Context, appID string) ([]gdnVersion, error) {
	var out listApplicationVersionsResponse
	if err := c.get(ctx, c.appsURL(appID+"/versions"), &out); err != nil {
		return nil, fmt.Errorf("list application versions: %w", err)
	}
	return out.Versions, nil
}

// listVersions returns an application's versions and its current default
// (production) version id. The app payload carries defaultVersionId but
// embeds only the current version, so the full list and the default id come
// from two calls: the versions endpoint and the app GET.
func (c *portalClient) listVersions(ctx context.Context, appID string) ([]gdnVersion, string, error) {
	app, err := c.getApp(ctx, appID)
	if err != nil {
		return nil, "", err
	}
	versions, err := c.getAppVersions(ctx, appID)
	if err != nil {
		return nil, "", err
	}
	return versions, app.DefaultVersionID, nil
}

func (c *portalClient) createApp(ctx context.Context, req createAppReq) (*gdnApp, error) {
	var out gdnApp
	if err := c.post(ctx, c.appsURL(""), req, &out); err != nil {
		return nil, fmt.Errorf("create application: %w", err)
	}
	return &out, nil
}

func (c *portalClient) createVersion(ctx context.Context, appID string, req createVersionReq) (*gdnVersion, error) {
	var out gdnVersion
	if err := c.post(ctx, c.appsURL(appID+"/versions"), req, &out); err != nil {
		return nil, fmt.Errorf("create application version: %w", err)
	}
	return &out, nil
}

func (c *portalClient) deleteApp(ctx context.Context, appID string) error {
	if err := c.delete(ctx, c.appsURL(appID)); err != nil {
		return fmt.Errorf("delete application: %w", err)
	}
	return nil
}

// setDefaultVersion PATCHes the app to point defaultVersionId at versionID.
func (c *portalClient) setDefaultVersion(ctx context.Context, appID, versionID string) error {
	type req struct {
		DefaultVersionID string `json:"defaultVersionId"`
	}
	if err := c.patch(ctx, c.appsURL(appID), req{DefaultVersionID: versionID}, nil); err != nil {
		return fmt.Errorf("set default version: %w", err)
	}
	return nil
}

type updateAppReq struct {
	Name                string      `json:"name,omitempty"`
	Description         string      `json:"description,omitempty"`
	SeatType            string      `json:"seatType,omitempty"`
	InstanceNames       []string    `json:"instanceNames,omitempty"`
	ApplicationFeatures *appFeature `json:"applicationFeatures,omitempty"`
}

// isEmpty reports whether the request would send no changes at all.
func (r updateAppReq) isEmpty() bool {
	return r.Name == "" && r.Description == "" && r.SeatType == "" &&
		len(r.InstanceNames) == 0 && r.ApplicationFeatures == nil
}

func (c *portalClient) updateApp(ctx context.Context, appID string, req updateAppReq) (*gdnApp, error) {
	var out gdnApp
	if err := c.patch(ctx, c.appsURL(appID), req, &out); err != nil {
		return nil, fmt.Errorf("update application: %w", err)
	}
	return &out, nil
}

func (c *portalClient) appsURL(suffix string) string {
	url := fmt.Sprintf("%s/v2/orgs/%s/gdn/apps", c.baseURL, c.org)
	if suffix != "" {
		url += "/" + suffix
	}
	return url
}

func (c *portalClient) get(ctx context.Context, url string, out any) error {
	return httpJSON(ctx, c.http, c.apiKey, http.MethodGet, url, nil, out)
}

func (c *portalClient) post(ctx context.Context, url string, in, out any) error {
	return httpJSON(ctx, c.http, c.apiKey, http.MethodPost, url, in, out)
}

func (c *portalClient) patch(ctx context.Context, url string, in, out any) error {
	return httpJSON(ctx, c.http, c.apiKey, http.MethodPatch, url, in, out)
}

func (c *portalClient) delete(ctx context.Context, url string) error {
	return httpJSON(ctx, c.http, c.apiKey, http.MethodDelete, url, nil, nil)
}
